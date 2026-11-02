package clipboard

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// pngBytes is a PNG's first bytes: enough to be one.
var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")

// A pasted image is one file by its content, and those older than a week
// are removed when another is pasted.
func TestSavePasted(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	p1, err := SavePasted(dir, pngBytes, now)
	if err != nil {
		t.Fatal(err)
	}
	if p2, _ := SavePasted(dir, pngBytes, now); p2 != p1 {
		t.Fatalf("the same image twice: %s, %s", p1, p2)
	}
	old := filepath.Join(dir, "old.png")
	os.WriteFile(old, pngBytes, 0o600)
	os.Chtimes(old, now.Add(-8*24*time.Hour), now.Add(-8*24*time.Hour))
	if _, err := SavePasted(dir, append(pngBytes, 2), now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err == nil {
		t.Fatal("an image older than a week was kept")
	}
	if _, err := os.Stat(p1); err != nil {
		t.Fatal("a recent image was removed")
	}
	if es, _ := os.ReadDir(dir); len(es) != 2 {
		t.Fatalf("%d files, want 2", len(es))
	}
}
