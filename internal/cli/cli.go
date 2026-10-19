// Package cli is the kairocode command (design 8.2).
package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/r-hashi01/kairocode/internal/acp"
	"github.com/r-hashi01/kairocode/internal/daemon"
	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/runtime"
	"github.com/r-hashi01/kairocode/internal/tui"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

const usage = `usage:
  kairocode [chat] [-dir DIR] [-data DIR] [-session ID]   talk with it in the terminal (kairo is the same)
  kairocode run [-dir DIR] [-data DIR] "<request>"   answer one request, then exit
  kairocode approvals [-data DIR] <session>          list the calls waiting for a person
  kairocode approve [-data DIR] [-deny] [-reason R] <key>   answer one
  kairocode answer [-data DIR] <key> <answer>         answer a flow's question
  kairocode flow graph [-dir DIR] [-width N] <name>   draw a flow of .kairocode/workflows (ADR 0003)
  kairocode flow show [-data DIR] [-follow] <session> the session's latest flow, as it has gone
  kairocode daemon [-data DIR]                       run the daemon in the foreground
  kairocode daemon [-data DIR] stop                  stop it (steps under way get up to 30s to finish)
  kairocode usage [-data DIR] -session ID [-json]     the tokens a session's model calls used
  kairocode acp [-data DIR]                          an ACP agent on stdin and stdout, for editors (ADR 0002)

One daemon per data directory runs the sessions (ADR 0008); the commands
above start it when it is not running, and it stops after 10 minutes with
nothing to do (KAIROCODE_DAEMON_IDLE). A command of another build replaces
it when it runs no step and no other client uses it. The data directory defaults to
$XDG_DATA_HOME/kairocode or ~/.local/share/kairocode.

KAIROCODE_ENGINE picks who runs the model loop of the sessions you start:
  api (default)   the Claude API (credentials as the Anthropic SDK resolves them)
  claude-code     your own Claude Code, signed in with your plan (ADR 0007);
                  KAIROCODE_CLAUDE names the claude command (default: claude)
KAIROCODE_MODEL picks the model. The daemon takes the rest from the
environment it starts in: KAIROCODE_CLAUDE, and KAIROCODE_PROVIDER=fake with
KAIROCODE_FAKE=<script.json> to script the API engine's replies (tests).
`

// Main runs the command with args (without the program name) and returns the
// process's exit code.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var err error
	switch {
	case len(args) > 0 && isHelp(args[0]):
		fmt.Fprint(stdout, usage)
		return 0
	case len(args) == 0 || strings.HasPrefix(args[0], "-"):
		// No command: the chat, as claude has it.
		err = chat(ctx, args, stdout, stderr)
	default:
		err = command(ctx, args[0], args[1:], stdout, stderr)
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

var errUnknownCommand = errors.New("unknown command")

// command runs the command named cmd.
func command(ctx context.Context, cmd string, args []string, stdout, stderr io.Writer) (err error) {
	switch cmd {
	case "chat":
		err = chat(ctx, args, stdout, stderr)
	case "run":
		err = run(ctx, args, stdout, stderr)
	case "daemon":
		err = runDaemon(ctx, args, stdout, stderr)
	case "approvals":
		err = approvals(ctx, args, stdout, stderr)
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
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return []string{exe, "daemon", "-data", data}, nil
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
	f, err := os.Open(exe)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
})

func runDaemon(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", runtime.DefaultDataDir(), "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.Arg(0) == "stop" {
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
	return daemon.Serve(ctx, daemon.Options{Dir: *data, Open: openRuntime, Idle: idle, Ready: stdout, Version: buildVersion(),
		// A daemon on its way out may drain its steps for up to DefaultDrain.
		WaitLock: daemon.DefaultDrain + 10*time.Second, Cut: cut})
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", ".", "working directory")
	data := fs.String("data", runtime.DefaultDataDir(), "data directory")
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
	c, err := connect(ctx, *data)
	if err != nil {
		return err
	}
	defer c.Close()
	// The session runs with this client's choice of engine and model, not
	// whatever the daemon was started with.
	s, err := c.CreateSession(ctx, workflows.SessionInput{
		Dir: abs, Engine: os.Getenv("KAIROCODE_ENGINE"), Model: os.Getenv("KAIROCODE_MODEL"),
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "session %s\n", s)
	// What the turn does is shown as it happens; its result comes from
	// WaitTurn, which does not depend on the stream.
	// Watching begins before the message is sent, so no event of the turn
	// is missed.
	watch, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	events, err := c.Events(watch, s)
	if err != nil {
		return err
	}
	turnDone := make(chan struct{})
	shown := make(chan struct{})
	n := -1
	sent := make(chan struct{})
	go func() {
		defer close(shown)
		showEvents(watch, c, s, events, stderr, sent, &n, turnDone)
	}()
	if flow, ok := strings.CutPrefix(request, "/flow "); ok { // a flow (ADR 0003)
		name, args, _ := strings.Cut(strings.TrimSpace(flow), " ")
		if name == "new" {
			n, err = c.NewFlow(ctx, s, args)
		} else {
			n, err = c.RunFlow(ctx, s, name, strings.TrimSpace(args))
		}
	} else {
		n, err = c.Send(ctx, s, request)
	}
	close(sent)
	if err != nil {
		return err
	}
	out, err := c.WaitTurn(ctx, s, n)
	close(turnDone)
	if err != nil {
		return err
	}
	<-shown
	fmt.Fprintln(stdout, out.Reply)
	if out.Stopped != "" {
		fmt.Fprintf(stderr, "stopped: %s\n", out.Stopped)
	}
	fmt.Fprintf(stderr, "steps %d, tokens in %d out %d (cache read %d)\n",
		out.Steps, out.Usage.Input, out.Usage.Output, out.Usage.CacheRead)
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
	p, err := c.Pending(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	if len(p) == 0 {
		fmt.Fprintln(stdout, "nothing waits for approval")
	}
	for _, x := range p {
		if x.Kind == runtime.KindQuestion {
			fmt.Fprintf(stdout, "%s\n  question: %s\n  answers: %s (kairocode answer <key> <answer>)\n", x.Key, x.Subject, strings.Join(x.Options, " | "))
			continue
		}
		fmt.Fprintf(stdout, "%s\n  %s %q (%s)\n", x.Key, x.Tool, x.Subject, x.Rule)
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
func showEvents(ctx context.Context, c *daemon.Client, s string, events <-chan runtime.Event, w io.Writer,
	sent <-chan struct{}, n *int, turnDone <-chan struct{}) {
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
			if ev.Type == runtime.EventTurnEnd {
				<-sent
				if ev.N == *n {
					return
				}
			}
			showEvent(w, ev)
		case <-until:
			return
		}
	}
}

// showEvent writes one event as a line, if it is one to show. Tokens are
// not: the reply is written whole when the turn ends.
func showEvent(w io.Writer, ev runtime.Event) {
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
	case runtime.EventToolStart:
		fmt.Fprintf(w, "· %s %s\n", ev.Tool, oneLine(string(ev.Input), 80))
	case runtime.EventToolEnd:
		if ev.IsError {
			fmt.Fprintf(w, "  ✗ %s: %s\n", ev.Tool, oneLine(ev.Output, 80))
		} else {
			fmt.Fprintf(w, "  ✓ %s\n", ev.Tool)
		}
	case runtime.EventApprovalRequested:
		fmt.Fprintf(w, "waiting for approval: %s %s (%s)\n  kairocode approve %s\n",
			ev.Pending.Tool, ev.Pending.Subject, ev.Pending.Rule, ev.Pending.Key)
	case runtime.EventReviewRequested:
		fmt.Fprintf(w, "cut off while it ran, did it take effect? %s %s\n  kairocode approve %s   (or -deny: it did not)\n",
			ev.Pending.Tool, ev.Pending.Subject, ev.Pending.Key)
	case runtime.EventQuestionRequested:
		fmt.Fprintf(w, "the flow asks: %s\n  answers: %s\n  kairocode answer %s <answer>\n",
			ev.Pending.Subject, strings.Join(ev.Pending.Options, " | "), ev.Pending.Key)
	case runtime.EventQuestionResolved:
		fmt.Fprintln(w, "  answered")
	case runtime.EventApprovalResolved:
		fmt.Fprintf(w, "  %s\n", answer(ev.OK, "approved", "declined"))
	case runtime.EventReviewResolved:
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
	if *session == "" {
		return errors.New("usage needs -session <ID>")
	}
	s := *session
	if !strings.HasPrefix(s, "s/") {
		s = runtime.SessionID(s)
	}
	c, err := connect(ctx, *data)
	if err != nil {
		return err
	}
	defer c.Close()
	u, err := c.Usage(ctx, s)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(u)
	}
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
	usage := errors.New("kairocode flow graph [-dir DIR] [-width N] <name> | kairocode flow show [-data DIR] [-width N] [-follow] <session> | kairocode flow stats [-data DIR] [-dir DIR]")
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
		var events <-chan runtime.Event
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
			if ev.Type != runtime.EventFlow || ev.Flow == nil || ev.Flow.Chart == nil {
				continue
			}
			lipgloss.Fprintln(stdout, "\n"+tui.ChartOf(ev.Flow, *width))
			if ev.Flow.Done {
				return nil
			}
		}
		return ctx.Err()
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
func writeFlowStats(w io.Writer, stats []runtime.FlowStat) error {
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
	return tw.Flush()
}
