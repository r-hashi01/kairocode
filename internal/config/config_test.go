package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The user's file comes before the project's; one not there is left out,
// one not readable is told while the others are still given.
func TestLayers(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	if ls, err := Layers(dir); err != nil || len(ls) != 0 {
		t.Fatalf("none: %v %v", ls, err)
	}
	os.WriteFile(ProjectFile(dir), []byte(`{"a": 2}`), 0o644)
	ls, err := Layers(dir)
	if err != nil || len(ls) != 1 || !ls[0].Project {
		t.Fatalf("the project's alone: %+v %v", ls, err)
	}
	os.MkdirAll(UserDir(), 0o700)
	os.WriteFile(UserFile(), []byte(`{"a": 1}`), 0o600)
	ls, _ = Layers(dir)
	var a []int
	for _, l := range ls {
		var v struct{ A int }
		if err := l.Decode(&v); err != nil {
			t.Fatal(err)
		}
		a = append(a, v.A)
	}
	if len(a) != 2 || a[0] != 1 || a[1] != 2 || ls[0].Project {
		t.Fatalf("order %v", a)
	}
	if u, ok, err := User(); !ok || err != nil || u.Path != filepath.Join(home, "kairocode", File) {
		t.Fatalf("user %+v %v %v", u, ok, err)
	}
	os.Remove(ProjectFile(dir))
	os.Mkdir(ProjectFile(dir), 0o755) // there, not readable as a file
	if ls, err := Layers(dir); err == nil || len(ls) != 1 {
		t.Fatalf("an unreadable one: %+v %v", ls, err)
	}
}
