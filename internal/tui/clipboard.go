package tui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// The clipboard's image, pasted (ADR 0026, 追記 2026-10-28): the terminal
// sends only text, so the UI reads the clipboard itself, with the
// system's own command, outside KairoCode's sandbox (the clipboard is the
// desktop's). Only these commands, with these arguments, on a key the user
// pressed: nothing from the model reaches them.

var (
	errNoImage = errors.New("no image on the clipboard")
	errNoTool  = errors.New("reading the clipboard's image needs wl-paste (Wayland) or xclip (X11)")
)

// clipLimit is how long reading the clipboard may take.
const clipLimit = 5 * time.Second

// pastedKeep is how long a pasted image's file is kept: long enough to be
// sent, and no longer.
const pastedKeep = 7 * 24 * time.Hour

// clipboardImage reads the clipboard's image, as PNG.
func clipboardImage(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, clipLimit)
	defer cancel()
	var b []byte
	var err error
	switch runtime.GOOS {
	case "darwin":
		b, err = macClipboard(ctx)
	case "linux":
		b, err = linuxClipboard(ctx)
	default:
		return nil, fmt.Errorf("reading the clipboard's image is not done on %s", runtime.GOOS)
	}
	if err != nil {
		return nil, err
	}
	if len(b) == 0 || http.DetectContentType(b) != "image/png" {
		return nil, errNoImage
	}
	return b, nil
}

// macClipboard has osascript write the clipboard's PNG to a file: it says
// raw bytes only as «data …», which a file avoids.
func macClipboard(ctx context.Context) ([]byte, error) {
	f, err := os.CreateTemp("", "kairocode-clip-*.png")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)
	quoted := `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(path) + `"`
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript",
		"-e", "set f to open for access POSIX file "+quoted+" with write permission",
		"-e", "try",
		"-e", "write (the clipboard as «class PNGf») to f",
		"-e", "on error",
		"-e", "close access f",
		"-e", "error number -1",
		"-e", "end try",
		"-e", "close access f")
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("reading the clipboard took longer than %v", clipLimit)
		}
		return nil, errNoImage // not an image: osascript could not make PNG of it
	}
	return os.ReadFile(path)
}

// linuxClipboard reads image/png with wl-paste on Wayland, else xclip.
func linuxClipboard(ctx context.Context) ([]byte, error) {
	var argv []string
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		if p, err := exec.LookPath("wl-paste"); err == nil {
			argv = []string{p, "--no-newline", "--type", "image/png"}
		}
	}
	if argv == nil {
		if p, err := exec.LookPath("xclip"); err == nil {
			argv = []string{p, "-selection", "clipboard", "-t", "image/png", "-o"}
		}
	}
	if argv == nil {
		return nil, errNoTool
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("reading the clipboard took longer than %v", clipLimit)
		}
		return nil, errNoImage // no image/png on the clipboard
	}
	return out.Bytes(), nil
}

// savePasted writes a pasted image to dir/<hash>.png (the same image is one
// file) and removes the ones older than pastedKeep.
func savePasted(dir string, png []byte, now time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if old, err := os.ReadDir(dir); err == nil {
		for _, e := range old {
			if info, err := e.Info(); err == nil && info.Mode().IsRegular() && now.Sub(info.ModTime()) > pastedKeep {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	sum := sha256.Sum256(png)
	path := filepath.Join(dir, hex.EncodeToString(sum[:16])+".png")
	if _, err := os.Stat(path); err == nil {
		os.Chtimes(path, now, now) // pasted again: kept from now
		return path, nil
	}
	tmp, err := os.CreateTemp(dir, ".paste-*")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(png); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return path, nil
}

// pastedMsg is the clipboard's image, saved (path), or why there is none.
// key is the paste key that asked (Ctrl+V), given on to the input when
// there is no image, so that the clipboard's text is pasted then; an empty
// paste has none.
type pastedMsg struct {
	path string
	err  error
	key  *tea.KeyPressMsg
}

// pasteImage reads the clipboard's image, apart from the UI's loop.
func (m *model) pasteImage(key *tea.KeyPressMsg) tea.Cmd {
	read, dir := m.clip, m.pastedDir()
	if read == nil || dir == "" {
		return nil
	}
	ctx := m.ctx
	return func() tea.Msg {
		png, err := read(ctx)
		if err != nil {
			return pastedMsg{err: err, key: key}
		}
		path, err := savePasted(dir, png, time.Now())
		return pastedMsg{path: path, err: err, key: key}
	}
}

// pastedDir is where pasted images are kept: the data directory's pasted/.
func (m *model) pastedDir() string {
	if m.o.Data == "" {
		return ""
	}
	return filepath.Join(m.o.Data, "pasted")
}

// pasted puts the image in the input. With no image, the key that
// asked goes on to the input (it pastes the clipboard's text); a reason
// other than "no image" is said in the status line.
func (m *model) pasted(msg pastedMsg) tea.Cmd {
	if msg.err == nil {
		m.attachImage(msg.path)
		return nil
	}
	if !errors.Is(msg.err, errNoImage) {
		m.note = msg.err.Error()
	}
	if msg.key == nil || m.state != stateReady {
		return nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(*msg.key)
	return cmd
}

// imageLabel matches the label an image stands for in the input.
var imageLabel = regexp.MustCompile(`\[Image #\d+\]`)

// attachImage puts an image (its file) in the input, as a label that
// stands for it there, as Claude Code does: [Image #n]. The label goes
// with the message as written; the image, with it as an image.
func (m *model) attachImage(path string) {
	label := fmt.Sprintf("[Image #%d]", len(m.images)+1)
	m.images[label] = path
	m.input.InsertString(label + " ")
}

// imagesIn are the images whose labels text has, in the order they are
// in it, each once.
func (m *model) imagesIn(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, label := range imageLabel.FindAllString(text, -1) {
		if p, ok := m.images[label]; ok && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}
