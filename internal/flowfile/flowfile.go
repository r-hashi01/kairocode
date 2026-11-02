// Package flowfile reads and writes a project's flow scripts (ADR 0003):
// <dir>/.kairocode/workflows/<name>.star, and their drafts (ADR 0030).
// Everything is read within dir (os.Root): a link out of the project is
// not followed.
package flowfile

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/r-hashi01/kairocode/internal/flowchart"
)

// Path is the script of flow name, relative to the project: its draft's
// with draft.
func Path(name string, draft bool) string {
	if draft {
		return path.Join(flowchart.DraftsDir, name+".star")
	}
	return path.Join(flowchart.Dir, name+".star")
}

// Read is the script of flow name in the project dir (its draft's with
// draft). One missing is fs.ErrNotExist.
func Read(dir, name string, draft bool) ([]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.ReadFile(Path(name, draft))
}

// Script is a flow's name and its script.
type Script struct {
	Name string
	Src  []byte
}

// List is the project's flows in dir, by name (its drafts with draft);
// none if there is no folder for them, or no project.
func List(dir string, draft bool) ([]Script, error) {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	sub := flowchart.Dir
	if draft {
		sub = flowchart.DraftsDir
	}
	f, err := root.Open(sub)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return nil, err
	}
	var out []Script
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".star")
		if !ok || e.IsDir() || !flowchart.ValidName(name) {
			continue
		}
		if b, err := root.ReadFile(path.Join(sub, e.Name())); err == nil {
			out = append(out, Script{Name: name, Src: b})
		}
	}
	slices.SortFunc(out, func(a, b Script) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Write replaces the published script of flow name in dir with src, whole
// or not at all.
func Write(dir, name string, src []byte) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	p := Path(name, false)
	tmp := p + ".edit"
	if err := root.WriteFile(tmp, src, 0o644); err != nil {
		return err
	}
	if err := root.Rename(tmp, p); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return nil
}
