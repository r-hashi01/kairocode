// Package lines reads and writes a project's lines (ADR 0043):
// <dir>/.kairocode/lines/<name>.json, each the row of stations a work item
// goes down. Everything is read within dir (os.Root): a link out of the
// project is not followed. What a line names (agents, flows, checks) is
// known to the runtime, which checks it with Known.
package lines

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/r-hashi01/kairocode/internal/workflows"
)

// Dir is where a project's lines are, relative to it.
const Dir = ".kairocode/lines"

var name = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// ValidName says whether n can name a line, or a station: lowercase
// letters, digits and dashes, beginning with a letter or digit.
func ValidName(n string) bool { return name.MatchString(n) && n != "keep" }

// Path is line name's file, relative to the project.
func Path(n string) string { return path.Join(Dir, n+".json") }

// Parse reads a line's file, as name, and checks its shape: what it names
// is checked by Known.Names.
func Parse(n string, data []byte) (workflows.Line, error) {
	var l workflows.Line
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		return workflows.Line{}, fmt.Errorf("line %s: %w", n, err)
	}
	l.Name = n
	if err := Shape(l); err != nil {
		return workflows.Line{}, err
	}
	return l, nil
}

// Shape checks a line's stations, whoever wrote it (a file, or the model
// putting one together on the spot): each named, once; one machine at
// most; what a machine takes only with it; a station with no machine has
// a gate.
func Shape(l workflows.Line) error {
	what := "the line"
	if l.Name != "" {
		what = "line " + l.Name
	}
	if len(l.Stations) == 0 {
		return fmt.Errorf("%s has no stations", what)
	}
	seen := map[string]bool{}
	for i, s := range l.Stations {
		at := fmt.Sprintf("%s, station %d", what, i+1)
		if s.Name != "" {
			at = fmt.Sprintf("%s, station %q", what, s.Name)
		}
		switch {
		case !ValidName(s.Name):
			return fmt.Errorf("%s: a station's name is lowercase letters, digits and dashes", at)
		case seen[s.Name]:
			return fmt.Errorf("%s: two stations have this name", at)
		case s.Agent != "" && s.Flow != "":
			return fmt.Errorf("%s: a station has one machine, an agent or a flow, not both", at)
		case s.Prompt != "" && s.Agent == "":
			return fmt.Errorf("%s: a prompt is for an agent", at)
		case s.Args != "" && s.Flow == "":
			return fmt.Errorf("%s: args are for a flow", at)
		case s.Agent == "" && s.Flow == "" && len(s.Checks) == 0:
			return fmt.Errorf("%s: a station with no agent or flow is its gate alone, and has no checks", at)
		case s.Rounds < 0:
			return fmt.Errorf("%s: rounds is %d, not 0 or more", at, s.Rounds)
		case s.Rounds > 0 && s.Agent == "":
			return fmt.Errorf("%s: rounds send an agent back; a flow or a gate is not sent back", at)
		}
		seen[s.Name] = true
	}
	return nil
}

// Known is what a line may name, as the project has it.
type Known struct {
	Agent func(string) bool
	Flow  func(string) bool
	Check func(string) bool
}

// Names checks what a line names: its agents, flows and checks are there.
func (k Known) Names(l workflows.Line) error {
	what := "the line"
	if l.Name != "" {
		what = "line " + l.Name
	}
	for _, s := range l.Stations {
		at := fmt.Sprintf("%s, station %q", what, s.Name)
		if s.Agent != "" && k.Agent != nil && !k.Agent(s.Agent) {
			return fmt.Errorf("%s: no agent named %q", at, s.Agent)
		}
		if s.Flow != "" && k.Flow != nil && !k.Flow(s.Flow) {
			return fmt.Errorf("%s: no flow named %q in .kairocode/workflows", at, s.Flow)
		}
		for _, c := range s.Checks {
			if k.Check != nil && !k.Check(c) {
				return fmt.Errorf("%s: no check named %q in kairocode.json (\"checks\")", at, c)
			}
		}
	}
	return nil
}

// Read is line name of the project in dir. One missing is
// fs.ErrNotExist.
func Read(dir, n string) (workflows.Line, error) {
	if !ValidName(n) {
		return workflows.Line{}, fmt.Errorf("%q is not a line's name", n)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return workflows.Line{}, err
	}
	defer root.Close()
	b, err := root.ReadFile(Path(n))
	if err != nil {
		return workflows.Line{}, err
	}
	return Parse(n, b)
}

// Entry is a line of the project, or why it could not be read.
type Entry struct {
	Name string
	Line workflows.Line
	Err  error
}

// List is the project's lines in dir, by name, a bad one with why; none if
// there is no folder for them.
func List(dir string) ([]Entry, error) {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, e := range entries {
		n, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() || !ValidName(n) {
			continue
		}
		b, err := root.ReadFile(path.Join(Dir, e.Name()))
		if err != nil {
			out = append(out, Entry{Name: n, Err: err})
			continue
		}
		l, err := Parse(n, b)
		out = append(out, Entry{Name: n, Line: l, Err: err})
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Write writes l as line name of the project in dir (/line keep): a new
// file, whole or not at all; one there already is not replaced.
func Write(dir, n string, l workflows.Line) error {
	if !ValidName(n) {
		return fmt.Errorf("%q is not a line's name", n)
	}
	l.Name = ""
	if err := Shape(l); err != nil {
		return err
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll(Dir, 0o755); err != nil {
		return err
	}
	p := Path(n)
	if _, err := root.Stat(p); err == nil {
		return fmt.Errorf("line %s is there already", n)
	}
	tmp := p + ".new"
	if err := root.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	if err := root.Rename(tmp, p); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return nil
}
