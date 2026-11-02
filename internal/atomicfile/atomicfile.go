// Package atomicfile writes a file whole or not at all: to a temporary file
// of its own in the same directory, synced, then renamed over the file. A
// reader sees the file as it was or as it is now, never in between; two
// writers at once do not write over each other's temporary file.
package atomicfile

import (
	"io/fs"
	"os"
	"path/filepath"
)

// Write writes b to path, with perm.
func Write(path string, b []byte, perm fs.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // gone once renamed; left if not
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
