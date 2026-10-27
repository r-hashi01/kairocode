package sandbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// helper is the test binary's part inside the sandbox (KC_SANDBOX_ROLE): it
// prints what it could do, a line each, and exits 0.
func helper(role string) int {
	switch role {
	case "loopback":
		// Its own server on localhost, and a client of it.
		if ln, err := net.Listen("tcp", "127.0.0.1:0"); err != nil {
			fmt.Println("tcp: no:", err)
		} else {
			go func() {
				if c, err := ln.Accept(); err == nil {
					c.Close()
				}
			}()
			if c, err := net.DialTimeout("tcp", ln.Addr().String(), 3*time.Second); err != nil {
				fmt.Println("tcp: no:", err)
			} else {
				c.Close()
				fmt.Println("tcp: yes")
			}
			ln.Close()
		}
		// A Unix socket in its own TMPDIR.
		if ln, err := net.Listen("unix", filepath.Join(os.TempDir(), "s")); err != nil {
			fmt.Println("unix: no:", err)
		} else {
			ln.Close()
			fmt.Println("unix: yes")
		}
		// Another's socket, outside it.
		if addr := os.Getenv("KC_SANDBOX_UNIX"); addr != "" {
			if c, err := net.DialTimeout("unix", addr, 3*time.Second); err != nil {
				fmt.Println("other unix: no")
			} else {
				c.Close()
				fmt.Println("other unix: yes")
			}
		}
		// Another's socket moved into its own directory, or its folder.
		if addr := os.Getenv("KC_SANDBOX_UNIX"); addr != "" {
			moved := filepath.Join(os.TempDir(), "moved.sock")
			if os.Rename(addr, moved) == nil {
				fmt.Println("moved socket: yes")
				if c, err := net.DialTimeout("unix", moved, 3*time.Second); err == nil {
					c.Close()
					fmt.Println("moved socket reached: yes")
				}
			}
			folder := filepath.Join(os.TempDir(), "moved")
			if os.Rename(filepath.Dir(addr), folder) == nil {
				fmt.Println("moved folder: yes")
				if c, err := net.DialTimeout("unix", filepath.Join(folder, filepath.Base(addr)), 3*time.Second); err == nil {
					c.Close()
					fmt.Println("moved folder reached: yes")
				}
			}
			if os.Remove(addr) == nil {
				fmt.Println("removed socket: yes")
			}
		}
		fmt.Println("tmpdir:", os.Getenv("TMPDIR"))
	case "available":
		fmt.Println("available:", Available() == nil)
	}
	return 0
}

// inSandbox runs the test binary as helper role under p.
func inSandbox(t *testing.T, p Policy, role string, env ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd, err := Command(ctx, p, "", os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(append(cmd.Env, "KC_SANDBOX_ROLE="+role), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return string(out)
}

// shortTemp is a directory for a Unix socket: a path short enough for one.
func shortTemp(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "kct-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// Loopback (ADR 0031): localhost, and Unix sockets in the command's own
// TMPDIR, which goes with it; not another's socket. Without it, neither
// localhost nor a socket. (Beyond this machine is not tried: the tests use
// no network. macOS's "localhost" is all of this machine's addresses.)
func TestSandboxLoopback(t *testing.T) {
	needSandbox(t)
	root := t.TempDir()
	// Another's socket (the daemon's, tmux's) in the temporary directory.
	other := filepath.Join(shortTemp(t), "o.sock")
	oln, err := net.Listen("unix", other)
	if err != nil {
		t.Fatal(err)
	}
	defer oln.Close()
	go func() {
		for {
			c, err := oln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	env := []string{"KC_SANDBOX_UNIX=" + other}

	dir, done, err := SocketDir()
	if err != nil {
		t.Fatal(err)
	}
	out := inSandbox(t, Policy{Mode: WorkspaceWrite, Root: root, Loopback: true, SocketDir: dir}, "loopback", env...)
	for _, want := range []string{"tcp: yes", "unix: yes", "other unix: no", "tmpdir: " + dir} {
		if !strings.Contains(out, want) {
			t.Errorf("loopback: want %q in\n%s", want, out)
		}
	}
	// Not by moving it either (in $TMPDIR, as the daemon's is, or /tmp).
	for _, not := range []string{"moved socket: yes", "moved folder: yes", "reached: yes", "removed socket: yes"} {
		if strings.Contains(out, not) {
			t.Errorf("loopback: %q in\n%s", not, out)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("another's socket: %v", err)
	}
	done()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("its directory is still there: %v", err)
	}

	if runtime.GOOS == "darwin" {
		// On Linux a command without the network has a localhost of its own
		// (its network namespace), and sockets are as before.
		out = inSandbox(t, Policy{Mode: WorkspaceWrite, Root: root}, "loopback", env...)
		for _, want := range []string{"tcp: no", "unix: no", "other unix: no"} {
			if !strings.Contains(out, want) {
				t.Errorf("without loopback: want %q in\n%s", want, out)
			}
		}
	}
}

// Loopback needs its own directory: without one, no command.
func TestSandboxLoopbackNeedsItsDirectory(t *testing.T) {
	if _, err := Command(context.Background(), Policy{Mode: WorkspaceWrite, Root: t.TempDir(), Loopback: true}, "", "true"); err == nil {
		t.Fatal("a loopback command without its directory")
	}
}

// In a sandbox, another cannot be started on macOS: there is none there
// (ADR 0031), and commands are not run unconfined.
func TestSandboxNestedIsUnavailable(t *testing.T) {
	needSandbox(t)
	if runtime.GOOS != "darwin" {
		t.Skip("macOS's sandbox cannot be nested; bubblewrap may be")
	}
	out := inSandbox(t, Policy{Mode: WorkspaceWrite, Root: t.TempDir()}, "available")
	if !strings.Contains(out, "available: false") {
		t.Fatalf("in a sandbox: %s", out)
	}
	if _, err := exec.LookPath("true"); err != nil {
		t.Skip("no true")
	}
}
