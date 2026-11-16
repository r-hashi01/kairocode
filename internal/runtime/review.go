package runtime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/config"
	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/permission"
	"github.com/r-hashi01/kairocode/internal/sandbox"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// A line's reviews (ADR 0044): who reviews (line.policy), the change a
// review reads (line.diff), and a person's review put as an approval
// (line.review).

// defaultReviewLines is the size past which a review by risk calls a
// person, when lines.reviewLines does not say.
const defaultReviewLines = 400

// maxDiff is how much of a change's text a review is given.
const maxDiff = 60_000

// linePolicyAction reads lines.people (the user's and the project's, both
// taken: a project cannot take the user's away) and lines.reviewLines (the
// smaller), as the line's turn starts: recorded with it.
func (r *Runtime) linePolicyAction(t *kairo.TaskContext, in workflows.LinePolicyInput) (workflows.LinePolicy, error) {
	layers, _ := config.Layers(in.Dir)
	return policyOf(layers), nil
}

// policyOf is the policy the layers of settings make: every layer's paths
// (a project only adds to the user's), and the smaller size.
func policyOf(layers []config.Layer) workflows.LinePolicy {
	out := workflows.LinePolicy{ReviewLines: defaultReviewLines}
	smaller := 0
	for _, l := range layers {
		var cfg struct {
			Lines struct {
				People      []string `json:"people"`
				ReviewLines int      `json:"reviewLines"`
			} `json:"lines"`
		}
		if l.Decode(&cfg) != nil {
			continue
		}
		for _, p := range cfg.Lines.People {
			if p = strings.TrimSpace(p); p != "" && !slices.Contains(out.People, p) {
				out.People = append(out.People, p)
			}
		}
		if n := cfg.Lines.ReviewLines; n > 0 && (smaller == 0 || n < smaller) {
			smaller = n
		}
	}
	if smaller > 0 {
		out.ReviewLines = smaller
	}
	return out
}

// maxUntracked is how much of a file git does not track yet a review is
// given.
const maxUntracked = 64 << 10

// lineDiffAction is the change of an item so far: from the commit its
// worktree started from (HEAD for a line run elsewhere), its work not yet
// committed among it, and the files git does not track yet. Paths are the
// repository's, as git has them (-z: none quoted; --no-renames: a file
// moved away is a path changed too), so a path that needs a person is not
// missed. It only reads: no external diff or text conversion is run, no
// optional lock taken, and a file not tracked is read only if it is a
// regular file in the folder, up to a size. git failing is an error: a
// review is never asked of a change not known.
func (r *Runtime) lineDiffAction(t *kairo.TaskContext, in workflows.LineDiffInput) (workflows.LineDiff, error) {
	start := "HEAD"
	if row, err := r.Store.Item(t, in.Session); err == nil && row.Start != "" {
		start = row.Start
	}
	top, err := diffGit(t, in.Dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return workflows.LineDiff{}, err
	}
	top = strings.TrimSpace(top)
	var out workflows.LineDiff
	names, err := diffGit(t, top, "diff", "--no-renames", "--name-only", "-z", start)
	if err != nil {
		return workflows.LineDiff{}, err
	}
	out.Paths = splitZ(names)
	num, err := diffGit(t, top, "diff", "--no-renames", "--numstat", "-z", start)
	if err != nil {
		return workflows.LineDiff{}, err
	}
	for _, rec := range splitZ(num) {
		f := strings.SplitN(rec, "\t", 3)
		if len(f) == 3 {
			a, _ := strconv.Atoi(f[0]) // "-" for a binary file: 0
			d, _ := strconv.Atoi(f[1])
			out.Lines += a + d
		}
	}
	text, err := diffGit(t, top, "diff", "--no-renames", "--no-color", "--no-ext-diff", "--no-textconv", start)
	if err != nil {
		return workflows.LineDiff{}, err
	}
	var b strings.Builder
	b.WriteString(text)
	others, err := diffGit(t, top, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return workflows.LineDiff{}, err
	}
	root, err := os.OpenRoot(top)
	if err != nil {
		return workflows.LineDiff{}, err
	}
	defer root.Close()
	for _, p := range splitZ(others) {
		out.Paths = append(out.Paths, p)
		fmt.Fprintf(&b, "\nnew file %s (not yet committed)", p)
		fi, err := root.Lstat(p)
		switch {
		case err != nil:
			fmt.Fprintf(&b, ": cannot be read: %v\n", err)
			continue
		case !fi.Mode().IsRegular():
			fmt.Fprintf(&b, ": not a regular file (%v), not read\n", fi.Mode().Type())
			continue
		}
		f, err := root.Open(p)
		if err != nil {
			fmt.Fprintf(&b, ": cannot be read: %v\n", err)
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, maxUntracked+1))
		f.Close()
		if err != nil {
			fmt.Fprintf(&b, ": cannot be read: %v\n", err)
			continue
		}
		if bytes.IndexByte(data, 0) >= 0 {
			fmt.Fprintf(&b, ": a binary file of %d bytes\n", fi.Size())
			out.Lines++
			continue
		}
		cut := len(data) > maxUntracked
		if cut {
			data = data[:maxUntracked]
		}
		lines := strings.Count(string(data), "\n")
		out.Lines += lines
		fmt.Fprintf(&b, " (%d lines):\n", lines)
		for _, l := range strings.SplitAfter(string(data), "\n") {
			if l != "" {
				b.WriteString("+" + l)
			}
		}
		if cut {
			b.WriteString("\n…(the rest of the file cut)\n")
		}
	}
	diff := strings.ReplaceAll(b.String(), "\x00", "") // nothing may pass for the preview's own marks
	if len(diff) > maxDiff {
		diff = diff[:maxDiff] + "\n…(the rest of the change cut; the branch has it whole)"
	}
	if strings.TrimSpace(diff) == "" {
		diff = "(no change)"
	}
	h, err := r.Store.PutMessage(t, msg.Message{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: diff}}})
	if err != nil {
		return workflows.LineDiff{}, err
	}
	out.Diff = h
	return out, nil
}

// diffGit runs git to read a change, in KairoCode's sandbox, reading only
// (an agent wrote the worktree: what its attributes would make git run
// runs there too): no hooks, paths as they are, no optional lock (the
// index is not refreshed). Its output is not trimmed.
func diffGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd, err := sandbox.Command(ctx, sandbox.Policy{Mode: sandbox.ReadOnly, Root: dir}, dir,
		append([]string{"git", "-c", "core.hooksPath=/dev/null", "-c", "core.quotePath=false"}, args...)...)
	if err != nil {
		return "", err
	}
	cmd.Env = append(cmd.Env, "GIT_OPTIONAL_LOCKS=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// splitZ splits git's -z output into its records.
func splitZ(s string) []string {
	var out []string
	for _, r := range strings.Split(s, "\x00") {
		if r != "" {
			out = append(out, r)
		}
	}
	return out
}

// lineReviewAction puts a change to a person (ADR 0044): what the approval
// of kind review the line's turn then waits for says (pendingIn lists it).
func (r *Runtime) lineReviewAction(t *kairo.TaskContext, in workflows.LineReviewInput) (workflows.PermissionOutput, error) {
	line := in.Line
	if line == "" {
		line = "put together on the spot"
	}
	rule := fmt.Sprintf("review the change of the line %s: %d lines in %d files", line, in.Diff.Lines, len(in.Diff.Paths))
	if in.Why != "" {
		rule += " (" + in.Why + ")"
	}
	rule += ". Yes takes it; no, with your words, sends it back."
	var b strings.Builder
	for j, o := range in.Outputs {
		if strings.TrimSpace(o) != "" {
			fmt.Fprintf(&b, "── What station %d left ──\n%s\n\n", j+1, o)
		}
	}
	b.WriteString("── The change ──\n")
	if m, err := r.Store.Message(t, in.Diff.Diff); err == nil {
		b.WriteString(m.Text())
	}
	return workflows.PermissionOutput{Action: string(permission.Ask), Rule: rule, Subject: in.Station, Preview: b.String()}, nil
}
