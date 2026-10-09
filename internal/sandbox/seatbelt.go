package sandbox

import (
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"
)

// The policies copied from Codex (third_party/codex-sandbox).
var (
	//go:embed seatbelt_base_policy.sbpl
	seatbeltBase string
	//go:embed seatbelt_network_policy.sbpl
	seatbeltNetwork string
)

// seatbeltPath is macOS's sandbox-exec, by absolute path so nothing on PATH
// can stand in for it.
var seatbeltPath = "/usr/bin/sandbox-exec"

// seatbeltPolicy is the SBPL policy for p and the -D parameters it uses.
// Paths go in parameters, never into the policy text, so no path needs
// quoting (after Codex's seatbelt.rs).
func seatbeltPolicy(p Policy, root string) (string, []string) {
	var b strings.Builder
	var params []string
	param := func(name, value string) string {
		params = append(params, fmt.Sprintf("-D%s=%s", name, value))
		return name
	}
	b.WriteString(seatbeltBase)
	b.WriteString("\n; reads\n(allow file-read*)\n")
	roots := writableRoots(p, root)
	// The protected names directly under every writable root. A root's
	// allowance leaves out all of them that lie under it, its own and those
	// of a root inside it (the working directory under $TMPDIR, say).
	var protected []string
	for _, w := range roots {
		for _, name := range Protected {
			protected = append(protected, filepath.Join(w, name))
		}
	}
	for i, w := range roots {
		r := param(fmt.Sprintf("WRITABLE_ROOT_%d", i), w)
		fmt.Fprintf(&b, "\n; writes under %s\n(allow file-write*\n  (require-all\n    (subpath (param %q))", r, r)
		for j, pp := range protected {
			if pp != w && !strings.HasPrefix(pp, w+string(filepath.Separator)) {
				continue
			}
			ro := param(fmt.Sprintf("WRITABLE_ROOT_%d_RO_%d", i, j), pp)
			// literal as well as subpath: a protected name that does not
			// exist yet cannot be made either.
			fmt.Fprintf(&b, "\n    (require-not (literal (param %q)))\n    (require-not (subpath (param %q)))", ro, ro)
		}
		b.WriteString("))\n")
		// The writable directory itself is not removed or renamed away.
		fmt.Fprintf(&b, "(deny file-write-unlink (require-all (literal (param %q)) (vnode-type DIRECTORY)))\n", r)
	}
	if p.Network {
		b.WriteString("\n; network\n")
		b.WriteString(seatbeltNetwork)
		b.WriteString("\n(allow network-outbound)\n(allow network-inbound)\n")
	}
	b.WriteString("\n(deny mach-lookup (xpc-service-name-prefix \"\"))\n")
	if len(roots) == 0 {
		// Nothing writable: no file descriptor can be reopened for writing.
		b.WriteString("(deny system-fcntl (fcntl-command 80 110))\n")
	}
	return b.String(), params
}
