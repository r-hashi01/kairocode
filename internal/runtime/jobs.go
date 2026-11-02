package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/r-hashi01/kairocode/internal/api"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/sandbox"
	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// Commands in the background (ADR 0028). Each runs under a runner of its
// own (kairocode jobs run <folder>), in a process group of its own: the
// daemon may stop or be replaced meanwhile, and the command goes on. Its
// folder, <data>/jobs/<id>, keeps what it is (spec.json), what it wrote
// (out), how it ended (status), the runner (pid), the runs waiting for it
// (waiters), and whether its end was given to the model (reported) and
// told (notified). Its end is told by the runtime that started it
// (waiting on the runner), or by the runner itself through the daemon.

const (
	jobOutMax       = 30000 // what one read of its output gives at most
	jobsPerSession  = 8
	jobMCPWait      = 10 * time.Minute // a wait over MCP: its client waits for the answer
	jobRunnerEnv    = "KAIROCODE_JOB_DATA"
	jobsDirName     = "jobs"
	jobStatusLost   = "lost"
	jobStatusStop   = "stopped"
	jobStatusTimed  = "timed out"
	jobFileSpec     = "spec.json"
	jobFileOut      = "out"
	jobFileStatus   = "status"
	jobFilePID      = "pid"
	jobFileWaiters  = "waiters"
	jobFileReported = "reported"
	jobFileNotified = "notified"
	jobFileStop     = "stop"           // bash_kill asked for it: a runner gone with no status was stopped
	jobFileLock     = "lock"           // held by the runner while it lives (a pid may be another's after a reboot)
	jobFileTold     = "told"           // the session was sent the message of its end
	jobStarting     = 30 * time.Second // a runner not holding its lock yet may be starting, this long
)

// jobSpec is a command in the background, as its folder keeps it.
type jobSpec struct {
	ID      string `json:"id"`
	Session string `json:"session"`
	Dir     string `json:"dir"` // the working directory
	Command string `json:"command"`
	Cwd     string `json:"cwd,omitempty"`
	Escape  bool   `json:"escape,omitempty"`
	// Loopback: with this machine's network, in the sandbox (ADR 0031).
	Loopback bool          `json:"loopback,omitempty"`
	Timeout  time.Duration `json:"timeout"`
	Started  time.Time     `json:"started"`
}

// jobManager keeps the commands in the background of this runtime's data
// directory.
type jobManager struct {
	r   *Runtime
	dir string
	mu  sync.Mutex
	// telling: commands whose end is being told here now.
	telling map[string]bool
}

// jobWaiter is a wait for a command's end: the run that waits, and the
// signal it waits on (one per call: two waits for one command in a run
// are told apart).
type jobWaiter struct {
	Run    string `json:"run"`
	Signal string `json:"signal"`
}

func newJobs(r *Runtime) *jobManager {
	return &jobManager{r: r, dir: filepath.Join(filepath.Dir(r.cfg.DB), jobsDirName), telling: map[string]bool{}}
}

// jobID is the ID of the command a tool call starts: the same for the
// same call, read back from the record (its idempotency key).
func jobID(key string) string {
	h := sha256.Sum256([]byte(key))
	return "bg_" + hex.EncodeToString(h[:])[:12]
}

func (m *jobManager) path(id string, file ...string) string {
	return filepath.Join(append([]string{m.dir, id}, file...)...)
}

func fileThere(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// spec reads a job's spec: only a job of session is there for it.
func (m *jobManager) spec(session, id string) (jobSpec, error) {
	var s jobSpec
	if !strings.HasPrefix(id, "bg_") || strings.ContainsAny(id, `/\.`) {
		return s, fmt.Errorf("no command %q in the background", id)
	}
	b, err := os.ReadFile(m.path(id, jobFileSpec))
	if err == nil {
		err = json.Unmarshal(b, &s)
	}
	if err != nil || s.Session != session {
		return jobSpec{}, fmt.Errorf("no command %q in the background of this session", id)
	}
	return s, nil
}

// running counts session's commands not ended.
func (m *jobManager) running(session string) int {
	return m.runningBySession()[session]
}

// runningBySession counts the commands not ended, by session.
func (m *jobManager) runningBySession() map[string]int {
	entries, _ := os.ReadDir(m.dir)
	out := map[string]int{}
	for _, e := range entries {
		var s jobSpec
		b, err := os.ReadFile(m.path(e.Name(), jobFileSpec))
		if err != nil || json.Unmarshal(b, &s) != nil {
			continue
		}
		if !fileThere(m.path(e.Name(), jobFileStatus)) {
			out[s.Session]++
		}
	}
	return out
}

// runnerArgv is how the runner is started: KairoCode's own binary, unless
// the runtime was given another (tests).
func (m *jobManager) runnerArgv() ([]string, error) {
	if len(m.r.cfg.JobRunner) > 0 {
		return m.r.cfg.JobRunner, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return []string{exe, "jobs", "run"}, nil
}

// start starts a command in the background for session; one whose
// folder is there already (the same call, run again) is not started twice.
func (m *jobManager) start(session, id, dir string, js tools.JobSpec) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if fileThere(m.path(id, jobFileSpec)) {
		return nil
	}
	if n := m.running(session); n >= jobsPerSession {
		return fmt.Errorf("%d commands of this session run in the background already, the most there may be: wait for one, or stop one with bash_kill", n)
	}
	argv, err := m.runnerArgv()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.path(id), 0o700); err != nil {
		return err
	}
	s := jobSpec{ID: id, Session: session, Dir: dir, Command: js.Command, Cwd: js.Cwd, Escape: js.Escape, Loopback: js.Loopback, Timeout: js.Timeout, Started: time.Now()}
	b, _ := json.Marshal(s)
	if err := os.WriteFile(m.path(id, jobFileSpec), b, 0o600); err != nil {
		return err
	}
	cmd := exec.Command(argv[0], append(argv[1:], m.path(id))...)
	cmd.Env = append(os.Environ(), jobRunnerEnv+"="+filepath.Dir(m.dir))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // goes on if this process is stopped
	// The runner writes its pid and holds its lock itself: one this
	// process does not live to see started is seen all the same.
	if err := cmd.Start(); err != nil {
		os.RemoveAll(m.path(id))
		return err
	}
	go func() {
		cmd.Wait()
		m.done(id)
	}()
	return nil
}

// view is a command's state, and what it wrote from since on.
func (m *jobManager) view(session, id string, since int64) (tools.JobView, error) {
	s, err := m.spec(session, id)
	if err != nil {
		return tools.JobView{}, err
	}
	v := tools.JobView{ID: id, Command: s.Command, Running: true}
	if b, err := os.ReadFile(m.path(id, jobFileStatus)); err == nil {
		v.Running, v.Status = false, strings.TrimSpace(string(b))
	}
	f, err := os.Open(m.path(id, jobFileOut))
	if err != nil {
		return v, nil // nothing written yet
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return v, err
	}
	v.Size = fi.Size()
	since = min(max(since, 0), v.Size)
	if v.Size-since > jobOutMax {
		since = v.Size - jobOutMax // its end: the newest
	}
	v.Since = since
	b := make([]byte, v.Size-since)
	n, _ := f.ReadAt(b, since)
	v.Output = string(b[:n])
	return v, nil
}

// report notes that a command's end was given to the model: it is not
// told to the session again.
func (m *jobManager) report(id string, v tools.JobView) {
	if !v.Running {
		os.WriteFile(m.path(id, jobFileReported), nil, 0o600)
	}
}

// alive says a command's runner still runs: it holds its lock (a pid
// alone may be another process's, after a reboot), or, not holding it
// yet, has only just been started.
func (m *jobManager) alive(id string) bool {
	f, err := os.OpenFile(m.path(id, jobFileLock), os.O_RDWR, 0o600)
	if err != nil {
		var s jobSpec
		b, _ := os.ReadFile(m.path(id, jobFileSpec))
		return json.Unmarshal(b, &s) == nil && time.Since(s.Started) < jobStarting
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true // held: the runner
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if !fileThere(m.path(id, jobFilePID)) {
		// Its lock file made, not yet held: a runner starting.
		var s jobSpec
		b, _ := os.ReadFile(m.path(id, jobFileSpec))
		return json.Unmarshal(b, &s) == nil && time.Since(s.Started) < jobStarting
	}
	return false
}

// kill stops a command: its runner stops what it runs, and says so.
func (m *jobManager) kill(session, id string) error {
	if _, err := m.spec(session, id); err != nil {
		return err
	}
	if fileThere(m.path(id, jobFileStatus)) || !m.alive(id) {
		return nil // ended already, or its runner is gone: done says how
	}
	b, err := os.ReadFile(m.path(id, jobFilePID))
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return fmt.Errorf("%s's runner is starting: stop it again in a moment", id)
	}
	// Noted first: a runner stopped before it could say so (just started,
	// say) was stopped all the same.
	if err := os.WriteFile(m.path(id, jobFileStop), nil, 0o600); err != nil {
		return err
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// readWaiters reads a command's waits (m.mu held).
func (m *jobManager) readWaiters(id string) []jobWaiter {
	var ws []jobWaiter
	if b, err := os.ReadFile(m.path(id, jobFileWaiters)); err == nil {
		json.Unmarshal(b, &ws)
	}
	return ws
}

func (m *jobManager) writeWaiters(id string, ws []jobWaiter) error {
	b, _ := json.Marshal(ws)
	tmp := m.path(id, jobFileWaiters+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path(id, jobFileWaiters))
}

// watch has run wait for a command's end, on a signal of its call's own:
// ended, it says so; else the wait is registered on disk (a daemon
// started after this one tells it too).
func (m *jobManager) watch(session, id, run, call string) (signal string, ended bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.spec(session, id); err != nil {
		return "", false, err
	}
	if fileThere(m.path(id, jobFileStatus)) {
		return "", true, nil
	}
	w := jobWaiter{Run: run, Signal: workflows.SignalJobPrefix + id + "/" + call}
	ws := m.readWaiters(id)
	if !slices.Contains(ws, w) {
		if err := m.writeWaiters(id, append(ws, w)); err != nil {
			return "", false, err
		}
	}
	return w.Signal, false, nil
}

// unwatch drops a wait that ran out: the command is not waited for by it
// any more (its end, then, is told to the session).
func (m *jobManager) unwatch(id, run, signal string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws := m.readWaiters(id)
	if i := slices.Index(ws, jobWaiter{Run: run, Signal: signal}); i >= 0 {
		m.writeWaiters(id, slices.Delete(ws, i, i+1))
	}
}

// done tells of a command's end, once: the waits for it are signalled;
// with none, and its end not given to the model, the session is sent a
// message. It is noted told only once all is told: a process stopped
// before tells it again from the next (a second signal does no harm).
// A runner gone with no status ended in a way not known.
func (m *jobManager) done(id string) {
	m.mu.Lock()
	if m.r.closing() || fileThere(m.path(id, jobFileNotified)) || m.telling[id] {
		m.mu.Unlock()
		return // a runtime that closes leaves it to the next (or the runner)
	}
	var s jobSpec
	b, err := os.ReadFile(m.path(id, jobFileSpec))
	if err != nil || json.Unmarshal(b, &s) != nil {
		m.mu.Unlock()
		return
	}
	if !fileThere(m.path(id, jobFileStatus)) {
		if m.alive(id) {
			m.mu.Unlock()
			return // not ended: its runner tells when it is
		}
		status := jobStatusLost
		if fileThere(m.path(id, jobFileStop)) {
			status = jobStatusStop
		}
		// Only where there is none: the runner may have written its own
		// just as it went.
		if f, err := os.OpenFile(m.path(id, jobFileStatus), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
			f.WriteString(status)
			f.Close()
		}
	}
	waiters := m.readWaiters(id)
	reported, told := fileThere(m.path(id, jobFileReported)), fileThere(m.path(id, jobFileTold))
	m.telling[id] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.telling, id)
		m.mu.Unlock()
	}()
	ctx := context.Background()
	heard := 0
	for _, w := range waiters {
		if err := m.r.K.Signal(ctx, w.Run, w.Signal, workflows.JobEnded{ID: id}); err != nil {
			m.r.logf("telling %s that %s ended: %v", w.Run, id, err) // a run that ended has no wait for it
			continue
		}
		heard++
	}
	if heard == 0 && !reported && !told { // no wait heard it: the session is told
		status, _ := os.ReadFile(m.path(id, jobFileStatus))
		text := fmt.Sprintf("[KairoCode] The command in the background %s ended (%s): %s\nbash_output {\"id\": %q} reads what it wrote.",
			id, strings.TrimSpace(string(status)), s.Command, id)
		if _, err := m.r.Send(ctx, s.Session, text); err != nil {
			m.r.logf("telling session %s that %s ended: %v", s.Session, id, err)
		} else {
			os.WriteFile(m.path(id, jobFileTold), nil, 0o600)
		}
	}
	if !m.r.closing() { // what a closing runtime sent may not have been kept
		os.WriteFile(m.path(id, jobFileNotified), nil, 0o600)
	}
}

// recover tells of the commands that ended while no runtime was here to:
// one whose runner went with no status (the machine stopped, say) is lost.
// One still running tells of itself through the daemon when it ends.
func (m *jobManager) recover() {
	entries, _ := os.ReadDir(m.dir)
	for _, e := range entries {
		id := e.Name()
		if !e.IsDir() || fileThere(m.path(id, jobFileNotified)) {
			continue
		}
		// On the runtime's background: stopped and waited for as it
		// closes, not left telling a closed runtime.
		if fileThere(m.path(id, jobFileStatus)) || !m.alive(id) {
			m.r.background(func(context.Context) { m.done(id) })
			continue
		}
		m.r.background(func(ctx context.Context) { m.outlive(ctx, id) })
	}
}

// outlive waits for the runner of a command this runtime did not start
// to go (its lock, taken when the process ends however it ends: SIGKILL
// too), then tells of its end. The runner tells of it itself as well;
// this is for one that cannot (killed, or the daemon not reached). The
// wait is the kernel's on the lock, not a poll; it ends with the runner,
// which runs out of time at the latest.
//
// The runtime closing (ctx) ends the wait here: the lock, when it comes,
// is let go of with nothing told (the next runtime finds the command
// ended, and tells of it).
func (m *jobManager) outlive(ctx context.Context, id string) {
	f, err := os.OpenFile(m.path(id, jobFileLock), os.O_RDWR, 0o600)
	if err != nil {
		return
	}
	got := make(chan error, 1)
	go func() { got <- syscall.Flock(int(f.Fd()), syscall.LOCK_EX) }()
	select {
	case err := <-got:
		if err == nil {
			syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		}
		f.Close()
		if err == nil {
			m.done(id)
		}
	case <-ctx.Done():
		go func() { // ends with the runner, which runs out of time at the latest
			<-got
			f.Close()
		}()
	}
}

// JobDone is told by a command's runner that it ended (the daemon's
// /jobs/<id>/done): for a runner the runtime that started it is no
// longer here to wait on.
func (r *Runtime) JobDone(id string) error {
	if !strings.HasPrefix(id, "bg_") || strings.ContainsAny(id, `/\.`) {
		return fmt.Errorf("no command %q", id)
	}
	if r.closing() {
		return ErrClosing
	}
	r.jobs.done(id)
	return nil
}

var ErrClosing = api.ErrClosing

// RunJob is a command's runner (kairocode jobs run <folder>): it runs the
// command in its sandbox, its output to the folder, until it ends, is
// stopped (SIGTERM) or runs out of time, and writes how it ended.
func RunJob(folder string) error {
	// Its lock, held while it lives, and its pid: what tells it is here.
	lock, err := os.OpenFile(filepath.Join(folder, jobFileLock), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	// Waited for: alive() takes it a moment to look (one runner is
	// started per folder).
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(folder, jobFilePID), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(folder, jobFileSpec))
	if err != nil {
		return err
	}
	var s jobSpec
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	out, err := os.OpenFile(filepath.Join(folder, jobFileOut), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	ctx, cancel := context.WithTimeout(context.Background(), s.Timeout)
	defer cancel()
	stopped := make(chan os.Signal, 1)
	signal.Notify(stopped, syscall.SIGTERM, syscall.SIGINT)
	var stop bool
	var mu sync.Mutex
	go func() {
		if _, ok := <-stopped; ok {
			mu.Lock()
			stop = true
			mu.Unlock()
			cancel()
		}
	}()
	mode := sandbox.WorkspaceWrite
	if s.Escape {
		mode = sandbox.FullAccess // approved by the user (escalate), as bash
	}
	p := sandbox.Policy{Mode: mode, Root: s.Dir}
	if s.Loopback && mode == sandbox.WorkspaceWrite {
		// Its own TMPDIR, gone with the command (ADR 0031). The data
		// directory (<data>/jobs/<id>), with the daemon's socket, out of its
		// sight: this runner is not the daemon, which hides it there.
		sandbox.HideFromLoopback(filepath.Dir(filepath.Dir(folder)))
		p.Loopback = true // without its directory, sandbox.Command refuses it
		if dir, done, err := sandbox.SocketDir(); err == nil {
			defer done()
			p.SocketDir = dir
		} else {
			fmt.Fprintf(out, "%v\n", err)
		}
	}
	cmd, err := sandbox.Command(ctx, p, s.Cwd, "/bin/sh", "-c", s.Command)
	status := ""
	if err != nil {
		fmt.Fprintf(out, "not started: %v\n", err)
		status = "exit 127"
	} else {
		cmd.Stdout, cmd.Stderr = out, out
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			if cmd.Process != nil {
				syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			return nil
		}
		err = cmd.Run()
		var exit *exec.ExitError
		mu.Lock()
		switch {
		case stop:
			status = jobStatusStop
		case ctx.Err() == context.DeadlineExceeded:
			status = jobStatusTimed + " after " + s.Timeout.String()
		case errors.As(err, &exit):
			status = fmt.Sprintf("exit %d", exit.ExitCode())
		case err != nil:
			fmt.Fprintf(out, "%v\n", err)
			status = "exit 127"
		default:
			status = "exit 0"
		}
		mu.Unlock()
	}
	signal.Stop(stopped)
	close(stopped)
	tmp := filepath.Join(folder, jobFileStatus+".tmp")
	if err := os.WriteFile(tmp, []byte(status), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(folder, jobFileStatus))
}

// jobEnv is Env.Jobs for a tool call of session, whose call key names the
// command it starts.
type jobEnv struct {
	m       *jobManager
	session string
	dir     string
	key     string
}

func (e jobEnv) Start(spec tools.JobSpec) (string, error) {
	id := jobID(e.key)
	return id, e.m.start(e.session, id, e.dir, spec)
}

func (e jobEnv) View(id string, since int64) (tools.JobView, error) {
	v, err := e.m.view(e.session, id, since)
	if err == nil {
		e.m.report(id, v)
	}
	return v, err
}

func (e jobEnv) Kill(id string) error { return e.m.kill(e.session, id) }

// jobWatch is bash_wait's first step: its input read, and the command
// either ended (the result) or watched for this run.
func (r *Runtime) jobWatch(t *kairo.TaskContext, in workflows.JobWatchInput) (workflows.JobWatchOutput, error) {
	args, err := r.toolArgs(t, in.Call)
	if err != nil {
		return workflows.JobWatchOutput{}, err
	}
	var a tools.JobArgs
	if json.Unmarshal(args, &a) != nil || a.ID == "" {
		out, err := r.toolResult(t, in.Call.Call, tools.Result{Output: "invalid input: want {id}", IsError: true})
		return workflows.JobWatchOutput{Done: true, Result: out}, err
	}
	session := sessionOf(in.Waiter)
	signal, ended, err := r.jobs.watch(session, a.ID, in.Waiter, in.Call.Call)
	if err != nil {
		out, err := r.toolResult(t, in.Call.Call, tools.Result{Output: err.Error(), IsError: true})
		return workflows.JobWatchOutput{Done: true, Result: out}, err
	}
	if ended {
		out, err := r.jobResultOf(t, session, a.ID, in.Call.Call, false)
		return workflows.JobWatchOutput{Done: true, Result: out}, err
	}
	var wait time.Duration // until it ends: its runner tells, no timer is needed
	if a.TimeoutMS > 0 {
		wait = min(time.Duration(a.TimeoutMS)*time.Millisecond, tools.JobMaxTimeout)
	}
	if in.Call.Call == "" && (wait == 0 || wait > jobMCPWait) { // over MCP: its client waits for the answer
		wait = jobMCPWait
	}
	return workflows.JobWatchOutput{ID: a.ID, Signal: signal, WaitMS: wait.Milliseconds()}, nil
}

// jobResult is bash_wait's last step: the command's state, after its end
// or the wait's.
func (r *Runtime) jobResult(t *kairo.TaskContext, in workflows.JobResultInput) (workflows.ToolOutput, error) {
	if in.TimedOut {
		// This wait is over: the command's end is not for it now.
		r.jobs.unwatch(in.ID, in.Waiter, in.Signal)
	}
	return r.jobResultOf(t, sessionOf(in.Waiter), in.ID, in.Call.Call, in.TimedOut)
}

func (r *Runtime) jobResultOf(ctx context.Context, session, id, call string, timedOut bool) (workflows.ToolOutput, error) {
	v, err := r.jobs.view(session, id, 0)
	if err != nil {
		return r.toolResult(ctx, call, tools.Result{Output: err.Error(), IsError: true})
	}
	r.jobs.report(id, v)
	text := v.Text()
	if timedOut && v.Running {
		text = "It has not ended yet (the wait ran out). " + text
	}
	return r.toolResult(ctx, call, tools.Result{Output: text, IsError: !v.Running && v.Status != "exit 0"})
}
