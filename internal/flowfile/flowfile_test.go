package flowfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// Flows are listed by name, read and written within the project; a link
// out of it is not followed.
func TestFlowFiles(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{dir, filepath.Join(dir, "gone")} {
		if got, err := List(d, false); err != nil || got != nil {
			t.Fatalf("no folder in %s: %v %v", d, got, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, ".kairocode", "workflows", "drafts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Write(dir, "b", []byte("b = 1\n")); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, Path("a", false)), []byte("a = 1\n"), 0o644)
	os.WriteFile(filepath.Join(dir, Path("a", true)), []byte("a = 2\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".kairocode", "workflows", "not a name.star"), nil, 0o644)
	out := t.TempDir()
	os.WriteFile(filepath.Join(out, "x.star"), []byte("secret"), 0o644)
	os.Symlink(filepath.Join(out, "x.star"), filepath.Join(dir, Path("out", false)))

	got, err := List(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range got {
		names = append(names, s.Name+"="+string(s.Src))
	}
	if want := "[a=a = 1\n b=b = 1\n]"; fmt.Sprint(names) != want {
		t.Fatalf("listed %q", names)
	}
	if b, err := Read(dir, "a", true); err != nil || string(b) != "a = 2\n" {
		t.Fatalf("draft %q %v", b, err)
	}
	if _, err := Read(dir, "out", false); err == nil {
		t.Fatal("read through a link out of the project")
	}
	if _, err := Read(dir, "none", false); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, Path("b", false)+".edit")); err == nil {
		t.Fatal("the write left its temporary file")
	}
}
