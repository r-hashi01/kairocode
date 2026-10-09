package tools

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
)

// Absent is the hash of a file that does not exist.
const Absent = "absent"

// Hash is a file content's hash, as plans record it.
func Hash(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

// current is the hash of the file at p now.
func current(env Env, p string) (string, error) {
	b, err := env.Root.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return Absent, nil
	}
	if err != nil {
		return "", err
	}
	return Hash(b), nil
}

// AfterWrite, if set, is called once a write is in place and before Apply
// returns: a crash test stops the process there.
var AfterWrite func(path string)

// Apply makes a planned write by comparing and replacing (design 3.1): the
// file as the plan expected it is written; the file as the plan would leave
// it is already done; anything else is a conflict, given to the model. The
// new content goes to a temporary file that is then renamed over the old,
// so no half-written file is ever seen. Done again, Apply does not write
// twice.
func Apply(env Env, p, before, after string, content []byte, summary string) Result {
	if protected(p) {
		return failed("%s is protected", p)
	}
	if st, err := env.Root.Lstat(p); err == nil && st.Mode()&fs.ModeSymlink != 0 {
		return failed("%s is a symbolic link; edit the file it points to", p)
	}
	now, err := current(env, p)
	if err != nil {
		return Result{Output: err.Error(), Unknown: true}
	}
	switch now {
	case after:
		return Result{Output: summary}
	case before:
	default:
		return failed("%s changed since it was read (someone else edited it). Read it again, then redo the change.", p)
	}
	if dir := path.Dir(p); dir != "." {
		if err := env.Root.MkdirAll(dir, 0o755); err != nil {
			return failed("cannot make %s: %v", dir, err)
		}
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return Result{Output: err.Error(), Unknown: true}
	}
	tmp := path.Join(path.Dir(p), fmt.Sprintf(".%s.kairocode-%x", path.Base(p), b))
	mode := fs.FileMode(0o644)
	if st, err := env.Root.Stat(p); err == nil {
		mode = st.Mode().Perm()
	}
	if err := env.Root.WriteFile(tmp, content, mode); err != nil {
		_ = env.Root.Remove(tmp)
		return failed("cannot write %s: %v", p, err)
	}
	// WriteFile's mode is cut by the umask; the file keeps its own.
	if err := env.Root.Chmod(tmp, mode); err != nil {
		_ = env.Root.Remove(tmp)
		return failed("cannot write %s: %v", p, err)
	}
	if err := env.Root.Rename(tmp, p); err != nil {
		_ = env.Root.Remove(tmp)
		// The rename may or may not have happened.
		return Result{Output: err.Error(), Unknown: true}
	}
	if AfterWrite != nil {
		AfterWrite(p)
	}
	return Result{Output: summary}
}
