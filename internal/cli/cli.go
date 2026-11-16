// Package cli is the kairocode command (design 8.2).
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/r-hashi01/kairocode/internal/flowfile"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/r-hashi01/kairocode/internal/acp"
	"github.com/r-hashi01/kairocode/internal/agents"
	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/daemon"
	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/runtime"
	"github.com/r-hashi01/kairocode/internal/tui"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

const usage = `usage:
  kairocode [chat] [-dir DIR] [-data DIR] [-session ID | -continue | -resume | -worktree[=NAME]]   talk with it in the terminal (kairo is the same; -worktree: in a new git worktree)
  kairocode sessions [-dir DIR] [-all | -repo] [-n N] [-json]   the sessions of a folder, the latest used first (-repo: of its repository, across its worktrees)
  kairocode run [-dir DIR] [-data DIR] [-provider P] [-model M] [-detach] [-worktree[=NAME]] "<request>"   answer one request, then exit
                                                     (-detach: stop when it waits for a person, exit 3)
  kairocode resume [-data DIR] [-detach] <session>   go on with a session run left, then close it
  kairocode fork [-data DIR] [-files] <session> [turn]   start another session from a turn of one (the last, unless named)
  kairocode log [-data DIR] [-json] <session>         the session's record: turns, calls, results, waits, use
  kairocode plugin [-dir DIR] [list|install N[@M]|update N|remove N|connect N|disconnect N]   plugins (ADR 0025, 0027)
  kairocode plugin marketplace [list|add SRC|update M|remove M]   the catalogs plugins are installed from
  kairocode approvals [-data DIR] <session>          list the calls waiting for a person (its work items' too)
  kairocode line [-dir DIR] [-data DIR] [-session ID | -provider P -model M] [-name N] <line> "<request>"   send a request down a standard line (ADR 0043)
  kairocode line [-data DIR] -keep <item> -session ID <name>   keep the line an item ran as .kairocode/lines/<name>.json
  kairocode items [-data DIR] [-json] [-drop ITEM [-force]] <session>   the work items a session sent down lines
  kairocode approve [-data DIR] [-deny] [-reason R] <key>   answer one
  kairocode answer [-data DIR] <key> <answer>         answer a flow's question
  kairocode flow graph [-dir DIR] [-width N] <name>   draw a flow of .kairocode/workflows (ADR 0003)
  kairocode flow show [-data DIR] [-follow] <session> the session's latest flow, as it has gone
  kairocode flow runs [-data DIR] [-dir DIR] [-flow N] the flows' runs here, kept once they end (ADR 0029)
  kairocode flow show [-data DIR] <run>              one of them: its chart, and its steps with their times
  kairocode daemon [-data DIR]                       run the daemon in the foreground
  kairocode daemon [-data DIR] stop                  stop it (steps under way get up to 30s to finish)
  kairocode usage [-data DIR] [-session ID] [-json]   what has been used today, this week (and in a session, turn by turn), against the limits
  kairocode acp [-data DIR]                          an ACP agent on stdin and stdout, for editors (ADR 0002)
  kairocode version                                  the version, its commit, Go and the platform

One daemon per data directory runs the sessions (ADR 0008); the commands
above start it when it is not running, and it stops after 10 minutes with
nothing to do (KAIROCODE_DAEMON_IDLE). A command of another build replaces
it when it runs no step and no other client uses it. The data directory defaults to
$XDG_DATA_HOME/kairocode or ~/.local/share/kairocode.

The provider runs the model loop; -provider and -model of run (or
KAIROCODE_ENGINE and KAIROCODE_MODEL) choose them for the sessions you
start, and /provider and /model in the chat switch them from the next
turn on (ADR 0014):
  anthropic (default)   the Claude API (credentials as the Anthropic SDK resolves them)
  claude-code           your own Claude Code, signed in with your plan (ADR 0007)
  codex                 your own Codex CLI
  copilot               your own GitHub Copilot CLI
  oai:<name>            an OpenAI-compatible API (oai:openai, oai:ollama, or one in
                        "providers" of your own kairocode.json; ADR 0035)
KAIROCODE_CLAUDE, KAIROCODE_CODEX and KAIROCODE_COPILOT name their commands.
The daemon takes them from the environment it starts in, and
KAIROCODE_PROVIDER=fake with KAIROCODE_FAKE=<script.json> to script the API
engine's replies (tests).
`

// Main runs the command with args (without the program name) and returns the
// process's exit code.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var err error
	switch {
	case len(args) > 0 && isHelp(args[0]):
		fmt.Fprint(stdout, usage)
		return 0
	case len(args) > 0 && isVersion(args[0]):
		printVersion(stdout)
		return 0
	case len(args) == 0 || strings.HasPrefix(args[0], "-"):
		// No command: the chat, as claude has it.
		err = chat(ctx, args, stdout, stderr)
	default:
		err = command(ctx, args[0], args[1:], stdout, stderr)
	}
	if errors.Is(err, errWaiting) {
		return 3
	}
	if errors.Is(err, errUnknownCommand) {
		fmt.Fprintf(stderr, "kairocode: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "kairocode: %v\n", err)
		return 1
	}
	return 0
}

func isHelp(arg string) bool {
	switch arg {
	case "-h", "-help", "--help", "help":
		return true
	}
	return false
}

func isVersion(arg string) bool {
	switch arg {
	case "version", "-version", "--version":
		return true
	}
	return false
}

var errUnknownCommand = errors.New("unknown command")

// command runs the command named cmd.
func command(ctx context.Context, cmd string, args []string, stdout, stderr io.Writer) (err error) {
	switch cmd {
	case "chat":
		err = chat(ctx, args, stdout, stderr)
	case "run":
		err = run(ctx, args, stdout, stderr)
	case "resume":
		err = resume(ctx, args, stdout, stderr)
	case "fork":
		err = fork(ctx, args, stdout, stderr)
	case "log":
		err = showLog(ctx, args, stdout, stderr)
	case "plugin":
		err = pluginCommand(ctx, args, stdout, stderr)
	case "sessions":
		err = listSessions(ctx, args, stdout, stderr)
	case "daemon":
		err = runDaemon(ctx, args, stdout, stderr)
	case "jobs":
		err = jobsCommand(ctx, args, stderr)
	case "approvals":
		err = approvals(ctx, args, stdout, stderr)
	case "line":
		err = lineCommand(ctx, args, stdout, stderr)
	case "items":
		err = itemsCommand(ctx, args, stdout, stderr)
	case "approve":
		err = approve(ctx, args, stderr)
	case "answer":
		err = answer(ctx, args, stderr)
	case "flow":
		err = flowCommand(ctx, args, stdout, stderr)
	case "usage":
		err = showUsage(ctx, args, stdout, stderr)
	case "acp":
		err = runACP(ctx, args, stdout, stderr)
	default:
		err = errUnknownCommand
	}
	return err
}

func newProvider() (provider.Provider, error) {
	switch p := os.Getenv("KAIROCODE_PROVIDER"); p {
	case "", "anthropic":
		return provider.NewAnthropic(), nil
	case "fake":
		path := os.Getenv("KAIROCODE_FAKE")
		if path == "" {
			return nil, errors.New("KAIROCODE_PROVIDER=fake needs KAIROCODE_FAKE=<script.json>")
		}
		return provider.LoadFake(path)
	default:
		return nil, fmt.Errorf("unknown provider %q", p)
	}
}

// openRuntime opens the daemon's runtime. Both engines are there; the
// environment names the default, which a session's own choice overrides.
func openRuntime(ctx context.Context, cfg runtime.Config) (*runtime.Runtime, error) {
	cfg.Engine, cfg.Model = os.Getenv("KAIROCODE_ENGINE"), os.Getenv("KAIROCODE_MODEL")
	cfg.ClaudePath = os.Getenv("KAIROCODE_CLAUDE")
	cfg.CodexPath, cfg.CopilotPath = os.Getenv("KAIROCODE_CODEX"), os.Getenv("KAIROCODE_COPILOT")
	// ACP's registry of agents (ADR 0015); KAIROCODE_ACP_REGISTRY names
	// another, or "off" none.
	switch reg := os.Getenv("KAIROCODE_ACP_REGISTRY"); reg {
	case "":
		cfg.AgentRegistry = agents.RegistryURL
	case "off":
	default:
		cfg.AgentRegistry = reg
	}
	p, err := newProvider()
	if err != nil {
		return nil, err
	}
	cfg.Provider = p
	return runtime.Open(ctx, cfg)
}

// daemonArgv is how a client starts the daemon: this program, in the
// foreground, writing "ready" to its standard output.
var daemonArgv = func(data string) ([]string, error) {
	exe, err := invokedPath()
	if err != nil {
		return nil, err
	}
	return []string{exe, "daemon", "-data", data}, nil
}

// invokedPath is the path this program was started by, made absolute, a
// link kept a link: a package manager installs a new build under another
// directory and moves the link (ADR 0019), so the path names the build
// installed now, where os.Executable names the one running.
func invokedPath() (string, error) {
	p := os.Args[0]
	if !strings.Contains(p, string(filepath.Separator)) {
		lp, err := exec.LookPath(p)
		if err != nil {
			return os.Executable()
		}
		p = lp
	}
	return filepath.Abs(p)
}

// connect reaches the daemon of data, starting it if none runs.
func connect(ctx context.Context, data string) (*daemon.Client, error) {
	argv, err := daemonArgv(data)
	if err != nil {
		return nil, err
	}
	c, err := daemon.Connect(ctx, data, daemon.Start{Argv: argv, Env: os.Environ(), Version: buildVersion()})
	if err == nil && c.Stale != "" {
		fmt.Fprintf(os.Stderr, "kairocode: %s\n", c.Stale)
	}
	return c, err
}

// buildVersion names this build of kairocode: the hash of its executable, so
// that a rebuilt binary does not talk to the daemon an older one started.
var buildVersion = sync.OnceValue(func() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	v, _ := daemon.FileVersion(exe)
	return v
})

func runDaemon(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", runtime.DefaultDataDir(), "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	stop := fs.Arg(0) == "stop"
	if stop {
		// Its flags after it too (daemon stop -data DIR): else they are
		// not read, and another data directory's daemon is stopped.
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return err
		}
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("daemon takes -data and stop only: %q", fs.Args())
	}
	if stop {
		running, err := daemon.Stop(ctx, *data)
		if err == nil && !running {
			fmt.Fprintln(stderr, "no daemon runs for", *data)
		}
		return err
	}
	var idle time.Duration
	if v := os.Getenv("KAIROCODE_DAEMON_IDLE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("KAIROCODE_DAEMON_IDLE: %w", err)
		}
		idle = d
	}
	// The client that started it closes the pipe after "ready": a later
	// write there must fail, not kill the daemon.
	signal.Ignore(syscall.SIGPIPE)
	ctx, cut, release := stopOnSignals(ctx)
	defer release()
	o := daemon.Options{Dir: *data, Open: openRuntime, Idle: idle, Ready: stdout, Version: buildVersion(),
		// A daemon on its way out may drain its steps for up to DefaultDrain.
		WaitLock: daemon.DefaultDrain + 10*time.Second, Cut: cut}
	// The build installed at the path it was started by: when it changes,
	// the daemon hands over to it (ADR 0019).
	if exe, err := filepath.Abs(os.Args[0]); err == nil {
		o.Installed = daemon.InstalledAt(exe)
		o.Successor = func() daemon.Start {
			return daemon.Start{Argv: append([]string{exe}, os.Args[1:]...), Env: os.Environ()}
		}
	}
	return daemon.Serve(ctx, o)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", ".", "working directory")
	data := fs.String("data", runtime.DefaultDataDir(), "data directory")
	detach := fs.Bool("detach", false, "stop when the turn waits for a person (exit 3); go on with kairocode resume")
	prov := fs.String("provider", os.Getenv("KAIROCODE_ENGINE"), "who runs the model: anthropic, claude-code, codex, copilot (ADR 0014)")
	model := fs.String("model", os.Getenv("KAIROCODE_MODEL"), "the model (default: the provider's)")
	var wt worktreeFlag
	fs.Var(&wt, "worktree", "run in a new git worktree of this folder's repository, on a branch kairo/<name> (-worktree=<name>; alone, the first free name)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	request := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if request == "" {
		return errors.New(`run needs a request: kairocode run "<request>"`)
	}
	abs, err := filepath.Abs(*dir) // the daemon runs elsewhere
	if err != nil {
		return err
	}
	if flow, ok := strings.CutPrefix(request, "/flow "); ok { // what a flow is to be given: no session for it
		if name, rest, _ := strings.Cut(strings.TrimSpace(flow), " "); strings.TrimSpace(rest) == "--help" {
			text, err := flowfile.FlowHelp(abs, name)
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(stdout, text)
			return err
		}
	}
	c, err := connect(ctx, *data)
	if err != nil {
		return err
	}
	defer c.Close()
	if wt.set {
		if abs, err = intoWorktree(ctx, c, abs, wt.name, stderr); err != nil {
			return err
		}
	}
	// The session runs with this client's choice of engine and model, not
	// whatever the daemon was started with.
	s, err := c.CreateSession(ctx, workflows.SessionInput{
		Dir: abs, Engine: *prov, Model: *model,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "session %s\n", s)
	return follow(ctx, c, s, *detach, stdout, stderr, func() (int, error) {
		if flow, ok := strings.CutPrefix(request, "/flow "); ok { // a flow (ADR 0003)
			name, args, _ := strings.Cut(strings.TrimSpace(flow), " ")
			if name == "new" {
				return c.NewFlow(ctx, s, args)
			}
			return c.RunFlow(ctx, s, name, strings.TrimSpace(args))
		}
		if args, ok := strings.CutPrefix(strings.TrimSpace(request), "/init"); ok && (args == "" || strings.ContainsAny(args[:1], " \t\n")) {
			return c.RunCommand(ctx, s, "init", strings.TrimSpace(args)) // a draft AGENTS.md (M8)
		}
		return c.Send(ctx, s, request)
	})
}

// resume goes on with a session run left (kairocode run -detach, or
// stopped): its latest turn is followed to its end, as run does, and the
// session is then closed, as run closes it. It is for run's sessions: one
// of chat or an editor is closed too.
func resume(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", runtime.DefaultDataDir(), "data directory")
	detach := fs.Bool("detach", false, "stop when the turn waits for a person (exit 3)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: kairocode resume [-data DIR] [-detach] <session>")
	}
	s := fs.Arg(0)
	if !strings.HasPrefix(s, "s/") {
		s = runtime.SessionID(s)
	}
	c, err := connect(ctx, *data)
	if err != nil {
		return err
	}
	defer c.Close()
	return follow(ctx, c, s, *detach, stdout, stderr, func() (int, error) { return c.LastTurn(ctx, s) })
}

// showLog prints a session's record (ADR 0023).
func showLog(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", runtime.DefaultDataDir(), "data directory")
	asJSON := fs.Bool("json", false, "as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: kairocode log [-data DIR] [-json] <session>")
	}
	s := fs.Arg(0)
	if !strings.HasPrefix(s, "s/") {
		s = runtime.SessionID(s)
	}
	c, err := connect(ctx, *data)
	if err != nil {
		return err
	}
	defer c.Close()
	l, err := c.Log(ctx, s)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(l)
	}
	_, err = io.WriteString(stdout, l.Text())
	return err
}

// fork starts a session from a turn of another (ADR 0021) and prints it.
func fork(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("fork", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", runtime.DefaultDataDir(), "data directory")
	files := fs.Bool("files", false, "put the working files back as they were after the turn")
	if err := fs.Parse(args); err != nil {
		return err
	}
	usage := errors.New("usage: kairocode fork [-data DIR] [-files] <session> [turn]")
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return usage
	}
	s := fs.Arg(0)
	if !strings.HasPrefix(s, "s/") {
		s = runtime.SessionID(s)
	}
	n := -1
	if fs.NArg() == 2 {
		v, err := strconv.Atoi(fs.Arg(1))
		if err != nil || v < 0 {
			return usage
		}
		n = v
	}
	c, err := connect(ctx, *data)
	if err != nil {
		return err
	}
	defer c.Close()
	id, from, err := c.Fork(ctx, s, n, *files)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s (from turn %d of %s)\n", strings.TrimPrefix(id, "s/"), from, strings.TrimPrefix(s, "s/"))
	return nil
}

// errWaiting: the turn waits for a person, and the command stopped
// (exit 3); the session goes on with kairocode resume.
var errWaiting = errors.New("the turn waits for a person")

// follow shows what turn n of session s does as it happens, n being what
// start returns (a turn it starts, or one under way), and prints its reply
// at its end, then closes the session. With detach, it stops when the
// turn waits for a person; stopped (Ctrl-C), it says how to go on.
func follow(ctx context.Context, c *daemon.Client, s string, detach bool, stdout, stderr io.Writer, start func() (int, error)) error {
	// What the turn does is shown as it happens; its result comes from
	// WaitTurn, which does not depend on the stream. Watching begins before
	// the turn starts, so no event of it is missed.
	watch, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	events, err := c.Events(watch, s)
	if err != nil {
		return err
	}
	turnDone := make(chan struct{})
	shown := make(chan struct{})
	waiting := make(chan struct{}, 1)
	n := -1
	sent := make(chan struct{})
	go func() {
		defer close(shown)
		showEvents(watch, c, s, events, stderr, sent, &n, turnDone, waiting)
	}()
	n, err = start()
	close(sent)
	if err != nil {
		return err
	}
	type result struct {
		out daemon.Turn
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := c.WaitTurn(watch, s, n)
		done <- result{out, err}
	}()
	var stopAt <-chan struct{} // nil: the turn is followed through its waits
	if detach {
		stopAt = waiting
	}
	var r result
	select {
	case r = <-done:
	case <-stopAt:
		fmt.Fprintf(stderr, "stopped while the turn waits; answer it, then go on with: kairocode resume %s\n", s)
		return errWaiting
	case <-ctx.Done():
		fmt.Fprintf(stderr, "\nstopped; the session goes on without this command: kairocode resume %s\n", s)
		return ctx.Err()
	}
	close(turnDone)
	if r.err != nil {
		return r.err
	}
	// The turn's last events come with its end, and the display returns at
	// turn.end. A turn that ended before this command watched (resume) has
	// sent its turn.end already, and the stream does not send past events:
	// the display gets a moment for what is still on its way, then stops.
	select {
	case <-shown:
	case <-time.After(time.Second):
		stopWatch()
		<-shown
	}
	fmt.Fprintln(stdout, r.out.Reply)
	if r.out.Stopped != "" {
		fmt.Fprintf(stderr, "stopped: %s\n", r.out.Stopped)
	}
	fmt.Fprintf(stderr, "steps %d, tokens in %d out %d (cache read %d)\n",
		r.out.Steps, r.out.Usage.Input, r.out.Usage.Output, r.out.Usage.CacheRead)
	return c.CloseSession(ctx, s)
}

func approvals(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("approvals", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", runtime.DefaultDataDir(), "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("approvals needs a session: kairocode approvals s/<id>")
	}
	c, err := connect(ctx, *data)
	if err != nil {
		return err
	}
	defer c.Close()
	p, err := c.PendingAll(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	if len(p) == 0 {
		fmt.Fprintln(stdout, "nothing waits for approval")
	}
	for _, x := range p {
		if x.Item != "" {
			fmt.Fprintf(stdout, "[item %s] ", x.Item)
		}
		if x.Kind == api.KindQuestion {
			fmt.Fprintf(stdout, "%s\n  question: %s\n  answers: %s (kairocode answer <key> <answer>)\n", x.Key, x.Subject, strings.Join(x.Options, " | "))
			continue
		}
		fmt.Fprintf(stdout, "%s\n  %s %q (%s)\n", x.Key, x.Tool, x.Subject, x.RuleText())
	}
	return nil
}

func approve(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", runtime.DefaultDataDir(), "data directory")
	deny := fs.Bool("deny", false, "decline the call")
	reason := fs.String("reason", "", "why (given to the model)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("approve needs a key: see kairocode approvals")
	}
	c, err := connect(ctx, *data)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Approve(ctx, fs.Arg(0), !*deny, *reason)
}

// answer answers a flow's question (ADR 0003) with one of its options.
func answer(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("answer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", runtime.DefaultDataDir(), "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return errors.New("answer needs a key and an answer: see kairocode approvals")
	}
	c, err := connect(ctx, *data)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Answer(ctx, fs.Arg(0), strings.Join(fs.Args()[1:], " "))
}

// stopOnSignals: the first SIGINT or SIGTERM ends the returned context (the
// daemon stops, letting the steps under way finish), and the second closes
// cut (it stops now, cutting them). release stops catching them.
func stopOnSignals(ctx context.Context) (context.Context, <-chan struct{}, func()) {
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	ctx, stop := context.WithCancel(ctx)
	cut := make(chan struct{})
	done := make(chan struct{})
	go func() {
		for n := 0; ; n++ {
			select {
			case <-sigs:
			case <-done:
				return
			}
			if n == 0 {
				stop()
				continue
			}
			close(cut)
			return
		}
	}()
	return ctx, cut, func() {
		signal.Stop(sigs)
		close(done)
		stop()
	}
}

// showEvents writes what session s does to w, from events, until the turn
// sent (n, known once sent is closed) ends. The stream may end early (the
// client fell behind): it watches again while the turn has not ended
// (turnDone). A stream begun again may have missed the turn's end, so it is
// read only until the turn is known to have ended.
func showEvents(ctx context.Context, c *daemon.Client, s string, events <-chan api.Event, w io.Writer,
	sent <-chan struct{}, n *int, turnDone <-chan struct{}, waiting chan<- struct{}) {
	var until <-chan struct{} // nil: read to the turn's end
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				select {
				case <-turnDone:
					return
				case <-ctx.Done():
					return
				default:
				}
				var err error
				if events, err = c.Events(ctx, s); err != nil {
					return
				}
				until = turnDone
				continue
			}
			if ev.Type == api.EventTurnEnd {
				<-sent
				if ev.N == *n {
					return
				}
			}
			showEvent(w, ev)
			switch ev.Type {
			case api.EventApprovalRequested, api.EventReviewRequested, api.EventQuestionRequested:
				select {
				case waiting <- struct{}{}:
				default:
				}
			}
		case <-until:
			return
		}
	}
}

// showEvent writes one event as a line, if it is one to show. Tokens are
// not: the reply is written whole when the turn ends.
func showEvent(w io.Writer, ev api.Event) {
	answer := func(ok *bool, yes, no string) string {
		switch {
		case ok == nil:
			return "answered"
		case *ok:
			return yes
		default:
			return no
		}
	}
	switch ev.Type {
	case api.EventToolStart:
		fmt.Fprintf(w, "· %s %s\n", ev.Tool, oneLine(string(ev.Input), 80))
	case api.EventToolEnd:
		if ev.IsError {
			fmt.Fprintf(w, "  ✗ %s: %s\n", ev.Tool, oneLine(ev.Output, 80))
		} else {
			fmt.Fprintf(w, "  ✓ %s\n", ev.Tool)
		}
	case api.EventApprovalRequested:
		fmt.Fprintf(w, "waiting for approval: %s %s (%s)\n  kairocode approve %s\n",
			ev.Pending.Tool, ev.Pending.Subject, ev.Pending.RuleText(), ev.Pending.Key)
	case api.EventReviewRequested:
		fmt.Fprintf(w, "%s %s %s\n  kairocode approve %s   (or -deny: it did not)\n", api.ReviewQuestion,
			ev.Pending.Tool, ev.Pending.Subject, ev.Pending.Key)
	case api.EventQuestionRequested:
		fmt.Fprintf(w, "the flow asks: %s\n  answers: %s\n  kairocode answer %s <answer>\n",
			ev.Pending.Subject, strings.Join(ev.Pending.Options, " | "), ev.Pending.Key)
	case api.EventQuestionResolved:
		fmt.Fprintln(w, "  answered")
	case api.EventApprovalResolved:
		fmt.Fprintf(w, "  %s\n", answer(ev.OK, "approved", "declined"))
	case api.EventReviewResolved:
		fmt.Fprintf(w, "  %s\n", answer(ev.OK, "it took effect", "it did not take effect"))
	}
}

// oneLine is s on one line, cut to max runes.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// stdin is what `kairocode acp` reads (a variable for tests).
var stdin io.Reader = os.Stdin

// runACP serves ACP on stdin and stdout (ADR 0002) as a client of the
// daemon. Its sessions run with KAIROCODE_ENGINE and KAIROCODE_MODEL.
func runACP(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("acp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", runtime.DefaultDataDir(), "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := connect(ctx, *data)
	if err != nil {
		return err
	}
	defer c.Close()
	return acp.Serve(ctx, c, stdin, stdout, acp.Options{Version: buildVersion(),
		Engine: os.Getenv("KAIROCODE_ENGINE"), Model: os.Getenv("KAIROCODE_MODEL"),
		Logger: slog.New(slog.NewTextHandler(stderr, nil))})
}

// showUsage prints what a session's model calls recorded they used.
func showUsage(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", runtime.DefaultDataDir(), "data directory")
	session := fs.String("session", "", "the session (as run prints it)")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s := *session
	if s != "" && !strings.HasPrefix(s, "s/") {
		s = runtime.SessionID(s)
	}
	c, err := connect(ctx, *data)
	if err != nil {
		return err
	}
	defer c.Close()
	// What has been used against the limits (ADR 0039), as /usage shows it:
	// without a session, against this folder's limits.
	wd, _ := os.Getwd()
	now, err := c.UsageIn(ctx, s, wd)
	if err != nil {
		return err
	}
	if s == "" {
		if *asJSON {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(now)
		}
		_, err := fmt.Fprint(stdout, now.Text())
		return err
	}
	u, err := c.Usage(ctx, s)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(u)
	}
	fmt.Fprintln(stdout, now.Text())
	line := func(name string, u msg.Usage) {
		fmt.Fprintf(stdout, "%-8s in %d  out %d  cache read %d  cache write %d\n", name, u.Input, u.Output, u.CacheRead, u.CacheWrite)
	}
	for _, t := range u.Turns {
		name := fmt.Sprintf("turn %d", t.N)
		if !t.Finished {
			name += "*"
		}
		line(name, t.Usage)
	}
	line("total", u.Total)
	return nil
}

// flowCommand: kairocode flow graph <name>, the chart of a flow from its
// script (nothing runs, and the daemon is not needed); kairocode flow show
// <session>, the session's latest flow with its calls' states, and with
// -follow drawn again as it goes on; kairocode flow stats, how often each
// flow of the project ran, from where, and how it ended.
func flowCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	usage := errors.New("kairocode flow graph [-dir DIR] [-width N] <name> | kairocode flow show [-data DIR] [-width N] [-follow] <session or run> | kairocode flow runs [-data DIR] [-dir DIR] [-flow NAME] [-tries | -no-tries] [-n N] [-json] | kairocode flow stats [-data DIR] [-dir DIR]")
	if len(args) == 0 {
		return usage
	}
	switch args[0] {
	case "graph":
		fs := flag.NewFlagSet("flow graph", flag.ContinueOnError)
		fs.SetOutput(stderr)
		dir := fs.String("dir", ".", "working directory")
		width := fs.Int("width", 100, "columns to draw in")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return usage
		}
		out, err := tui.FlowGraph(*dir, fs.Arg(0), *width)
		if err != nil {
			return err
		}
		_, err = lipgloss.Fprintln(stdout, out)
		return err
	case "show":
		fs := flag.NewFlagSet("flow show", flag.ContinueOnError)
		fs.SetOutput(stderr)
		data := fs.String("data", runtime.DefaultDataDir(), "data directory")
		width := fs.Int("width", 100, "columns to draw in")
		follow := fs.Bool("follow", false, "draw it again as it goes on, until it ends")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return usage
		}
		s := fs.Arg(0)
		if !strings.HasPrefix(s, "s/") {
			s = runtime.SessionID(s)
		}
		c, err := connect(ctx, *data)
		if err != nil {
			return err
		}
		defer c.Close()
		if strings.Count(s, "/") > 2 && !*follow { // a flow's run (kairocode flow runs gives its id)
			f, err := c.FlowRun(ctx, s)
			if err != nil {
				return err
			}
			if f.View != nil {
				lipgloss.Fprintln(stdout, tui.ChartOf(f.View, *width))
			}
			_, err = fmt.Fprint(stdout, "\n"+f.Text())
			return err
		}
		var events <-chan api.Event
		if *follow { // watched before it is looked at: nothing between is missed
			if events, err = c.Events(ctx, s); err != nil {
				return err
			}
		}
		v, err := c.LastFlow(ctx, s)
		if err != nil {
			return err
		}
		lipgloss.Fprintln(stdout, tui.ChartOf(v, *width))
		if !*follow || v.Done {
			return nil
		}
		for ev := range events {
			if ev.Type != api.EventFlow || ev.Flow == nil || ev.Flow.Chart == nil {
				continue
			}
			lipgloss.Fprintln(stdout, "\n"+tui.ChartOf(ev.Flow, *width))
			if ev.Flow.Done {
				return nil
			}
		}
		return ctx.Err()
	case "runs":
		fs := flag.NewFlagSet("flow runs", flag.ContinueOnError)
		fs.SetOutput(stderr)
		data := fs.String("data", runtime.DefaultDataDir(), "data directory")
		dir := fs.String("dir", ".", "working directory (\"\": every folder's)")
		flow := fs.String("flow", "", "only this flow's runs")
		tries := fs.Bool("tries", false, "only the tries of flows before they were written (ADR 0017)")
		noTries := fs.Bool("no-tries", false, "without the tries")
		n := fs.Int("n", 30, "at most this many (0: all)")
		asJSON := fs.Bool("json", false, "as JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return usage
		}
		abs := ""
		if *dir != "" {
			var err error
			if abs, err = filepath.Abs(*dir); err != nil {
				return err
			}
		}
		c, err := connect(ctx, *data)
		if err != nil {
			return err
		}
		defer c.Close()
		only := ""
		switch {
		case *tries:
			only = "tries"
		case *noTries:
			only = "runs"
		}
		runs, err := c.FlowRuns(ctx, abs, *flow, only, *n)
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(stdout).Encode(runs)
		}
		if len(runs) == 0 {
			_, err := fmt.Fprintln(stdout, "no flow has run here (or none is kept)")
			return err
		}
		for _, f := range runs {
			fmt.Fprintln(stdout, f.Line())
			fmt.Fprintln(stdout, "    kairocode flow show "+f.ID)
		}
		return nil
	case "stats":
		fs := flag.NewFlagSet("flow stats", flag.ContinueOnError)
		fs.SetOutput(stderr)
		data := fs.String("data", runtime.DefaultDataDir(), "data directory")
		dir := fs.String("dir", ".", "working directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return usage
		}
		abs, err := filepath.Abs(*dir)
		if err != nil {
			return err
		}
		c, err := connect(ctx, *data)
		if err != nil {
			return err
		}
		defer c.Close()
		stats, err := c.FlowStats(ctx, abs)
		if err != nil {
			return err
		}
		return writeFlowStats(stdout, stats)
	}
	return usage
}

// writeFlowStats puts flow stats in a table: a flow that is no longer in
// the project is marked so.
func writeFlowStats(w io.Writer, stats []api.FlowStat) error {
	if len(stats) == 0 {
		_, err := fmt.Fprintln(w, "no flows: none in "+runtime.FlowsDir+", and none ran")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FLOW\tRUNS\tOK\tFAILED\tRUNNING\tFROM\tLAST")
	for _, s := range stats {
		name, last := s.Name, "never"
		if !s.Defined {
			name += " (gone)"
		}
		if !s.Last.IsZero() {
			last = s.Last.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\t%s\n", name, s.Runs, s.OK, s.Failed, s.Running, runtime.FlowFrom(s.From), last)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	// Each flow's gates (ADR 0042): how much of the work they checked
	// passed at once, after being sent back, or not at all.
	for _, s := range stats {
		if len(s.Gates) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s: gates\n", s.Name)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  GATE\tCHECKS\tAT\tPASSED AT ONCE\tPASSED LATER\tSTOPPED")
		for _, g := range s.Gates {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%d\t%d\t%d\n", g.Kind, g.Checks, g.At, g.First, g.Later, g.Stopped)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	return nil
}
