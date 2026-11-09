// Package runtime opens kairo, declares KairoCode's actions and workflows on
// it, and gives the server and the CLI a small API over sessions.
package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"
	_ "modernc.org/sqlite"

	"github.com/r-hashi01/kairocode/internal/agents"
	"github.com/r-hashi01/kairocode/internal/lsp"
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
	// notes: the sessions to look at for requests to tell of (ADR 0034).
	notes *notifier
	// copilotHooks: one change of copilot's settings at a time.
	copilotHooksMu sync.Mutex
	auth           auths

	sendMu sync.Mutex
	// worktreeMu: one worktree is made at a time, so two asked for at once
	// with no name do not take the same free one (ADR 0037).
	worktreeMu sync.Mutex
	// rates are the providers' rate limits (ADR 0040).
	rates *rateLimiter

	approveMu sync.Mutex // serializes answers (Approve)

	// switchMu serializes changes to the switches waiting for their
	// session's next event (ADR 0014), kept in the store.
	switchMu sync.Mutex

	closeOnce sync.Once
	closeErr  error
	closed    atomic.Bool
	// bg: what the runtime does by itself (keeping flows' runs, ADR 0029),
	// stopped and waited for as it closes.
	bgMu     sync.Mutex
	bgCtx    context.Context
	bgCancel context.CancelFunc
	bg       sync.WaitGroup

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
	if cfg.Engine == "" {
		cfg.Engine = workflows.EngineAPI
	}
	model, err := engineModel(cfg.Engine, cfg.Model, cfg.Provider != nil)
	if errors.Is(err, errNoAPIProvider) {
		return nil, errors.New("runtime: the API engine needs a model provider")
	}
	if err != nil {
		return nil, fmt.Errorf("runtime: %w", err)
	}
	cfg.Model = model
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
	r.rates = newRateLimiter()
	r.ext.logf = r.logf
	r.bgCtx, r.bgCancel = context.WithCancel(context.Background())
	r.jobs = newJobs(r)
	r.notes = newNotifier()
	if id := registryAgent(cfg.Engine); id != "" && cfg.Engine != workflows.EngineCodex && cfg.Engine != workflows.EngineCopilot &&
		!r.knownAgent(ctx, id) {
		r.lsp.Close()
		r.ev.close()
		closeDB(db)
		return nil, fmt.Errorf("runtime: the engine %q: %w: %s", cfg.Engine, ErrNoProvider, id)
	}
	fail := func(err error) (*Runtime, error) {
		r.bgCancel()
		r.ev.close()
		r.lsp.Close()
		closeDB(db)
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
	r.background(r.keepMissed) // the flows' runs a stopped process did not keep (ADR 0029)
	r.background(r.notifyLoop) // telling of requests outside the machine (ADR 0034)
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

// background runs f on its own, stopped (its ctx) and waited for as the
// runtime closes; not once it is closing.
func (r *Runtime) background(f func(ctx context.Context)) {
	r.bgMu.Lock()
	defer r.bgMu.Unlock()
	if r.closed.Load() || r.bgCtx == nil {
		return
	}
	r.bg.Add(1)
	go func() {
		defer r.bg.Done()
		f(r.bgCtx)
	}()
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
		r.bgMu.Lock()
		r.closed.Store(true)
		r.bgMu.Unlock()
		if r.bgCancel != nil {
			r.bgCancel()
		}
		r.bg.Wait()
		r.ev.close()
		err := r.mcp.close()
		err = errors.Join(err, r.K.Close())
		r.lsp.Close()
		r.ext.close()
		r.closeErr = errors.Join(err, closeDB(r.db))
	})
	return r.closeErr
}

// closeDB closes db and returns once it holds no connection. database/sql's
// Close does not wait for its opener: a connection asked for before (a
// query cut short as kairo closed, with another waiting: the pool has one)
// is opened after Close returns and only then closed, touching the files
// (kairo.db-wal) of a closed runtime, after a daemon let go of its lock.
// The pool's count of open connections, which counts that one from when it
// is asked for, is the only sign of it.
// A connection that stays (one leaked, or the opener stopped with one asked
// for) is an error after a while, not a wait without end.
func closeDB(db *sql.DB) error {
	err := db.Close()
	for end := time.Now().Add(5 * time.Second); db.Stats().OpenConnections > 0; {
		if time.Now().After(end) {
			return errors.Join(err, fmt.Errorf("closing the database: %d connections still open after 5s", db.Stats().OpenConnections))
		}
		time.Sleep(time.Millisecond)
	}
	return err
}

// logf logs what the runtime could not do but goes on without.
func (r *Runtime) logf(format string, args ...any) {
	r.cfg.Logger.Warn(fmt.Sprintf(format, args...))
}
