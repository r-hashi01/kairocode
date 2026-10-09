//go:build !linux

package runtime

import "syscall"

// claudeProcAttr starts claude in a process group of its own. Elsewhere than
// Linux a child outlives a parent that dies; the next run of the turn stops
// it (stopStaleClaude).
func claudeProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
