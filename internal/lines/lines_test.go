package lines

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/r-hashi01/kairocode/internal/workflows"
)

func put(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, Dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A project's lines are read from .kairocode/lines/<name>.json (ADR 0043);
// a bad one is named with why, and what a line names is checked against
// what the project has.
func TestLinesRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if l, err := List(dir); err != nil || l != nil {
		t.Fatalf("no lines: %v %v", l, err)
	}
	put(t, dir, "ship.json", `{
  "description": "From a request to a branch ready to merge",
  "stations": [
    {"name": "plan", "agent": "plan", "prompt": "Write the plan."},
    {"name": "build", "agent": "build", "checks": ["test"], "rounds": 2},
    {"name": "review", "flow": "review"},
    {"name": "verify", "checks": ["test", "lint"]}
  ]
}`)
	l, err := Read(dir, "ship")
	if err != nil {
		t.Fatal(err)
	}
	if l.Name != "ship" || len(l.Stations) != 4 || l.Stations[1].Rounds != 2 || l.Stations[2].Flow != "review" || len(l.Stations[3].Checks) != 2 {
		t.Fatalf("line %+v", l)
	}
	if _, err := Read(dir, "none"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a line not there: %v", err)
	}

	for name, c := range map[string]struct{ body, why string }{
		"unknown":  {`{"stations": [{"name": "a", "agent": "build", "model": "x"}]}`, `unknown field "model"`},
		"empty":    {`{"stations": []}`, "no stations"},
		"two":      {`{"stations": [{"name": "a", "agent": "build", "flow": "f"}]}`, "not both"},
		"same":     {`{"stations": [{"name": "a", "agent": "build"}, {"name": "a", "agent": "plan"}]}`, "two stations have this name"},
		"badname":  {`{"stations": [{"name": "A b", "agent": "build"}]}`, "lowercase"},
		"prompt":   {`{"stations": [{"name": "a", "flow": "f", "prompt": "x"}]}`, "a prompt is for an agent"},
		"args":     {`{"stations": [{"name": "a", "agent": "build", "args": "x"}]}`, "args are for a flow"},
		"nothing":  {`{"stations": [{"name": "a"}]}`, "gate alone, and has no checks"},
		"rounds":   {`{"stations": [{"name": "a", "flow": "f", "checks": ["test"], "rounds": 2}]}`, "not sent back"},
		"negative": {`{"stations": [{"name": "a", "agent": "build", "rounds": -1}]}`, "not 0 or more"},
	} {
		put(t, dir, name+".json", c.body)
		if _, err := Read(dir, name); err == nil || !strings.Contains(err.Error(), c.why) || !strings.Contains(err.Error(), "line "+name) {
			t.Errorf("%s: %v, want %q", name, err, c.why)
		}
	}
	all, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	bad := 0
	for _, e := range all {
		if e.Err != nil {
			bad++
		}
	}
	if len(all) != 11 || all[len(all)-1].Name != "unknown" || bad != 10 {
		t.Fatalf("listed %d, %d bad", len(all), bad)
	}

	known := Known{
		Agent: func(n string) bool { return n == "plan" || n == "build" },
		Flow:  func(n string) bool { return n == "review" },
		Check: func(n string) bool { return n == "test" },
	}
	if err := known.Names(l); err == nil || !strings.Contains(err.Error(), `station "verify": no check named "lint"`) {
		t.Fatalf("an undeclared check: %v", err)
	}
	l.Stations[3].Checks = []string{"test"}
	if err := known.Names(l); err != nil {
		t.Fatal(err)
	}
	l.Stations[2].Flow = "gone"
	if err := known.Names(l); err == nil || !strings.Contains(err.Error(), `no flow named "gone"`) {
		t.Fatalf("an unknown flow: %v", err)
	}
}

// A line put together on the spot is kept as a file of the project, new,
// never over one there.
func TestLineWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l := workflows.Line{Stations: []workflows.Station{{Name: "build", Agent: "build", Checks: []string{"test"}}}}
	if err := Write(dir, "quick", l); err != nil {
		t.Fatal(err)
	}
	got, err := Read(dir, "quick")
	if err != nil || got.Name != "quick" || got.Stations[0].Checks[0] != "test" {
		t.Fatalf("read back %+v %v", got, err)
	}
	if err := Write(dir, "quick", l); err == nil || !strings.Contains(err.Error(), "there already") {
		t.Fatalf("written over: %v", err)
	}
	if err := Write(dir, "keep", l); err == nil {
		t.Fatal("a line named keep")
	}
}
