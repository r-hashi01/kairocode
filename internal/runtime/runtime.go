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
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"
	_ "modernc.org/sqlite"

	"github.com/r-hashi01/kairocode/internal/agents"
	"github.com/r-hashi01/kairocode/internal/lsp"
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
	// ClaudePath is the claude command of the Claude Code engine;
	// CodexPath and CopilotPath, the codex and copilot commands of theirs
	// (ADR 0014).
	ClaudePath  string
	CodexPath   string // the ACP agent: codex-acp
	CopilotPath string // copilot, run with --acp
	// CopilotHome is copilot's settings directory, where KairoCode turns
	// copilot's own hooks off (ADR 0018; default: $COPILOT_HOME, else
	// ~/.copilot; with NoCLIs, none: nothing is written).
	CopilotHome string
	// AgentsDir keeps the ACP agents installed from the registry (ADR
	// 0015; default: agents beside DB); AgentRegistry is the registry's
	// URL (agents.RegistryURL for ACP's; "" reads none).
	AgentsDir     string
	AgentRegistry string
	// ExtraTools are offered besides the built-in ones (tests).
	ExtraTools []tools.Def
	// JobRunner runs a command in the background, given its folder
	// (ADR 0028; default: this binary's kairocode jobs run).
	JobRunner []string
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

	// turn, in tests, stands in for the turn's function.
	turn func(*kairo.Context, workflows.TurnInput) (workflows.TurnOutput, error)
	// onlyVersion, in tests, declares the session and the turn at that old
	// version only ("2", "3" or "4"), as a build of that version did.
	onlyVersion string
	// onlyFlowVersion, in tests, declares the flow at that old version
	// only ("1").
	onlyFlowVersion string
	// flowFunc, in tests, runs the flows of the current version in place
	// of workflows.RunFlow: code that changed under the same version.
	flowFunc func(*kairo.Context, workflows.FlowInput) (workflows.FlowOutput, error)
}

type Runtime struct {
	K     *kairo.Kairo
	ks    *kairo.SQLStore
	Store *store.Store
	db    *sql.DB
	cfg   Config
	mcp   *mcpServer
	ev    *events
	// lsp keeps the language servers whose errors are added to a write's
	// result (ADR 0009).
	lsp *lsp.Manager
	// agents are the ACP agents of the registry (ADR 0015); auth, what is
	// known of each provider's sign-in.
	agents *agents.Manager
	// ext keeps the MCP servers' sessions (ADR 0022).
	ext extServers
	// jobs are the commands in the background (ADR 0028).
	jobs *jobManager
	// copilotHooks: one change of copilot's settings at a time.
	copilotHooksMu sync.Mutex
	auth           auths

	sendMu sync.Mutex

	approveMu sync.Mutex // serializes answers (Approve)

	// switches are the switches of provider and model (ADR 0014) that wait
	// for their session's next event.
	switchMu sync.Mutex
	switches map[string]*workflows.Switch

	closeOnce sync.Once
	closeErr  error
	closed    atomic.Bool

	mu      sync.Mutex
	changed chan struct{} // closed and replaced whenever a run starts or settles here
}

// NoCLIs is the environment variable that, set to "1", keeps the runtime
// from finding the user's claude, codex and copilot by name (tests: they
// never run the real ones, invariant 9).
const NoCLIs = "KAIROCODE_NO_CLIS"

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
	if cfg.Engine != "" {
		e, ok := engineOf(cfg.Engine) // a provider's name: its engine's
		if !ok {
			return nil, fmt.Errorf("runtime: unknown engine %q", cfg.Engine)
		}
		cfg.Engine = e
	}
	switch cfg.Engine {
	case "", workflows.EngineAPI:
		cfg.Engine = workflows.EngineAPI
		if cfg.Provider == nil {
			return nil, errors.New("runtime: the API engine needs a model provider")
		}
		if cfg.Model == "" {
			cfg.Model = provider.DefaultModel
		}
	case workflows.EngineClaudeCode, workflows.EngineCodex, workflows.EngineCopilot:
		// The model is the CLI's own default unless one is named.
	default:
		if !strings.HasPrefix(cfg.Engine, workflows.EngineACP) {
			return nil, fmt.Errorf("runtime: unknown engine %q", cfg.Engine)
		}
	}
	// A session may ask for any engine whatever the default. With NoCLIs
	// (tests), the user's own CLIs are not found unless named.
	if os.Getenv(NoCLIs) == "1" {
		none := filepath.Join(os.TempDir(), "kairocode-no-cli")
		for _, p := range []*string{&cfg.ClaudePath, &cfg.CodexPath, &cfg.CopilotPath} {
			if *p == "" {
				*p = none
			}
		}
	}
	if cfg.ClaudePath == "" {
		cfg.ClaudePath = "claude"
	}
	if cfg.CodexPath == "" {
		cfg.CodexPath = "codex-acp"
	}
	if cfg.CopilotPath == "" {
		cfg.CopilotPath = "copilot"
	}
	if cfg.MaxSteps == 0 {
		cfg.MaxSteps = 50
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.CopilotHome == "" && os.Getenv(NoCLIs) != "1" {
		cfg.CopilotHome = os.Getenv("COPILOT_HOME")
		if cfg.CopilotHome == "" {
			if home, err := os.UserHomeDir(); err == nil {
				cfg.CopilotHome = filepath.Join(home, ".copilot")
			} else {
				cfg.Logger.Warn("copilot's settings cannot be found, so its own hooks are not turned off (ADR 0018)", "err", err)
			}
		}
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
	if cfg.AgentsDir == "" {
		cfg.AgentsDir = filepath.Join(filepath.Dir(cfg.DB), "agents")
	}
	r := &Runtime{db: db, cfg: cfg, changed: make(chan struct{}), lsp: lsp.NewManager(cfg.Logger),
		agents: agents.New(cfg.AgentsDir, cfg.AgentRegistry)}
	r.ev = newEvents(r)
	r.ext.logf = r.logf
	r.jobs = newJobs(r)
	if id := registryAgent(cfg.Engine); id != "" && cfg.Engine != workflows.EngineCodex && cfg.Engine != workflows.EngineCopilot &&
		!r.knownAgent(ctx, id) {
		r.lsp.Close()
		r.ev.close()
		db.Close()
		return nil, fmt.Errorf("runtime: the engine %q: %w: %s", cfg.Engine, ErrNoProvider, id)
	}
	fail := func(err error) (*Runtime, error) {
		r.ev.close()
		r.lsp.Close()
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
	r.ks = ks
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
	r.jobs.recover()
	return r, nil
}

// Awaiting says something here needs the process to stay up though
// nothing runs (ADR 0028): a wait with a deadline or a sleep (kairo's
// timers fire only in a process). A command in the background does not: its runner
// starts a daemon to tell of its end.
func (r *Runtime) Awaiting(ctx context.Context) bool {
	ts, err := r.ks.DueTimers(ctx, math.MaxInt64, 1)
	return err == nil && len(ts) > 0
}

// closing says the runtime is closing (or closed).
func (r *Runtime) closing() bool { return r.closed.Load() }

// Drain stops the runtime at the edges of its steps (kairo's Drain): no new
// step starts, and the steps under way finish with their outcomes recorded,
// so a real one needs no review in the next process. It returns once they
// have, or when ctx ends; draining has begun either way. Close after it.
func (r *Runtime) Drain(ctx context.Context) error { return r.K.Drain(ctx) }

// Close closes the runtime; closing it again does nothing.
func (r *Runtime) Close() error {
	r.closeOnce.Do(func() {
		r.closed.Store(true)
		r.ev.close()
		err := r.mcp.close()
		err = errors.Join(err, r.K.Close())
		r.lsp.Close()
		r.ext.close()
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
	case workflows.SessionVersion6:
		kairo.Workflow(r.K, workflows.Session, workflows.RunSessionV6, kairo.WorkflowVersion(v))
		kairo.Workflow(r.K, workflows.Turn, r.turnWorkflow(), kairo.WorkflowVersion(workflows.TurnVersion))
	case workflows.SessionVersion5:
		kairo.Workflow(r.K, workflows.Session, workflows.RunSessionV5, kairo.WorkflowVersion(v))
		kairo.Workflow(r.K, workflows.Turn, workflows.RunTurn, kairo.WorkflowVersion(workflows.TurnVersion))
	case workflows.SessionVersion4:
		kairo.Workflow(r.K, workflows.Session, workflows.RunSessionV4, kairo.WorkflowVersion(v))
		kairo.Workflow(r.K, workflows.Turn, workflows.RunTurn, kairo.WorkflowVersion(workflows.TurnVersion))
	case workflows.SessionVersion3, workflows.SessionVersion2:
		kairo.Workflow(r.K, workflows.Session, workflows.RunSessionV3, kairo.WorkflowVersion(v))
		kairo.Workflow(r.K, workflows.Turn, workflows.RunTurnV3, kairo.WorkflowVersion(v))
	default:
		kairo.Workflow(r.K, workflows.Session, workflows.RunSession, kairo.WorkflowVersion(workflows.SessionVersion))
		kairo.Workflow(r.K, workflows.Turn, r.turnWorkflow(), kairo.WorkflowVersion(workflows.TurnVersion))
		// Version 6 finishes as it started: a turn is taken as its result
		// said (kairo-gap #22).
		kairo.Workflow(r.K, workflows.Session, workflows.RunSessionV6, kairo.WorkflowVersion(workflows.SessionVersion6), kairo.Draining())
		// Version 5 finishes as it started: no switch of engine or model.
		kairo.Workflow(r.K, workflows.Session, workflows.RunSessionV5, kairo.WorkflowVersion(workflows.SessionVersion5), kairo.Draining())
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
		run := workflows.RunFlow
		if r.cfg.flowFunc != nil {
			run = r.cfg.flowFunc
		}
		kairo.Workflow(r.K, workflows.Flow, run, kairo.WorkflowVersion(workflows.FlowVersion))
		// Versions 2 and 1 finish as they started: no flow.step, and in 1
		// no At either.
		kairo.Workflow(r.K, workflows.Flow, workflows.RunFlowV2, kairo.WorkflowVersion(workflows.FlowVersion2), kairo.Draining())
		kairo.Workflow(r.K, workflows.Flow, workflows.RunFlowV1, kairo.WorkflowVersion(workflows.FlowVersion1), kairo.Draining())
	}
	kairo.Action(r.K, workflows.ActFlowStep, kairo.Unprotected, r.flowStep)
	kairo.Action(r.K, workflows.ActFlowLoad, kairo.Unprotected, r.flowLoad)
	kairo.Action(r.K, workflows.ActFlowWould, kairo.Unprotected, r.flowWould)
	// bash_wait (ADR 0028): watching a command and reading it change
	// nothing; done again, they find the same.
	kairo.Action(r.K, workflows.ActJobWatch, kairo.Unprotected, r.jobWatch)
	kairo.Action(r.K, workflows.ActJobResult, kairo.Unprotected, r.jobResult)
	kairo.Action(r.K, workflows.ActFlowPut, kairo.Unprotected, r.flowPut)
	kairo.Action(r.K, workflows.ActFlowText, kairo.Unprotected, r.flowText)
	kairo.Action(r.K, workflows.ActFlowAsk, kairo.Unprotected,
		func(*kairo.TaskContext, workflows.FlowAskInput) (bool, error) { return true, nil }) // recorded: Pending reads it
	kairo.Action(r.K, workflows.ActFlowDecide, kairo.Unprotected, r.flowDecide, kairo.Timeout(10*time.Minute))
	kairo.Action(r.K, workflows.ActHookTrust, kairo.Unprotected, r.hookTrustAction) // a record, written once
	kairo.Action(r.K, workflows.ActHookCheck, kairo.Unprotected, r.hookCheckAction)
	kairo.Action(r.K, workflows.ActHookDecide, kairo.Unprotected, r.hookDecideAction) // a record, written once
	kairo.Action(r.K, workflows.ActHookCommand, kairo.Real, r.hookCommandAction)      // a command: not run again unasked
	kairo.Action(r.K, workflows.ActHookNote, kairo.Unprotected, r.hookNoteAction)
	kairo.Action(r.K, workflows.ActTaskArgs, kairo.Unprotected, r.taskArgs)
	kairo.Action(r.K, workflows.ActTaskResult, kairo.Unprotected,
		func(t *kairo.TaskContext, in workflows.TaskResultInput) (workflows.ToolOutput, error) {
			return r.toolResult(t, in.Call, tools.Result{Output: in.Text, IsError: in.IsError})
		})
	kairo.Action(r.K, workflows.ActClaudeTurn, kairo.Unprotected, r.claudeTurn, kairo.Timeout(time.Hour))
	kairo.Action(r.K, workflows.ActCodexTurn, kairo.Unprotected, r.acpTurn(workflows.EngineCodex), kairo.Timeout(time.Hour))
	kairo.Action(r.K, workflows.ActCopilotTurn, kairo.Unprotected, r.acpTurn(workflows.EngineCopilot), kairo.Timeout(time.Hour))
	kairo.Action(r.K, workflows.ActACPTurn, kairo.Unprotected,
		func(t *kairo.TaskContext, in workflows.ClaudeTurnInput) (workflows.ClaudeTurnOutput, error) {
			return r.acpTurn(in.Engine)(t, in)
		}, kairo.Timeout(time.Hour))
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
	kairo.Action(r.K, workflows.ActHistoryCompact, kairo.Unprotected, r.historyCompact, kairo.Timeout(10*time.Minute))
	// External tools (ADR 0022): listed as the turn starts (a server that
	// lists its tools again lists the same, as far as KairoCode can tell);
	// run by their declared effect: read, or real (invariant 3).
	kairo.Action(r.K, workflows.ActExtLoad, kairo.Unprotected, r.extLoad, kairo.Timeout(2*time.Minute))
	kairo.Action(r.K, workflows.ActToolExtRead, kairo.Unprotected, r.extTool)
	kairo.Action(r.K, workflows.ActToolExtReal, kairo.Real, r.extTool)
	kairo.Action(r.K, workflows.ActTurnSettled, kairo.Unprotected, r.turnSettled) // it reads kairo's record
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
	if !res.IsError {
		if errs := r.lspErrors(t, in.Dir, in.Path, content); errs != "" {
			res.Output += "\n\n" + errs
		}
	}
	out, err := r.toolResult(t, in.Call, res)
	if err != nil {
		return out, kairo.Unknown(fmt.Errorf("the write was made, its result was not stored: %w", err))
	}
	return out, nil
}

// lspErrors are the errors the language server for the file at path (in
// dir) sees in content, as text for the write's result (ADR 0009). Which
// server, if any, is decided here from the settings, and shows in the
// result, which is recorded. No server, or none in time: "".
func (r *Runtime) lspErrors(ctx context.Context, dir, path string, content []byte) string {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return ""
	}
	servers, err := lsp.Servers(filepath.Join(userConfigDir(), ConfigFile), filepath.Join(real, ConfigFile))
	if err != nil {
		r.logf("lsp: %v", err) // what is left out; the servers given are used
	}
	return r.lsp.Errors(ctx, servers, real, filepath.Join(real, filepath.FromSlash(path)), content)
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
	env := tools.Env{Root: root, Dir: in.Dir,
		Jobs: jobEnv{m: r.jobs, session: sessionOf(t.RunID), dir: in.Dir, key: t.IdempotencyKey}}
	if d.Name == tools.FetchTool.Name || d.Name == tools.HTTPTool.Name {
		if env.Net, err = userNet(); err != nil {
			return r.toolResult(t, in.Call, tools.Result{Output: err.Error(), IsError: true})
		}
	}
	res := d.Run(t, env, args)
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
	resp, err := r.cfg.Provider.Complete(t, provider.Request{Model: in.Model, Effort: in.Effort, System: system, Tools: specs, Messages: ms,
		OnText: func(text string) { t.Emit([]byte(text)) }})
	if errors.Is(err, provider.ErrNoCredentials) {
		r.authSeen(ProviderAnthropic, AuthNeeded, nil)
	} else if err == nil {
		r.authSeen(ProviderAnthropic, AuthSignedIn, nil)
	}
	if errors.Is(err, provider.ErrContextTooLong) {
		// Recorded as such: the turn compacts the conversation and asks
		// again (ADR 0020).
		return workflows.ModelReply{Stop: workflows.StopContext}, nil
	}
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
	if in.Engine == "" {
		// The provider last chosen, if it still runs here (2026-10-20).
		if last, err := r.Store.LastProvider(ctx); err == nil && last != "" && r.available(ctx, last) {
			in.Engine = last
		}
	} else {
		e, _ := engineOf(in.Engine)
		if err := r.Store.SetLastProvider(ctx, providerOf(e)); err != nil {
			return "", err
		}
	}
	if in.Engine != "" {
		in.Engine, _ = engineOf(in.Engine) // a provider's name: its engine's
		if _, _, err := r.engineFor(in.Engine, in.Model); err != nil {
			return "", err
		}
		if id := registryAgent(in.Engine); id != "" && !r.knownAgent(ctx, id) {
			return "", fmt.Errorf("%w: %s", ErrNoProvider, id)
		}
	}
	// A session that names no model or effort gets those last chosen for
	// its provider (ADR 0014, 追記).
	engine := in.Engine
	if engine == "" {
		engine = r.cfg.Engine
	}
	if in.Model == "" || in.Effort == "" {
		pref, err := r.Store.ProviderPref(ctx, providerOf(engine))
		if err != nil {
			return "", err
		}
		if in.Model == "" && pref.Model != "" {
			in.Model, in.Engine = pref.Model, engine
		}
		if in.Effort == "" && pref.Effort != "" {
			in.Effort, in.Engine = pref.Effort, engine
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
// Compact starts a turn that only compacts the session's conversation
// (/compact, ADR 0020), and returns its number.
func (r *Runtime) Compact(ctx context.Context, session string) (int, error) {
	return r.signal(ctx, session, workflows.SessionEvent{Kind: workflows.EventCompact})
}

func (r *Runtime) Redo(ctx context.Context, session string) (int, error) {
	return r.undo(ctx, session, workflows.EventRedo)
}

func (r *Runtime) undo(ctx context.Context, session, kind string) (int, error) {
	info, err := r.K.Get(ctx, session)
	if err != nil {
		return 0, err
	}
	if !slices.Contains([]string{workflows.SessionVersion, workflows.SessionVersion6, workflows.SessionVersion5, workflows.SessionVersion4}, info.Version) {
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
	sw := r.takeSwitch(session)
	ev.Switch = sw
	if err := r.K.Signal(ctx, session, workflows.SignalMessage, ev); err != nil {
		if sw != nil { // not sent: it waits for the next event
			r.switchMu.Lock()
			if r.switches[session] == nil {
				r.switches[session] = sw
			}
			r.switchMu.Unlock()
		}
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
				// cancelled while the session had its value. The session
				// (version 7) reads back how the run was recorded and takes
				// it as cancelled (turn.settled).
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

// LastTurn is the number of the session's latest turn; false if it has
// none.
func (r *Runtime) LastTurn(ctx context.Context, session string) (int, bool, error) {
	kids, err := r.K.Children(ctx, session)
	if err != nil {
		return 0, false, err
	}
	last, found := 0, false
	for _, k := range kids {
		if k.Workflow != workflows.Turn {
			continue
		}
		var w struct {
			Input workflows.TurnInput `json:"input"`
		}
		if err := json.Unmarshal(k.Input, &w); err != nil {
			return 0, false, fmt.Errorf("turn %s: %w", k.RunID, err)
		}
		if !found || w.Input.N > last {
			last, found = w.Input.N, true
		}
	}
	return last, found, nil
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

// turnSettled says whether turn n of a session was recorded cancelled
// (kairo-gap #22). The session has its result, so its run has settled: the
// embedded runtime records a run's end before its parent is given it, and
// a turn is kept as long as its session runs.
func (r *Runtime) turnSettled(t *kairo.TaskContext, in workflows.TurnSettledInput) (workflows.TurnSettledOutput, error) {
	info, ok, err := r.TurnRun(t, in.Session, in.N)
	if err != nil {
		return workflows.TurnSettledOutput{}, kairo.Retryable(err)
	}
	if !ok || !info.Finished() {
		return workflows.TurnSettledOutput{}, fmt.Errorf("turn %d of %s: no settled run", in.N, in.Session)
	}
	return workflows.TurnSettledOutput{Cancelled: info.Status == "cancelled"}, nil
}

// turnWorkflow is the turn's function: RunTurn, or a test's stand-in.
func (r *Runtime) turnWorkflow() func(*kairo.Context, workflows.TurnInput) (workflows.TurnOutput, error) {
	if r.cfg.turn != nil {
		return r.cfg.turn
	}
	return workflows.RunTurn
}

// ErrNoForkPoint: the session has no finished turn to fork from there.
// ErrNoSnapshot: there is no record of the files after it, to put back.
// ErrForkWhileRunning: the files are not put back while a turn of the
// session runs in them.
var (
	ErrNoForkPoint      = errors.New("no finished turn to fork from")
	ErrNoSnapshot       = errors.New("there is no snapshot of the files after that turn, to put them back as they were")
	ErrForkWhileRunning = errors.New("a turn of the session is running in its files: fork with --files once it ends")
)

// Fork starts session id from turn n of session from (n < 0: its last
// finished turn), with that turn's conversation and settings; from is not
// changed (ADR 0021). With files, the new session's first turn puts the
// working files back as they were after turn n. It returns the session
// and the turn it was forked from.
func (r *Runtime) Fork(ctx context.Context, id, from string, n int, files bool) (string, int, error) {
	info, err := r.K.Get(ctx, from)
	if err != nil {
		return "", 0, err
	}
	var src workflows.SessionInput
	if err := json.Unmarshal(input(info), &src); err != nil {
		return "", 0, err
	}
	kids, err := r.K.Children(ctx, from)
	if err != nil {
		return "", 0, err
	}
	type done struct {
		in  workflows.TurnInput
		out workflows.TurnOutput
	}
	var turns []done // finished, by number
	running := false
	for _, k := range kids {
		if k.Workflow != workflows.Turn {
			continue
		}
		if !k.Finished() {
			running = true
		}
		var d done
		if k.Status != "completed" || json.Unmarshal(input(k), &d.in) != nil || runOutput(k.Output, &d.out) != nil || d.out.Head == "" {
			continue
		}
		turns = append(turns, d)
	}
	slices.SortFunc(turns, func(a, b done) int { return a.in.N - b.in.N })
	at := -1
	for i, d := range turns {
		if n < 0 || d.in.N == n {
			at = i
		}
	}
	if at < 0 {
		return "", 0, ErrNoForkPoint
	}
	tin, tout := turns[at].in, turns[at].out
	tree := ""
	if files {
		if running {
			return "", 0, ErrForkWhileRunning // its restore would change files under the turn's tools
		}
		// The files after the turn: its end's snapshot; for an undo or a
		// redo, the snapshot it put back; for a turn that changed no file
		// (/compact), the nearest one before it.
		for i := at; i >= 0 && tree == ""; i-- {
			d := turns[i]
			switch {
			case d.out.After != "":
				tree = d.out.After
			case d.in.Undo != nil && d.out.Restored:
				tree = d.in.Undo.Tree
			}
		}
		if tree == "" {
			return "", 0, ErrNoSnapshot
		}
	}
	engine := tin.Engine
	if engine == "" {
		engine = r.cfg.Engine // the daemon's, which the turn ran with
	}
	s, err := r.CreateSession(ctx, id, workflows.SessionInput{Dir: src.Dir, Agent: src.Agent, Engine: engine, Model: tin.Model,
		Effort: tin.Effort, History: tout.Head, From: &workflows.ForkRef{Session: from, Turn: tin.N}})
	if err != nil {
		return "", 0, err
	}
	if tree != "" {
		if _, err := r.signal(ctx, s, workflows.SessionEvent{Kind: workflows.EventForkFiles,
			Restore: &workflows.UndoInput{Of: tin.N, Tree: tree}}); err != nil {
			return "", 0, err
		}
	}
	return s, tin.N, nil
}
