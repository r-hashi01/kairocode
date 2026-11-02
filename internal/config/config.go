// Package config is where KairoCode's settings are: kairocode.json, the
// user's ($XDG_CONFIG_HOME/kairocode, else ~/.config/kairocode) and a
// project's (at its root), and the user's other files beside it. It finds
// and reads them; what a section means is its reader's.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// File is the settings file's name, in a project and in the user's
// folder.
const File = "kairocode.json"

// Home is the user's folder of settings, of every program:
// $XDG_CONFIG_HOME, else ~/.config; "" if there is no home to find it in.
func Home() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config")
}

// UserDir is KairoCode's folder in it: the user's kairocode.json, plugins,
// marketplaces, credentials.
func UserDir() string { return filepath.Join(Home(), "kairocode") }

// UserFile is the user's kairocode.json.
func UserFile() string { return filepath.Join(UserDir(), File) }

// ProjectFile is the kairocode.json of the project in dir.
func ProjectFile(dir string) string { return filepath.Join(dir, File) }

// Layer is one kairocode.json as read.
type Layer struct {
	Path    string
	Project bool // the project's, not the user's
	Data    []byte
}

// Layers are the user's kairocode.json, then the project's in dir (none
// for dir ""), each if it is there. One there but not readable is left
// out and told in the error, with the others still given: a reader that
// goes on without it may.
func Layers(dir string) ([]Layer, error) {
	paths := []string{UserFile()}
	if dir != "" {
		paths = append(paths, ProjectFile(dir))
	}
	var out []Layer
	var errs []error
	for i, p := range paths {
		b, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, Layer{Path: p, Project: i == 1, Data: b})
	}
	return out, errors.Join(errs...)
}

// Decode reads layer's section into v (a struct of the section's fields,
// tagged as kairocode.json names them), saying which file was wrong.
func (l Layer) Decode(v any) error {
	if err := json.Unmarshal(l.Data, v); err != nil {
		return fmt.Errorf("%s: %w", l.Path, err)
	}
	return nil
}

// User is the user's kairocode.json, if it is there: for the settings a
// project's cannot set.
func User() (Layer, bool, error) {
	ls, err := Layers("")
	if err != nil || len(ls) == 0 {
		return Layer{}, false, err
	}
	return ls[0], true, nil
}
