// Package daemon is the one process per data directory that runs kairo
// (ADR 0008), and the client every other kairocode process uses to reach it.
//
// The daemon holds daemon.lock for as long as it runs, so a second one does
// not start. It serves the HTTP API (internal/server) on a Unix socket and
// writes where it is to server.json. Everything that happens between live
// processes is then an event inside the daemon: an approval is a Signal
// there, and the turn that waits for it wakes at once.
package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/runtime"
	"github.com/r-hashi01/kairocode/internal/server"
)

const (
	lockFile   = "daemon.lock"
	startLock  = "start.lock"
	infoFile   = "server.json"
	socketFile = "kairocode.sock"
	logFile    = "daemon.log"

	// Owner is the daemon's name in kairo's leases. Only one daemon runs per
	// data directory, so a daemon started after one was killed takes over
	// what it left at once, without waiting for its leases to run out.
	//
	// Safe only while nothing else opens the database under this name:
	// kairo expires every lease of the name at Open, and a second process
	// with it would take steps that still run (and run them twice). The
	// lock keeps it to one process per SQLite file; a shared database
	// (PostgreSQL, several hosts) needs a name per host.
	Owner = "kairocode-daemon"

	// DefaultIdle is how long a daemon with nothing to run and no client
	// stays up.
	DefaultIdle = 10 * time.Minute
	// DefaultDrain is how long a daemon that stops waits for the steps under
	// way to finish.
	DefaultDrain = 30 * time.Second
)

// ErrRunning: another daemon runs for the data directory.
var ErrRunning = errors.New("a daemon already runs for this data directory")

// Info is server.json: how a client reaches the daemon.
type Info struct {
	Socket  string `json:"socket"`
	Token   string `json:"token"`
	PID     int    `json:"pid"`
	Version string `json:"version,omitempty"`
}

// ReadInfo reads the data directory's server.json.
func ReadInfo(dir string) (Info, error) {
	var in Info
	b, err := os.ReadFile(filepath.Join(dir, infoFile))
	if err != nil {
		return in, err
	}
	return in, json.Unmarshal(b, &in)
}

type Options struct {
	// Dir is the data directory: the database, the lock, server.json and the
	// socket live there.
	Dir string
	// Open opens the runtime with cfg (DB, Owner and Observe set by the
	// daemon); the caller adds the engine and the model.
	Open func(ctx context.Context, cfg runtime.Config) (*runtime.Runtime, error)
	// Idle is how long the daemon stays up with no step running and no
	// client connected (DefaultIdle when zero; negative: forever).
	Idle time.Duration
	// Ready, if set, is told "ready" once clients can connect.
	Ready io.Writer
	// WaitLock is how long to wait for a daemon that holds the data
	// directory to end (zero: not at all). A client starts a daemon only
	// when it found none answering: one that holds the lock then is one on
	// its way out (idle, or killed and not yet gone).
	WaitLock time.Duration
	// Version names the build. A client of another build stops the daemon
	// and starts its own, when no step runs and no other client uses it.
	Version string
	// DrainTimeout is how long a daemon that stops lets the steps under way
	// finish (DefaultDrain when zero). A step still running then is cut, as
	// in a crash: a real one waits in the next daemon for review.
	DrainTimeout time.Duration
	// Cut, when closed, ends the drain at once: the steps still running are
	// cut (a second Ctrl-C).
	Cut <-chan struct{}
}

// Serve runs the daemon until ctx ends, it has been idle for Idle, or a
// client stops it. It returns ErrRunning if another daemon holds the data
// directory.
//
// It stops between steps: no new step starts, and the steps under way
// finish with their outcomes recorded (kairo's Drain), for up to
// DrainTimeout. A step still running then is cut, as in a crash, and a real
// one waits in the next daemon for a person to say whether it took effect
// (invariant 4).
func Serve(ctx context.Context, o Options) error {
	if o.Idle == 0 {
		o.Idle = DefaultIdle
	}
	if o.DrainTimeout == 0 {
		o.DrainTimeout = DefaultDrain
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return err
	}
	lock, err := takeLock(ctx, filepath.Join(o.Dir, lockFile), o.WaitLock)
	if err != nil {
		return err
	}
	// shutdown is the only cleanup, so nothing is removed after the lock is
	// released (a removal then could hit the next daemon's files). It lets
	// go in the order a client sees it: no server.json (a new client starts
	// a daemon, which waits for the lock), no listener, the runtime closed,
	// the socket removed, the lock released.
	var (
		rt             *runtime.Runtime
		ln             net.Listener
		sock, infoPath string
	)
	shutdown := sync.OnceFunc(func() {
		if rt != nil {
			// Draining begins before server.json goes, so a client that
			// finds it gone knows no new step starts here.
			done, cancel := context.WithCancel(context.Background())
			cancel()
			rt.Drain(done)
		}
		if infoPath != "" {
			os.Remove(infoPath)
		}
		if ln != nil {
			ln.Close()
		}
		if rt != nil {
			dctx, cancel := context.WithTimeout(context.Background(), o.DrainTimeout)
			if o.Cut != nil {
				go func() {
					select {
					case <-o.Cut:
						cancel()
					case <-dctx.Done():
					}
				}()
			}
			if err := rt.Drain(dctx); err != nil {
				slog.Warn("daemon: steps still run; they are cut", "err", err)
			}
			cancel()
			if err := rt.Close(); err != nil {
				slog.Warn("daemon: closing the runtime", "err", err)
			}
		}
		if sock != "" {
			os.Remove(sock)
		}
		lock.Close()
	})
	defer shutdown()

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	idle := newIdleTimer(o.Idle, stop)
	defer idle.close()

	if rt, err = o.Open(ctx, runtime.Config{DB: runtime.DBIn(o.Dir), Owner: Owner, Observe: idle.observe}); err != nil {
		return err
	}
	if sock, err = socketPath(o.Dir); err != nil {
		return err
	}
	os.Remove(sock) // left by a daemon that was killed; the lock says it is gone
	if ln, err = net.Listen("unix", sock); err != nil {
		sock = "" // not ours to remove
		return err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		return err
	}
	token := newToken()
	info, _ := json.Marshal(Info{Socket: sock, Token: token, PID: os.Getpid(), Version: o.Version})
	infoPath = filepath.Join(o.Dir, infoFile)
	if err := writeFile0600(infoPath, info); err != nil {
		return err
	}

	api := server.New(rt, token, newToken)
	mux := http.NewServeMux()
	mux.Handle("/", api)
	mux.Handle("GET /daemon/status", api.Authorized(func(w http.ResponseWriter, r *http.Request) {
		steps, clients := idle.counts()
		writeJSON(w, Status{Version: o.Version, PID: os.Getpid(), Steps: steps, Clients: clients})
	}))
	// answered is closed once a stop asked for over the API has been
	// answered: the process must not end before (the client would see the
	// connection drop instead).
	answered := make(chan struct{})
	var stopping atomic.Bool
	mux.Handle("POST /daemon/stop", api.Authorized(func(w http.ResponseWriter, r *http.Request) {
		// ?idle: only if no step runs and no other client is connected (a
		// client of another build replacing the daemon). Checked here, not
		// from an earlier status; a step that starts after the check is
		// drained.
		if r.URL.Query().Has("idle") {
			if steps, clients := idle.counts(); steps > 0 || clients > 1 {
				http.Error(w, "busy", http.StatusConflict)
				return
			}
		}
		if !stopping.CompareAndSwap(false, true) {
			http.Error(w, "stopping", http.StatusConflict)
			return
		}
		defer close(answered)
		shutdown()
		w.WriteHeader(http.StatusNoContent)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		stop()
	}))
	srv := &http.Server{Handler: mux, ConnState: idle.connState}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	idle.check()
	if o.Ready != nil {
		fmt.Fprintln(o.Ready, "ready")
	}
	err = srv.Serve(ln)
	if stopping.Load() {
		select {
		case <-answered:
		case <-time.After(o.DrainTimeout + 10*time.Second): // the drain is in the handler
		}
	}
	if !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

// takeLock takes the lock file at path, waiting up to wait for a daemon on
// its way out to release it. It returns the open file that holds the lock;
// closing it releases the lock. The wait is the kernel's (a blocking flock),
// not a poll.
func takeLock(ctx context.Context, path string, wait time.Duration) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return f, nil
	}
	f.Close()
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, err
	}
	if wait <= 0 {
		return nil, ErrRunning
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	f, err = flockCtx(ctx, path)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, ErrRunning
	}
	return f, err
}

// flockCtx opens path and takes an exclusive lock on it, giving up when ctx
// ends. A lock taken after it gave up is released at once.
func flockCtx(ctx context.Context, path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	got := make(chan error, 1)
	go func() { got <- syscall.Flock(int(f.Fd()), syscall.LOCK_EX) }()
	select {
	case err := <-got:
		if err != nil {
			f.Close()
			return nil, err
		}
		return f, nil
	case <-ctx.Done():
		// The goroutine ends when the lock comes, and lets it go.
		go func() {
			<-got
			f.Close()
		}()
		return nil, ctx.Err()
	}
}

// socketPath is where the daemon of dir listens: in dir, unless that path is
// too long for a Unix socket (about 104 bytes on macOS); then in a directory
// only this user can enter: $XDG_RUNTIME_DIR, or kairocode-<uid> in the
// temporary directory (checked to be this user's own, mode 0700, since on
// Linux that is the shared /tmp).
func socketPath(dir string) (string, error) {
	if p := filepath.Join(dir, socketFile); len(p) < 100 {
		return p, nil
	}
	abs, _ := filepath.Abs(dir)
	h := sha256.Sum256([]byte(abs))
	name := "kairocode-" + hex.EncodeToString(h[:8]) + ".sock"
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, name), nil
	}
	d := filepath.Join(os.TempDir(), fmt.Sprintf("kairocode-%d", os.Getuid()))
	if err := os.Mkdir(d, 0o700); err != nil && !os.IsExist(err) {
		return "", err
	}
	st, err := os.Lstat(d)
	if err != nil {
		return "", err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !st.IsDir() || st.Mode().Perm() != 0o700 || !ok || int(sys.Uid) != os.Getuid() {
		return "", fmt.Errorf("%s is not a directory only this user can enter; remove it or set XDG_RUNTIME_DIR", d)
	}
	return filepath.Join(d, name), nil
}

// Status is what GET /daemon/status says.
type Status struct {
	Version string `json:"version"`
	PID     int    `json:"pid"`
	// Steps is the number of steps running; Clients the number of
	// connections, the asking one included.
	Steps   int `json:"steps"`
	Clients int `json:"clients"`
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeFile0600 writes a file only its owner can read, in place atomically.
func writeFile0600(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil { // the umask may not have kept it
		return err
	}
	return os.Rename(tmp, path)
}

func newToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// idleTimer stops the daemon once no step has run and no client has been
// connected for its duration. It is told of both as they happen (kairo's
// observations, the server's connections): nothing is polled.
type idleTimer struct {
	d    time.Duration
	stop func()

	mu    sync.Mutex
	steps int
	conns int
	t     *time.Timer
	gen   int // which timer is t: one stopped too late still fires
}

func newIdleTimer(d time.Duration, stop func()) *idleTimer {
	return &idleTimer{d: d, stop: stop}
}

func (i *idleTimer) observe(o kairo.Observation) {
	switch o.Kind {
	case kairo.ObsStepStarted:
		i.add(&i.steps, 1)
	case kairo.ObsStepFinished:
		i.add(&i.steps, -1)
	}
}

func (i *idleTimer) connState(_ net.Conn, s http.ConnState) {
	switch s {
	case http.StateNew:
		i.add(&i.conns, 1)
	case http.StateClosed, http.StateHijacked:
		i.add(&i.conns, -1)
	}
}

func (i *idleTimer) counts() (steps, conns int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.steps, i.conns
}

func (i *idleTimer) add(n *int, d int) {
	i.mu.Lock()
	// Never below zero: a finish without a start (kairo's callback of an
	// asynchronous action) must not hide a step that runs.
	*n = max(*n+d, 0)
	i.mu.Unlock()
	i.check()
}

// check starts the timer when the daemon became idle, and stops it when it
// is not.
func (i *idleTimer) check() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.d < 0 {
		return
	}
	busy := i.steps > 0 || i.conns > 0
	switch {
	case busy && i.t != nil:
		i.t.Stop()
		i.t = nil
	case !busy && i.t == nil:
		i.gen++
		gen := i.gen
		i.t = time.AfterFunc(i.d, func() { i.fire(gen) })
	}
}

// fire stops the daemon if timer gen is still the idle timer: one stopped
// too late to keep it from firing is not.
func (i *idleTimer) fire(gen int) {
	i.mu.Lock()
	idle := i.t != nil && i.gen == gen && i.steps == 0 && i.conns == 0
	i.mu.Unlock()
	if idle {
		i.stop()
	}
}

func (i *idleTimer) close() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.t != nil {
		i.t.Stop()
	}
	i.d = -1
}
