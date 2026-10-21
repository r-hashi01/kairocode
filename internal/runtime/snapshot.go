package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/sandbox"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// Snapshots of the working files (design 7) are git trees in a bare
// repository of the data directory, one per working directory, apart from
// the user's repository and its index. The settings (sandbox.Protected) are
// neither recorded nor restored: commands in the sandbox may not write
// them, and an undo is no way to change them.

// afterRestore, if set, is called once a restore has put the files back and
// before its result is recorded: a crash test stops the process there.
var afterRestore func()

// snapshotStore is the bare repository that holds dir's snapshots.
func (r *Runtime) snapshotStore(dir string) string {
	h := sha256.Sum256([]byte(dir))
	return filepath.Join(filepath.Dir(r.cfg.DB), "snapshot", hex.EncodeToString(h[:12]))
}

// snapGit runs git on dir's snapshots, in the sandbox, with the store and
// index given.
func snapGit(ctx context.Context, dir, store, index string, args ...string) (string, error) {
	return gitEnv(ctx, dir, store, []string{"GIT_DIR=" + store, "GIT_WORK_TREE=" + dir, "GIT_INDEX_FILE=" + index}, nil, args...)
}

// gitEnv runs git in dir's sandbox, store writable, with env added and
// stdin given. Its output is not trimmed of NULs.
func gitEnv(ctx context.Context, dir, store string, env []string, stdin []byte, args ...string) (string, error) {
	cmd, err := sandbox.Command(ctx, sandbox.Policy{Mode: sandbox.WorkspaceWrite, Root: dir, Writable: []string{store}}, "",
		append([]string{"git"}, args...)...)
	if err != nil {
		return "", err
	}
	cmd.Env = append(cmd.Env, env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), &gitError{args: args, err: err, stderr: strings.TrimSpace(errb.String())}
	}
	return strings.TrimSpace(out.String()), nil
}

type gitError struct {
	args   []string
	err    error
	stderr string
}

func (e *gitError) Error() string { return fmt.Sprintf("git %s: %v: %s", e.args[0], e.err, e.stderr) }

func (e *gitError) Unwrap() error { return e.err }

// openStore makes dir's snapshot store if it is not there, and returns it
// with a fresh index file name (removed by the caller).
func (r *Runtime) openStore(ctx context.Context, dir string) (store, index string, err error) {
	store = r.snapshotStore(dir)
	if err := os.MkdirAll(store, 0o700); err != nil {
		return "", "", err
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	index = filepath.Join(store, "index-"+hex.EncodeToString(b))
	if _, err := os.Stat(filepath.Join(store, "config")); err == nil {
		return store, index, nil
	}
	// Two sessions in one directory may make it at once.
	lock, err := os.OpenFile(store+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", "", err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return "", "", err
	}
	if _, err := os.Stat(filepath.Join(store, "config")); err == nil {
		return store, index, nil
	}
	if _, err := gitEnv(ctx, dir, store, nil, nil, "init", "--bare", "--quiet", store); err != nil {
		return "", "", err
	}
	// Snapshots are trees no ref names: git must not prune them.
	if _, err := gitEnv(ctx, dir, store, nil, nil, "--git-dir="+store, "config", "gc.auto", "0"); err != nil {
		return "", "", err
	}
	return store, index, nil
}

func removeIndex(index string) {
	os.Remove(index)
	os.Remove(index + ".lock")
}

// excluded are pathspecs for the files snapshots leave alone.
func excluded() []string {
	out := []string{"--", "."}
	for _, name := range sandbox.Protected {
		out = append(out, ":(exclude,top)"+name)
	}
	return out
}

// snapshot records dir's files as a tree and returns its hash. Ignored
// files (.gitignore) are left out, so an undo leaves them alone; a
// repository inside dir is recorded as itself (a gitlink), not its files.
func (r *Runtime) snapshot(ctx context.Context, dir string) (string, error) {
	dir = filepath.Clean(dir)
	store, index, err := r.openStore(ctx, dir)
	if err != nil {
		return "", err
	}
	defer removeIndex(index)
	if _, err := snapGit(ctx, dir, store, index, append([]string{"add", "--all"}, excluded()...)...); err != nil {
		return "", err
	}
	return snapGit(ctx, dir, store, index, "write-tree")
}

// restore makes dir's files what tree has. Of the files tree does not
// have, it removes only those that from (the other end of the change being
// taken back) has: files made outside that change stay, and so do files
// the restored .gitignore ignores, and repositories inside dir. It returns
// the snapshot of the files as they were before it.
func (r *Runtime) restore(ctx context.Context, dir, tree, from string) (string, error) {
	dir = filepath.Clean(dir)
	prior, err := r.snapshot(ctx, dir)
	if err != nil {
		return "", err
	}
	store, index, err := r.openStore(ctx, dir)
	if err != nil {
		return "", err
	}
	defer removeIndex(index)
	if _, err := snapGit(ctx, dir, store, index, "read-tree", tree); err != nil {
		return "", err
	}
	if _, err := snapGit(ctx, dir, store, index, "checkout-index", "--all", "--force", "--quiet"); err != nil {
		return "", err
	}
	if from == "" {
		return prior, nil
	}
	// Files from has and tree does not (raw lines: ":<mode> <mode> <sha>
	// <sha> A", then the path), that are there now.
	raw, err := snapGit(ctx, dir, store, index, "diff-tree", "-r", "-z", "--diff-filter=A", tree, from)
	if err != nil {
		return "", err
	}
	listed, err := snapGit(ctx, dir, store, index, "ls-tree", "-r", "-z", "--name-only", prior)
	if err != nil {
		return "", err
	}
	there := map[string]bool{}
	for _, p := range strings.Split(listed, "\x00") {
		there[p] = true
	}
	var gone []string
	fields := strings.Split(raw, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		meta := strings.Fields(fields[i])
		path := fields[i+1]
		if len(meta) < 2 || meta[1] == "160000" || !there[path] {
			continue // a repository inside dir, or not there now
		}
		gone = append(gone, path)
	}
	if len(gone) == 0 {
		return prior, nil
	}
	// Ignored by the .gitignore just restored: not the change's to remove.
	ignored := map[string]bool{}
	in := []byte(strings.Join(gone, "\x00") + "\x00")
	listedIgnored, err := gitEnv(ctx, dir, store, []string{"GIT_DIR=" + store, "GIT_WORK_TREE=" + dir, "GIT_INDEX_FILE=" + index}, in,
		"check-ignore", "--no-index", "-z", "--stdin")
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
		return "", err // 1: none is ignored
	}
	for _, p := range strings.Split(listedIgnored, "\x00") {
		ignored[p] = true
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	for _, p := range gone {
		if ignored[p] {
			continue
		}
		// os.Root keeps the removal inside dir, symbolic links or not.
		if err := root.Remove(filepath.FromSlash(p)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return prior, nil
}

// snapshotAction records the working files at the start or the end of a
// turn. A snapshot that cannot be made (no git, no sandbox) does not stop
// the turn: it is recorded as missing, and that turn cannot be undone.
func (r *Runtime) snapshotAction(t *kairo.TaskContext, in workflows.SnapshotInput) (workflows.SnapshotOutput, error) {
	tree, err := r.snapshot(t, in.Dir)
	if err != nil {
		if t.Err() != nil {
			return workflows.SnapshotOutput{}, err
		}
		return workflows.SnapshotOutput{Error: err.Error()}, nil
	}
	return workflows.SnapshotOutput{Tree: tree}, nil
}

// restoreAction puts the working files back for an undo or a redo, and
// stores what it did as a message for the conversation. Doing it again
// leaves the files the same; one that fails part-way is tried again.
func (r *Runtime) restoreAction(t *kairo.TaskContext, in workflows.RestoreInput) (workflows.RestoreOutput, error) {
	u := in.Undo
	tag, verb, when, none := "[undo]", "undid", "before", "Nothing to undo."
	if u.Redo {
		tag, verb, when, none = "[redo]", "redid", "after", "Nothing to redo."
	}
	if u.Fork {
		tag = "[fork]"
	}
	out := workflows.RestoreOutput{}
	var text string
	switch {
	case u.Of < 0:
		text = none
	case u.Tree == "":
		out.Skipped = true
		text = fmt.Sprintf("Turn %d has no snapshot of the files: it was passed over, and the files were not changed.", u.Of)
	default:
		prior, err := r.restore(t, in.Dir, u.Tree, u.From)
		if err != nil {
			return out, err
		}
		if afterRestore != nil {
			afterRestore()
		}
		out.Restored, out.Prior = true, prior
		text = fmt.Sprintf("The user %s turn %d: the working files are back as they were %s it.", verb, u.Of, when)
		if u.Fork {
			text = fmt.Sprintf("The session was forked from turn %d of another: the working files are back as they were after it.", u.Of)
		}
	}
	h, err := r.Store.PutMessage(t, msg.Message{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: tag + " " + text}}})
	if err != nil {
		return workflows.RestoreOutput{}, err
	}
	out.Message = h
	return out, nil
}
