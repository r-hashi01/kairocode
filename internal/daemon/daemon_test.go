package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/lsp"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/runtime"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// The test binary is also the daemon a client starts (KC_DAEMON_CHILD).
func TestMain(m *testing.M) {
	// No language server of this machine is started (ADR 0009).
	os.Setenv(lsp.Off, "off")
	// Nor the user's claude, codex and copilot (invariant 9).
	os.Setenv("KAIROCODE_NO_CLIS", "1")
	// Nor ACP's registry of agents (ADR 0015): no test reaches the network.
	os.Setenv("KAIROCODE_ACP_REGISTRY", "off")
	if os.Getenv("KC_DAEMON_CHILD") == "1" {
		err := Serve(context.Background(), Options{Dir: os.Getenv("KC_DATA"), Open: openFake, Idle: -1, Ready: os.Stdout, WaitLock: 10 * time.Second,
			Version: os.Getenv("KC_VERSION"), DrainTimeout: drainTimeout()})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	home := isolatedHome()
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// drainTimeout is the child daemon's DrainTimeout (KC_DRAIN; default when unset).
func drainTimeout() time.Duration {
	d, _ := time.ParseDuration(os.Getenv("KC_DRAIN"))
	return d
}

// openFake opens the runtime with the fake provider KC_FAKE names.
func openFake(ctx context.Context, cfg runtime.Config) (*runtime.Runtime, error) {
	p, err := provider.LoadFake(os.Getenv("KC_FAKE"))
	if err != nil {
		return nil, err
	}
	cfg.Provider = p
	return runtime.Open(ctx, cfg)
}

var writeScript = []provider.FakeReply{
	{Tools: []provider.FakeTool{{Name: "write", Input: json.RawMessage(`{"path":"notes.txt","content":"hello\n"}`)}}},
	{Text: "done"},
}

// dataDir is a data directory whose daemon is killed when the test ends.
func dataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { killDaemon(dir) })
	return dir
}

func killDaemon(dir string) {
	if info, err := ReadInfo(dir); err == nil {
		syscall.Kill(info.PID, syscall.SIGKILL)
	}
}

// writeFake writes a fake provider's script and returns its path.
func writeFake(t *testing.T, path string, f provider.Fake) string {
	t.Helper()
	b, _ := json.Marshal(f)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// childStart starts this test binary as the daemon of data.
func childStart(data, fake string) Start {
	return Start{
		Argv: []string{os.Args[0], "-test.run=^$"},
		Env:  append(os.Environ(), "KC_DAEMON_CHILD=1", "KC_DATA="+data, "KC_FAKE="+fake),
	}
}

// workspace is a directory whose kairocode.json holds permission.
func workspace(t *testing.T, permission string) string {
	t.Helper()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, runtime.ConfigFile), []byte(`{"permission": `+permission+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

func waitPending(t *testing.T, ctx context.Context, c *Client, session string) []runtime.Pending {
	t.Helper()
	for {
		p, err := c.Pending(ctx, session)
		if err != nil {
			t.Fatal(err)
		}
		if len(p) > 0 {
			return p
		}
		select {
		case <-ctx.Done():
			t.Fatal("nothing came to wait for approval")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A second daemon for the same data directory does not start.
func TestSecondDaemonDoesNotStart(t *testing.T) {
	data := t.TempDir()
	fake := writeFake(t, filepath.Join(t.TempDir(), "fake.json"), provider.Fake{HoldAt: -1})
	t.Setenv("KC_FAKE", fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, Options{Dir: data, Open: openFake, Idle: -1, Ready: signalWriter(ready)})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatal(err)
	}
	if err := Serve(ctx, Options{Dir: data, Open: openFake, Idle: -1}); !errors.Is(err, ErrRunning) {
		t.Fatalf("a second daemon: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, infoFile)); !os.IsNotExist(err) {
		t.Errorf("server.json left behind: %v", err)
	}
}

// signalWriter closes ch at the first write.
type signalWriter chan struct{}

func (w signalWriter) Write(b []byte) (int, error) {
	close(w)
	return len(b), nil
}

// A client starts the daemon when none runs; the next client finds it.
// server.json can be read only by its owner.
func TestClientStartsTheDaemon(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a daemon process")
	}
	data := dataDir(t)
	start := childStart(data, writeFake(t, filepath.Join(t.TempDir(), "fake.json"), provider.Fake{HoldAt: -1}))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a, err := Connect(ctx, data, start)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Connect(ctx, data, start)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if a.PID() != b.PID() || a.PID() == os.Getpid() {
		t.Fatalf("pids %d and %d (test %d)", a.PID(), b.PID(), os.Getpid())
	}
	st, err := os.Stat(filepath.Join(data, infoFile))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("server.json mode %v", st.Mode().Perm())
	}
}

// TestApprovalFromAnotherClient: one client waits for a turn that waits for
// approval; another gives it. The turn goes on as an event in the daemon,
// well inside kairo's lease (30s), which is left at its default: the sweep
// did not find it (ADR 0008).
func TestApprovalFromAnotherClient(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a daemon process")
	}
	data := dataDir(t)
	ws := workspace(t, `{"write": "ask"}`)
	start := childStart(data, writeFake(t, filepath.Join(t.TempDir(), "fake.json"), provider.Fake{Script: writeScript, HoldAt: -1}))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a, err := Connect(ctx, data, start)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	s, err := a.CreateSession(ctx, workflows.SessionInput{Dir: ws})
	if err != nil {
		t.Fatal(err)
	}
	n, err := a.Send(ctx, s, "write the notes")
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		turn Turn
		err  error
	}
	waited := make(chan result, 1)
	go func() {
		out, err := a.WaitTurn(ctx, s, n)
		waited <- result{out, err}
	}()

	b, err := Connect(ctx, data, start)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	p := waitPending(t, ctx, b, s)
	approved := time.Now()
	if err := b.Approve(ctx, p[0].Key, true, ""); err != nil {
		t.Fatal(err)
	}
	r := <-waited
	if r.err != nil {
		t.Fatal(r.err)
	}
	took := time.Since(approved)
	t.Logf("the turn went on %v after the approval", took.Round(time.Millisecond))
	if took > 2*time.Second {
		t.Errorf("the turn went on %v after the approval: found by a sweep, not told", took)
	}
	if r.turn.Reply != "done" {
		t.Fatalf("reply %q", r.turn.Reply)
	}
	if got, err := os.ReadFile(filepath.Join(ws, "notes.txt")); err != nil || string(got) != "hello\n" {
		t.Fatalf("the approved write was not made: %q %v", got, err)
	}
}

// TestCrashDaemonRestart: the daemon is killed in the middle of a model call;
// the next client starts a new one, which takes over at once (the same
// owner, no lease to wait out) and finishes the turn.
func TestCrashDaemonRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("kills a daemon process")
	}
	data := dataDir(t)
	ws := workspace(t, `{}`)
	dir := t.TempDir()
	hold := filepath.Join(dir, "held")
	script := []provider.FakeReply{{Text: "done"}}
	fake := writeFake(t, filepath.Join(dir, "fake.json"), provider.Fake{Script: script, HoldAt: 0, HoldFile: hold})
	start := childStart(data, fake)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a, err := Connect(ctx, data, start)
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.CreateSession(ctx, workflows.SessionInput{Dir: ws})
	if err != nil {
		t.Fatal(err)
	}
	n, err := a.Send(ctx, s, "hi")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(hold); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("the model call never started")
		case <-time.After(10 * time.Millisecond):
		}
	}
	old := a.PID()
	a.Close()
	if err := syscall.Kill(old, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	// Gone (reaped by the client that started it), so the next client
	// cannot reach it on its way out.
	for syscall.Kill(old, 0) == nil {
		select {
		case <-ctx.Done():
			t.Fatal("the killed daemon did not go")
		case <-time.After(10 * time.Millisecond):
		}
	}

	writeFake(t, fake, provider.Fake{Script: script, HoldAt: -1})
	restarted := time.Now()
	b, err := Connect(ctx, data, start)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.PID() == old {
		t.Fatal("reached the killed daemon")
	}
	out, err := b.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(restarted)
	t.Logf("the turn finished %v after the restart", took.Round(time.Millisecond))
	if out.Reply != "done" {
		t.Fatalf("reply %q", out.Reply)
	}
	if took > 20*time.Second {
		t.Errorf("took %v: waited out the killed daemon's lease", took)
	}
}

// startServe runs a daemon in this process and returns once it is ready,
// with the channel its end is sent to.
func startServe(t *testing.T, ctx context.Context, o Options) <-chan error {
	t.Helper()
	ready := make(chan struct{})
	done := make(chan error, 1)
	o.Ready = signalWriter(ready)
	go func() { done <- Serve(ctx, o) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatal(err)
	}
	return done
}

// A daemon started while the last one is on its way out waits for its lock
// and takes over, instead of failing (ADR 0008).
func TestDaemonWaitsForTheOneOnItsWayOut(t *testing.T) {
	data := t.TempDir()
	t.Setenv("KC_FAKE", writeFake(t, filepath.Join(t.TempDir(), "fake.json"), provider.Fake{HoldAt: -1}))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	oldCtx, stopOld := context.WithCancel(ctx)
	old := startServe(t, oldCtx, Options{Dir: data, Open: openFake, Idle: -1})
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, Options{Dir: data, Open: openFake, Idle: -1, Ready: signalWriter(ready), WaitLock: 20 * time.Second})
	}()
	stopOld()
	if err := <-old; err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("the new daemon did not take over: %v", err)
	}
	cancel()
	<-done
}

// A daemon does not stop while a client waits on it, even with nothing to
// run (a turn waiting for approval), and stops once the client goes.
func TestDaemonStaysWhileAClientWaits(t *testing.T) {
	data := t.TempDir()
	ws := workspace(t, `{"write": "ask"}`)
	t.Setenv("KC_FAKE", writeFake(t, filepath.Join(t.TempDir(), "fake.json"), provider.Fake{Script: writeScript, HoldAt: -1}))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := startServe(t, ctx, Options{Dir: data, Open: openFake, Idle: 200 * time.Millisecond})
	c, err := dial(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := c.CreateSession(ctx, workflows.SessionInput{Dir: ws})
	n, _ := c.Send(ctx, s, "write the notes")
	waitPending(t, ctx, c, s)
	wctx, stopWaiting := context.WithCancel(ctx)
	go c.WaitTurn(wctx, s, n)
	select {
	case err := <-done:
		t.Fatalf("stopped while a client waited: %v", err)
	case <-time.After(time.Second): // five idle periods
	}
	stopWaiting()
	c.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("did not stop once the client went")
	}
}

// A data directory whose path is too long for a socket puts the socket in a
// directory only this user can enter.
func TestLongDataDirSocket(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 100))
	p, err := socketPath(long)
	if err != nil {
		t.Fatal(err)
	}
	if len(p) >= 100 {
		t.Fatalf("socket path still long: %s", p)
	}
	st, err := os.Stat(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Errorf("%s has mode %v", filepath.Dir(p), st.Mode().Perm())
	}
}

// childStartAt is childStart for a build named version.
func childStartAt(data, fake, version string) Start {
	s := childStart(data, fake)
	s.Env = append(s.Env, "KC_VERSION="+version)
	s.Version = version
	return s
}

// A client of another build stops a daemon no one else uses and starts its
// own (Codex checks the daemon's build too).
func TestOtherBuildReplacesTheDaemon(t *testing.T) {
	if testing.Short() {
		t.Skip("starts daemon processes")
	}
	data := dataDir(t)
	fake := writeFake(t, filepath.Join(t.TempDir(), "fake.json"), provider.Fake{HoldAt: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a, err := Connect(ctx, data, childStartAt(data, fake, "v1"))
	if err != nil {
		t.Fatal(err)
	}
	old := a.PID()
	a.Close()
	b, err := Connect(ctx, data, childStartAt(data, fake, "v2"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	st, err := b.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.PID == old || st.Version != "v2" || b.Stale != "" {
		t.Fatalf("status %+v (old pid %d), stale %q", st, old, b.Stale)
	}
}

// A daemon of another build that other clients use is left to them, and
// the client is told.
func TestOtherBuildKeepsABusyDaemon(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a daemon process")
	}
	data := dataDir(t)
	fake := writeFake(t, filepath.Join(t.TempDir(), "fake.json"), provider.Fake{HoldAt: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a, err := Connect(ctx, data, childStartAt(data, fake, "v1"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close() // its connection stays open: a client that uses the daemon
	b, err := Connect(ctx, data, childStartAt(data, fake, "v2"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.PID() != a.PID() || b.Stale == "" {
		t.Fatalf("pids %d %d, stale %q", a.PID(), b.PID(), b.Stale)
	}
}

// A daemon of another build that runs a step is not stopped: the client
// would wait for the step, or cut it if it outlasts the drain.
func TestOtherBuildKeepsADaemonThatRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a daemon process")
	}
	data := dataDir(t)
	dir := t.TempDir()
	hold := filepath.Join(dir, "held")
	fake := writeFake(t, filepath.Join(dir, "fake.json"), provider.Fake{Script: []provider.FakeReply{{Text: "done"}}, HoldAt: 0, HoldFile: hold})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a, err := Connect(ctx, data, childStartAt(data, fake, "v1"))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := a.CreateSession(ctx, workflows.SessionInput{Dir: workspace(t, `{}`)})
	a.Send(ctx, s, "hi")
	for {
		if _, err := os.Stat(hold); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("the model call never started")
		case <-time.After(10 * time.Millisecond):
		}
	}
	old := a.PID()
	a.Close()
	b, err := Connect(ctx, data, childStartAt(data, fake, "v2"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.PID() != old || b.Stale == "" {
		t.Fatalf("pid %d (was %d), stale %q", b.PID(), old, b.Stale)
	}
}

// `kairocode daemon stop`: it returns once the daemon has let go of the
// data directory.
func TestStopTheDaemon(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a daemon process")
	}
	data := dataDir(t)
	fake := writeFake(t, filepath.Join(t.TempDir(), "fake.json"), provider.Fake{HoldAt: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a, err := Connect(ctx, data, childStart(data, fake))
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	if running, err := Stop(ctx, data); err != nil || !running {
		t.Fatalf("stop: %v %v", running, err)
	}
	if _, err := ReadInfo(data); !os.IsNotExist(err) {
		t.Fatalf("server.json after the stop: %v", err)
	}
	lock, err := takeLock(ctx, filepath.Join(data, lockFile), 0)
	if err != nil {
		t.Fatalf("the lock after the stop: %v", err)
	}
	lock.Close()
	if running, err := Stop(ctx, data); err != nil || running {
		t.Fatalf("a second stop: %v %v", running, err)
	}
}

// A daemon that stops lets the step under way finish and records its
// outcome: the next daemon goes on from it, and does not run it again.
func TestStopLetsTheRunningStepFinish(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a daemon process")
	}
	data := dataDir(t)
	dir := t.TempDir()
	hold, release, calls := filepath.Join(dir, "held"), filepath.Join(dir, "release"), filepath.Join(dir, "calls")
	fake := writeFake(t, filepath.Join(dir, "fake.json"), provider.Fake{Script: []provider.FakeReply{{Text: "done"}},
		HoldAt: 0, HoldFile: hold, ReleaseFile: release, CallLog: calls})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a, err := Connect(ctx, data, childStart(data, fake))
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.CreateSession(ctx, workflows.SessionInput{Dir: workspace(t, `{}`)})
	if err != nil {
		t.Fatal(err)
	}
	n, err := a.Send(ctx, s, "hi")
	if err != nil {
		t.Fatal(err)
	}
	waitFile(t, ctx, hold)
	a.Close()
	stopped := make(chan error, 1)
	go func() {
		_, err := Stop(ctx, data)
		stopped <- err
	}()
	// server.json goes once draining has begun: the model call is released
	// only then, so it finishes while the daemon drains.
	for {
		if _, err := ReadInfo(data); os.IsNotExist(err) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("the daemon did not begin to stop")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	b, err := Connect(ctx, data, childStart(data, fake))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	turn, err := b.WaitTurn(ctx, s, n)
	if err != nil || turn.Reply != "done" {
		t.Fatalf("turn: %+v %v", turn, err)
	}
	log, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(log), "\n"); got != 1 {
		t.Fatalf("the model was called %d times, want 1:\n%s", got, log)
	}
}

// A step that outlasts the drain is cut, and the stop is still answered
// once the daemon has let go: the daemon does not end before its reply.
// The drain runs past 10s, the wait the daemon had for its reply before.
func TestStopCutsAStepThatOutlastsTheDrain(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a daemon process and drains for 11s")
	}
	data := dataDir(t)
	dir := t.TempDir()
	hold := filepath.Join(dir, "held")
	fake := writeFake(t, filepath.Join(dir, "fake.json"), provider.Fake{Script: []provider.FakeReply{{Text: "done"}},
		HoldAt: 0, HoldFile: hold})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	start := childStart(data, fake)
	start.Env = append(start.Env, "KC_DRAIN=11s")
	a, err := Connect(ctx, data, start)
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.CreateSession(ctx, workflows.SessionInput{Dir: workspace(t, `{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Send(ctx, s, "hi"); err != nil {
		t.Fatal(err)
	}
	waitFile(t, ctx, hold)
	a.Close()
	if running, err := Stop(ctx, data); err != nil || !running {
		t.Fatalf("stop: %v %v", running, err)
	}
	lock, err := takeLock(ctx, filepath.Join(data, lockFile), 0)
	if err != nil {
		t.Fatalf("the lock after the stop: %v", err)
	}
	lock.Close()
}

// waitFile waits for path to appear.
func waitFile(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s never appeared", path)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A daemon with nothing to run and no client stops by itself.
func TestIdleDaemonStops(t *testing.T) {
	data := t.TempDir()
	t.Setenv("KC_FAKE", writeFake(t, filepath.Join(t.TempDir(), "fake.json"), provider.Fake{HoldAt: -1}))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := Serve(ctx, Options{Dir: data, Open: openFake, Idle: 100 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("did not stop when idle")
	}
}

// nextEvent is the next event that is not a token.
func nextEvent(t *testing.T, ctx context.Context, events <-chan runtime.Event) runtime.Event {
	t.Helper()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("the events ended")
			}
			if ev.Type != runtime.EventToken {
				return ev
			}
		case <-ctx.Done():
			t.Fatal("no event:", ctx.Err())
		}
	}
}

// Two clients watch a session through the daemon. Both are asked for the
// approval; the first answer goes through, the other is refused, and both
// are told the answer and follow the turn to its end (issue #5).
func TestApprovalToManyClients(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a daemon process")
	}
	data := dataDir(t)
	ws := workspace(t, `{"write": "ask"}`)
	start := childStart(data, writeFake(t, filepath.Join(t.TempDir(), "fake.json"), provider.Fake{Script: writeScript, HoldAt: -1}))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a, err := Connect(ctx, data, start)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Connect(ctx, data, start)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	s, err := a.CreateSession(ctx, workflows.SessionInput{Dir: ws})
	if err != nil {
		t.Fatal(err)
	}
	ea, err := a.Events(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	eb, err := b.Events(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Send(ctx, s, "write the notes"); err != nil {
		t.Fatal(err)
	}
	var key string
	for _, events := range []<-chan runtime.Event{ea, eb} {
		for {
			ev := nextEvent(t, ctx, events)
			if ev.Type == runtime.EventApprovalRequested {
				if key != "" && ev.Pending.Key != key {
					t.Fatalf("two keys: %s %s", key, ev.Pending.Key)
				}
				key = ev.Pending.Key
				break
			}
		}
	}
	if err := b.Approve(ctx, key, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := a.Approve(ctx, key, false, ""); !errors.Is(err, runtime.ErrAnswered) {
		t.Fatalf("the second answer: %v", err)
	}
	for _, events := range []<-chan runtime.Event{ea, eb} {
		var resolved bool
		for {
			ev := nextEvent(t, ctx, events)
			if ev.Type == runtime.EventApprovalResolved {
				if ev.Key != key || ev.OK == nil || !*ev.OK {
					t.Fatalf("resolved %+v", ev)
				}
				resolved = true
			}
			if ev.Type == runtime.EventTurnEnd {
				if !resolved || ev.Reply != "done" {
					t.Fatalf("turn end %+v (resolved %v)", ev, resolved)
				}
				break
			}
		}
	}
}

// TestCrashDaemonEventsAfterRestart: the daemon is killed while a command
// runs. A client that comes back starts a new daemon and is told first that
// the call waits for review; its answer settles it, and the turn ends
// (issue #7).
func TestCrashDaemonEventsAfterRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("crash test: starts and kills a daemon")
	}
	data := dataDir(t)
	ws := workspace(t, `{"bash": "allow"}`)
	start := childStart(data, writeFake(t, filepath.Join(t.TempDir(), "fake.json"), provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "bash", Input: json.RawMessage(`{"command":"echo ran >> count.txt; touch started; sleep 5"}`)}}},
		{Text: "done"},
	}}))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a, err := Connect(ctx, data, start)
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.CreateSession(ctx, workflows.SessionInput{Dir: ws})
	if err != nil {
		t.Fatal(err)
	}
	n, err := a.Send(ctx, s, "count")
	if err != nil {
		t.Fatal(err)
	}
	waitFile(t, ctx, filepath.Join(ws, "started"))
	if err := syscall.Kill(a.PID(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	a.Close()

	b, err := Connect(ctx, data, start)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	events, err := b.Events(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	review := nextEvent(t, ctx, events)
	if review.Type != runtime.EventReviewRequested || review.Pending == nil || review.Tool != "bash" {
		t.Fatalf("first event %+v", review)
	}
	if err := b.Approve(ctx, review.Pending.Key, true, ""); err != nil {
		t.Fatal(err)
	}
	for {
		ev := nextEvent(t, ctx, events)
		if ev.Type == runtime.EventTurnEnd {
			if ev.N != n || ev.Reply != "done" {
				t.Fatalf("turn end %+v", ev)
			}
			break
		}
	}
	if got, _ := os.ReadFile(filepath.Join(ws, "count.txt")); string(got) != "ran\n" {
		t.Fatalf("the command ran again: %q", got)
	}
}

// isolatedHome points HOME and XDG_CONFIG_HOME at a fresh directory, so the
// user's own settings, agents and skills stay out of the tests (and of the
// processes they start). It returns the directory, to remove.
func isolatedHome() string {
	home, err := os.MkdirTemp("", "kc-home")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}
