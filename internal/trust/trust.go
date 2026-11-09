// Package trust keeps the folders the user said they trust (ADR 0010): the
// chat asks before it reads a folder's kairocode.json and .opencode, whose
// rules and instructions the session would follow. The list is the
// client's, in the data directory; the sessions never read it.
package trust

import (
	"encoding/json"
	"errors"
	"github.com/r-hashi01/kairocode/internal/gitdir"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// file is where the list is kept in a data directory.
func file(data string) string { return filepath.Join(data, "trust.json") }

type list struct {
	Trusted []string `json:"trusted"`
}

func read(data string) (list, error) {
	var l list
	b, err := os.ReadFile(file(data))
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return l, errors.New("trust: " + file(data) + ": " + err.Error())
	}
	return l, nil
}

// real is dir as an absolute path with its links resolved: a link into a
// folder is trusted as that folder.
func real(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// Trusted reports whether dir, or a folder above it, is trusted.
func Trusted(data, dir string) (bool, error) {
	d, err := real(dir)
	if err != nil {
		return false, err
	}
	l, err := read(data)
	if err != nil {
		return false, err
	}
	// A worktree of a trusted repository is trusted (ADR 0037).
	in := []string{d}
	if m := gitdir.Main(d); m != d {
		in = append(in, m)
	}
	for _, t := range l.Trusted {
		for _, d := range in {
			if rel, err := filepath.Rel(t, d); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return true, nil
			}
		}
	}
	return false, nil
}

// Trust records that dir is trusted, and with it the folders below it.
func Trust(data, dir string) error {
	d, err := real(dir)
	if err != nil {
		return err
	}
	l, err := read(data)
	if err != nil {
		return err
	}
	if slices.Contains(l.Trusted, d) {
		return nil
	}
	l.Trusted = append(l.Trusted, d)
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(data, 0o700); err != nil {
		return err
	}
	// Written whole, then put in place: a reader sees the old list or the
	// new one, never half of one.
	tmp, err := os.CreateTemp(data, "trust-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file(data))
}
