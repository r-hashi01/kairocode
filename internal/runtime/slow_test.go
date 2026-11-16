package runtime

import (
	"bytes"
	"runtime/pprof"
	"testing"
	"time"
)

// stacksIfSlow logs every goroutine's stack if what takes longer than d,
// to see what it waits for where it cannot be seen otherwise (a CI
// runner); stop, once what ended, does nothing more.
func stacksIfSlow(t *testing.T, d time.Duration, what string) (stop func()) {
	t.Helper()
	timer := time.AfterFunc(d, func() {
		var b bytes.Buffer
		_ = pprof.Lookup("goroutine").WriteTo(&b, 1)
		t.Logf("%s has taken %v; the goroutines:\n%s", what, d, b.String())
	})
	return func() { timer.Stop() }
}
