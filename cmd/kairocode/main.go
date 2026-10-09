// Command kairocode is a coding agent on kairo: it survives crashes, waits
// without a process, and runs subagents in parallel without redoing finished
// work.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/r-hashi01/kairocode/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Main(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
