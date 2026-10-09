package sandbox

import "testing"

// withoutSandbox makes the sandbox look missing for the test.
func withoutSandbox(t *testing.T) {
	old := seatbeltPath
	seatbeltPath = "/nonexistent/sandbox-exec"
	t.Cleanup(func() { seatbeltPath = old })
}
