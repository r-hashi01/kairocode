package tools

import (
	"os/exec"
	"syscall"
)

// procAttr starts a command in a process group of its own, so stopping it
// stops what it started too.
func procAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	return cmd.Process.Kill()
}
