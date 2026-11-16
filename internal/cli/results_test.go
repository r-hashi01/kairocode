package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/provider"
)

// resultsWorkspace is a folder with settings, and the fake provider
// answering from f (written beside it).
func resultsWorkspace(t *testing.T, settings string, f provider.Fake) string {
	t.Helper()
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if settings != "" {
		if err := os.WriteFile(filepath.Join(ws, "kairocode.json"), []byte(settings), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "fake.json")
	if err := os.WriteFile(fake, b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KAIROCODE_PROVIDER", "fake")
	t.Setenv("KAIROCODE_FAKE", fake)
	return ws
}

// runResult reads the one JSON object stdout holds.
func runResult(t *testing.T, stdout string) api.RunResult {
	t.Helper()
	var r api.RunResult
	dec := json.NewDecoder(strings.NewReader(stdout))
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("stdout is not a result: %v: %q", err, stdout)
	}
	if dec.More() {
		t.Fatalf("stdout holds more than the result: %q", stdout)
	}
	return r
}

// Each way a turn ends has its exit code, and -json says the same
// (ADR 0041).
func TestRunExitCodes(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a daemon process")
	}
	write := json.RawMessage(`{"path":"notes.txt","content":"hi\n"}`)
	cases := []struct {
		name     string
		settings string
		script   []provider.FakeReply
		args     []string
		code     int
		status   string
		stopped  string
		reply    string
	}{
		{name: "done", script: []provider.FakeReply{{Text: "all done"}}, code: 0, status: api.StatusDone, reply: "all done"},
		{name: "refusal", script: []provider.FakeReply{{Text: "no", Stop: provider.StopRefusal}}, code: 4, status: api.StatusStopped, stopped: "refusal", reply: "no"},
		{name: "limit", settings: `{"permission": {"write": "allow"}, "limits": {"session": {"tokens": 10}}}`,
			script: []provider.FakeReply{{Tools: []provider.FakeTool{{Name: "write", Input: write}}}, {Text: "written"}},
			code:   4, status: api.StatusStopped, stopped: api.StoppedLimit},
		{name: "detach", settings: `{"permission": {"write": "ask"}}`, args: []string{"-detach"},
			script: []provider.FakeReply{{Tools: []provider.FakeTool{{Name: "write", Input: write}}}, {Text: "written"}},
			code:   3, status: api.StatusWaiting},
		{name: "failed", script: nil, code: 1, status: api.StatusError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := resultsWorkspace(t, tc.settings, provider.Fake{Script: tc.script, HoldAt: -1})
			data := dataDir(t)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			args := append([]string{"run", "-json", "-dir", ws, "-data", data}, tc.args...)
			var stdout, stderr bytes.Buffer
			code := Main(ctx, append(args, "write", "notes"), &stdout, &stderr)
			if code != tc.code {
				t.Fatalf("exit %d, want %d: %s", code, tc.code, stderr.String())
			}
			r := runResult(t, stdout.String())
			if r.Status != tc.status || r.Stopped != tc.stopped || !strings.HasPrefix(r.Session, "s/") || r.Turn == nil || *r.Turn != 0 {
				t.Fatalf("result %+v", r)
			}
			switch tc.status {
			case api.StatusWaiting:
				if len(r.Waiting) != 1 || r.Waiting[0].Tool != "write" || r.Reply != nil {
					t.Fatalf("waiting %+v", r)
				}
			case api.StatusError:
				if r.Error == "" || r.Reply != nil {
					t.Fatalf("error %+v", r)
				}
			default:
				if r.Reply == nil || r.Steps == nil || r.Usage == nil || r.Usage.Output == 0 {
					t.Fatalf("ended without its reply, steps and usage: %s", stdout.String())
				}
				if tc.reply != "" && *r.Reply != tc.reply {
					t.Fatalf("reply %q", *r.Reply)
				}
			}
		})
	}
}

// A turn cancelled from another client exits 5; this command interrupted
// exits 130, and resume takes the turn up (ADR 0041).
func TestRunCancelledAndInterrupted(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a daemon process")
	}
	dir := t.TempDir()
	hold, release := filepath.Join(dir, "held"), filepath.Join(dir, "release")
	ws := resultsWorkspace(t, "", provider.Fake{Script: []provider.FakeReply{{Text: "done"}}, HoldAt: 0, HoldFile: hold, ReleaseFile: release})
	data := dataDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	held := func() bool { _, err := os.Stat(hold); return err == nil }

	// Interrupted while the model call is held.
	cmd, stopCmd := context.WithCancel(ctx)
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	code := make(chan int, 1)
	go func() { code <- Main(cmd, []string{"run", "-json", "-dir", ws, "-data", data, "hi"}, stdout, stderr) }()
	until(t, ctx, "the model call starts", held)
	stopCmd()
	if c := <-code; c != 130 {
		t.Fatalf("interrupted: exit %d: %s", c, stderr.String())
	}
	r := runResult(t, stdout.String())
	if r.Status != api.StatusInterrupted || r.Turn == nil || r.Reply != nil {
		t.Fatalf("interrupted: %+v", r)
	}
	// The turn went on in the daemon: resume follows it to its end.
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if c := Main(ctx, []string{"resume", "-json", "-data", data, r.Session}, &out, &errs); c != 0 {
		t.Fatalf("resume: exit %d: %s", c, errs.String())
	}
	if r := runResult(t, out.String()); r.Status != api.StatusDone || r.Reply == nil || *r.Reply != "done" {
		t.Fatalf("resume: %s", out.String())
	}

	// Cancelled from another client: a new session, held again.
	if err := os.Remove(release); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	stdout, stderr = &lockedBuffer{}, &lockedBuffer{}
	go func() { code <- Main(ctx, []string{"run", "-json", "-dir", ws, "-data", data, "hi"}, stdout, stderr) }()
	until(t, ctx, "the model call starts", held)
	session := ""
	until(t, ctx, "the session is named", func() bool {
		s, _, ok := strings.Cut(strings.TrimPrefix(stderr.String(), "session "), "\n")
		session = s
		return ok
	})
	c, err := connect(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Cancel(ctx, session); err != nil {
		t.Fatal(err)
	}
	if got := <-code; got != 5 {
		t.Fatalf("cancelled: exit %d: %s", got, stderr.String())
	}
	if r := runResult(t, stdout.String()); r.Status != api.StatusCancelled || r.Session != session {
		t.Fatalf("cancelled: %s", stdout.String())
	}
}

// Wrong use exits 2; an error before a session prints no JSON.
func TestRunWrongUse(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Main(context.Background(), []string{"run", "-json", "-nosuchflag", "x"}, &stdout, &stderr); code != 2 || stdout.Len() != 0 {
		t.Fatalf("exit %d: %q %q", code, stdout.String(), stderr.String())
	}
	stderr.Reset()
	if code := Main(context.Background(), []string{"resume", "-json"}, &stdout, &stderr); code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "usage: kairocode resume") {
		t.Fatalf("exit %d: %q %q", code, stdout.String(), stderr.String())
	}
}

// approvals -json lists what waits as an array, [] when nothing does.
func TestApprovalsJSON(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a daemon process")
	}
	ws := resultsWorkspace(t, `{"permission": {"write": "ask"}}`, provider.Fake{Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "write", Input: json.RawMessage(`{"path":"notes.txt","content":"hi\n"}`)}}}, {Text: "written"},
	}, HoldAt: -1})
	data := dataDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	if c := Main(ctx, []string{"run", "-detach", "-json", "-dir", ws, "-data", data, "write", "notes"}, &stdout, &stderr); c != 3 {
		t.Fatalf("run: exit %d: %s", c, stderr.String())
	}
	r := runResult(t, stdout.String())
	var out bytes.Buffer
	if c := Main(ctx, []string{"approvals", "-json", "-data", data, r.Session}, &out, &stderr); c != 0 {
		t.Fatalf("approvals: exit %d: %s", c, stderr.String())
	}
	var p []api.Pending
	if err := json.Unmarshal(out.Bytes(), &p); err != nil || len(p) != 1 || p[0].Key != r.Waiting[0].Key {
		t.Fatalf("approvals %q: %v", out.String(), err)
	}
	if c := Main(ctx, []string{"approve", "-data", data, p[0].Key}, &out, &stderr); c != 0 {
		t.Fatalf("approve: exit %d: %s", c, stderr.String())
	}
	out.Reset()
	if c := Main(ctx, []string{"resume", "-json", "-data", data, r.Session}, &out, &stderr); c != 0 {
		t.Fatalf("resume: exit %d: %s", c, stderr.String())
	}
	out.Reset()
	if c := Main(ctx, []string{"approvals", "-json", "-data", data, r.Session}, &out, &stderr); c != 0 || strings.TrimSpace(out.String()) != "[]" {
		t.Fatalf("approvals after: exit %d: %q", c, out.String())
	}
}

// Interrupted before a session exists, run exits 130 with no JSON: there
// is no turn to go on with.
func TestRunInterruptedBeforeASession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	if code := Main(ctx, []string{"run", "-json", "-dir", t.TempDir(), "-data", dataDir(t), "hi"}, &stdout, &stderr); code != 130 || stdout.Len() != 0 {
		t.Fatalf("exit %d: %q %q", code, stdout.String(), stderr.String())
	}
}
