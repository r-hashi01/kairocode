package workflows

import (
	"fmt"
	"slices"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"
)

// Check is a check a project or the user declares in kairocode.json (ADR
// 0042): a command run in KairoCode's sandbox that passes on exit 0.
type Check struct {
	Name    string `json:"name"`
	Command string `json:"command"`
	// Timeout in seconds; 0: DefaultCheckTimeout.
	Timeout int `json:"timeout,omitempty"`
	// Protect are globs of paths (relative to the folder) that work the
	// check gates may not change.
	Protect []string `json:"protect,omitempty"`
	// Project: declared by the project, so asked about before it first
	// runs in a folder; Source, the file it is declared in.
	Project bool   `json:"project,omitempty"`
	Source  string `json:"source,omitempty"`
}

// Checks are the checks as a turn started (Agent.Checks): their
// definitions by name, the ones a plain turn must pass, and how often a
// gate sends the work back.
type Checks struct {
	Defs   []Check  `json:"defs,omitempty"`
	Turn   []string `json:"turn,omitempty"`
	Rounds int      `json:"rounds,omitempty"`
}

// def is the check named name, if declared.
func (c *Checks) def(name string) (Check, bool) {
	if c == nil {
		return Check{}, false
	}
	i := slices.IndexFunc(c.Defs, func(d Check) bool { return d.Name == name })
	if i < 0 {
		return Check{}, false
	}
	return c.Defs[i], true
}

// CheckVerdict is how a check of a gate last ended. Output is its output
// (the end of it) in kc_message, by hash (invariant 5).
type CheckVerdict struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Exit   int    `json:"exit,omitempty"`
	Output string `json:"output,omitempty"`
	// Note says why it did not pass, other than its exit: it was not run,
	// it ran too long, or files it protects were changed.
	Note string `json:"note,omitempty"`
}

// CheckRunInput runs a check. Round is the gate's round, so each round's
// run is a call of its own.
type CheckRunInput struct {
	Dir   string `json:"dir"`
	Check Check  `json:"check"`
	Round int    `json:"round"`
	// At: a flow's check(), where in the script (for the flow's chart).
	At string `json:"at,omitempty"`
}

// CheckRunOutput is a check's run: its exit (-1: it ran too long), its
// output in kc_message, and the end of it for the model.
type CheckRunOutput struct {
	Exit   int    `json:"exit"`
	Output string `json:"output"`
	Tail   string `json:"tail"`
}

// CheckProtectInput: the files of Dir that match Globs, hashed.
type CheckProtectInput struct {
	Dir   string   `json:"dir"`
	Globs []string `json:"globs"`
}

// CheckModesInput asks how each of a folder's checks may run: as the user
// said before, or to be asked (CheckAsk).
type CheckModesInput struct {
	Dir    string  `json:"dir"`
	Checks []Check `json:"checks"`
}

// CheckDecideInput keeps how the user said a check of a folder runs.
type CheckDecideInput struct {
	Dir   string `json:"dir"`
	Check Check  `json:"check"`
	Mode  string `json:"mode"`
}

// How a check may run in a folder: the user's own checks always run; a
// project's, as the user said.
const (
	CheckRun   = "run"
	CheckNever = "never"
	CheckAsk   = "ask"
)

const (
	// StopChecks: the turn ended with a check of its gate not passing.
	StopChecks = "checks"
	// DefaultCheckRounds is how often a gate sends work back when the
	// settings do not say.
	DefaultCheckRounds = 3
	// DefaultCheckTimeout is how long a check runs when it does not say.
	DefaultCheckTimeout = 600
)

var checkOptions = []string{"Run it in KairoCode's sandbox", "Do not run it"}

// gate is the checks work must pass before it is taken as done (ADR 0042),
// as a turn or a flow uses it.
type gate struct {
	dir     string
	key     string // names its questions, unique in the run
	checks  []Check
	rounds  int
	protect []string
	before  string // the protected files' hash as the work started
	spent   int    // rounds it sent the work back
	modes   []string
	at      string // a flow's check(): where in the script
}

// newGate is the gate of the named checks, or nil with none. A name not
// declared is an error: the work could not be checked as it asks. With
// guard and files to protect, they are hashed now, before the work.
func newGate(ctx *kairo.Context, dir, key string, checks *Checks, names []string, rounds int, guard bool) (*gate, error) {
	if len(names) == 0 {
		return nil, nil
	}
	g := &gate{dir: dir, key: key, rounds: rounds}
	if g.rounds <= 0 && checks != nil {
		g.rounds = checks.Rounds
	}
	if g.rounds <= 0 {
		g.rounds = DefaultCheckRounds
	}
	for _, name := range names {
		c, ok := checks.def(name)
		if !ok {
			return nil, fmt.Errorf("no check named %q in kairocode.json (\"checks\")", name)
		}
		if slices.ContainsFunc(g.checks, func(d Check) bool { return d.Name == name }) {
			continue
		}
		g.checks = append(g.checks, c)
		if !guard {
			continue
		}
		for _, p := range c.Protect {
			if !slices.Contains(g.protect, p) {
				g.protect = append(g.protect, p)
			}
		}
	}
	if len(g.protect) > 0 {
		h, err := kairo.Call[string](ctx, ActCheckProtect, CheckProtectInput{Dir: dir, Globs: g.protect})
		if err != nil {
			return nil, err
		}
		g.before = h
	}
	return g, nil
}

// decide settles, once, how each check runs: a project's not settled
// before is asked about, and the process may stop while it waits
// (invariant 7).
func (g *gate) decide(ctx *kairo.Context) error {
	if g.modes != nil {
		return nil
	}
	modes, err := kairo.Call[[]string](ctx, ActCheckModes, CheckModesInput{Dir: g.dir, Checks: g.checks})
	if err != nil {
		return err
	}
	if len(modes) != len(g.checks) {
		return fmt.Errorf("check.modes: %d modes for %d checks", len(modes), len(g.checks))
	}
	for i, c := range g.checks {
		if modes[i] != CheckAsk {
			continue
		}
		id := fmt.Sprintf("check_%s_%d", g.key, i)
		q := FlowAskInput{ID: id, Options: checkOptions, Question: fmt.Sprintf(
			"The project's check %q (%s) decides whether work is done:\n\n    %s\n\nIn KairoCode's sandbox it may write in the project only, with no network but this machine's.", c.Name, c.Source, c.Command)}
		if _, err := kairo.Call[bool](ctx, ActFlowAsk, q); err != nil {
			return err
		}
		a, err := kairo.WaitFor[FlowAnswer](ctx, SignalAnswerPrefix+id)
		if err != nil {
			return err
		}
		modes[i] = CheckNever
		if a.Option == checkOptions[0] {
			modes[i] = CheckRun
		}
		if _, err := kairo.Call[bool](ctx, ActCheckDecide, CheckDecideInput{Dir: g.dir, Check: c, Mode: modes[i]}); err != nil {
			return err
		}
	}
	g.modes = modes
	return nil
}

// gateResult is a run of a gate: each check's verdict, whether all passed,
// and, when not, what to tell the agent, and whether it may try again.
type gateResult struct {
	verdicts []CheckVerdict
	passed   bool
	note     string
	again    bool
}

// run runs the gate's checks at once, and sees whether the protected files
// changed since the work started.
func (g *gate) run(ctx *kairo.Context) (gateResult, error) {
	if err := g.decide(ctx); err != nil {
		return gateResult{}, err
	}
	res := gateResult{verdicts: make([]CheckVerdict, len(g.checks)), passed: true}
	outs := make([]CheckRunOutput, len(g.checks))
	var fns []func(*kairo.Context) error
	for i, c := range g.checks {
		res.verdicts[i] = CheckVerdict{Name: c.Name}
		if g.modes[i] == CheckNever {
			continue
		}
		fns = append(fns, func(ctx *kairo.Context) error {
			out, err := kairo.Call[CheckRunOutput](ctx, ActCheckRun, CheckRunInput{Dir: g.dir, Check: c, Round: g.spent, At: g.at})
			outs[i] = out
			return err
		})
	}
	if len(fns) > 0 {
		if err := kairo.Parallel(ctx, fns...); err != nil {
			return gateResult{}, err
		}
	}
	changed := false
	if len(g.protect) > 0 {
		h, err := kairo.Call[string](ctx, ActCheckProtect, CheckProtectInput{Dir: g.dir, Globs: g.protect})
		if err != nil {
			return gateResult{}, err
		}
		changed = h != g.before
	}
	var failed []string
	declined := false
	for i, c := range g.checks {
		v := &res.verdicts[i]
		switch {
		case g.modes[i] == CheckNever:
			v.Note = "not run: the user said not to run it in this folder"
			declined = true
		default:
			v.Exit, v.Output = outs[i].Exit, outs[i].Output
			v.Passed = outs[i].Exit == 0
			if outs[i].Exit < 0 {
				v.Note = fmt.Sprintf("it ran longer than %ds", timeoutOf(c))
			}
		}
		if v.Passed && changed && len(c.Protect) > 0 {
			v.Passed = false
			v.Note = "files it protects were changed: " + strings.Join(c.Protect, ", ")
		}
		if v.Passed {
			continue
		}
		res.passed = false
		text := fmt.Sprintf("- %s (%s)", c.Name, c.Command)
		switch {
		case v.Note != "":
			text += ": " + v.Note
		default:
			text += fmt.Sprintf(": exit %d", v.Exit)
		}
		if t := strings.TrimSpace(outs[i].Tail); t != "" && g.modes[i] != CheckNever {
			text += "\n" + t
		}
		failed = append(failed, text)
	}
	if res.passed {
		return res, nil
	}
	res.again = !declined && g.spent < g.rounds
	res.note = "The checks that decide whether your work is done did not pass:\n" + strings.Join(failed, "\n\n")
	if len(g.protect) > 0 {
		res.note += "\n\nDo not change the files the checks protect (" + strings.Join(g.protect, ", ") + "); put back any you changed."
	}
	res.note += "\nPut it right, then finish."
	return res, nil
}

func timeoutOf(c Check) int {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultCheckTimeout
}

// gateNames are the checks a turn's gate runs: a flow's agent's, as the
// flow named them; a plain turn's, as the settings name them.
func gateNames(in TurnInput, agent Agent) []string {
	if in.Dry {
		return nil // a try (ADR 0017): its agents write nothing to check
	}
	if in.Sub {
		return in.Checks
	}
	if agent.Checks == nil || in.Flow != nil || in.Compact {
		return nil
	}
	return agent.Checks.Turn
}

// protected are the globs the gate protects (none without a gate).
func (g *gate) protected() []string {
	if g == nil {
		return nil
	}
	return g.protect
}

// passGate runs the gate on the work out holds. All passing, it returns
// "" and the work is done. Not passing with rounds left, it returns the
// note to send the agent back with; else the work stops (StopChecks),
// its verdicts in out.
func passGate(ctx *kairo.Context, g *gate, out *TurnOutput) (string, error) {
	res, err := g.run(ctx)
	if err != nil {
		return "", err
	}
	out.Checks, out.CheckRounds = res.verdicts, g.spent
	if res.passed {
		return "", nil
	}
	if !res.again {
		out.Stopped = StopChecks
		return "", nil
	}
	g.spent++
	return res.note, nil
}

// notDone ends the reply of work its gate stopped with what did not pass,
// so the person and the next turn's model are told it is not done.
func notDone(ctx *kairo.Context, out *TurnOutput) error {
	var failed []string
	for _, v := range out.Checks {
		if !v.Passed {
			failed = append(failed, v.Name)
		}
	}
	text, err := kairo.Call[string](ctx, ActFlowText, out.Reply)
	if err != nil {
		return err
	}
	text = strings.TrimSpace(text + "\n\n[Not done: the checks that decide it did not pass: " + strings.Join(failed, ", ") + ".]")
	out.Reply, err = kairo.Call[string](ctx, ActFlowPut, FlowPutInput{Role: "assistant", Text: text})
	return err
}
