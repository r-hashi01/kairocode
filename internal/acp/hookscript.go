package acp

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A hook's question says the command it runs (ADR 0018); the script that
// command runs is shown with it, read as the question is shown (nothing of
// it is recorded): what runs is seen before it is chosen to.

// scriptLines is how much of a script is shown.
const scriptLines = 60

// hookScripts is the scripts the command in question (its indented line)
// runs, as text to put under it: each word of it that is a file, in dir or
// from ~, its first lines. "" when it names none.
func hookScripts(question, dir string) string {
	var command string
	for _, l := range strings.Split(question, "\n") {
		if strings.HasPrefix(l, "    ") {
			command = strings.TrimSpace(l)
			break
		}
	}
	var b strings.Builder
	seen := map[string]bool{}
	for _, w := range strings.Fields(command) {
		w = strings.Trim(w, `'"`)
		if strings.HasPrefix(w, "~/") {
			if h, err := os.UserHomeDir(); err == nil {
				w = filepath.Join(h, w[2:])
			}
		} else if !filepath.IsAbs(w) && dir != "" {
			w = filepath.Join(dir, w)
		}
		st, err := os.Stat(w)
		if err != nil || !st.Mode().IsRegular() || seen[w] {
			continue
		}
		seen[w] = true
		lines, more, err := firstLinesOf(w, scriptLines)
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "\n\n── %s ──\n%s", w, strings.Join(lines, "\n"))
		if more {
			fmt.Fprintf(&b, "\n… (more lines: read the file before choosing)")
		}
	}
	return b.String()
}

// firstLinesOf is the first n lines of the file, and whether it has more.
func firstLinesOf(path string, n int) ([]string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	var out []string
	for sc.Scan() {
		if len(out) == n {
			return out, true, nil
		}
		out = append(out, sc.Text())
	}
	return out, false, sc.Err()
}
