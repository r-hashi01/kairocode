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
// only an image on it), is saved in the data directory and stands in the
// input as [Image #n], as in Claude Code; the same image twice is one
// file. Sent, the message keeps the labels and has the images with it.
func TestPasteImage(t *testing.T) {
	data := t.TempDir()
	d, a := start(t, Options{Data: data})
	d.m.clip = func(context.Context) ([]byte, error) { return pngBytes, nil }
	d.send(ctrlV())
	if v := d.m.input.Value(); v != "[Image #1] " {
		t.Fatalf("the input after Ctrl+V: %q", v)
	}
	path := d.m.images["[Image #1]"]
	if filepath.Dir(path) != filepath.Join(data, "pasted") || !IsImage(path) {
		t.Fatalf("the image is kept as %q", path)
	}
	if b, err := os.ReadFile(path); err != nil || !bytes.Equal(b, pngBytes) {
		t.Fatalf("the saved image: %v", err)
	}
	d.send(tea.PasteMsg{Content: ""})
	if v := d.m.input.Value(); v != "[Image #1] [Image #2] " {
		t.Fatalf("the input after an empty paste: %q", v)
	}
	if es, _ := os.ReadDir(filepath.Join(data, "pasted")); len(es) != 1 {
		t.Fatalf("%d files for one image", len(es))
	}
	d.m.input.InsertString("what is this?")
	d.send(key("enter"))
	if len(a.prompts) != 1 || a.prompts[0] != "[Image #1] [Image #2] what is this?" {
		t.Fatalf("sent %q", a.prompts)
	}
	if len(a.files) != 1 || a.files[0] != path {
		t.Fatalf("sent with %q, want the image once", a.files)
	}
}

// An image file dropped on the terminal (its path pasted) stands in the
// input as a label too; a label typed by hand, of no image, sends none.
func TestDroppedImage(t *testing.T) {
	d, a := start(t, Options{Data: t.TempDir()})
	img := filepath.Join(t.TempDir(), "shot one.png")
	os.WriteFile(img, pngBytes, 0o600)
	d.send(tea.PasteMsg{Content: "'" + img + "'"})
	if v := d.m.input.Value(); v != "[Image #1] " {
		t.Fatalf("the input after a dropped image: %q", v)
	}
	d.m.input.InsertString("and [Image #7]")
	d.send(key("enter"))
	if len(a.files) != 1 || a.files[0] != img {
		t.Fatalf("sent with %q", a.files)
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

// A large paste stands in the input as a label, and goes with the message
// in its place; the conversation shows the label. A small one is pasted.
func TestLargePaste(t *testing.T) {
	d, a := start(t, Options{})
	big := strings.Repeat("log line\n", 41) + "end"
	d.send(tea.PasteMsg{Content: "small"})
	d.send(tea.PasteMsg{Content: big})
	if v := d.m.input.Value(); v != "small[Pasted text #1 +42 lines] " {
		t.Fatalf("the input: %q", v)
	}
	d.m.input.InsertString("why?")
	d.send(key("enter"))
	if len(a.prompts) != 1 || a.prompts[0] != "small"+big+" why?" {
		t.Fatalf("sent %q", a.prompts)
	}
	if out := d.out(); !strings.Contains(out, "[Pasted text #1 +42 lines]") || strings.Contains(out, "log line") {
		t.Fatalf("the conversation shows\n%s", out)
	}
}
