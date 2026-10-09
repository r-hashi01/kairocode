package sandbox

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	seccomp "github.com/elastic/go-seccomp-bpf"
	"golang.org/x/net/bpf"
)

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
	for _, w := range roots {
		args = append(args, "--bind", w, w)
	}
	for _, w := range roots {
		for _, name := range protectedNames(p) {
			pp := filepath.Join(w, name)
			if _, err := os.Lstat(pp); err == nil {
				args = append(args, "--ro-bind", pp, pp)
			}
		}
	}
	if !p.Network {
		args = append(args, "--unshare-net")
	}
	filter, err := seccompFilter()
	if err != nil {
		return nil, err
	}
	args = append(args, "--seccomp", "3", "--")
	cmd := exec.CommandContext(ctx, path, append(args, argv...)...)
	cmd.ExtraFiles = []*os.File{filter} // fd 3 in bubblewrap
	return cmd, nil
}

// seccompFilter is the filter bubblewrap loads (--seccomp): calls that let
// a process reach into others or around the filter fail with EPERM, as in
// Codex.
func seccompFilter() (*os.File, error) {
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
	raw, err := bpf.Assemble(insts)
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
