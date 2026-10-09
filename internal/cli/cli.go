// Package cli is the kairocode command (design 8.2).
package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/runtime"
	"github.com/r-hashi01/kairocode/internal/server"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

const usage = `usage:
  kairocode run [-dir DIR] [-db FILE] "<request>"   answer one request, then exit
  kairocode serve [-addr ADDR] [-db FILE]            run the HTTP API

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
	case "serve":
		err = serve(ctx, args[1:], stderr)
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

func openRuntime(ctx context.Context, db string) (*runtime.Runtime, error) {
	cfg := runtime.Config{DB: db, Engine: os.Getenv("KAIROCODE_ENGINE"), Model: os.Getenv("KAIROCODE_MODEL")}
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

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", ".", "working directory")
	db := fs.String("db", runtime.DefaultDB(), "session database")
	if err := fs.Parse(args); err != nil {
		return err
	}
	request := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if request == "" {
		return errors.New(`run needs a request: kairocode run "<request>"`)
	}
	rt, err := openRuntime(ctx, *db)
	if err != nil {
		return err
	}
	defer rt.Close()
	s, err := rt.CreateSession(ctx, newID(), workflows.SessionInput{Dir: *dir})
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "session %s\n", s)
	n, err := rt.Send(ctx, s, request)
	if err != nil {
		return err
	}
	out, err := rt.WaitTurn(ctx, s, n)
	if err != nil {
		return err
	}
	text, err := rt.Reply(ctx, out.Reply)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, text)
	if out.Stopped != "" {
		fmt.Fprintf(stderr, "stopped: %s\n", out.Stopped)
	}
	fmt.Fprintf(stderr, "steps %d, tokens in %d out %d (cache read %d)\n",
		out.Steps, out.Usage.Input, out.Usage.Output, out.Usage.CacheRead)
	return rt.CloseSession(ctx, s)
}

func serve(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:4097", "listen address")
	db := fs.String("db", runtime.DefaultDB(), "session database")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rt, err := openRuntime(ctx, *db)
	if err != nil {
		return err
	}
	defer rt.Close()
	token := os.Getenv("KAIROCODE_SERVER_TOKEN")
	if token == "" {
		token = newID() + newID()
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "listening on http://%s (token in KAIROCODE_SERVER_TOKEN, or: %s)\n", ln.Addr(), token)
	srv := &http.Server{Handler: server.New(rt, token, newID)}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
