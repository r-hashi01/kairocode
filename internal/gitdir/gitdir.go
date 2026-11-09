// Package gitdir finds a linked git worktree's repository from the files
// git leaves, without running git (ADR 0037): the worktree's .git file
// names its own git directory, whose commondir is the repository's.
package gitdir

import (
	"os"
	"path/filepath"
	"strings"
)

// Linked is the common git directory of the repository dir is a linked
// worktree of, and true; "" and false if dir is not one. A worktree counts
// only if its repository lists it back: its own git directory is
// <common>/worktrees/<name>, its commondir leads back to that <common>, and
// its gitdir file names dir's .git. Files made in dir itself (a .git file
// naming a folder beside it) lead only to that folder, so they cannot
// make dir another repository's worktree.
func Linked(dir string) (string, bool) {
	dotgit := filepath.Join(dir, ".git")
	b, err := os.ReadFile(dotgit)
	if err != nil { // a directory (a main checkout), or none
		return "", false
	}
	own, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
	if !ok {
		return "", false
	}
	if !filepath.IsAbs(own) {
		own = filepath.Join(dir, own)
	}
	own, err = filepath.EvalSymlinks(own)
	if err != nil || filepath.Base(filepath.Dir(own)) != "worktrees" {
		return "", false
	}
	back, err := os.ReadFile(filepath.Join(own, "gitdir"))
	if err != nil || !same(strings.TrimSpace(string(back)), dotgit) {
		return "", false
	}
	c, err := os.ReadFile(filepath.Join(own, "commondir"))
	if err != nil {
		return "", false
	}
	common := strings.TrimSpace(string(c))
	if !filepath.IsAbs(common) {
		common = filepath.Join(own, common)
	}
	if !same(common, filepath.Dir(filepath.Dir(own))) {
		return "", false
	}
	return filepath.Dir(filepath.Dir(own)), true
}

// Main is the main checkout of the repository dir is a linked worktree
// of (the folder its common .git is in), else dir.
func Main(dir string) string {
	if common, ok := Linked(dir); ok && filepath.Base(common) == ".git" {
		return filepath.Dir(common)
	}
	return dir
}

// same reports whether a and b are the same path, links resolved.
func same(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}
