package tui

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// pngBytes is the start of a PNG: enough for its kind to be seen.
var pngBytes = append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), bytes.Repeat([]byte{1}, 32)...)

func ctrlV() tea.KeyPressMsg { return tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl} }

// The clipboard's image, pasted with Ctrl+V or an empty paste (Cmd+V with
// only an image on it), is saved in the data directory and named in the
// input as @path; the same image twice is one file (ADR 0026).
func TestPasteImage(t *testing.T) {
	data := t.TempDir()
	d, _ := start(t, Options{Data: data})
	d.m.clip = func(context.Context) ([]byte, error) { return pngBytes, nil }
	d.send(ctrlV())
	v := d.m.input.Value()
	if !strings.HasPrefix(v, "@"+filepath.Join(data, "pasted")+string(filepath.Separator)) || !strings.HasSuffix(v, ".png ") {
		t.Fatalf("the input after Ctrl+V: %q", v)
	}
	path := strings.TrimSuffix(strings.TrimPrefix(v, "@"), " ")
	if b, err := os.ReadFile(path); err != nil || !bytes.Equal(b, pngBytes) {
		t.Fatalf("the saved image: %v", err)
	}
	if !IsImage(path) {
		t.Fatalf("%s is not taken as an image", path)
	}
	d.send(tea.PasteMsg{Content: ""})
	if want := v + "@" + path + " "; d.m.input.Value() != want {
		t.Fatalf("the input after an empty paste: %q, want %q", d.m.input.Value(), want)
	}
	if es, _ := os.ReadDir(filepath.Join(data, "pasted")); len(es) != 1 {
		t.Fatalf("%d files for one image", len(es))
	}
}

// With no image, Ctrl+V goes on to the input (it pastes the clipboard's
// text) and says nothing; an empty paste does nothing. A reason other than
// "no image" is said.
func TestPasteNoImage(t *testing.T) {
	d, _ := start(t, Options{Data: t.TempDir()})
	d.m.clip = func(context.Context) ([]byte, error) { return nil, errNoImage }
	cmd := d.m.pasteImage(new(ctrlV()))
	msg, ok := cmd().(pastedMsg)
	if !ok || !errors.Is(msg.err, errNoImage) || msg.key == nil {
		t.Fatalf("got %+v", msg)
	}
	// What the input does with the key (reading the system's clipboard)
	// is not run here: only that it was given the key.
	if d.m.pasted(msg); d.m.note != "" || strings.Contains(d.m.input.Value(), "@") {
		t.Fatalf("note %q, input %q", d.m.note, d.m.input.Value())
	}
	d.send(tea.PasteMsg{Content: ""})
	if d.m.note != "" || d.m.input.Value() != "" {
		t.Fatalf("an empty paste: note %q, input %q", d.m.note, d.m.input.Value())
	}

	d.m.clip = func(context.Context) ([]byte, error) { return nil, errNoTool }
	d.send(tea.PasteMsg{Content: ""})
	if !strings.Contains(d.m.note, "xclip") || d.m.input.Value() != "" {
		t.Fatalf("no tool: note %q, input %q", d.m.note, d.m.input.Value())
	}
}

// With no data directory, Ctrl+V is the input's own.
func TestPasteWithNoDataDir(t *testing.T) {
	d, _ := start(t, Options{})
	d.m.clip = func(context.Context) ([]byte, error) {
		t.Fatal("the clipboard was read")
		return nil, nil
	}
	if cmd := d.m.key(ctrlV()); cmd != nil {
		if _, ok := cmd().(pastedMsg); ok {
			t.Fatal("an image was looked for")
		}
	}
}

// A pasted image is one file by its content, and those older than a week
// are removed when another is pasted.
func TestSavePasted(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	p1, err := savePasted(dir, pngBytes, now)
	if err != nil {
		t.Fatal(err)
	}
	if p2, _ := savePasted(dir, pngBytes, now); p2 != p1 {
		t.Fatalf("the same image twice: %s, %s", p1, p2)
	}
	old := filepath.Join(dir, "old.png")
	os.WriteFile(old, pngBytes, 0o600)
	os.Chtimes(old, now.Add(-8*24*time.Hour), now.Add(-8*24*time.Hour))
	if _, err := savePasted(dir, append(pngBytes, 2), now); err != nil {
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
