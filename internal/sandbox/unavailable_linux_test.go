package sandbox

import (
	"sync"
	"testing"
)

// withoutSandbox makes the sandbox look missing for the test.
func withoutSandbox(t *testing.T) {
	old := bwrapPath
	bwrapPath = "/nonexistent/bwrap"
	availOnce, availErr = sync.Once{}, nil
	t.Cleanup(func() {
		bwrapPath = old
		availOnce, availErr = sync.Once{}, nil
	})
}
