//go:build !linux || !(amd64 || arm64)

package sandbox

// rawUnixSocket and socketPair are about the Linux filter.
func rawUnixSocket() string { return "-" }
func socketPair() string    { return "-" }
