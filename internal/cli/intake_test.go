package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/sandbox/sandboxtest"
)

// intakeRepo is a git repository with the standard lines quick (one
// agent) and breaks (a flow that never passes), and settings that let a
// line stop after one item that did not pass.
func intakeRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	ws := filepath.Join(t.TempDir(), "ws")
	for name, body := range map[string]string{
		"kairocode.json":                    `{"permission": {"write": "allow", "line": "allow"}, "lines": {"stopAfter": 1}}`,
		".kairocode/lines/quick.json":       `{"description": "one agent", "stations": [{"name": "build", "agent": "build"}]}`,
		".kairocode/lines/breaks.json":      `{"stations": [{"name": "try", "flow": "fails"}]}`,
		".kairocode/workflows/fails.star":   "def main(input):\n    return {\"ok\": False, \"message\": \"it never passes\"}\n",
		".kairocode/lines/ci.json":          `{"stations": [{"name": "ci", "flow": "ci_wait"}]}`,
		".kairocode/workflows/ci_wait.star": "def main(input):\n    ci = wait(\"ci.result\", timeout = \"1h\")\n    return {\"ok\": ci.ok and ci.value[\"passed\"], \"message\": \"CI said \" + ci.output}\n",
	} {
		p := filepath.Join(ws, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"},
		{"-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "-m", "first"}} {
		if out, err := exec.Command("git", append([]string{"-C", ws}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return ws
}

// fakeGH puts a gh on PATH that answers repo view and issue list (only
// for the label kairo) as GitHub would, and logs how it was called.
func fakeGH(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "calls")
	script := `#!/bin/sh
echo "$*" >> '` + log + `'
case "$1 $2" in
"repo view") echo '{"nameWithOwner": "o/r"}' ;;
"issue list")
  case "$*" in
  *"--label kairo"*) echo '[{"number": 1, "title": "Fix the login", "body": "It breaks.", "url": "https://github.com/o/r/issues/1"},
    {"number": 2, "title": "Add notes", "body": "", "url": "https://github.com/o/r/issues/2"}]' ;;
  *) echo '[]' ;;
  esac ;;
"run list") echo '[{"databaseId": 7, "status": "completed", "conclusion": "success", "headSha": "abc", "workflowName": "check", "url": "https://github.com/o/r/actions/runs/7"},
    {"databaseId": 8, "status": "in_progress", "conclusion": "", "headSha": "abd", "workflowName": "check", "url": "https://github.com/o/r/actions/runs/8"}]' ;;
*) echo "unexpected: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// kairocode intake github takes in the labelled open issues, each as the
// work github:o/r#<n>; a second pass takes in nothing new (ADR 0047).
func TestIntakeGitHub(t *testing.T) {
	sandboxtest.Require(t)
	ws := intakeRepo(t)
	calls := fakeGH(t)
	t.Setenv("KAIROCODE_PROVIDER", "fake")
	t.Setenv("KAIROCODE_FAKE", fakeScript(t, t.TempDir(), provider.FakeReply{Text: "ok"}))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	data := dataDir(t)
	stopAtEnd(t, data)
	pass := func() []api.Intake {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := Main(ctx, []string{"intake", "github", "-dir", ws, "-data", data, "-line", "quick", "-json"}, &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		var out []api.Intake
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			t.Fatalf("%v: %s", err, stdout.String())
		}
		return out
	}
	first := pass()
	if len(first) != 2 || !first[0].New || !first[1].New || first[0].Item.ID == first[1].Item.ID {
		t.Fatalf("first pass %+v", first)
	}
	second := pass()
	if len(second) != 2 || second[0].New || second[1].New || second[0].Item.ID != first[0].Item.ID {
		t.Fatalf("second pass %+v", second)
	}
	if b, _ := os.ReadFile(calls); !strings.Contains(string(b), "--label kairo") {
		t.Fatalf("gh was called: %s", b)
	}
	// The same work by its key, from kairocode intake: there already.
	var stdout, stderr bytes.Buffer
	if code := Main(ctx, []string{"intake", "-dir", ws, "-data", data, "-line", "quick", "-key", "github:o/r#1", "fix it"}, &stdout, &stderr); code != 0 ||
		!strings.HasPrefix(stdout.String(), "there already: item "+first[0].Item.Name) {
		t.Fatalf("exit %d: %s %s", code, stdout.String(), stderr.String())
	}
}

// The intake's exit codes: 2 a wrong use, 6 a line that stopped itself;
// an event to an item whose line ended is refused (1).
func TestIntakeExitCodes(t *testing.T) {
	sandboxtest.Require(t)
	ws := intakeRepo(t)
	t.Setenv("KAIROCODE_PROVIDER", "fake")
	t.Setenv("KAIROCODE_FAKE", fakeScript(t, t.TempDir(), provider.FakeReply{Text: "ok"}))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	data := dataDir(t)
	stopAtEnd(t, data)
	run := func(args ...string) (int, string) {
		var out bytes.Buffer
		code := Main(ctx, append([]string{args[0], "-dir", ws, "-data", data}, args[1:]...), &out, &out)
		return code, out.String()
	}
	if code, out := run("intake", "-line", "quick", "no key given"); code != 2 {
		t.Fatalf("no key: exit %d: %s", code, out)
	}
	code, out := run("intake", "-line", "breaks", "-key", "cron:a", "-json", "try it")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var in api.Intake
	if err := json.Unmarshal([]byte(out), &in); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for { // its line ends: stopped at its station, the line with it
		var items bytes.Buffer
		Main(ctx, []string{"items", "-data", data, "-json", in.Item.Of}, &items, &items)
		if strings.Contains(items.String(), `"state": "stopped"`) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("items: %s", items.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
	if code, out := run("intake", "-line", "breaks", "-key", "cron:b", "try again"); code != 6 || !strings.Contains(out, "stopped itself") {
		t.Fatalf("a stopped line: exit %d: %s", code, out)
	}
	if code, out := run("event", "-item", in.Item.Name, "-name", "ci.result", `{"ok": true}`); code != 1 || !strings.Contains(out, "no wait will take the event") {
		t.Fatalf("an event to an item that ended: exit %d: %s", code, out)
	}
	if code, out := run("event", "-item", in.Item.Name, "-name", "ci.result", `{not json`); code != 2 {
		t.Fatalf("an event that is not JSON: exit %d: %s", code, out)
	}
}

// A station that waits for CI: the item taken in waits at it, without a
// process, until kairocode event gives it ci.result by the item's branch;
// the line then goes on (ADR 0047).
func TestItemWaitsForCI(t *testing.T) {
	sandboxtest.Require(t)
	ws := intakeRepo(t)
	t.Setenv("KAIROCODE_PROVIDER", "fake")
	t.Setenv("KAIROCODE_FAKE", fakeScript(t, t.TempDir(), provider.FakeReply{Text: "ok"}))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	data := dataDir(t)
	stopAtEnd(t, data)
	var out bytes.Buffer
	if code := Main(ctx, []string{"intake", "-dir", ws, "-data", data, "-line", "ci", "-key", "github:o/r#9", "-json", "ship it"}, &out, &out); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	var in api.Intake
	if err := json.Unmarshal(out.Bytes(), &in); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	state := func() string {
		var items bytes.Buffer
		Main(ctx, []string{"items", "-data", data, "-json", in.Item.Of}, &items, &items)
		var list []api.Item
		json.Unmarshal(items.Bytes(), &list)
		if len(list) != 1 {
			return items.String()
		}
		return list[0].State
	}
	for _, id := range []string{"run-7", "run-7"} { // sent twice under one id: taken once
		out.Reset()
		if code := Main(ctx, []string{"event", "-dir", ws, "-data", data, "-item", "kairo/" + in.Item.Name, "-name", "ci.result", "-id", id,
			`{"passed": true, "run": 7}`}, &out, &out); code != 0 && state() == api.ItemRunning {
			t.Fatalf("exit %d: %s", code, out.String())
		}
	}
	for state() != api.ItemDone {
		select {
		case <-ctx.Done():
			t.Fatalf("the item: %s", state())
		case <-time.After(50 * time.Millisecond):
		}
	}
	var log bytes.Buffer
	Main(ctx, []string{"log", "-data", data, in.Item.ID}, &log, &log)
	// One wait, given the event once.
	if n := strings.Count(log.String(), "wait event/ci.result"); n != 1 || !strings.Contains(log.String(), `{"passed":true,"run":7}`) {
		t.Fatalf("%d waits for ci.result in the item's record:\n%s", n, log.String())
	}
}

// kairocode intake github -ci gives each item in progress the workflow
// runs finished on its branch, as ci.result under the run's id: a station
// waiting for CI takes the finished run, once, however many passes give it
// (ADR 0047).
func TestIntakeGitHubCI(t *testing.T) {
	sandboxtest.Require(t)
	ws := intakeRepo(t)
	calls := fakeGH(t)
	t.Setenv("KAIROCODE_PROVIDER", "fake")
	t.Setenv("KAIROCODE_FAKE", fakeScript(t, t.TempDir(), provider.FakeReply{Text: "ok"}))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	data := dataDir(t)
	stopAtEnd(t, data)
	var out bytes.Buffer
	if code := Main(ctx, []string{"intake", "-dir", ws, "-data", data, "-line", "ci", "-key", "cron:release", "-json", "release it"}, &out, &out); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	var in api.Intake
	if err := json.Unmarshal(out.Bytes(), &in); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	for range 2 { // a schedule's passes: no labelled issue, the CI's runs
		out.Reset()
		if code := Main(ctx, []string{"intake", "github", "-dir", ws, "-data", data, "-line", "quick", "-label", "none", "-ci"}, &out, &out); code != 0 {
			t.Fatalf("exit %d: %s", code, out.String())
		}
	}
	for {
		var items bytes.Buffer
		Main(ctx, []string{"items", "-data", data, "-json", in.Item.Of}, &items, &items)
		if strings.Contains(items.String(), `"state": "done"`) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("items: %s", items.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
	var log bytes.Buffer
	Main(ctx, []string{"log", "-data", data, in.Item.ID}, &log, &log)
	if n := strings.Count(log.String(), "wait event/ci.result"); n != 1 || !strings.Contains(log.String(), `"run":7`) {
		t.Fatalf("%d waits for ci.result in the item's record:\n%s", n, log.String())
	}
	if b, _ := os.ReadFile(calls); !strings.Contains(string(b), "run list --branch kairo/"+in.Item.Name) {
		t.Fatalf("gh was called: %s", b)
	}
}

// stopAtEnd stops data's daemon as the test ends, letting the steps under
// way finish (a snapshot's git writing in the test's folder), before the
// folder is removed.
func stopAtEnd(t *testing.T, data string) {
	t.Cleanup(func() {
		var out bytes.Buffer
		Main(context.Background(), []string{"daemon", "stop", "-data", data}, &out, &out)
	})
}
