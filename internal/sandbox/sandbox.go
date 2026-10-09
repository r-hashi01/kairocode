// Package sandbox runs the commands KairoCode starts inside the operating
// system's sandbox, after Codex (ADR 0005): writes only under the session's
// working directory and the temporary directories, no network unless asked,
// and the directories that hold KairoCode's and other agents' settings kept
// out of reach. Nothing else in KairoCode starts a command another way
// (AGENTS.md); where there is no sandbox, Command fails rather than run
// without one.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Mode int

const (
	// ReadOnly: nothing may be written, nothing reached over the network.
	ReadOnly Mode = iota
	// WorkspaceWrite: writes under the working directory and the temporary
	// directories only.
	WorkspaceWrite
	// FullAccess: no sandbox. Only for a call the user approved (escalation).
	FullAccess
)

func (m Mode) String() string {
	switch m {
	case ReadOnly:
		return "read-only"
	case WorkspaceWrite:
		return "workspace-write"
	case FullAccess:
		return "full-access"
	}
	return "unknown"
}

// Policy is how a command is confined.
type Policy struct {
	Mode Mode
	// Root is the session's working directory, as fixed when the session
	// started. A directory the model names counts only inside it.
	Root string
	// Writable are more directories to allow writes under (settings).
	Writable []string
	// Network allows network access.
	Network bool
}

// Protected names, directly under a writable directory, are not written even
// there: they hold the repository's history and the settings, hooks and
// permissions of KairoCode and other agents (ADR 0005), kairocode.json among
// them. A command in the sandbox cannot change what it is allowed to do.
//
// On Linux only those that exist are kept: one that does not exist yet can
// be made (see platformCommand).
var Protected = []string{".git", ".kairocode", ".opencode", ".claude", ".agents", "kairocode.json"}

// ErrUnavailable: there is no sandbox here. The caller asks the user every
// time instead of running without one (ADR 0005: fail closed).
var ErrUnavailable = errors.New("sandbox: not available on this system")

// Command makes the command to run argv in dir under p. dir must lie inside
// p.Root; a directory a model gave that does not is refused, never taken as
// a place to write.
func Command(ctx context.Context, p Policy, dir string, argv ...string) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, errors.New("sandbox: no command")
	}
	root, err := filepath.EvalSymlinks(p.Root)
	if err != nil {
		return nil, fmt.Errorf("sandbox: working directory: %w", err)
	}
	if dir == "" {
		dir = root
	} else {
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(root, dir)
		}
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return nil, fmt.Errorf("sandbox: %w", err)
		}
		if real != root && !strings.HasPrefix(real, root+string(filepath.Separator)) {
			return nil, fmt.Errorf("sandbox: %s is outside the working directory %s", dir, root)
		}
		dir = real
	}
	env := Env(p)
	if p.Mode == FullAccess {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir, cmd.Env = dir, env
		return cmd, nil
	}
	cmd, err := platformCommand(ctx, p, root, argv)
	if err != nil {
		return nil, err
	}
	cmd.Dir, cmd.Env = dir, env
	return cmd, nil
}

// writableRoots are the real paths a policy lets a command write under.
func writableRoots(p Policy, root string) []string {
	if p.Mode != WorkspaceWrite {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(d string) {
		if d == "" {
			return
		}
		real, err := filepath.EvalSymlinks(d)
		if err != nil || seen[real] {
			return
		}
		seen[real] = true
		out = append(out, real)
	}
	add(root)
	add("/tmp")
	add(os.TempDir())
	for _, d := range p.Writable {
		add(d)
	}
	return out
}

// Env is a command's environment: this process's, less anything that looks
// like a secret (ADR 0005, invariant 6), with KAIROCODE_SANDBOX saying how
// the command is confined.
func Env(p Policy) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if secretName(name) || strings.HasPrefix(name, "KAIROCODE_SANDBOX") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "KAIROCODE_SANDBOX="+p.Mode.String())
	if !p.Network && p.Mode != FullAccess {
		env = append(env, "KAIROCODE_SANDBOX_NETWORK_DISABLED=1")
	}
	return env
}

func secretName(name string) bool {
	u := strings.ToUpper(name)
	for _, s := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL"} {
		if strings.Contains(u, s) {
			return true
		}
	}
	return false
}
