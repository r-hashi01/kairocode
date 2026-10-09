package runtime

import "syscall"

// claudeProcAttr starts claude in a process group of its own, and has the
// kernel stop it if this process dies.
func claudeProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
