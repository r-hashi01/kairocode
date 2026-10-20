package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/acp"
	"github.com/r-hashi01/kairocode/internal/daemon"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/trust"
)

// chatting runs the chat with args, on a stdin the test writes to.
type chatting struct {
	in   *io.PipeWriter
	out  *lockedBuffer
	errs *lockedBuffer
	code chan int
}

func startChat(t *testing.T, ctx context.Context, args ...string) *chatting {
	t.Helper()
	inR, inW := io.Pipe()
	stdin = inR
	t.Cleanup(func() { stdin = os.Stdin })
	c := &chatting{in: inW, out: &lockedBuffer{}, errs: &lockedBuffer{}, code: make(chan int, 1)}
	go func() { c.code <- Main(ctx, args, c.out, c.errs) }()
	return c
}

// say types a line.
func (c *chatting) say(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(c.in, line+"\n"); err != nil {
		t.Fatal(err)
	}
}

// wait waits until the chat has written want (count times in all).
func (c *chatting) wait(t *testing.T, ctx context.Context, want string, count int) {
	t.Helper()
	until(t, ctx, "the chat writes "+want, func() bool { return strings.Count(c.out.String(), want) >= count })
}

// end ends the input and waits for the chat to end well.
func (c *chatting) end(t *testing.T) {
	t.Helper()
	c.in.Close()
	if code := <-c.code; code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, c.errs.String(), c.out.String())
	}
}

// The chat (`kairo`): a turn streams its reply, shows the tool call, asks
// for permission in the terminal and goes on with the answer; leaving says
// how to come back, and -session takes the session up again with what was
// said.
func TestChat(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a daemon process")
	}
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "kairocode.json"), []byte(`{"permission": {"write": "ask"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KAIROCODE_PROVIDER", "fake")
	t.Setenv("KAIROCODE_FAKE", fakeScript(t, dir,
		provider.FakeReply{Tools: []provider.FakeTool{{Name: "write", Input: json.RawMessage(`{"path":"notes.txt","content":"hi\n"}`)}}},
		provider.FakeReply{Text: "I wrote the notes for you"},
		provider.FakeReply{Text: "nothing more"},
	))
	data := dataDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c := startChat(t, ctx, "chat", "-dir", ws, "-data", data)
	c.wait(t, ctx, "trust it? (y/n): ", 1)
	c.say(t, "y")
	c.wait(t, ctx, "> ", 1)
	c.say(t, "write notes")
	c.wait(t, ctx, "choose 1-2 (y/n, or n and what to do instead): ", 1)
	for _, want := range []string{"· write notes.txt", "? write notes.txt", "1) Allow", "2) Reject"} {
		if !strings.Contains(c.out.String(), want) {
			t.Errorf("no %q in\n%s", want, c.out.String())
		}
	}
	c.say(t, "maybe") // not an answer: asked again
	c.wait(t, ctx, "choose 1-2 (y/n, or n and what to do instead): ", 2)
	c.say(t, "y")
	c.wait(t, ctx, "> ", 2)
	if b, err := os.ReadFile(filepath.Join(ws, "notes.txt")); err != nil || string(b) != "hi\n" {
		t.Fatalf("notes.txt %q %v", b, err)
	}
	if out := c.out.String(); !strings.Contains(out, "I wrote the notes for you") {
		t.Fatalf("no reply in\n%s", out)
	}
	c.say(t, "/help")
	c.wait(t, ctx, "/undo", 1)
	c.say(t, "/exit")
	if code := <-c.code; code != 0 {
		t.Fatalf("exit %d: %s", code, c.errs.String())
	}
	out := c.out.String()
	_, rest, ok := strings.Cut(out, "kairo -session ")
	session, _, _ := strings.Cut(rest, " ")
	if !ok || session == "" {
		t.Fatalf("no way back in\n%s", out)
	}

	// Taken up again, without the chat command: what was said comes first.
	c = startChat(t, ctx, "-dir", ws, "-data", data, "-session", session)
	c.wait(t, ctx, "> ", 2)
	for _, want := range []string{"> write notes\n", "· write notes.txt", "I wrote the notes for you"} {
		if !strings.Contains(c.out.String(), want) {
			t.Errorf("no %q in what came again\n%s", want, c.out.String())
		}
	}
	c.say(t, "anything else?")
	c.wait(t, ctx, "nothing more", 1)
	c.end(t)
}

// `kairo` with no arguments is the chat, in the current directory, on the
// default data directory.
func TestNoCommandIsTheChat(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a daemon process")
	}
	t.Setenv("KAIROCODE_PROVIDER", "fake")
	t.Setenv("KAIROCODE_FAKE", fakeScript(t, t.TempDir()))
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	t.Cleanup(func() {
		if info, err := daemon.ReadInfo(filepath.Join(xdg, "kairocode")); err == nil {
			syscall.Kill(info.PID, syscall.SIGKILL)
		}
	})
	ws := t.TempDir()
	t.Chdir(ws)
	if err := trust.Trust(filepath.Join(xdg, "kairocode"), ws); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := startChat(t, ctx)
	c.wait(t, ctx, "> ", 1)
	c.end(t)
	if real, _ := filepath.EvalSymlinks(ws); !strings.Contains(c.out.String(), "kairo · "+ws) && !strings.Contains(c.out.String(), "kairo · "+real) {
		t.Fatalf("not in %s:\n%s", ws, c.out.String())
	}
}

// The question for permission: y and n where one option allows and another
// rejects, an option's number, and anything else asks again. A review's
// options both "reject": y is no answer there. A request withdrawn (it was
// answered elsewhere) stops asking.
func TestChatAsks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	inR, inW := io.Pipe()
	defer inW.Close()
	out := &lockedBuffer{}
	tm := &term{out: out, input: make(chan string)}
	go tm.read(ctx, inR)
	allow := []acp.PermissionOption{{ID: "allow_once", Name: "Allow", Kind: "allow_once"}, {ID: "reject_once", Name: "Reject", Kind: "reject_once"}}
	review := []acp.PermissionOption{{ID: "did_not_take_effect", Name: "No", Kind: "reject_once"}, {ID: "took_effect", Name: "Yes", Kind: "reject_once"}}
	for _, c := range []struct {
		options []acp.PermissionOption
		typed   []string
		want    string
	}{
		{allow, []string{"y"}, "allow_once"},
		{allow, []string{"N"}, "reject_once"},
		{allow, []string{"what", "3", "2"}, "reject_once"},
		{review, []string{"y", "2"}, "took_effect"},
		{review, []string{"1"}, "did_not_take_effect"},
		{allow, []string{"n write it to b.txt instead"}, "reject_once: write it to b.txt instead"},
	} {
		got := make(chan string, 1)
		go func() {
			a := tm.permission(ctx, acp.PermissionRequest{Title: "write x", Options: c.options})
			if a.Reason != "" {
				a.ID += ": " + a.Reason
			}
			got <- a.ID
		}()
		for i, line := range c.typed {
			until(t, ctx, "asked", tm.isAsking)
			io.WriteString(inW, line+"\n")
			if i < len(c.typed)-1 {
				until(t, ctx, "asked again", func() bool { return strings.HasSuffix(out.String(), ": ") })
			}
		}
		if g := <-got; g != c.want {
			t.Errorf("%v typed %q: %q, want %q", c.options, c.typed, g, c.want)
		}
	}
	withdrawn, stop := context.WithCancel(ctx)
	got := make(chan string, 1)
	go func() { got <- tm.permission(withdrawn, acp.PermissionRequest{Title: "write y", Options: allow}).ID }()
	until(t, ctx, "asked", tm.isAsking)
	stop()
	if g := <-got; g != "" || !strings.Contains(out.String(), "(answered elsewhere)") {
		t.Fatalf("withdrawn: %q\n%s", g, out.String())
	}
	// The next line goes to the prompt, not to a question.
	io.WriteString(inW, "hello\n")
	if l := <-tm.input; l != "hello" {
		t.Fatalf("the prompt got %q", l)
	}
}

// A folder not trusted: no starts nothing (the session would read its
// rules); the answer is not kept, and the next chat asks again.
func TestChatInAFolderNotTrusted(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a daemon process")
	}
	t.Setenv("KAIROCODE_PROVIDER", "fake")
	t.Setenv("KAIROCODE_FAKE", fakeScript(t, t.TempDir()))
	data := dataDir(t)
	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := startChat(t, ctx, "-dir", ws, "-data", data)
	c.wait(t, ctx, "trust it? (y/n): ", 1)
	c.say(t, "n")
	if code := <-c.code; code != 0 {
		t.Fatalf("exit %d: %s", code, c.errs.String())
	}
	if out := c.out.String(); !strings.Contains(out, "Nothing in") || strings.Contains(out, "session") {
		t.Fatalf("out\n%s", out)
	}
	if ok, err := trust.Trusted(data, ws); ok || err != nil {
		t.Fatalf("trusted after no: %v %v", ok, err)
	}
}
