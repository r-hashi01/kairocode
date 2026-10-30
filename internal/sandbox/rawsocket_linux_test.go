//go:build linux && (amd64 || arm64)

package sandbox

import "syscall"

// rawUnixSocket makes a Unix socket by the call itself, with bits above the
// int family set: the kernel reads AF_UNIX.
func rawUnixSocket() string {
	fd, _, errno := syscall.RawSyscall(syscall.SYS_SOCKET, 1<<32|syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if errno != 0 {
		return "no"
	}
	syscall.Close(int(fd))
	return "yes"
}

// socketPair says whether a pair of connected Unix sockets can be made.
func socketPair() string {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return "no"
	}
	syscall.Close(fds[0])
	syscall.Close(fds[1])
	return "yes"
}
