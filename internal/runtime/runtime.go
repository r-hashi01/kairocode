// Package runtime opens kairo, declares KairoCode's actions and workflows on
// it, and gives the server and the CLI a small API over sessions.
package runtime

import (
	"cmp"
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
	// Engine runs the model loop of a session that names none:
	// workflows.EngineAPI (the default) or workflows.EngineClaudeCode (ADR
	// 0007). A session may name the other: the API engine then needs
	// Provider.
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
	// Observe, if set, is also given what kairo observes here (the daemon
	// tells from it whether a step runs, ADR 0008).
	Observe func(kairo.Observation)
	// Now is kairo's clock (tests; the real clock when nil).
	Now func() time.Time

	// onlyVersion, in tests, declares the session and the turn at that old
	// version only ("2", "3" or "4"), as a build of that version did.
	onlyVersion string
	// onlyFlowVersion, in tests, declares the flow at that old version
	// only ("1").
	onlyFlowVersion string
}

type Runtime struct {
	K     *kairo.Kairo
	Store *store.Store
	db    *sql.DB
	cfg   Config
	mcp   *mcpServer
	ev    *events

	sendMu sync.Mutex

	approveMu sync.Mutex // serializes answers (Approve)

	closeOnce sync.Once
	closeErr  error

	mu      sync.Mutex
	changed chan struct{} // closed and replaced whenever a run starts or settles here
}

// DefaultDataDir is where a user's sessions live, and where their daemon
// runs (ADR 0008).
func DefaultDataDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "kairocode")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "kairocode")
}

// DBIn is the database in data directory dir.
func DBIn(dir string) string { return filepath.Join(dir, "kairo.db") }

// DefaultDB is the database in the default data directory.
func DefaultDB() string { return DBIn(DefaultDataDir()) }

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
	default:
		return nil, fmt.Errorf("runtime: unknown engine %q", cfg.Engine)
	}
	if cfg.ClaudePath == "" { // a session may ask for Claude Code whatever the default
		cfg.ClaudePath = "claude"
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
	r.ev = newEvents(r)
	fail := func(err error) (*Runtime, error) {
		r.ev.close()
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
		Now:     cfg.Now,
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

// Drain stops the runtime at the edges of its steps (kairo's Drain): no new
// step starts, and the steps under way finish with their outcomes recorded,
// so a real one needs no review in the next process. It returns once they
// have, or when ctx ends; draining has begun either way. Close after it.
func (r *Runtime) Drain(ctx context.Context) error { return r.K.Drain(ctx) }

// Close closes the runtime; closing it again does nothing.
func (r *Runtime) Close() error {
	r.closeOnce.Do(func() {
		r.ev.close()
		err := r.mcp.close()
		err = errors.Join(err, r.K.Close())
		r.closeErr = errors.Join(err, r.db.Close())
	})
	return r.closeErr
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
	if r.cfg.Observe != nil {
		r.cfg.Observe(o)
	}
	r.ev.observe(o)
	if o.Kind != kairo.ObsRunStarted && o.Kind != kairo.ObsRunSettled {
		return
	}
	if o.Kind == kairo.ObsRunSettled && o.Status == "blocked" && r.mcp != nil {
		r.mcp.blocked(o.Parent)
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
	switch v := r.cfg.onlyVersion; v {
	case workflows.SessionVersion4:
		kairo.Workflow(r.K, workflows.Session, workflows.RunSessionV4, kairo.WorkflowVersion(v))
		kairo.Workflow(r.K, workflows.Turn, workflows.RunTurn, kairo.WorkflowVersion(workflows.TurnVersion))
	case workflows.SessionVersion3, workflows.SessionVersion2:
		kairo.Workflow(r.K, workflows.Session, workflows.RunSessionV3, kairo.WorkflowVersion(v))
		kairo.Workflow(r.K, workflows.Turn, workflows.RunTurnV3, kairo.WorkflowVersion(v))
	default:
		kairo.Workflow(r.K, workflows.Session, workflows.RunSession, kairo.WorkflowVersion(workflows.SessionVersion))
		kairo.Workflow(r.K, workflows.Turn, workflows.RunTurn, kairo.WorkflowVersion(workflows.TurnVersion))
		// Version 4 finishes as it started: a stopped turn is not kept.
		kairo.Workflow(r.K, workflows.Session, workflows.RunSessionV4, kairo.WorkflowVersion(workflows.SessionVersion4), kairo.Draining())
		// Versions 3 and 2 finish with the functions they started with.
		for _, v := range []string{workflows.SessionVersion3, workflows.SessionVersion2} {
			kairo.Workflow(r.K, workflows.Session, workflows.RunSessionV3, kairo.WorkflowVersion(v), kairo.Draining())
		}
		for _, v := range []string{workflows.TurnVersion3, workflows.TurnVersion2} {
			kairo.Workflow(r.K, workflows.Turn, workflows.RunTurnV3, kairo.WorkflowVersion(v), kairo.Draining())
		}
	}
	kairo.Workflow(r.K, workflows.MCPCall, workflows.RunMCPCall, kairo.WorkflowVersion(workflows.MCPCallVersion))
	if r.cfg.onlyFlowVersion == workflows.FlowVersion1 {
		kairo.Workflow(r.K, workflows.Flow, workflows.RunFlowV1, kairo.WorkflowVersion(workflows.FlowVersion1))
	} else {
		kairo.Workflow(r.K, workflows.Flow, workflows.RunFlow, kairo.WorkflowVersion(workflows.FlowVersion))
		// Version 1 finishes as it started: its calls carry no At.
		kairo.Workflow(r.K, workflows.Flow, workflows.RunFlowV1, kairo.WorkflowVersion(workflows.FlowVersion1), kairo.Draining())
	}
	kairo.Action(r.K, workflows.ActFlowLoad, kairo.Unprotected, flowLoad)
	kairo.Action(r.K, workflows.ActFlowPut, kairo.Unprotected, r.flowPut)
	kairo.Action(r.K, workflows.ActFlowText, kairo.Unprotected, r.flowText)
	kairo.Action(r.K, workflows.ActFlowAsk, kairo.Unprotected,
		func(*kairo.TaskContext, workflows.FlowAskInput) (bool, error) { return true, nil }) // recorded: Pending reads it
	kairo.Action(r.K, workflows.ActFlowDecide, kairo.Unprotected, r.flowDecide, kairo.Timeout(10*time.Minute))
	kairo.Action(r.K, workflows.ActHookTrust, kairo.Unprotected, r.hookTrustAction) // a record, written once
	kairo.Action(r.K, workflows.ActClaudeTurn, kairo.Unprotected, r.claudeTurn, kairo.Timeout(time.Hour))
	kairo.Action(r.K, workflows.ActPermissionCheck, kairo.Unprotected, r.permissionCheck)
	kairo.Action(r.K, workflows.ActToolDenied, kairo.Unprotected, r.toolDenied)
	kairo.Action(r.K, workflows.ActMCPAsk, kairo.Unprotected, r.mcpAsk)
	kairo.Action(r.K, workflows.ActMCPGrant, kairo.Unprotected, r.mcpGrant)

	kairo.Action(r.K, workflows.ActAgentLoad, kairo.Unprotected, r.agentLoad)
	kairo.Action(r.K, workflows.ActModel, kairo.Unprotected, r.model, kairo.Timeout(10*time.Minute))
	kairo.Action(r.K, workflows.ActHistoryExtend, kairo.Unprotected,
		func(t *kairo.TaskContext, in workflows.HistoryExtendInput) (string, error) {
			return r.Store.Extend(t, in.Prev, in.Messages)
		})
	kairo.Action(r.K, workflows.ActHistoryCancelled, kairo.Unprotected, r.historyCancelled)
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
	// A snapshot writes only to its store; a restore makes the files what a
	// tree has, the same however often it runs.
	kairo.Action(r.K, workflows.ActSnapshot, eff, r.snapshotAction, opts...)
	kairo.Action(r.K, workflows.ActRestore, eff, r.restoreAction, opts...)
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
	// The text streams to those who watch the session (live output, not a
	// record: the message is recorded once it is whole).
	resp, err := r.cfg.Provider.Complete(t, provider.Request{Model: in.Model, System: system, Tools: specs, Messages: ms,
		OnText: func(text string) { t.Emit([]byte(text)) }})
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
	if in.Engine != "" {
		if _, _, err := r.engineFor(in.Engine, in.Model); err != nil {
			return "", err
		}
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
	return r.signal(ctx, session, workflows.SessionEvent{Kind: workflows.EventMessage, Message: h})
}

// ErrNoUndo: the session started before undo existed (version 3 or
// older): it would take an undo for a message.
var ErrNoUndo = errors.New("this session started before undo existed: start a new session to undo")

// Undo asks the session to put the working files back as they were before
// its last turn in effect (design 7). It returns the number of the turn
// that does it.
func (r *Runtime) Undo(ctx context.Context, session string) (int, error) {
	return r.undo(ctx, session, workflows.EventUndo)
}

// Redo asks the session to put back what its last undo took away.
func (r *Runtime) Redo(ctx context.Context, session string) (int, error) {
	return r.undo(ctx, session, workflows.EventRedo)
}

func (r *Runtime) undo(ctx context.Context, session, kind string) (int, error) {
	info, err := r.K.Get(ctx, session)
	if err != nil {
		return 0, err
	}
	if info.Version != workflows.SessionVersion && info.Version != workflows.SessionVersion4 {
		return 0, ErrNoUndo
	}
	return r.signal(ctx, session, workflows.SessionEvent{Kind: kind})
}

// signal gives the session an event and returns the number of the turn
// that takes it.
func (r *Runtime) signal(ctx context.Context, session string, ev workflows.SessionEvent) (int, error) {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	n, err := r.sent(ctx, session)
	if err != nil {
		return 0, err
	}
	if err := r.K.Signal(ctx, session, workflows.SignalMessage, ev); err != nil {
		return 0, err
	}
	return n, nil
}

// stoppedNote is what the conversation says of a turn the user stopped
// (ADR 0011), in the assistant's place: the next turn's model reads it.
const stoppedNote = "[The user stopped this turn before it finished. Tool calls made in it may have changed the working files; look before going on.]"

// historyCancelled keeps a stopped turn in the conversation: the message
// it was asked with, and the note.
func (r *Runtime) historyCancelled(t *kairo.TaskContext, in workflows.HistoryCancelledInput) (string, error) {
	note, err := r.Store.PutMessage(t, msg.Message{Role: msg.Assistant, Parts: []msg.Part{{Type: msg.Text, Text: stoppedNote}}})
	if err != nil {
		return "", err
	}
	return r.Store.Extend(t, in.Prev, []string{in.Message, note})
}

// StoppedCancelled is why a turn the user stopped ended, as clients are
// told it (a turn's Stopped).
const StoppedCancelled = "cancelled"

// ErrNoTurnToCancel: the session's last turn has ended (or none was sent).
var ErrNoTurnToCancel = errors.New("no turn is running")

// Cancel stops the session's turn under way (ADR 0011): kairo cancels it,
// with what it waits for and the steps under way. It returns the turn's
// number. Turns run one at a time: the one under way is the one not ended,
// whatever was sent after it. With none under way, the last turn sent is
// waited for if it has not begun (a stop pressed just after a send).
func (r *Runtime) Cancel(ctx context.Context, session string) (int, error) {
	for {
		changed := r.changes()
		kids, err := r.K.Children(ctx, session)
		if err != nil {
			return 0, err
		}
		for _, k := range kids {
			if k.Workflow == workflows.Turn && !k.Finished() {
				var w struct {
					Input workflows.TurnInput `json:"input"`
				}
				_ = json.Unmarshal(k.Input, &w)
				// kairo-gap #22: were the turn's function to return between
				// the look above and this cancel, kairo would record the run
				// cancelled while the session had its value.
				return w.Input.N, r.K.Cancel(ctx, k.RunID)
			}
		}
		r.sendMu.Lock()
		n, err := r.sent(ctx, session)
		r.sendMu.Unlock()
		if err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, ErrNoTurnToCancel
		}
		if _, ok, err := r.TurnRun(ctx, session, n-1); err != nil || ok {
			return n - 1, cmp.Or(err, ErrNoTurnToCancel) // it has ended
		}
		info, err := r.K.Get(ctx, session)
		if err != nil {
			return 0, err
		}
		if info.Finished() { // the last event closed it
			return 0, ErrNoTurnToCancel
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
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

// logf logs what the runtime could not do but goes on without.
func (r *Runtime) logf(format string, args ...any) {
	r.cfg.Logger.Warn(fmt.Sprintf(format, args...))
}

// History is the session's conversation as of its last finished turn: what
// a client that comes back shows (ACP's session/load).
func (r *Runtime) History(ctx context.Context, session string) ([]msg.Message, error) {
	if _, err := r.K.Get(ctx, session); err != nil {
		return nil, err
	}
	runs, err := r.K.Children(ctx, session)
	if err != nil {
		return nil, err
	}
	last, run := -1, ""
	for _, t := range runs {
		if t.Workflow != workflows.Turn || t.Status != "completed" {
			continue
		}
		var in workflows.TurnInput
		if json.Unmarshal(input(t), &in) == nil && in.N > last {
			last, run = in.N, t.RunID
		}
	}
	if run == "" {
		return nil, nil
	}
	out, err := kairo.Await[workflows.TurnOutput](ctx, r.K, run)
	if err != nil {
		return nil, err
	}
	hashes, err := r.Store.Conversation(ctx, out.Head)
	if err != nil {
		return nil, err
	}
	return r.Store.Messages(ctx, hashes)
}
