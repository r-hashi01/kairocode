package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// The log is moved aside once it is logKeep or more, over the one moved
// aside before (ADR 0045).
func TestRotateLog(t *testing.T) {
	log := filepath.Join(t.TempDir(), logFile)
	if err := os.WriteFile(log, []byte("small"), 0o600); err != nil {
		t.Fatal(err)
	}
	rotateLog(log)
	if _, err := os.Stat(log + ".1"); err == nil {
		t.Fatal("a small log was rotated")
	}
	if err := os.WriteFile(log, make([]byte, logKeep), 0o600); err != nil {
		t.Fatal(err)
	}
	rotateLog(log)
	if info, err := os.Stat(log + ".1"); err != nil || info.Size() != logKeep {
		t.Fatalf("not rotated: %v", err)
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("the log is still there")
	}
}
