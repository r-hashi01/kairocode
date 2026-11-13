package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/config"
	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/permission"
	"github.com/r-hashi01/kairocode/internal/sandbox"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// maxCheckTimeout is the longest a check may say it runs: check.run's own
// timeout is a little more.
const maxCheckTimeout = time.Hour

// readChecks reads the checks declared in kairocode.json (ADR 0042), the
// user's then the project's:
//
//	"checks": {
//	  "test":   {"command": "go test ./...", "timeout": "10m", "protect": ["*_test.go"]},
//	  "turn":   ["test"],
//	  "rounds": 3
//	}
//
// A check the project declares takes the place of the user's of that name,
// and is asked about before it first runs. "turn" lists the checks a plain
// turn must pass: the user's and the project's are both kept. Of two
// "rounds", the smaller holds. nil: none declared.
func readChecks(dir string) (*workflows.Checks, error) {
	defs := map[string]workflows.Check{}
	var out workflows.Checks
	var bad error
	layers, _ := config.Layers(dir)
	for _, l := range layers {
		var cfg struct {
			Checks map[string]json.RawMessage `json:"checks"`
		}
		if l.Decode(&cfg) != nil {
			continue
		}
		for _, name := range slices.Sorted(func(yield func(string) bool) {
			for k := range cfg.Checks {
				if !yield(k) {
					return
				}
			}
		}) {
			raw := cfg.Checks[name]
			switch name {
			case "turn":
				var names []string
				if json.Unmarshal(raw, &names) != nil {
					bad = &settingError{path: l.Path, what: `checks.turn is not a list of the checks' names`}
					continue
				}
				for _, n := range names {
					if !slices.Contains(out.Turn, n) {
						out.Turn = append(out.Turn, n)
					}
				}
				continue
			case "rounds":
				var n int
				if json.Unmarshal(raw, &n) != nil || n < 1 {
					bad = &settingError{path: l.Path, what: "checks.rounds is not a number of 1 or more"}
					continue
				}
				out.Rounds = smaller(out.Rounds, n)
				continue
			}
			var c struct {
				Command string   `json:"command"`
				Timeout string   `json:"timeout"`
				Protect []string `json:"protect"`
			}
			if json.Unmarshal(raw, &c) != nil || strings.TrimSpace(c.Command) == "" {
				bad = &settingError{path: l.Path, what: fmt.Sprintf(`checks.%s has no "command"`, name)}
				continue
			}
			def := workflows.Check{Name: name, Command: c.Command, Protect: c.Protect, Project: l.Project, Source: l.Path}
			if c.Timeout != "" {
				d, err := time.ParseDuration(c.Timeout)
				if err != nil || d <= 0 || d > maxCheckTimeout {
					bad = &settingError{path: l.Path, what: fmt.Sprintf(`checks.%s.timeout %s is not a time up to 1h ("10m")`, name, c.Timeout)}
					continue
				}
				def.Timeout = int((d + time.Second - 1) / time.Second)
			}
			defs[name] = def
		}
	}
	if bad != nil {
		return nil, bad
	}
	if len(defs) == 0 && len(out.Turn) == 0 {
		return nil, nil
	}
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for k := range defs {
			if !yield(k) {
				return
			}
		}
	}) {
		out.Defs = append(out.Defs, defs[name])
	}
	return &out, nil
}

// checkRunAction runs a check (ADR 0042) in KairoCode's sandbox: it may
// write in the folder only, with no network but this machine's. It passes
// on exit 0. It observes, so a run cut off runs again (check.run is
// declared to): no person is asked.
func (r *Runtime) checkRunAction(t *kairo.TaskContext, in workflows.CheckRunInput) (workflows.CheckRunOutput, error) {
	limit := time.Duration(workflows.DefaultCheckTimeout) * time.Second
	if in.Check.Timeout > 0 {
		limit = time.Duration(in.Check.Timeout) * time.Second
	}
	ctx, cancel := context.WithTimeout(t, limit)
	defer cancel()
	dir, done, err := sandbox.SocketDir()
	if err != nil {
		return workflows.CheckRunOutput{}, kairo.Retryable(err)
	}
	defer done()
	policy := sandbox.Policy{Mode: sandbox.WorkspaceWrite, Root: in.Dir, Loopback: true, SocketDir: dir}
	cmd, err := sandbox.Command(ctx, policy, in.Dir, "sh", "-c", in.Check.Command)
	if err != nil {
		return workflows.CheckRunOutput{}, err
	}
	// Its own process group, stopped whole at its timeout or when the
	// turn stops: a test binary that go test started does not outlive it
	// holding the output open.
	cmd.SysProcAttr = claudeProcAttr()
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 5 * time.Second
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		return workflows.CheckRunOutput{}, kairo.Retryable(err) // it did not start: nothing ran
	}
	err = cmd.Wait()
	out := workflows.CheckRunOutput{}
	var exit *exec.ExitError
	switch {
	case t.Err() != nil:
		// Cut off: what it did is not known, and a check may run again
		// (check.run is idempotent): no one is asked.
		return workflows.CheckRunOutput{}, kairo.Unknown(t.Err())
	case err == nil:
	case ctx.Err() != nil:
		out.Exit = -1
	case errors.As(err, &exit):
		out.Exit = exit.ExitCode()
		if out.Exit <= 0 {
			out.Exit = 1 // ended by a signal: not passed
		}
	default:
		return workflows.CheckRunOutput{}, kairo.Unknown(err)
	}
	text := output.String()
	if out.Exit < 0 {
		text += fmt.Sprintf("\n[the check ran longer than %v, and was stopped]", limit)
	}
	out.Tail = lastRunes(text, 4000)
	if out.Output, err = r.Store.PutMessage(t, msg.Message{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: lastRunes(text, 200_000)}}}); err != nil {
		return workflows.CheckRunOutput{}, kairo.Unknown(err) // it ran: run again
	}
	return out, nil
}

// lastRunes is the last n characters of s, marked as cut where they are.
func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}

// checkProtectAction hashes the files of the folder that match the globs
// (as the permission rules match, * crossing /): their paths and
// contents. .git is not looked in.
func (r *Runtime) checkProtectAction(t *kairo.TaskContext, in workflows.CheckProtectInput) (string, error) {
	return protectHash(in.Dir, in.Globs)
}

func protectHash(dir string, globs []string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == dir {
				return err
			}
			return nil // gone or not readable: not there
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !slices.ContainsFunc(globs, func(g string) bool { return permission.Match(g, rel) }) {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		fh := sha256.New()
		if _, err := io.Copy(fh, f); err != nil {
			return nil
		}
		fmt.Fprintf(h, "%s\x00%x\n", rel, fh.Sum(nil))
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// kindCheckDecision is what the store keeps when the user says whether a
// project's check runs in a folder: by the folder and the command.
const kindCheckDecision = "check-decision"

func checkDecisionOf(dir string, c workflows.Check, mode string) []byte {
	b, _ := json.Marshal(map[string]string{"dir": dir, "command": c.Command, "mode": mode})
	return b
}

// checkModesAction says how each check may run: the user's own always;
// a project's as the user said in this folder, else ask.
func (r *Runtime) checkModesAction(t *kairo.TaskContext, in workflows.CheckModesInput) ([]string, error) {
	out := make([]string, len(in.Checks))
	for i, c := range in.Checks {
		out[i] = workflows.CheckRun
		if !c.Project {
			continue
		}
		out[i] = workflows.CheckAsk
		for _, mode := range []string{workflows.CheckRun, workflows.CheckNever} {
			_, _, err := r.Store.Get(t, store.Hash(kindCheckDecision, checkDecisionOf(in.Dir, c, mode)))
			if err == nil {
				out[i] = mode
				break
			}
			if !errors.Is(err, store.ErrNotFound) {
				return nil, err
			}
		}
	}
	return out, nil
}

// checkDecideAction keeps how the user said a project's check runs.
func (r *Runtime) checkDecideAction(t *kairo.TaskContext, in workflows.CheckDecideInput) (bool, error) {
	_, err := r.Store.Put(t, kindCheckDecision, checkDecisionOf(in.Dir, in.Check, in.Mode))
	return true, err
}

// protects is the glob of globs that path (a tool's, in dir or under it)
// matches, if any.
func protects(dir, path string, globs []string) (string, bool) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	rel, err := filepath.Rel(dir, filepath.Clean(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	for _, g := range globs {
		if permission.Match(g, rel) {
			return g, true
		}
	}
	return "", false
}
