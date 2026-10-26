// envcheck says whether this environment can run the tests (scripts/check.sh
// runs it before them): they listen on 127.0.0.1 and on Unix sockets (fake
// servers, the daemon), and start commands in KairoCode's sandbox. An agent's
// own sandbox (Codex's, say) forbids these; the tests would then fail by
// the hundred for what they cannot do, not for what is wrong. It does not
// pass them: it fails first, saying what is missing and where to run them.
package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func main() {
	var missing []string
	if l, err := net.Listen("tcp", "127.0.0.1:0"); err != nil {
		missing = append(missing, "listening on 127.0.0.1 ("+err.Error()+")")
	} else {
		l.Close()
	}
	if dir, err := os.MkdirTemp("", "kc-envcheck-"); err != nil {
		missing = append(missing, "a temporary folder ("+err.Error()+")")
	} else {
		if l, err := net.Listen("unix", filepath.Join(dir, "s")); err != nil {
			missing = append(missing, "listening on a Unix socket ("+err.Error()+")")
		} else {
			l.Close()
		}
		os.RemoveAll(dir)
	}
	if runtime.GOOS == "darwin" {
		// KairoCode's sandbox is macOS's (ADR 0005); one cannot be put in
		// another.
		if out, err := exec.Command("/usr/bin/sandbox-exec", "-p", "(version 1)(allow default)", "/usr/bin/true").CombinedOutput(); err != nil {
			missing = append(missing, fmt.Sprintf("starting a command in a sandbox (%v: %s)", err, out))
		}
	}
	if len(missing) == 0 {
		return
	}
	fmt.Fprintln(os.Stderr, "check: the tests cannot run here: this environment does not allow")
	for _, m := range missing {
		fmt.Fprintln(os.Stderr, "  - "+m)
	}
	fmt.Fprintln(os.Stderr, "It is a sandbox around this command (an agent's, say), not KairoCode failing. Run scripts/check.sh outside it:")
	fmt.Fprintln(os.Stderr, "in KairoCode, bash with sandbox \"escalate\"; in Codex, escalated permissions. See AGENTS.md.")
	os.Exit(2)
}
