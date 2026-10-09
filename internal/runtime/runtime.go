// Package runtime opens kairo, declares KairoCode's actions and workflows on
// it, and gives the server and the CLI a small API over sessions.
package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"
	_ "modernc.org/sqlite"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

type Config struct {
	// DB is the SQLite file kairo and the message store share.
	DB string
	// Engine runs the model loop: workflows.EngineAPI (the default) or
	// workflows.EngineClaudeCode (ADR 0007).
	Engine string
	// Provider is the model provider of the API engine.
	Provider provider.Provider
	// ClaudePath is the claude command of the Claude Code engine.
	ClaudePath string
	// ExtraTools are offered besides the built-in ones (tests).
	ExtraTools []tools.Def
	// Model is the model an agent uses unless it names one.
	Model    string
	MaxSteps int
	// Owner names this process in kairo's leases. A process that opens with
	// the name its earlier self had takes over what that self left running.
	Owner string
	// Lease is how long kairo leases a step to this process (kairo's default
	// when zero).
	Lease  time.Duration
	Logger *slog.Logger
}

type Runtime struct {
	K     *kairo.Kairo
	Store *store.Store
	db    *sql.DB
	cfg   Config
	mcp   *mcpServer

	sendMu sync.Mutex

	mu      sync.Mutex
	changed chan struct{} // closed and replaced whenever a run starts or settles here
}

// DefaultDB is where a user's sessions live.
func DefaultDB() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "kairocode", "kairo.db")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "kairocode", "kairo.db")
}

func Open(ctx context.Context, cfg Config) (*Runtime, error) {
	switch cfg.Engine {
	case "", workflows.EngineAPI:
		cfg.Engine = workflows.EngineAPI
		if cfg.Provider == nil {
			return nil, errors.New("runtime: the API engine needs a model provider")
		}
		if cfg.Model == "" {
			cfg.Model = provider.DefaultModel
		}
	case workflows.EngineClaudeCode:
		// The model is Claude Code's own default unless one is named.
		if cfg.ClaudePath == "" {
			cfg.ClaudePath = "claude"
		}
	default:
		return nil, fmt.Errorf("runtime: unknown engine %q", cfg.Engine)
	}
	if cfg.MaxSteps == 0 {
		cfg.MaxSteps = 50
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DB), 0o700); err != nil {
		return nil, err
	}
	// Every pooled connection gets the busy timeout and WAL, not just the
	// first one.
	db, err := sql.Open("sqlite", "file:"+cfg.DB+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	r := &Runtime{db: db, cfg: cfg, changed: make(chan struct{})}
	fail := func(err error) (*Runtime, error) {
		db.Close()
		return nil, err
	}
	if r.Store, err = store.New(ctx, db); err != nil {
		return fail(err)
	}
	ks, err := kairo.NewSQLStore(db, kairo.SQLite)
	if err != nil {
		return fail(err)
	}
	r.K, err = kairo.Open(ctx, kairo.Options{
		Store:   ks,
		Owner:   cfg.Owner,
		Lease:   cfg.Lease,
		Logger:  cfg.Logger,
		Observe: r.observe,
	})
	if err != nil {
		return fail(err)
	}
	if r.mcp, err = startMCP(r); err != nil {
		r.K.Close()
		return fail(err)
	}
	r.declare()
	if err := r.K.Start(ctx); err != nil {
		r.mcp.close()
		r.K.Close()
		return fail(err)
	}
	return r, nil
}

func (r *Runtime) Close() error {
	err := r.mcp.close()
	err = errors.Join(err, r.K.Close())
	return errors.Join(err, r.db.Close())
}

// toolDefs is every tool this runtime offers.
func (r *Runtime) toolDefs() []tools.Def {
	return append(tools.Builtin(), r.cfg.ExtraTools...)
}

func (r *Runtime) tool(name string) (tools.Def, bool) {
	for _, d := range r.toolDefs() {
		if d.Name == name {
			return d, true
		}
	}
	return tools.Def{}, false
}

func (r *Runtime) observe(o kairo.Observation) {
	if o.Kind != kairo.ObsRunStarted && o.Kind != kairo.ObsRunSettled {
		return
	}
	r.mu.Lock()
	close(r.changed)
	r.changed = make(chan struct{})
	r.mu.Unlock()
}

// changes is closed at the next run that starts or settles here.
func (r *Runtime) changes() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.changed
}

// effect is how kairo treats a tool of effect e (AGENTS.md, invariant 3).
func effect(e tools.Effect) (kairo.Effect, []kairo.ActionOption) {
	switch e {
	case tools.Read:
		return kairo.Unprotected, nil
	case tools.Idempotent:
		return kairo.Real, []kairo.ActionOption{kairo.IdempotentRetry()}
	default:
		return kairo.Real, nil
	}
}

func (r *Runtime) declare() {
	kairo.Workflow(r.K, workflows.Session, workflows.RunSession, kairo.WorkflowVersion(workflows.SessionVersion))
	kairo.Workflow(r.K, workflows.Turn, workflows.RunTurn, kairo.WorkflowVersion(workflows.TurnVersion))
	kairo.Workflow(r.K, workflows.MCPCall, workflows.RunMCPCall, kairo.WorkflowVersion(workflows.MCPCallVersion))
	kairo.Action(r.K, workflows.ActClaudeTurn, kairo.Unprotected, r.claudeTurn, kairo.Timeout(time.Hour))
	kairo.Action(r.K, workflows.ActPermissionCheck, kairo.Unprotected, r.permissionCheck)
	kairo.Action(r.K, workflows.ActToolDenied, kairo.Unprotected, r.toolDenied)

	kairo.Action(r.K, workflows.ActAgentLoad, kairo.Unprotected, r.agentLoad)
	kairo.Action(r.K, workflows.ActModel, kairo.Unprotected, r.model, kairo.Timeout(10*time.Minute))
	kairo.Action(r.K, workflows.ActHistoryExtend, kairo.Unprotected,
		func(t *kairo.TaskContext, in workflows.HistoryExtendInput) (string, error) {
			return r.Store.Extend(t, in.Prev, in.Messages)
		})
	kairo.Action(r.K, workflows.ActToolMissing, kairo.Unprotected,
		func(t *kairo.TaskContext, in workflows.ToolInput) (workflows.ToolOutput, error) {
			call, err := r.toolCall(t, in)
			if err != nil {
				return workflows.ToolOutput{}, err
			}
			return r.toolResult(t, in.Call, tools.Result{Output: "no such tool: " + call.Name, IsError: true})
		})
	kairo.Action(r.K, workflows.ActToolSkipped, kairo.Unprotected,
		func(t *kairo.TaskContext, in workflows.ToolInput) (workflows.ToolOutput, error) {
			return r.toolResult(t, in.Call, tools.Result{Output: "not run: the reply that asked for it was cut off", IsError: true})
		})
	for _, d := range r.toolDefs() {
		if d.Plan != nil {
			kairo.Action(r.K, workflows.ActToolPrefix+d.Name+workflows.ActPlanSuffix, kairo.Unprotected,
				func(t *kairo.TaskContext, in workflows.ToolInput) (workflows.PlanOutput, error) {
					return r.planTool(t, d, in)
				})
			if d.Apply != nil {
				eff, opts := effect(d.Effect)
				kairo.Action(r.K, workflows.ActToolPrefix+d.Name+workflows.ActApplySuffix, eff,
					func(t *kairo.TaskContext, in workflows.PlanApplyInput) (workflows.ToolOutput, error) {
						return r.applyPlan(t, d, in)
					}, opts...)
			}
			continue
		}
		eff, opts := effect(d.Effect)
		kairo.Action(r.K, workflows.ActToolPrefix+d.Name, eff, func(t *kairo.TaskContext, in workflows.ToolInput) (workflows.ToolOutput, error) {
			return r.runTool(t, d, in)
		}, opts...)
	}
	eff, opts := effect(tools.Idempotent)
	kairo.Action(r.K, workflows.ActFileApply, eff, r.applyWrite, opts...)
}

// toolArgs is a call's arguments: given (MCP), or read from its message.
func (r *Runtime) toolArgs(ctx context.Context, in workflows.ToolInput) (json.RawMessage, error) {
	if in.Args != nil {
		return in.Args, nil
	}
	call, err := r.toolCall(ctx, in)
	if err != nil {
		return nil, err
	}
	return call.Input, nil
}

// planTool works a write out without making it. It only reads; the content
// it would write is stored by hash, so the record of the plan stays small.
func (r *Runtime) planTool(t *kairo.TaskContext, d tools.Def, in workflows.ToolInput) (workflows.PlanOutput, error) {
	args, err := r.toolArgs(t, in)
	if err != nil {
		return workflows.PlanOutput{}, err
	}
	root, err := os.OpenRoot(in.Dir)
	if err != nil {
		out, err := r.toolResult(t, in.Call, tools.Result{Output: "cannot open the working directory: " + err.Error(), IsError: true})
		return workflows.PlanOutput{Done: true, Result: out}, err
	}
	defer root.Close()
	p := d.Plan(t, tools.Env{Root: root, Dir: in.Dir}, args)
	if p.Fail != nil {
		out, err := r.toolResult(t, in.Call, *p.Fail)
		return workflows.PlanOutput{Done: true, Result: out}, err
	}
	if d.Apply != nil {
		apply, err := json.Marshal(workflows.PlanApplyInput{Dir: in.Dir, Call: in.Call, Data: p.Data})
		return workflows.PlanOutput{Action: workflows.ActToolPrefix + d.Name + workflows.ActApplySuffix, Apply: apply}, err
	}
	content, err := r.Store.Put(t, store.KindContent, p.Content)
	if err != nil {
		return workflows.PlanOutput{}, err
	}
	apply, err := json.Marshal(workflows.ApplyInput{
		Dir: in.Dir, Call: in.Call, Path: p.Path, Before: p.Before, After: p.After, Content: content, Summary: p.Summary,
	})
	return workflows.PlanOutput{Action: workflows.ActFileApply, Apply: apply}, err
}

// applyPlan makes a tool's own plan; done again, the tool does not act twice.
func (r *Runtime) applyPlan(t *kairo.TaskContext, d tools.Def, in workflows.PlanApplyInput) (workflows.ToolOutput, error) {
	root, err := os.OpenRoot(in.Dir)
	if err != nil {
		return r.toolResult(t, in.Call, tools.Result{Output: "cannot open the working directory: " + err.Error(), IsError: true})
	}
	defer root.Close()
	res := d.Apply(t, tools.Env{Root: root, Dir: in.Dir}, in.Data)
	if res.Unknown {
		return workflows.ToolOutput{}, kairo.Unknown(errors.New(res.Output))
	}
	out, err := r.toolResult(t, in.Call, res)
	if err != nil {
		return out, kairo.Unknown(fmt.Errorf("the tool acted, its result was not stored: %w", err))
	}
	return out, nil
}

// applyWrite makes a planned write (idempotent: compare, then replace).
func (r *Runtime) applyWrite(t *kairo.TaskContext, in workflows.ApplyInput) (workflows.ToolOutput, error) {
	kind, content, err := r.Store.Get(t, in.Content)
	if err != nil {
		return workflows.ToolOutput{}, err
	}
	if kind != store.KindContent {
		return workflows.ToolOutput{}, fmt.Errorf("%s is %s, not content", in.Content, kind)
	}
	root, err := os.OpenRoot(in.Dir)
	if err != nil {
		return r.toolResult(t, in.Call, tools.Result{Output: "cannot open the working directory: " + err.Error(), IsError: true})
	}
	defer root.Close()
	res := tools.Apply(tools.Env{Root: root, Dir: in.Dir}, in.Path, in.Before, in.After, content, in.Summary)
	if res.Unknown {
		return workflows.ToolOutput{}, kairo.Unknown(errors.New(res.Output))
	}
	out, err := r.toolResult(t, in.Call, res)
	if err != nil {
		return out, kairo.Unknown(fmt.Errorf("the write was made, its result was not stored: %w", err))
	}
	return out, nil
}

// runTool runs one tool call in the working directory and stores its result.
// For a tool that acts (not Read), anything that goes wrong once it may have
// acted is an unknown outcome, never a plain failure (invariant 4): kairo
// stops a real tool for review and checks an idempotent one again.
func (r *Runtime) runTool(t *kairo.TaskContext, d tools.Def, in workflows.ToolInput) (workflows.ToolOutput, error) {
	args, err := r.toolArgs(t, in)
	if err != nil {
		return workflows.ToolOutput{}, err
	}
	root, err := os.OpenRoot(in.Dir)
	if err != nil {
		return r.toolResult(t, in.Call, tools.Result{Output: "cannot open the working directory: " + err.Error(), IsError: true})
	}
	defer root.Close()
	res := d.Run(t, tools.Env{Root: root, Dir: in.Dir}, args)
	if res.Unknown && d.Effect != tools.Read {
		return workflows.ToolOutput{}, kairo.Unknown(errors.New(res.Output))
	}
	out, err := r.toolResult(t, in.Call, res)
	if err != nil && d.Effect != tools.Read {
		return out, kairo.Unknown(fmt.Errorf("the tool ran, its result was not stored: %w", err))
	}
	return out, err
}

// toolCall finds the tool_use the input names in its assistant message.
func (r *Runtime) toolCall(ctx context.Context, in workflows.ToolInput) (msg.Part, error) {
	m, err := r.Store.Message(ctx, in.Message)
	if err != nil {
		return msg.Part{}, err
	}
	for _, p := range m.Parts {
		if p.Type == msg.ToolUse && p.ID == in.Call {
			return p, nil
		}
	}
	return msg.Part{}, fmt.Errorf("no tool call %s in %s", in.Call, in.Message)
}

func (r *Runtime) toolResult(ctx context.Context, id string, res tools.Result) (workflows.ToolOutput, error) {
	h, err := r.Store.PutMessage(ctx, msg.Message{Role: msg.User, Parts: []msg.Part{{
		Type: msg.ToolResult, ToolUseID: id, Text: res.Output, IsError: res.IsError,
	}}})
	return workflows.ToolOutput{Message: h, IsError: res.IsError}, err
}

func (r *Runtime) model(t *kairo.TaskContext, in workflows.ModelInput) (workflows.ModelReply, error) {
	var system string
	if err := r.Store.GetJSON(t, in.System, store.KindSystem, &system); err != nil {
		return workflows.ModelReply{}, err
	}
	var specs []provider.ToolSpec
	if err := r.Store.GetJSON(t, in.Tools, store.KindTools, &specs); err != nil {
		return workflows.ModelReply{}, err
	}
	hashes, err := r.Store.Conversation(t, in.History)
	if err != nil {
		return workflows.ModelReply{}, err
	}
	ms, err := r.Store.Messages(t, append(hashes, in.Turn...))
	if err != nil {
		return workflows.ModelReply{}, err
	}
	resp, err := r.cfg.Provider.Complete(t, provider.Request{Model: in.Model, System: system, Tools: specs, Messages: ms})
	if err != nil {
		var permanent *provider.PermanentError
		if t.Err() != nil || errors.As(err, &permanent) {
			return workflows.ModelReply{}, err
		}
		// A model call is safe to repeat; let kairo try again.
		return workflows.ModelReply{}, kairo.Retryable(err)
	}
	h, err := r.Store.PutMessage(t, resp.Message)
	if err != nil {
		return workflows.ModelReply{}, kairo.Retryable(err)
	}
	out := workflows.ModelReply{Message: h, Stop: resp.Stop, Usage: resp.Usage}
	for _, p := range resp.Message.Parts {
		if p.Type == msg.ToolUse {
			out.Calls = append(out.Calls, workflows.ToolCall{ID: p.ID, Name: p.Name})
		}
	}
	return out, nil
}

// SessionID is the kairo id of session id (AGENTS.md, invariant 8).
func SessionID(id string) string { return "s/" + id }

// CreateSession starts session id in dir. Creating it again returns the same
// session.
func (r *Runtime) CreateSession(ctx context.Context, id string, in workflows.SessionInput) (string, error) {
	if in.Agent == "" {
		in.Agent = "build"
	}
	abs, err := filepath.Abs(in.Dir)
	if err != nil {
		return "", err
	}
	in.Dir = abs
	return r.K.Submit(ctx, workflows.Session, in, kairo.WithID(SessionID(id)))
}

// Send stores text as a user message and gives it to the session. It returns
// the number of the turn that will answer it.
func (r *Runtime) Send(ctx context.Context, session, text string) (int, error) {
	h, err := r.Store.PutMessage(ctx, msg.Message{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: text}}})
	if err != nil {
		return 0, err
	}
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	n, err := r.sent(ctx, session)
	if err != nil {
		return 0, err
	}
	if err := r.K.Signal(ctx, session, workflows.SignalMessage, workflows.SessionEvent{Kind: workflows.EventMessage, Message: h}); err != nil {
		return 0, err
	}
	return n, nil
}

// CloseSession ends a session after the turns it has been sent.
func (r *Runtime) CloseSession(ctx context.Context, session string) error {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	return r.K.Signal(ctx, session, workflows.SignalMessage, workflows.SessionEvent{Kind: workflows.EventClose})
}

// sent counts the events a session has been sent: each is a finished wait
// for the message signal (a signal sent before its wait finishes a wait made
// for it, kairo ADR 0059).
//
// kairo-gap #14: Signal does not say which wait it filled, so the count is
// right only while sends go through this process's sendMu.
func (r *Runtime) sent(ctx context.Context, session string) (int, error) {
	kids, err := r.K.Children(ctx, session)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, k := range kids {
		if k.Plan == "kairo.wait/"+workflows.SignalMessage && k.Finished() {
			n++
		}
	}
	return n, nil
}

// TurnRun is the kairo run of turn n of a session, or "" if the session has
// not started it yet.
func (r *Runtime) TurnRun(ctx context.Context, session string, n int) (kairo.RunInfo, bool, error) {
	kids, err := r.K.Children(ctx, session)
	if err != nil {
		return kairo.RunInfo{}, false, err
	}
	for _, k := range kids {
		if k.Workflow != workflows.Turn {
			continue
		}
		var w struct {
			Input workflows.TurnInput `json:"input"`
		}
		if err := json.Unmarshal(k.Input, &w); err != nil {
			return kairo.RunInfo{}, false, fmt.Errorf("turn %s: %w", k.RunID, err)
		}
		if w.Input.N == n {
			return k, true, nil
		}
	}
	return kairo.RunInfo{}, false, nil
}

// WaitTurn waits for turn n of a session to finish and returns its result.
// It waits on runs starting and settling in this process, not on a timer
// (kairo-gap #15: a turn another process starts is noticed only once found).
func (r *Runtime) WaitTurn(ctx context.Context, session string, n int) (workflows.TurnOutput, error) {
	for {
		changed := r.changes()
		run, ok, err := r.TurnRun(ctx, session, n)
		if err != nil {
			return workflows.TurnOutput{}, err
		}
		if ok {
			return kairo.Await[workflows.TurnOutput](ctx, r.K, run.RunID)
		}
		info, err := r.K.Get(ctx, session)
		if err != nil {
			return workflows.TurnOutput{}, err
		}
		if info.Finished() {
			return workflows.TurnOutput{}, fmt.Errorf("session %s ended before turn %d", session, n)
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return workflows.TurnOutput{}, ctx.Err()
		}
	}
}

// Reply is the text of an assistant message.
func (r *Runtime) Reply(ctx context.Context, hash string) (string, error) {
	m, err := r.Store.Message(ctx, hash)
	if err != nil {
		return "", err
	}
	return m.Text(), nil
}
