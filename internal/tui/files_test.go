package tui

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// An image is pointed to from anywhere (a screenshot on the desktop), its
// spaces escaped as a terminal pastes a dropped file; other files only in
// the session's folder (ADR 0026).
func TestMentionedImages(t *testing.T) {
	dir, away := t.TempDir(), t.TempDir()
	shot := filepath.Join(away, "Screen Shot.png")
	notes := filepath.Join(away, "notes.txt")
	local := filepath.Join(dir, "a.go")
	for _, f := range []string{shot, notes, local} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := Mentioned(dir, "look at "+mentionOf(shot)+" and @"+notes+" and @a.go")
	if !slices.Equal(got, []string{shot, local}) {
		t.Fatalf("mentioned %q", got)
	}

	for pasted, want := range map[string]string{
		filepath.Join(away, `Screen\ Shot.png`): shot,
		"'" + shot + "'":                        shot,
		" " + `"` + shot + `"` + "\n":           shot,
		notes:                                   "",
		"Screen Shot.png":                       "",
		filepath.Join(away, "gone.png"):         "",
	} {
		if got := imagePath(pasted); got != want {
			t.Errorf("pasted %q: %q, want %q", pasted, got, want)
		}
	}
}
