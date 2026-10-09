package tools

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// skipDirs are not walked by list, glob and grep.
var skipDirs = map[string]bool{".git": true, "node_modules": true}

// resolve turns a path the model gave into an absolute path inside root, or
// fails. Symbolic links are followed as far as the path exists, so a link
// cannot lead out of root.
func resolve(root, p string) (string, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if p == "" {
		p = "."
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(realRoot, p)
	}
	p = filepath.Clean(p)
	// Resolve the longest prefix that exists; keep the rest as written.
	rest := ""
	cur := p
	for {
		r, err := filepath.EvalSymlinks(cur)
		if err == nil {
			cur = filepath.Join(r, rest)
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		// A link to something missing would be followed by the write that
		// creates it, possibly outside root: refuse it.
		if fi, lerr := os.Lstat(cur); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("path goes through a symbolic link to nothing: " + cur)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
	if cur != realRoot && !strings.HasPrefix(cur, realRoot+string(os.PathSeparator)) {
		return "", errors.New("path is outside the working directory: " + p)
	}
	return cur, nil
}

// rel is p relative to root, with forward slashes, for output.
func rel(root, p string) string {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root
	}
	r, err := filepath.Rel(realRoot, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(r)
}

// walkFiles calls fn for each regular file under dir, in lexical order,
// skipping skipDirs. fn returns false to stop.
func walkFiles(dir string, fn func(path string) bool) error {
	stop := errors.New("stop")
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir {
				return err
			}
			return nil // unreadable entries are skipped
		}
		if d.IsDir() {
			if p != dir && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if !fn(p) {
			return stop
		}
		return nil
	})
	if errors.Is(err, stop) {
		return nil
	}
	return err
}
