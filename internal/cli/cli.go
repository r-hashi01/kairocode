// Package cli is the kairocode command (design 8.2).
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/r-hashi01/kairocode/internal/daemon"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/runtime"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

const usage = `usage:
  kairocode run [-dir DIR] [-data DIR] "<request>"   answer one request, then exit
  kairocode approvals [-data DIR] <session>          list the calls waiting for a person
  kairocode approve [-data DIR] [-deny] [-reason R] <key>   answer one
  kairocode daemon [-data DIR]                       run the daemon in the foreground

One daemon per data directory runs the sessions (ADR 0008); the commands
above start it when it is not running, and it stops after 10 minutes with
nothing to do (KAIROCODE_DAEMON_IDLE). The data directory defaults to
$XDG_DATA_HOME/kairocode or ~/.local/share/kairocode.

The daemon takes its settings from the environment it starts in:
KAIROCODE_ENGINE picks who runs the model loop:
  api (default)   the Claude API (credentials as the Anthropic SDK resolves them)
  claude-code     your own Claude Code, signed in with your plan (ADR 0007);
                  KAIROCODE_CLAUDE names the claude command (default: claude)
KAIROCODE_MODEL picks the model. KAIROCODE_PROVIDER=fake with
KAIROCODE_FAKE=<script.json> scripts the API engine's replies (tests).
`

// Main runs the command with args (without the program name) and returns the
// process's exit code.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "run":
		err = run(ctx, args[1:], stdout, stderr)
	case "daemon":
		err = runDaemon(ctx, args[1:], stdout, stderr)
	case "approvals":
		err = approvals(ctx, args[1:], stdout, stderr)
	case "approve":
		err = approve(ctx, args[1:], stderr)
	case "-h", "-help", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "kairocode: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "kairocode: %v\n", err)
		return 1
	}
	return 0
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

// openRuntime opens the daemon's runtime with the engine and the model the
// environment names.
func openRuntime(ctx context.Context, cfg runtime.Config) (*runtime.Runtime, error) {
	cfg.Engine, cfg.Model = os.Getenv("KAIROCODE_ENGINE"), os.Getenv("KAIROCODE_MODEL")
	if cfg.Engine == workflows.EngineClaudeCode {
		cfg.ClaudePath = os.Getenv("KAIROCODE_CLAUDE")
		return runtime.Open(ctx, cfg)
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
	return daemon.Connect(ctx, data, daemon.Start{Argv: argv, Env: os.Environ()})
}

func runDaemon(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", runtime.DefaultDataDir(), "data directory")
	if err := fs.Parse(args); err != nil {
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
	return daemon.Serve(ctx, daemon.Options{Dir: *data, Open: openRuntime, Idle: idle, Ready: stdout, WaitLock: 10 * time.Second})
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
	s, err := c.CreateSession(ctx, abs, "")
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "session %s\n", s)
	n, err := c.Send(ctx, s, request)
	if err != nil {
		return err
	}
	out, err := c.WaitTurn(ctx, s, n)
	if err != nil {
		return err
	}
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
