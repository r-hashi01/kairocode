package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/r-hashi01/kairocode/internal/daemon"
	"github.com/r-hashi01/kairocode/internal/runtime"
)

// kairocode jobs run <folder> (ADR 0028): the runner of a command in the
// background, started by the daemon. It runs the command, writes how it
// ended, and tells the daemon, which may have stopped or been replaced
// meanwhile: one is started if none runs. The daemon of another build is
// not replaced for it.
func jobsCommand(ctx context.Context, args []string, stderr io.Writer) error {
	if len(args) != 2 || args[0] != "run" {
		return errors.New("usage: kairocode jobs run <folder> (the daemon starts it)")
	}
	folder := args[1]
	if err := runtime.RunJob(folder); err != nil {
		return err
	}
	data := os.Getenv("KAIROCODE_JOB_DATA")
	if data == "" {
		return nil
	}
	argv, err := daemonArgv(data)
	if err != nil {
		return err
	}
	// Told even if the command was stopped (SIGTERM ends ctx), and again
	// while the daemon is closing: the next is started to hear it.
	tell, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for {
		err = tellDone(tell, data, argv, filepath.Base(folder))
		if err == nil || tell.Err() != nil {
			return err
		}
		select {
		case <-time.After(2 * time.Second):
		case <-tell.Done():
			return err
		}
	}
}

func tellDone(ctx context.Context, data string, argv []string, id string) error {
	c, err := daemon.Connect(ctx, data, daemon.Start{Argv: argv, Env: os.Environ()})
	if err != nil {
		return err
	}
	defer c.Close()
	return c.JobDone(ctx, id)
}
