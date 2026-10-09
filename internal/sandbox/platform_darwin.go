package sandbox

import (
	"context"
	"os"
	"os/exec"
)

// Available says whether commands can be confined here.
func Available() error {
	if _, err := os.Stat(seatbeltPath); err != nil {
		return ErrUnavailable
	}
	return nil
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
