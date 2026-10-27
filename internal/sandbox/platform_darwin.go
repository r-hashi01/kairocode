package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

var (
	availMu   sync.Mutex
	availPath string // the sandbox-exec availErr is about
	availErr  error
)

// Available says whether commands can be confined here: sandbox-exec is
// there and can start one. It cannot inside another sandbox (an agent's,
// or KairoCode's own: ADR 0031); then there is none, as where it is missing.
func Available() error {
	availMu.Lock()
	defer availMu.Unlock()
	if availPath == seatbeltPath {
		return availErr
	}
	availPath, availErr = seatbeltPath, nil
	if _, err := os.Stat(seatbeltPath); err != nil {
		availErr = ErrUnavailable
		return availErr
	}
	probe := exec.Command(seatbeltPath, "-p", "(version 1)(allow default)", "/usr/bin/true")
	if out, err := probe.CombinedOutput(); err != nil {
		availErr = fmt.Errorf("%w: sandbox-exec cannot start one here (in another sandbox?): %v: %s", ErrUnavailable, err, strings.TrimSpace(string(out)))
	}
	return availErr
}

func platformCommand(ctx context.Context, p Policy, root string, argv []string) (*exec.Cmd, error) {
	if err := Available(); err != nil {
		return nil, err
	}
	policy, params := seatbeltPolicy(p, root)
	args := append([]string{"-p", policy}, params...)
	args = append(append(args, "--"), argv...)
	return exec.CommandContext(ctx, seatbeltPath, args...), nil
}
