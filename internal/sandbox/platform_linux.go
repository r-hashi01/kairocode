package sandbox

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"

	seccomp "github.com/elastic/go-seccomp-bpf"
	"github.com/elastic/go-seccomp-bpf/arch"
	"golang.org/x/net/bpf"
)

// unixDomain is AF_UNIX, socket's first argument.
const unixDomain = 1

// bwrapPath is bubblewrap, found on PATH once.
var bwrapPath = "bwrap"

var (
	availOnce sync.Once
	availErr  error
)

// Available says whether commands can be confined here: bubblewrap is
// installed and may make the namespaces (it cannot under some AppArmor
// settings, or in a container that forbids user namespaces).
func Available() error {
	availOnce.Do(func() {
		path, err := exec.LookPath(bwrapPath)
		if err != nil {
			availErr = fmt.Errorf("%w: no bubblewrap (bwrap)", ErrUnavailable)
			return
		}
		probe := exec.Command(path, "--ro-bind", "/", "/", "--dev", "/dev", "--unshare-user", "--unshare-pid", "--", "true")
		if out, err := probe.CombinedOutput(); err != nil {
			availErr = fmt.Errorf("%w: bubblewrap cannot make namespaces here: %v: %s", ErrUnavailable, err, strings.TrimSpace(string(out)))
		}
	})
	return availErr
}

// platformCommand runs argv under bubblewrap (after Codex): the whole file
// system read-only, the writable roots bound writable again, the protected
// names under them read-only again, no network namespace unless allowed,
// and a seccomp filter.
//
// A network namespace does not keep Unix sockets apart, and a read-only
// mount does not refuse a connect: without the network, a command makes no
// Unix socket at all (as on macOS), and a loopback one sees the temporary
// and runtime directories empty, where others' sockets are (the daemon's,
// tmux's, ssh-agent's: ADR 0031).
//
// A protected name that does not exist yet can be made: binding it
// read-only would need it to exist (Codex has the same open issue). macOS
// refuses that too.
func platformCommand(ctx context.Context, p Policy, root string, argv []string) (*exec.Cmd, error) {
	if err := Available(); err != nil {
		return nil, err
	}
	path, _ := exec.LookPath(bwrapPath)
	args := []string{
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--unshare-user", "--unshare-pid", "--unshare-ipc",
		"--die-with-parent", "--new-session",
	}
	roots := writableRoots(p, root)
	bound := roots
	var later []string // hidden directories under a writable root
	if p.Loopback && !p.Network {
		bound = nil
		for _, w := range roots {
			if w != p.SocketDir {
				bound = append(bound, w)
			}
		}
		dirs, sockets := socketPlaces()
		for _, d := range dirs {
			if slices.Contains(bound, d) {
				return nil, fmt.Errorf("sandbox: loopback in %s, which holds others' sockets", d)
			}
			if within(d, bound) {
				later = append(later, d)
			} else {
				args = append(args, "--tmpfs", d)
			}
		}
		for _, s := range sockets {
			args = append(args, "--ro-bind", "/dev/null", s)
		}
	}
	for _, w := range bound {
		args = append(args, "--bind", w, w)
	}
	args = append(args, protect(p, bound)...)
	// A writable root binds back what is under it: what is hidden there is
	// hidden again after it (and after its protected names), and the
	// command's own directory bound last.
	for _, d := range later {
		args = append(args, "--tmpfs", d)
	}
	if len(bound) < len(roots) {
		args = append(args, "--bind", p.SocketDir, p.SocketDir)
		args = append(args, protect(p, []string{p.SocketDir})...)
	}
	if !p.Network {
		args = append(args, "--unshare-net")
	}
	filter, err := seccompFilter(!p.Network && !p.Loopback)
	if err != nil {
		return nil, err
	}
	args = append(args, "--seccomp", "3", "--")
	cmd := exec.CommandContext(ctx, path, append(args, argv...)...)
	cmd.ExtraFiles = []*os.File{filter} // fd 3 in bubblewrap
	return cmd, nil
}

// protect binds the protected names under roots read-only again.
func protect(p Policy, roots []string) []string {
	var args []string
	for _, w := range roots {
		for _, name := range protectedNames(p) {
			pp := filepath.Join(w, name)
			if _, err := os.Lstat(pp); err == nil {
				args = append(args, "--ro-bind", pp, pp)
			}
		}
	}
	return args
}

// within says whether d is under one of dirs.
func within(d string, dirs []string) bool {
	for _, w := range dirs {
		if strings.HasPrefix(d, w+"/") || w == "/" {
			return true
		}
	}
	return false
}

// socketPlaces are where others' Unix sockets are, for a loopback command
// to see empty: the temporary and runtime directories, the daemon's data
// directory (HideFromLoopback), and the sockets directly in /run (docker's).
// Sockets elsewhere (under the home directory: gpg-agent's) stay in sight:
// Linux has no rule for which socket a process may reach (ADR 0031).
func socketPlaces() (dirs, sockets []string) {
	cands := []string{"/tmp", "/var/tmp", os.TempDir(), os.Getenv("XDG_RUNTIME_DIR"), fmt.Sprintf("/run/user/%d", os.Getuid())}
	for _, d := range append(cands, hiddenFromLoopback()...) {
		if d == "" {
			continue
		}
		real, err := filepath.EvalSymlinks(d)
		// One under another is kept: a writable root may lie between them.
		if err != nil || real == "/" || slices.Contains(dirs, real) {
			continue
		}
		dirs = append(dirs, real)
	}
	if entries, err := os.ReadDir("/run"); err == nil {
		for _, e := range entries {
			if e.Type()&os.ModeSocket != 0 {
				sockets = append(sockets, filepath.Join("/run", e.Name()))
			}
		}
	}
	return dirs, sockets
}

// seccompFilter is the filter bubblewrap loads (--seccomp): calls that let
// a process reach into others or around the filter fail with EPERM, as in
// Codex. With noUnix, so does making a Unix socket (socketpair stays).
func seccompFilter(noUnix bool) (*os.File, error) {
	policy := seccomp.Policy{
		DefaultAction: seccomp.ActionAllow,
		Syscalls: []seccomp.SyscallGroup{{
			Action: seccomp.ActionErrno,
			Names: []string{
				"ptrace", "process_vm_readv", "process_vm_writev",
				"io_uring_setup", "io_uring_enter", "io_uring_register",
			},
		}},
	}
	insts, err := policy.Assemble()
	if err != nil {
		return nil, err
	}
	pre, err := filterPrefix(noUnix)
	if err != nil {
		return nil, err
	}
	raw, err := bpf.Assemble(append(pre, insts...))
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp("", "kairocode-seccomp-")
	if err != nil {
		return nil, err
	}
	os.Remove(f.Name()) // kept open, gone from the directory
	b := make([]byte, 8*len(raw))
	for i, in := range raw {
		binary.NativeEndian.PutUint16(b[8*i:], in.Op)
		b[8*i+2], b[8*i+3] = in.Jt, in.Jf
		binary.NativeEndian.PutUint32(b[8*i+4:], in.K)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// filterPrefix comes before the library's filter. It refuses every call of
// another architecture (the library lets those through: i386's socket by
// int 0x80 on amd64 would pass it). With noUnix, it refuses socket with
// AF_UNIX, looking at the low 32 bits alone: the kernel reads the family as
// an int, and the library compares all 64 (1<<32|AF_UNIX would pass it).
func filterPrefix(noUnix bool) ([]bpf.Instruction, error) {
	info, err := arch.GetInfo("")
	if err != nil {
		return nil, err
	}
	eperm := bpf.RetConstant{Val: uint32(seccomp.ActionErrno) | uint32(syscall.EPERM)}
	pre := []bpf.Instruction{
		bpf.LoadAbsolute{Off: 4, Size: 4}, // arch
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: uint32(info.ID), SkipTrue: 1},
		eperm,
	}
	if !noUnix {
		return pre, nil
	}
	nr, ok := info.SyscallNames["socket"]
	if !ok {
		return nil, fmt.Errorf("sandbox: no socket call on %s", info.Name)
	}
	lo := uint32(16) // args[0], its low half on a little-endian machine
	if binary.NativeEndian.Uint16([]byte{0, 1}) == 1 {
		lo += 4
	}
	return append(pre,
		bpf.LoadAbsolute{Off: 0, Size: 4}, // nr
		bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: uint32(nr), SkipTrue: 3},
		bpf.LoadAbsolute{Off: lo, Size: 4},
		bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: unixDomain, SkipTrue: 1},
		eperm,
	), nil
}
