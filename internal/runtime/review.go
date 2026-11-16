package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/config"
	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/permission"
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

// lineDiffAction is the change of an item so far: from the commit its
// worktree started from (HEAD for a line run elsewhere), its work not yet
// committed among it, and the files git does not track yet. It only reads:
// the worktree and its index are left as they are.
func (r *Runtime) lineDiffAction(t *kairo.TaskContext, in workflows.LineDiffInput) (workflows.LineDiff, error) {
	start := "HEAD"
	if row, err := r.Store.Item(t, in.Session); err == nil && row.Start != "" {
		start = row.Start
	}
	var out workflows.LineDiff
	var text strings.Builder
	if names, err := gitIn(t, in.Dir, "diff", "--name-only", start); err == nil && names != "" {
		out.Paths = strings.Split(names, "\n")
	}
	if num, err := gitIn(t, in.Dir, "diff", "--numstat", start); err == nil {
		for _, l := range strings.Split(num, "\n") {
			f := strings.Fields(l)
			if len(f) >= 2 {
				a, _ := strconv.Atoi(f[0])
				d, _ := strconv.Atoi(f[1])
				out.Lines += a + d
			}
		}
	}
	if d, err := gitIn(t, in.Dir, "diff", "--no-color", start); err == nil {
		text.WriteString(d)
	}
	if others, err := gitIn(t, in.Dir, "ls-files", "--others", "--exclude-standard"); err == nil && others != "" {
		for _, p := range strings.Split(others, "\n") {
			b, err := os.ReadFile(filepath.Join(in.Dir, p))
			if err != nil {
				continue
			}
			out.Paths = append(out.Paths, p)
			lines := strings.Count(string(b), "\n")
			out.Lines += lines
			fmt.Fprintf(&text, "\nnew file %s (%d lines, not yet committed):\n", p, lines)
			for _, l := range strings.SplitAfter(string(b), "\n") {
				if l != "" {
					text.WriteString("+" + l)
				}
			}
		}
	}
	diff := text.String()
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
