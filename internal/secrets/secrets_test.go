package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

// The file store (KAIROCODE_SECRETS=file): a value set is got back, a
// value deleted is gone, and the file only the user can read.
func TestFileStore(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("KAIROCODE_SECRETS", "file")
	s := Open()
	if _, ok, err := s.Get("notes/token"); ok || err != nil {
		t.Fatalf("none yet: %v %v", ok, err)
	}
	if err := s.Set("notes/token", "kc-test-value"); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := Open().Get("notes/token"); !ok || err != nil || v != "kc-test-value" {
		t.Fatalf("got %q %v %v", v, ok, err)
	}
	fi, err := os.Stat(filepath.Join(cfg, "kairocode", "credentials.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the file: %v %v", fi, err)
	}
	if err := s.Delete("notes/token"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get("notes/token"); ok {
		t.Fatal("deleted, and still there")
	}
}
