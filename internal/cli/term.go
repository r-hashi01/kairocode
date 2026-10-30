//go:build darwin || linux

package cli

import (
	"os"
	"syscall"
	"unsafe"
)

// noEcho stops f, a terminal, from showing what is typed (a credential,
// ADR 0025), and returns how to put it back. On a file that is not a
// terminal it fails, and nothing is changed.
func noEcho(f *os.File) (func(), error) {
	var t syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(ioctlGetTermios), uintptr(unsafe.Pointer(&t))); e != 0 {
		return nil, e
	}
	old := t
	t.Lflag &^= syscall.ECHO
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(ioctlSetTermios), uintptr(unsafe.Pointer(&t))); e != 0 {
		return nil, e
	}
	return func() {
		syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(ioctlSetTermios), uintptr(unsafe.Pointer(&old)))
	}, nil
}
