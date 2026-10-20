package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/r-hashi01/kairocode/internal/acp"
	"github.com/r-hashi01/kairocode/internal/runtime"
	"github.com/r-hashi01/kairocode/internal/trust"
	"github.com/r-hashi01/kairocode/internal/tui"
)

// chat talks with the agent in the terminal (`kairo`, `kairocode chat`):
// in the TUI on a terminal (ADR 0010), a line at a time otherwise. Either is
// an ACP client (ADR 0002) of the ACP agent it runs in this process on the
// daemon, as an editor would be: the session runs in the daemon, and
// leaving the chat leaves it there to take up again. A folder not trusted
// yet is asked about before anything in it is read.
func chat(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", ".", "working directory")
	data := fs.String("data", runtime.DefaultDataDir(), "data directory")
	session := fs.String("session", "", "a session to take up again (as the chat printed it)")
	prov := fs.String("provider", os.Getenv("KAIROCODE_ENGINE"), "who runs the model: anthropic, claude-code, codex, copilot (/provider switches it)")
	model := fs.String("model", os.Getenv("KAIROCODE_MODEL"), "the model (/model switches it)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("chat takes no arguments, only flags: %q", fs.Args())
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	trusted, err := trust.Trusted(*data, abs)
	if err != nil {
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

	// The agent, in this process, on two pipes.
	toAgentR, toAgentW := io.Pipe()
	fromAgentR, fromAgentW := io.Pipe()
	agentCtx, stopAgent := context.WithCancel(ctx)
	agentDone := make(chan struct{})
	go func() {
		defer close(agentDone)
		defer fromAgentW.Close()
		err := acp.Serve(agentCtx, c, toAgentR, fromAgentW, acp.Options{Version: buildVersion(),
			Engine: *prov, Model: *model,
			Logger: slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))})
		if err != nil && agentCtx.Err() == nil {
			fmt.Fprintf(stderr, "kairocode: the agent: %v\n", err)
		}
	}()
	defer func() {
		toAgentW.Close()   // the agent reads to its end
		fromAgentR.Close() // and what it still writes fails, not blocks
		stopAgent()
		<-agentDone
	}()

	if in, out, ok := terminal(stdout); ok {
		if !trusted {
			yes, err := tui.AskTrust(ctx, in, out, abs)
			if err != nil {
				return err
			}
			if !yes {
				fmt.Fprintf(out, "Nothing in %s was read.\n", abs)
				return nil
			}
			if err := trust.Trust(*data, abs); err != nil {
				return err
			}
		}
		ui := tui.New(ctx, in, out, tui.Options{Dir: abs, Session: s, Version: buildVersion(), Engine: *prov, Model: *model,
			Usage: func(ctx context.Context, s string) (int64, error) {
				u, err := c.Usage(ctx, s)
				return u.Total.Input + u.Total.Output, err
			}})
		cl := acp.Dial(ctx, fromAgentR, toAgentW, acp.ClientHandler{Update: ui.Update, Permission: ui.Permission})
		if _, err := cl.Initialize(ctx); err != nil {
			return err
		}
		s, err := ui.Run(cl)
		if s != "" {
			fmt.Fprintf(out, "The session stays in the daemon: kairo -session %s takes it up again.\n", strings.TrimPrefix(s, "s/"))
		}
		return err
	}
	return lineChat(ctx, fromAgentR, toAgentW, stdout, *data, abs, s, trusted)
}

// lineChat is the chat a line at a time, on what is not a terminal.
func lineChat(ctx context.Context, fromAgent io.Reader, toAgent io.Writer, stdout io.Writer, data, abs, s string, trusted bool) error {
	t := &term{out: stdout, input: make(chan string)}
	go t.read(ctx, stdin)
	if !trusted {
		t.line("Do you trust the files in " + abs + "?")
		t.line("kairo will follow its permission rules (kairocode.json), and the agents, commands and skills in .opencode and .claude.")
		t.mu.Lock()
		io.WriteString(t.out, "trust it? (y/n): ")
		t.midLine = true
		t.mu.Unlock()
		var answer string
		select {
		case <-ctx.Done():
			return nil
		case l := <-t.input:
			answer = strings.ToLower(strings.TrimSpace(l))
			t.typed()
		}
		if answer != "y" && answer != "yes" {
			t.line("Nothing in " + abs + " was read.")
			return nil
		}
		if err := trust.Trust(data, abs); err != nil {
			return err
		}
	}
	cl := acp.Dial(ctx, fromAgent, toAgent, acp.ClientHandler{Update: t.update, Permission: t.permission})
	if _, err := cl.Initialize(ctx); err != nil {
		return err
	}
	banner := func(s string) {
		t.line(fmt.Sprintf("kairo · %s · session %s\n/help for commands, Ctrl-D to leave", abs, strings.TrimPrefix(s, "s/")))
	}
	if s == "" {
		var err error
		if s, err = cl.NewSession(ctx, abs); err != nil {
			return err
		}
		banner(s)
	} else {
		banner(s) // then what was said in it
		if err := cl.LoadSession(ctx, s, abs); err != nil {
			return err
		}
	}
	defer func() {
		t.line(fmt.Sprintf("The session stays in the daemon: kairo -session %s takes it up again.", strings.TrimPrefix(s, "s/")))
	}()
	for {
		t.prompt()
		var text string
		select {
		case <-ctx.Done():
			return nil
		case l, ok := <-t.input:
			if !ok {
				return nil
			}
			text = strings.TrimSpace(l)
			t.typed()
		}
		switch text {
		case "":
			continue
		case "/exit", "/quit":
			return nil
		case "/help":
			t.help()
			continue
		}
		stop, err := cl.Prompt(ctx, s, text)
		switch {
		case ctx.Err() != nil:
			t.line("Left during the turn: it goes on in the daemon.")
			return nil
		case err != nil:
			t.line("error: " + err.Error())
		case stop != "end_turn":
			t.line("(stopped: " + stop + ")")
		default:
			t.line("")
		}
	}
}

// term is the chat's terminal: what the agent sends is written to out as it
// comes, and the lines read from the user go to the prompt, or to the
// question for permission when one is asked.
type term struct {
	mu  sync.Mutex
	out io.Writer
	// midLine: what was written last does not end a line.
	midLine  bool
	commands []acp.Command
	// asking receives the next line while a question is asked.
	asking chan string

	input chan string // lines for the prompt; closed at the input's end
	ask   sync.Mutex  // one question at a time
}

// read gives the lines of r to the question being asked, or to the prompt.
// A read from the terminal cannot be cancelled: the goroutine ends with the
// input, or with the process; ctx stops it handing lines on.
func (t *term) read(ctx context.Context, r io.Reader) {
	defer close(t.input)
	lines := bufio.NewScanner(r)
	for lines.Scan() {
		t.mu.Lock()
		to := t.asking
		t.mu.Unlock()
		if to == nil {
			to = t.input
		}
		select {
		case to <- lines.Text():
		case <-ctx.Done():
			return
		}
	}
}

// isAsking reports whether a question waits for its answer.
func (t *term) isAsking() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.asking != nil
}

// line writes s as a line of its own.
func (t *term) line(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lineLocked(s)
}

func (t *term) lineLocked(s string) {
	if t.midLine {
		io.WriteString(t.out, "\n")
	}
	io.WriteString(t.out, s+"\n")
	t.midLine = false
}

func (t *term) prompt() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.midLine {
		io.WriteString(t.out, "\n")
	}
	io.WriteString(t.out, "> ")
	t.midLine = true
}

// typed: the user ended a line (the terminal echoed it).
func (t *term) typed() {
	t.mu.Lock()
	t.midLine = false
	t.mu.Unlock()
}

func (t *term) help() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lineLocked("/exit  leave (the session stays in the daemon)")
	for _, c := range t.commands {
		t.lineLocked(fmt.Sprintf("/%-5s %s", c.Name, c.Description))
	}
}

// update writes what the agent sends.
func (t *term) update(u acp.Update) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch u.Kind {
	case "agent_message_chunk":
		if u.Text != "" {
			io.WriteString(t.out, u.Text)
			t.midLine = !strings.HasSuffix(u.Text, "\n")
		}
	case "user_message_chunk": // a session taken up again: what was said
		t.lineLocked("> " + u.Text)
	case "tool_call":
		t.lineLocked("· " + u.Title)
	case "tool_call_update":
		switch {
		case u.Status == "failed":
			t.lineLocked("  ✗ " + oneLine(u.Text, 100))
		case u.Status == "completed" && strings.TrimSpace(u.Text) != "":
			t.lineLocked("  ⎿ " + oneLine(u.Text, 100))
		}
	case "available_commands_update":
		t.commands = u.Commands
	}
}

// permission asks the user, and waits for an answer: an option's number, or
// y or n where one option allows and another rejects.
// "n <words>" refuses, and tells the model what to do instead; "t
// <words>", where a flow can be tried first, tries it with those words
// (ADR 0017).
func (t *term) permission(ctx context.Context, p acp.PermissionRequest) acp.PermissionAnswer {
	t.ask.Lock()
	defer t.ask.Unlock()
	answer := make(chan string)
	t.mu.Lock()
	t.asking = answer
	t.lineLocked("? " + p.Title)
	for i, o := range p.Options {
		t.lineLocked(fmt.Sprintf("  %d) %s", i+1, o.Name))
	}
	yes, no := shortcut(p.Options)
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.asking = nil
		t.mu.Unlock()
	}()
	ask := fmt.Sprintf("choose 1-%d", len(p.Options))
	if yes != "" {
		ask += " (y/n, or n and what to do instead)"
	}
	try := slices.ContainsFunc(p.Options, func(o acp.PermissionOption) bool { return o.ID == acp.TryOption })
	if try {
		ask += " (or t and the words to try it with)"
	}
	for {
		t.mu.Lock()
		io.WriteString(t.out, ask+": ")
		t.midLine = true
		t.mu.Unlock()
		select {
		case <-ctx.Done():
			t.line("(answered elsewhere)")
			return acp.PermissionAnswer{}
		case typed := <-answer:
			t.typed()
			typed = strings.TrimSpace(typed)
			l := strings.ToLower(typed)
			switch {
			case l == "y" && yes != "":
				return acp.PermissionAnswer{ID: yes}
			case l == "n" && no != "":
				return acp.PermissionAnswer{ID: no}
			case strings.HasPrefix(l, "n ") && no != "":
				return acp.PermissionAnswer{ID: no, Reason: strings.TrimSpace(typed[2:])}
			case try && (l == "t" || strings.HasPrefix(l, "t ")):
				return acp.PermissionAnswer{ID: acp.TryOption, Reason: strings.TrimSpace(typed[1:])}
			}
			if n, err := strconv.Atoi(l); err == nil && n >= 1 && n <= len(p.Options) {
				return acp.PermissionAnswer{ID: p.Options[n-1].ID}
			}
		}
	}
}

// shortcut is the options y and n choose: only when one allows and another
// rejects (a review's options both "reject": they take numbers).
func shortcut(options []acp.PermissionOption) (yes, no string) {
	for _, o := range options {
		switch {
		case yes == "" && strings.HasPrefix(o.Kind, "allow"):
			yes = o.ID
		case no == "" && strings.HasPrefix(o.Kind, "reject"):
			no = o.ID
		}
	}
	if yes == "" || no == "" {
		return "", ""
	}
	return yes, no
}

// terminal is the terminal the chat runs its TUI on: stdin and stdout, when
// both are one (and KAIROCODE_PLAIN does not ask for lines).
func terminal(stdout io.Writer) (io.Reader, io.Writer, bool) {
	in, ok1 := stdin.(*os.File)
	out, ok2 := stdout.(*os.File)
	if !ok1 || !ok2 || os.Getenv("KAIROCODE_PLAIN") != "" {
		return nil, nil, false
	}
	for _, f := range []*os.File{in, out} {
		info, err := f.Stat()
		if err != nil || info.Mode()&os.ModeCharDevice == 0 {
			return nil, nil, false
		}
	}
	return in, out, true
}
