package trust

import (
	"os"
	"path/filepath"
	"testing"
)

// A trusted folder is trusted with the folders below it, through links, and
// not its neighbours or the folders above; the list is the user's own (0600).
func TestTrust(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	for _, d := range []string{filepath.Join(repo, "pkg"), filepath.Join(root, "repo2")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	if ok, err := Trusted(data, repo); ok || err != nil {
		t.Fatalf("trusted before asking: %v %v", ok, err)
	}
	if err := Trust(data, repo); err != nil {
		t.Fatal(err)
	}
	if err := Trust(data, link); err != nil { // the same folder: once
		t.Fatal(err)
	}
	for dir, want := range map[string]bool{
		repo:                         true,
		filepath.Join(repo, "pkg"):   true,
		link:                         true,
		filepath.Join(root, "repo2"): false, // a name that begins the same
		root:                         false,
	} {
		if ok, err := Trusted(data, dir); ok != want || err != nil {
			t.Errorf("%s: %v %v, want %v", dir, ok, err, want)
		}
	}
	l, err := read(data)
	if err != nil || len(l.Trusted) != 1 {
		t.Fatalf("list %v %v", l, err)
	}
	info, err := os.Stat(file(data))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("trust.json %v %v", info, err)
	}
}

// A list that cannot be read is an error, not "nothing is trusted" quietly
// written over.
func TestTrustUnreadable(t *testing.T) {
	data := t.TempDir()
	if err := os.WriteFile(file(data), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Trusted(data, t.TempDir()); err == nil {
		t.Fatal("read a broken list")
	}
	if err := Trust(data, t.TempDir()); err == nil {
		t.Fatal("wrote over a broken list")
	}
}
