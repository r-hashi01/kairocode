package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/daemon"
	"github.com/r-hashi01/kairocode/internal/runtime"
)

// exitWith is an error that ends the command with code.
type exitWith struct {
	code int
	error
}

func (e exitWith) Unwrap() error { return e.error }

// intakeCommand takes work in from outside (ADR 0047): a request sent down
// a standard line of the project as the work its key names. The same key
// sent again is the same item. Exit 0 taken in (or there already), 1 an
// error, 2 a wrong use, 6 the line stopped itself.
func intakeCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 && args[0] == "github" {
		return intakeGitHub(ctx, args[1:], stdout, stderr)
	}
	fs := flag.NewFlagSet("intake", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", ".", "a folder of the project's repository")
	data := fs.String("data", runtime.DefaultDataDir(), "data directory")
	line := fs.String("line", "", "the standard line to send it down")
	key := fs.String("key", "", "the work outside: github:owner/repo#12, cron:deps:2026-W47, pagerduty:Q1W2E3")
	asJSON := fs.Bool("json", false, "print the item as JSON")
	prov, model := intakeProvider(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *line == "" || *key == "" || fs.NArg() == 0 {
		return badUse{error: errors.New(`usage: kairocode intake [-dir DIR] -line <line> -key <key> [-json] "<request>"`)}
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	c, err := connect(ctx, *data)
	if err != nil {
		return err
	}
	defer c.Close()
	in, err := c.Intake(ctx, abs, *line, *key, strings.Join(fs.Args(), " "), *prov, *model)
	if err != nil {
		return intakeError(err)
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(in)
	}
	fmt.Fprintln(stdout, intakeText(in))
	return nil
}

// intakeProvider is the flags that choose the intake conversation's
// provider and model when it is first made, as run's.
func intakeProvider(fs *flag.FlagSet) (prov, model *string) {
	prov = fs.String("provider", os.Getenv("KAIROCODE_ENGINE"), "the intake conversation's provider, when it is first made (default: the one last chosen)")
	model = fs.String("model", os.Getenv("KAIROCODE_MODEL"), "the intake conversation's model, when it is first made")
	return prov, model
}

// intakeError is err of an intake: 6 for a line that stopped itself.
func intakeError(err error) error {
	if errors.Is(err, api.ErrLineStopped) {
		return exitWith{6, err}
	}
	return err
}

// intakeText says what an intake came to.
func intakeText(in api.Intake) string {
	it := in.Item
	how := "taken in"
	if !in.New {
		how = "there already"
	}
	s := fmt.Sprintf("%s: item %s on the line %s, %s", how, it.Name, it.Line, it.State)
	if it.State == api.ItemQueued {
		s += fmt.Sprintf(" (%d before it)", it.Position)
	}
	return s + " (kairocode items " + it.Of + ")"
}

// intakeGitHub takes in the open issues of the repository's GitHub remote
// that carry a label (ADR 0047), through gh with the user's own
// credentials: one pass, run from a schedule or an Action. An issue is the
// work github:<owner>/<repo>#<n>, so one taken in before is not again.
func intakeGitHub(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("intake github", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", ".", "a folder of the project's repository")
	data := fs.String("data", runtime.DefaultDataDir(), "data directory")
	line := fs.String("line", "", "the standard line to send the issues down")
	label := fs.String("label", "kairo", "take the open issues with this label")
	asJSON := fs.Bool("json", false, "print what each issue came to as JSON")
	ci := fs.Bool("ci", false, "also give each item in progress its branch's finished workflow runs, as the event ci.result")
	prov, model := intakeProvider(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *line == "" || fs.NArg() != 0 {
		return badUse{error: errors.New(`usage: kairocode intake github [-dir DIR] [-provider P -model M] -line <line> [-label kairo] [-ci] [-json]`)}
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	var repo struct {
		NameWithOwner string `json:"nameWithOwner"`
	}
	if err := ghJSON(ctx, abs, &repo, "repo", "view", "--json", "nameWithOwner"); err != nil {
		return err
	}
	var issues []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		URL    string `json:"url"`
	}
	if err := ghJSON(ctx, abs, &issues, "issue", "list", "--state", "open", "--label", *label, "--limit", "200",
		"--json", "number,title,body,url"); err != nil {
		return err
	}
	c, err := connect(ctx, *data)
	if err != nil {
		return err
	}
	defer c.Close()
	var out []api.Intake
	taken := 0
	for _, is := range issues {
		key := fmt.Sprintf("github:%s#%d", repo.NameWithOwner, is.Number)
		request := strings.TrimSpace(is.Title + "\n\n" + is.Body + "\n\n" + is.URL)
		in, err := c.Intake(ctx, abs, *line, key, request, *prov, *model)
		if err != nil {
			return intakeError(fmt.Errorf("%s: %w", key, err))
		}
		if in.New {
			taken++
		}
		out = append(out, in)
		if !*asJSON {
			fmt.Fprintf(stdout, "%s  %s\n", key, intakeText(in))
		}
	}
	if *ci {
		given, err := ciResults(ctx, c, abs)
		if err != nil {
			return err
		}
		fmt.Fprintf(stderr, "%d CI run(s) given to items as ci.result\n", given)
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	fmt.Fprintf(stderr, "%d issue(s) labelled %s, %d taken in now\n", len(issues), *label, taken)
	return nil
}

// ciResults gives each item of dir's repository still in progress the
// GitHub Actions runs finished on its branch, as the event ci.result (ADR
// 0047): one event a run, under the run's id, so a run given before is
// not given again. It returns how many it gave.
func ciResults(ctx context.Context, c *daemon.Client, dir string) (int, error) {
	items, err := c.RepoItems(ctx, dir)
	if err != nil {
		return 0, err
	}
	given := 0
	for _, it := range items {
		switch it.State {
		case api.ItemQueued, api.ItemRunning, api.ItemWaiting:
		default:
			continue
		}
		if it.Branch == "" {
			continue
		}
		var runs []struct {
			ID         int64  `json:"databaseId"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			SHA        string `json:"headSha"`
			Workflow   string `json:"workflowName"`
			URL        string `json:"url"`
		}
		if err := ghJSON(ctx, dir, &runs, "run", "list", "--branch", it.Branch, "--limit", "20",
			"--json", "databaseId,status,conclusion,headSha,workflowName,url"); err != nil {
			return given, err
		}
		// gh lists the newest first: given oldest first, as they came.
		slices.Reverse(runs)
		for _, run := range runs {
			if run.Status != "completed" {
				continue
			}
			value, _ := json.Marshal(map[string]any{"passed": run.Conclusion == "success", "conclusion": run.Conclusion,
				"run": run.ID, "workflow": run.Workflow, "sha": run.SHA, "url": run.URL, "branch": it.Branch})
			_, err := c.Event(ctx, dir, it.ID, "ci.result", fmt.Sprintf("github-run:%d", run.ID), value)
			if errors.Is(err, api.ErrEventEnded) {
				break // its line ended meanwhile
			}
			if err != nil {
				return given, err
			}
			given++
		}
	}
	return given, nil
}

// ghJSON runs gh in dir with args, and reads what it prints as JSON into
// out. gh runs as the user would run it, with their own credentials (ADR
// 0047).
func ghJSON(ctx context.Context, dir string, out any, args ...string) error {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("gh %s: %w: %s", strings.Join(args[:2], " "), err, strings.TrimSpace(stderr.String()))
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("gh %s: %w", strings.Join(args[:2], " "), err)
	}
	return nil
}

// eventCommand gives an outside event to a work item (ADR 0047): a flow
// at one of its stations takes it with wait(name). Sent twice under one id
// it is taken once; sent before the wait, it is kept for it; sent to an
// item that ended, it is refused (exit 1).
func eventCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("event", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", ".", "a folder of the project's repository")
	data := fs.String("data", runtime.DefaultDataDir(), "data directory")
	item := fs.String("item", "", "the item: its name, its session, or its branch (kairo/<name>)")
	name := fs.String("name", "", "the event's name, as a flow waits for it: wait(\"<name>\")")
	id := fs.String("id", "", "the event's id: sent again under it, it is taken once (default: the hash of its name and value)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *item == "" || *name == "" || fs.NArg() > 1 {
		return badUse{error: errors.New(`usage: kairocode event [-dir DIR] -item <item> -name <name> [-id ID] '<JSON>'`)}
	}
	value := json.RawMessage("null")
	if fs.NArg() == 1 {
		value = json.RawMessage(fs.Arg(0))
		if !json.Valid(value) {
			return badUse{error: fmt.Errorf("the event's value is not JSON: %s", fs.Arg(0))}
		}
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	c, err := connect(ctx, *data)
	if err != nil {
		return err
	}
	defer c.Close()
	it, err := c.Event(ctx, abs, *item, *name, *id, value)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s given to the item %s (%s)\n", *name, it.Name, it.State)
	return nil
}
