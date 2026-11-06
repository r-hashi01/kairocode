package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/sandbox/sandboxtest"
	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// waitFlow starts a command in the background, reads it, and waits for it
// (ADR 0028). The command runs until the file release is in the working
// directory.
const waitFlow = `
def main(input):
    s = tool("bash", command = "echo begun; while [ ! -f release ]; do sleep 0.05; done; echo ended; exit 3", background = True)
    if not s.ok:
        return "not started: " + s.error
    id = s.output.split("started in the background: ")[1].split(" ")[0]
    seen = tool("bash_output", id = id)
    w = tool("bash_wait", id = id)
    return id + "\n" + seen.output + "\n---\n" + w.output
`

// stopJob stops a command of session in the background and waits until
// its end is told. Until then its runner and r write in its folder, which
// t.TempDir's cleanup removes; and a kill before the runner wrote its pid
// stops nothing.
func stopJob(t *testing.T, r *Runtime, session, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	waitUntil(t, ctx, "the runner", func() bool { return fileThere(r.jobs.path(id, jobFilePID)) })
	if err := (jobEnv{m: r.jobs, session: session}).Kill(id); err != nil {
		t.Fatal(err)
	}
	waitTold(t, ctx, r, id)
}

// waitTold waits until a command's end is told: the last r writes in its
// folder.
func waitTold(t *testing.T, ctx context.Context, r *Runtime, id string) {
	t.Helper()
	waitUntil(t, ctx, "its end told", func() bool { return fileThere(r.jobs.path(id, jobFileNotified)) })
}

// waitAllTold waits until the end of every command of r is told: one
// ended, and waited for, may still be told when the test ends.
func waitAllTold(t *testing.T, ctx context.Context, r *Runtime) {
	t.Helper()
	entries, _ := os.ReadDir(r.jobs.dir)
	for _, e := range entries {
		if e.IsDir() {
			waitTold(t, ctx, r, e.Name())
		}
	}
}

// jobFolder waits for the one command in the background of r to be
// waited for, and gives its folder.
func waitedJob(t *testing.T, r *Runtime) string {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(r.jobs.dir)
		for _, e := range entries {
			if fileThere(r.jobs.path(e.Name(), jobFileWaiters)) {
				return r.jobs.path(e.Name())
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no command was waited for")
	return ""
}

// A flow starts a command in the background, reads what it wrote, and
// waits for its end: the wait is a signal, and the command's end wakes it
// with its output and status (ADR 0028).
func TestBackgroundCommandWaited(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/long.star": waitFlow})
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	done := make(chan string, 1)
	session := make(chan string, 1)
	go func() {
		_, text, s := flowTurn(t, r, ws, "long", "")
		session <- s
		done <- text
	}()
	waitedJob(t, r)
	if err := os.WriteFile(filepath.Join(ws, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var text string
	select {
	case text = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the wait did not end")
	}
	before, waited, _ := strings.Cut(text, "\n---\n")
	if !strings.HasPrefix(text, "bg_") || !strings.Contains(before, "begun") || !strings.Contains(before, ": running") {
		t.Fatalf("the read while it ran:\n%s", before)
	}
	if !strings.Contains(waited, "ended: exit 3") || !strings.Contains(waited, "begun\nended") {
		t.Fatalf("the wait's result:\n%s", waited)
	}
	// Waited for, its end is not told to the session as well.
	s := <-session
	id, _, _ := strings.Cut(text, "\n")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	waitUntil(t, ctx, "its end told", func() bool { return fileThere(r.jobs.path(id, jobFileNotified)) })
	if conv := strings.Join(conversationOf(t, r, s, true), "\n"); strings.Contains(conv, "[KairoCode]") {
		t.Fatalf("told again:\n%s", conv)
	}
}

// Waiting survives the runtime closed and opened again: the wait is in
// kairo and the waiter on disk; the runner, going on meanwhile, tells the
// runtime that is there when the command ends (through the daemon:
// JobDone).
func TestBackgroundWaitSurvivesRestart(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/long.star": waitFlow})
	db := filepath.Join(t.TempDir(), "k.db")
	r1 := open(t, db, &provider.Fake{HoldAt: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	s, err := r1.CreateSession(ctx, "restart", workflows.SessionInput{Dir: ws})
	if err != nil {
		t.Fatal(err)
	}
	n, err := r1.RunFlow(ctx, s, "long", "")
	if err != nil {
		t.Fatal(err)
	}
	folder := waitedJob(t, r1)
	r1.Close()

	r2 := open(t, db, &provider.Fake{HoldAt: -1})
	if err := os.WriteFile(filepath.Join(ws, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, ctx, "the command's end", func() bool { return fileThere(filepath.Join(folder, jobFileStatus)) })
	if err := r2.JobDone(filepath.Base(folder)); err != nil { // the runner, through the daemon
		t.Fatal(err)
	}
	out, err := r2.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := r2.Reply(ctx, out.Reply)
	if !strings.Contains(text, "ended: exit 3") {
		t.Fatalf("after the restart:\n%s", text)
	}
}

// A command not waited for, ended, is told to its session: a message
// starts the next turn. One whose end was read is not told again.
func TestBackgroundEndToldToTheSession(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/start.star": `
def main(input):
    s = tool("bash", command = "while [ ! -f release ]; do sleep 0.05; done; echo built", background = True)
    return s.output
`})
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1,
		Script: []provider.FakeReply{{Text: "I read it"}, {Text: "I read it"}}}) // the session's title may take one
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, text, s := flowTurn(t, r, ws, "start", "")
	id := strings.Split(strings.Split(text, "started in the background: ")[1], " ")[0]
	if err := os.WriteFile(filepath.Join(ws, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var conv string
	waitUntil(t, ctx, "the next turn", func() bool {
		conv = strings.Join(conversationOf(t, r, s, true), "\n")
		return strings.Contains(conv, "I read it")
	})
	if !strings.Contains(conv, "[KairoCode] The command in the background "+id+" ended (exit 0)") || !strings.Contains(conv, "I read it") {
		t.Fatalf("not told:\n%s", conv)
	}
	waitAllTold(t, ctx, r)
}

// A command runs out of its time, or is stopped; a session has at most
// jobsPerSession at once.
func TestBackgroundTimeoutKillAndLimit(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	ws := repo(t)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	env := func(key string) jobEnv { return jobEnv{m: r.jobs, session: "s/limits", dir: ws, key: key} }
	id, err := env("timed").Start(tools.JobSpec{Command: "sleep 30", Timeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, ctx, "the timeout", func() bool { v, _ := env("").View(id, 0); return !v.Running })
	waitTold(t, ctx, r, id)
	if v, _ := env("").View(id, 0); !strings.HasPrefix(v.Status, "timed out") {
		t.Fatalf("timed out: %+v", v)
	}
	if _, err := env("x").View(id, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := (jobEnv{m: r.jobs, session: "s/other", dir: ws}).View(id, 0); err == nil {
		t.Fatal("another session read it")
	}

	var ids []string
	for i := range jobsPerSession {
		id, err := env("k" + string(rune('a'+i))).Start(tools.JobSpec{Command: "sleep 30", Timeout: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, err := env("one-more").Start(tools.JobSpec{Command: "sleep 30", Timeout: time.Minute}); err == nil {
		t.Fatal("more than the most at once")
	}
	for _, id := range ids {
		stopJob(t, r, "s/limits", id)
		if v, _ := env("").View(id, 0); v.Status != jobStatusStop {
			t.Fatalf("stopped: %+v", v)
		}
	}
}

// crashJobChild is TestCrashBackgroundWait's child: it runs the waiting
// flow and is killed once the flow waits for its command.
func crashJobChild() {
	ctx := context.Background()
	r, err := Open(ctx, Config{DB: os.Getenv("KC_DB"), Provider: &provider.Fake{HoldAt: -1}, Owner: "crash", JobRunner: jobRunner})
	if err != nil {
		panic(err)
	}
	s, err := r.CreateSession(ctx, "crash", workflows.SessionInput{Dir: os.Getenv("KC_DIR")})
	if err != nil {
		panic(err)
	}
	if _, err := r.RunFlow(ctx, s, "long", ""); err != nil {
		panic(err)
	}
	for {
		entries, _ := os.ReadDir(r.jobs.dir)
		for _, e := range entries {
			if fileThere(r.jobs.path(e.Name(), jobFileWaiters)) {
				os.WriteFile(os.Getenv("KC_READY"), nil, 0o600)
				select {}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCrashBackgroundWait: the process killed while a flow waits for its
// command in the background, the command goes on; the next process takes
// the wait up, and the command's end (told by its runner) ends it. The
// command ran once, and was started once (ADR 0028, invariants 4 and 7).
func TestCrashBackgroundWait(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	if testing.Short() {
		t.Skip("starts a child process and kills it")
	}
	dir := t.TempDir()
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/long.star": waitFlow})
	db, ready := filepath.Join(dir, "k.db"), filepath.Join(dir, "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "KC_CRASH_JOB_CHILD=1", "KC_DB="+db, "KC_DIR="+ws, "KC_READY="+ready)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, ready, cmd)
	cmd.Process.Signal(syscall.SIGKILL)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := Open(ctx, Config{DB: db, Owner: "crash", Provider: &provider.Fake{HoldAt: -1}, JobRunner: jobRunner})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	entries, _ := os.ReadDir(r.jobs.dir)
	if len(entries) != 1 {
		t.Fatalf("%d commands in the background, want 1", len(entries))
	}
	id := entries[0].Name()
	if err := os.WriteFile(filepath.Join(ws, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, ctx, "the command's end", func() bool { return fileThere(r.jobs.path(id, jobFileStatus)) })
	if err := r.JobDone(id); err != nil { // its runner, through the daemon
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, SessionID("crash"), 0)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := r.Reply(ctx, out.Reply)
	if !strings.Contains(text, "ended: exit 3") {
		t.Fatalf("reply:\n%s", text)
	}
	b, _ := os.ReadFile(r.jobs.path(id, jobFileOut))
	if string(b) != "begun\nended\n" {
		t.Fatalf("the command's output %q: it ran more than once", b)
	}
	entries, _ = os.ReadDir(r.jobs.dir)
	if len(entries) != 1 {
		t.Fatalf("%d commands after the restart: one was started again", len(entries))
	}
}

// A wait that ran out does not wait any more: the command's end, later,
// is told to the session (ADR 0028, 5).
func TestBackgroundWaitRanOutThenEnded(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/short.star": `
def main(input):
    s = tool("bash", command = "while [ ! -f release ]; do sleep 0.05; done; echo built", background = True)
    id = s.output.split("started in the background: ")[1].split(" ")[0]
    w = tool("bash_wait", id = id, timeout_ms = 200)
    return id + "\n" + w.output
`})
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1,
		Script: []provider.FakeReply{{Text: "I read it"}, {Text: "I read it"}}}) // the session's title may take one
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, text, s := flowTurn(t, r, ws, "short", "")
	id, waited, _ := strings.Cut(text, "\n")
	if !strings.Contains(waited, "It has not ended yet") {
		t.Fatalf("the wait:\n%s", waited)
	}
	if ws := r.jobs.readWaiters(id); len(ws) != 0 {
		t.Fatalf("still waited for by %v", ws)
	}
	if err := os.WriteFile(filepath.Join(ws, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, ctx, "the session told", func() bool {
		return strings.Contains(strings.Join(conversationOf(t, r, s, true), "\n"), "[KairoCode] The command in the background "+id+" ended (exit 0)")
	})
	waitAllTold(t, ctx, r)
}

// Two waits for one command in one run (calls at once) each have a signal
// of their own: one does not take the other's.
func TestBackgroundTwoWaitsInARun(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	e := jobEnv{m: r.jobs, session: "s/two", dir: repo(t), key: "two"}
	id, err := e.Start(tools.JobSpec{Command: "sleep 30", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer stopJob(t, r, "s/two", id)
	a, _, err := r.jobs.watch("s/two", id, "s/two/turn", "call_a")
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := r.jobs.watch("s/two", id, "s/two/turn", "call_b")
	again, _, _ := r.jobs.watch("s/two", id, "s/two/turn", "call_a") // the same call, run again
	if a == b || a != again || len(r.jobs.readWaiters(id)) != 2 {
		t.Fatalf("signals %q %q %q, waits %v", a, b, again, r.jobs.readWaiters(id))
	}
	r.jobs.unwatch(id, "s/two/turn", a)
	if ws := r.jobs.readWaiters(id); len(ws) != 1 || ws[0].Signal != b {
		t.Fatalf("after one ran out: %v", ws)
	}
}

// A runner is alive while it holds its lock, not because its pid is some
// process's (after a reboot, a pid is another's).
func TestJobRunnerAlive(t *testing.T) {
	t.Parallel()
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	id := "bg_000000000001"
	os.MkdirAll(r.jobs.path(id), 0o700)
	spec := func(started time.Time) {
		b, _ := json.Marshal(jobSpec{ID: id, Session: "s/x", Started: started})
		os.WriteFile(r.jobs.path(id, jobFileSpec), b, 0o600)
	}
	spec(time.Now())
	if !r.jobs.alive(id) {
		t.Fatal("a runner just started (no lock yet) taken for gone")
	}
	spec(time.Now().Add(-time.Hour))
	os.WriteFile(r.jobs.path(id, jobFilePID), []byte(strconv.Itoa(os.Getpid())), 0o600) // a live pid, not a runner's
	if r.jobs.alive(id) {
		t.Fatal("a pid alone taken for a runner")
	}
	f, _ := os.OpenFile(r.jobs.path(id, jobFileLock), os.O_CREATE|os.O_RDWR, 0o600)
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	other, _ := os.OpenFile(r.jobs.path(id, jobFileLock), os.O_RDWR, 0o600) // flock is per open file
	defer other.Close()
	if err := syscall.Flock(int(other.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		t.Fatal("the lock is not exclusive")
	}
	if !r.jobs.alive(id) {
		t.Fatal("a runner holding its lock taken for gone")
	}
}

// A wait with no deadline arms no timer: the daemon may stop meanwhile,
// and is started again by the command's runner when it ends. One with a
// deadline has a timer: the daemon stays up while it waits (ADR 0028).
func TestAwaitingWhileWaiting(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	ws := repo(t)
	putFiles(t, ws, map[string]string{
		".kairocode/workflows/long.star":  waitFlow,
		".kairocode/workflows/timed.star": strings.Replace(waitFlow, `tool("bash_wait", id = id)`, `tool("bash_wait", id = id, timeout_ms = 120000)`, 1),
	})
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, c := range []struct {
		flow     string
		awaiting bool
	}{{"long", false}, {"timed", true}} {
		os.Remove(filepath.Join(ws, "release"))
		os.RemoveAll(r.jobs.dir)
		done := make(chan string, 1)
		go func() {
			_, text, _ := flowTurn(t, r, ws, c.flow, "")
			done <- text
		}()
		waitUntil(t, ctx, "the wait", func() bool { return waiting(ctx, r) })
		if got := r.Awaiting(ctx); got != c.awaiting {
			t.Fatalf("%s: awaiting %v while it waits", c.flow, got)
		}
		os.WriteFile(filepath.Join(ws, "release"), nil, 0o644)
		if text := <-done; !strings.Contains(text, "ended: exit 3") {
			t.Fatalf("%s: %s", c.flow, text)
		}
		waitUntil(t, ctx, "nothing awaited", func() bool { return !r.Awaiting(ctx) })
		waitAllTold(t, ctx, r)
	}
}

// waiting says a run of r waits for a command's end (kairo's wait on it).
func waiting(ctx context.Context, r *Runtime) bool {
	var walk func(id string) bool
	walk = func(id string) bool {
		kids, _ := r.K.Children(ctx, id)
		for _, k := range kids {
			if strings.HasPrefix(k.Plan, "kairo.wait/"+workflows.SignalJobPrefix) && k.Status == "running" {
				return true
			}
			if walk(k.RunID) {
				return true
			}
		}
		return false
	}
	runs, _ := r.Sessions(ctx, "", 100)
	for _, s := range runs {
		if walk(s.ID) {
			return true
		}
	}
	return false
}

// A runtime that did not start a command's runner (the daemon replaced
// meanwhile) still learns of its end when the runner is killed and cannot
// tell it: its lock is let go with the process (ADR 0028).
func TestRunnerKilledAfterRestart(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	db := filepath.Join(t.TempDir(), "k.db")
	ws := repo(t)
	r1 := open(t, db, &provider.Fake{HoldAt: -1})
	id, err := (jobEnv{m: r1.jobs, session: "s/killed", dir: ws, key: "killed"}).Start(tools.JobSpec{Command: "sleep 30", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	waitUntil(t, ctx, "the runner", func() bool { return fileThere(r1.jobs.path(id, jobFilePID)) })
	r1.Close()

	r2 := open(t, db, &provider.Fake{HoldAt: -1})
	b, _ := os.ReadFile(r2.jobs.path(id, jobFilePID))
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil { // the runner and its command
		t.Fatal(err)
	}
	waitUntil(t, ctx, "its end told", func() bool { return fileThere(r2.jobs.path(id, jobFileNotified)) })
	if v, _ := (jobEnv{m: r2.jobs, session: "s/killed"}).View(id, 0); v.Running || v.Status != jobStatusLost {
		t.Fatalf("after the runner was killed: %+v", v)
	}
}
