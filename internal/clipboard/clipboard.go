// Package clipboard reads the clipboard's image, for a paste (ADR 0026,
// addendum 2026-10-28): the terminal sends only text, so the UI reads the
// clipboard itself, with the system's own command, outside KairoCode's
// sandbox (the clipboard is the desktop's). Only these commands, with
// these arguments, on a key the user pressed: nothing from the model
// reaches them.
package clipboard

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
	"runtime"
	"strings"
	"time"
)

// ErrNoImage: the clipboard holds no image. ErrNoTool: there is no
// command here to read it with.
var (
	ErrNoImage = errors.New("no image on the clipboard")
	ErrNoTool  = errors.New("reading the clipboard's image needs wl-paste (Wayland) or xclip (X11)")
)

// clipLimit is how long reading the clipboard may take.
const clipLimit = 5 * time.Second

// pastedKeep is how long a pasted image's file is kept: long enough to be
// sent, and no longer.
const pastedKeep = 7 * 24 * time.Hour

// Image reads the clipboard's image, as PNG (ErrNoImage: there is none).
func Image(ctx context.Context) ([]byte, error) {
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
		return nil, ErrNoImage
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
		return nil, ErrNoImage // not an image: osascript could not make PNG of it
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
		return nil, ErrNoTool
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("reading the clipboard took longer than %v", clipLimit)
		}
		return nil, ErrNoImage // no image/png on the clipboard
	}
	return out.Bytes(), nil
}

// SavePasted writes a pasted image to dir/<hash>.png (the same image is one
// file) and removes the ones older than pastedKeep.
func SavePasted(dir string, png []byte, now time.Time) (string, error) {
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
