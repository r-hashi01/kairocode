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
	"strconv"
	"strings"
	"sync"

	"github.com/r-hashi01/kairocode/internal/acp"
	"github.com/r-hashi01/kairocode/internal/runtime"
)

// chat talks with the agent in the terminal, a line at a time (`kairo`,
// `kairocode chat`). It is an ACP client (ADR 0002) of the ACP agent it runs
// in this process on the daemon, as an editor would be: the session runs in
// the daemon, and leaving the chat leaves it there to take up again.
func chat(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", ".", "working directory")
	data := fs.String("data", runtime.DefaultDataDir(), "data directory")
	session := fs.String("session", "", "a session to take up again (as the chat printed it)")
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
			Engine: os.Getenv("KAIROCODE_ENGINE"), Model: os.Getenv("KAIROCODE_MODEL"),
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

	t := &term{out: stdout, input: make(chan string)}
	go t.read(ctx, stdin)
	cl := acp.Dial(ctx, fromAgentR, toAgentW, acp.ClientHandler{Update: t.update, Permission: t.permission})
	if _, err := cl.Initialize(ctx); err != nil {
		return err
	}
	banner := func(s string) {
		t.line(fmt.Sprintf("kairo · %s · session %s\n/help for commands, Ctrl-D to leave", abs, strings.TrimPrefix(s, "s/")))
	}
	s := *session
	if s == "" {
		if s, err = cl.NewSession(ctx, abs); err != nil {
			return err
		}
		banner(s)
	} else {
		if !strings.HasPrefix(s, "s/") {
			s = runtime.SessionID(s)
		}
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
func (t *term) permission(ctx context.Context, p acp.PermissionRequest) string {
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
		ask += " (y/n)"
	}
	for {
		t.mu.Lock()
		io.WriteString(t.out, ask+": ")
		t.midLine = true
		t.mu.Unlock()
		select {
		case <-ctx.Done():
			t.line("(answered elsewhere)")
			return ""
		case l := <-answer:
			t.typed()
			l = strings.ToLower(strings.TrimSpace(l))
			switch {
			case l == "y" && yes != "":
				return yes
			case l == "n" && no != "":
				return no
			}
			if n, err := strconv.Atoi(l); err == nil && n >= 1 && n <= len(p.Options) {
				return p.Options[n-1].ID
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
