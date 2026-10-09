package sandbox

import (
	"context"
	"os/exec"
)

// Linux comes next (bubblewrap); until then there is no sandbox here.
func Available() error { return ErrUnavailable }

func platformCommand(context.Context, Policy, string, []string) (*exec.Cmd, error) {
	return nil, ErrUnavailable
}
