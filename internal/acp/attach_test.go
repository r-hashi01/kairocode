package acp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A file a prompt points to is put in with its text, a folder with its
// entries; one outside the session's folder (or reached through a link
// out of it) is not read, nor a binary file whole (ADR 0002, @ mentions).
func TestAttachment(t *testing.T) {
	dir, out := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o600)
	os.MkdirAll(filepath.Join(dir, "pkg", "sub"), 0o700)
	os.WriteFile(filepath.Join(dir, "pkg", "b.go"), nil, 0o600)
	os.WriteFile(filepath.Join(dir, "bin"), []byte{0x7f, 'E', 0, 1}, 0o600)
	os.WriteFile(filepath.Join(dir, "big.txt"), []byte(strings.Repeat("x", attachMax+10)), 0o600)
	os.WriteFile(filepath.Join(out, "secret"), []byte("kc-test-outside"), 0o600)
	os.Symlink(filepath.Join(out, "secret"), filepath.Join(dir, "link"))

	for _, c := range []struct {
		uri, want, not string
	}{
		{"file://" + filepath.Join(dir, "a.go"), "<file path=\"a.go\">\npackage a\n</file>", ""},
		{"a.go", "package a", ""},
		{"file://" + filepath.Join(dir, "pkg"), "<folder path=\"pkg\">\nb.go\nsub/\n</folder>", ""},
		{"file://" + filepath.Join(dir, "bin"), "a binary file", "\x00"},
		{"file://" + filepath.Join(dir, "big.txt"), "cut at", ""},
		{"file://" + filepath.Join(out, "secret"), "outside the session's folder", "kc-test-outside"},
		{"../" + filepath.Base(out) + "/secret", "outside the session's folder", "kc-test-outside"},
		{"file://" + filepath.Join(dir, "link"), "outside the session's folder", "kc-test-outside"},
		{"file://" + filepath.Join(dir, "none"), "does not exist", ""},
	} {
		got := attachment(dir, c.uri)
		if !strings.Contains(got, c.want) || c.not != "" && strings.Contains(got, c.not) {
			t.Errorf("%s:\n%s", c.uri, got)
		}
	}
	if got := attachment(dir, "https://example.com/x"); got != "" {
		t.Errorf("a web link: %q", got)
	}
}
