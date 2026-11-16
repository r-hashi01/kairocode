package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// spawn starts the daemon and returns once it is ready or has ended. It is
// KairoCode's own process, not a command a tool runs, so it starts outside
// the sandbox (ADR 0008; the invariants allow this file). If ctx ends before
// the daemon is ready, the daemon is killed.
func spawn(ctx context.Context, dir string, start Start) error {
	rotateLog(filepath.Join(dir, logFile))
	logf, err := os.OpenFile(filepath.Join(dir, logFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(start.Argv[0], start.Argv[1:]...)
	cmd.Env = start.Env
	cmd.Stdin = nil
	cmd.Stderr = logf
	// Its own session: the daemon outlives the client, and the client's
	// terminal signals (Ctrl-C) do not reach it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reaps the daemon when it exits, so a client that lives long (ACP, the
	// TUI) keeps no zombie. It ends with the daemon, or with this process.
	go cmd.Wait()
	err = waitReady(ctx, stdout, dir)
	if ctx.Err() != nil {
		cmd.Process.Kill()
	}
	stdout.Close() // the daemon writes nothing more there
	return err
}

// logKeep is how large the daemon's log may grow before it is rotated
// (ADR 0045): one old log is kept beside it.
const logKeep = 10 << 20

// rotateLog moves the log aside as a daemon starts, once it is logKeep or
// more, over the one moved aside before.
func rotateLog(path string) {
	if info, err := os.Stat(path); err == nil && info.Size() >= logKeep {
		os.Rename(path, path+".1")
	}
}
