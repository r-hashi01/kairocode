package tools

import (
	"errors"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/r-hashi01/kairocode/internal/sandbox"
)

// skipDirs are not walked by list, glob and grep.
var skipDirs = map[string]bool{".git": true, "node_modules": true}

// rel turns a path the model gave into a slash-separated path relative to the
// working directory. It only rewrites the name: env.Root refuses, at each
// step and atomically, a name that leads outside the directory, through ".."
// or a symbolic link.
func rel(env Env, p string) (string, error) {
	if p == "" {
		return ".", nil
	}
	if filepath.IsAbs(p) {
		r, ok := within(env.Dir, p)
		if !ok {
			if real, err := filepath.EvalSymlinks(env.Dir); err == nil {
				r, ok = within(real, p)
			}
		}
		if !ok {
			return "", errors.New("path is outside the working directory: " + p)
		}
		p = r
	}
	p = path.Clean(filepath.ToSlash(p))
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", errors.New("path is outside the working directory: " + p)
	}
	return p, nil
}

func within(dir, p string) (string, bool) {
	r, err := filepath.Rel(dir, filepath.Clean(p))
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", false
	}
	return r, true
}

// protected says whether p (relative to the working directory) is one of
// the names kept out of reach directly under it, or under one: .git, the
// agents' settings, kairocode.json (ADR 0005). write and edit refuse them
// as the sandbox does, so no tool can change what the next turn may do or
// plant a git hook.
func protected(p string) bool {
	first, _, _ := strings.Cut(p, "/")
	return slices.Contains(sandbox.Protected, first)
}

// walkFiles calls fn with the slash-separated path of each regular file under
// dir (itself relative to the working directory), in lexical order, skipping
// skipDirs. fn returns false to stop.
func walkFiles(env Env, dir string, fn func(p string) bool) error {
	err := fs.WalkDir(env.Root.FS(), dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir {
				return err
			}
			return nil // unreadable entries are skipped
		}
		if d.IsDir() {
			if p != dir && skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if !fn(p) {
			return fs.SkipAll
		}
		return nil
	})
	return err
}
