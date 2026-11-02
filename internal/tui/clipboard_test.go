package tui

import (
	"bytes"
	"context"
	"errors"
	"github.com/r-hashi01/kairocode/internal/clipboard"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	d.m.clip = func(context.Context) ([]byte, error) { return nil, clipboard.ErrNoImage }
	cmd := d.m.pasteImage(new(ctrlV()))
	msg, ok := cmd().(pastedMsg)
	if !ok || !errors.Is(msg.err, clipboard.ErrNoImage) || msg.key == nil {
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

	d.m.clip = func(context.Context) ([]byte, error) { return nil, clipboard.ErrNoTool }
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

// A large paste stands in the input as a label that says how it begins,
// and goes with the message in its place; the conversation shows the
// label, not the text. A small one is pasted.
func TestLargePaste(t *testing.T) {
	d, a := start(t, Options{})
	big := strings.Repeat("log line\n", 41) + "end"
	d.send(tea.PasteMsg{Content: "small"})
	d.send(tea.PasteMsg{Content: big})
	label := `[Pasted text #1 +42 lines · "log line"]`
	if v := d.m.input.Value(); v != "small"+label+" " {
		t.Fatalf("the input: %q", v)
	}
	if !strings.Contains(d.view(), "^O open paste") {
		t.Fatalf("Ctrl+O is not said:\n%s", d.view())
	}
	d.m.input.InsertString("why?")
	d.send(key("enter"))
	if len(a.prompts) != 1 || a.prompts[0] != "small"+big+" why?" {
		t.Fatalf("sent %q", a.prompts)
	}
	if out := d.out(); !strings.Contains(out, label) || strings.Count(out, "log line") != 1 {
		t.Fatalf("the conversation shows\n%s", out)
	}
}

// Ctrl+O opens a paste in the input, to change it there; what is sent is
// what the input then holds. The label's beginning is cut, without the
// quotes and brackets that would end it.
func TestOpenPaste(t *testing.T) {
	d, a := start(t, Options{})
	big := "\n  \"quoted\" [bracket] and a first line far longer than a label holds\n" + strings.Repeat("x\n", 20)
	d.send(tea.PasteMsg{Content: big})
	label := `[Pasted text #1 +23 lines · "quoted [bracket and a fi…"]`
	if v := d.m.input.Value(); v != label+" " {
		t.Fatalf("the input: %q", v)
	}
	d.send(key("ctrl+o"))
	if v := d.m.input.Value(); v != big+" " {
		t.Fatalf("opened: %q", v)
	}
	if strings.Contains(d.view(), "^O open paste") {
		t.Fatal("Ctrl+O is still said")
	}
	d.m.input.SetValue(strings.Replace(d.m.input.Value(), "quoted", "changed", 1))
	d.send(key("enter"))
	if len(a.prompts) != 1 || !strings.Contains(a.prompts[0], "changed") {
		t.Fatalf("sent %q", a.prompts)
	}
}
