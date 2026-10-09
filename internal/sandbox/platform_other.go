//go:build !darwin && !linux

package sandbox

import (
	"context"
	"os/exec"
)

func Available() error { return ErrUnavailable }

func platformCommand(context.Context, Policy, string, []string) (*exec.Cmd, error) {
	return nil, ErrUnavailable
}
