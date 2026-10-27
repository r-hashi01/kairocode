// Package sandboxtest skips a test that starts commands in KairoCode's
// sandbox where there is none, and notes so: inside another sandbox (an
// agent's, or KairoCode's own) one cannot be started (ADR 0031), and
// scripts/check.sh says how many tests did not run (KAIROCODE_SKIPPED).
package sandboxtest

import (
	"fmt"
	"os"
	"testing"

	"github.com/r-hashi01/kairocode/internal/sandbox"
)

// Require skips t where commands cannot be confined.
func Require(t testing.TB) {
	t.Helper()
	err := sandbox.Available()
	if err == nil {
		return
	}
	if path := os.Getenv("KAIROCODE_SKIPPED"); path != "" {
		if f, ferr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); ferr == nil {
			fmt.Fprintln(f, t.Name())
			f.Close()
		}
	}
	t.Skip("no sandbox:", err)
}
