package workflows

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/msg"
)

// runLineTurn is a work item's first turn (ADR 0043): it runs the item's
// line, its stations in order, each a recorded child (invariant 8: told
// apart by where on the line it is, and by its round when it runs again),
// so a resumed item takes up the station it was at and runs no finished
// one again. The line stops at the first station that does not pass. What
// each came to is the turn's output, and a summary its reply.
//
// With reviews (turn version 7, ADR 0044): review stations, which send the
// item back to an earlier station, and the review by a person a change
// touching lines.people gets at the line's end. Without, the calls of
// version 6.
func runLineTurn(ctx *kairo.Context, in TurnInput, reviews bool) (TurnOutput, error) {
	line := in.Line
	request, err := kairo.Call[string](ctx, ActFlowText, in.Message)
	if err != nil {
		return TurnOutput{}, err
	}
	var policy LinePolicy
	if reviews {
		if policy, err = kairo.Call[LinePolicy](ctx, ActLinePolicy, LinePolicyInput{Dir: in.Dir}); err != nil {
			return TurnOutput{}, err
		}
	}
	stations := slices.Clone(line.Stations)
	var out TurnOutput
	var checks *Checks // the project's, read once a gate needs them
	loadChecks := func() (*Checks, error) {
		if checks == nil {
			a, err := kairo.Call[Agent](ctx, ActAgentLoad, AgentLoadInput{Dir: in.Dir, Agent: in.Agent, Engine: in.Engine, Model: in.Model})
			if err != nil {
				return nil, err
			}
			checks = &Checks{}
			if a.Checks != nil {
				checks = a.Checks
			}
		}
		return checks, nil
	}
	outputs := make([]string, len(stations)) // what each station left, its last run
	notes := map[int]string{}                // the words a station sent back to is given
	runs := map[int]int{}                    // how often each station ran
	sentBack := map[int]int{}                // how often each review sent back
	personSaw := ""                          // the change a person last took, by its hash
	peopleChecked := false
	for i := 0; ; {
		if i == len(stations) {
			// A change touching a path that always needs a person gets
			// one, whatever the line says (ADR 0044, 2): unless a person
			// took the change as it is now, by its hash (any station may
			// have changed it since a person's yes).
			if !reviews || peopleChecked || len(policy.People) == 0 {
				break
			}
			peopleChecked = true
			d, err := kairo.Call[LineDiff](ctx, ActLineDiff, LineDiffInput{Session: in.Session, Dir: in.Dir, At: "people"})
			if err != nil {
				return TurnOutput{}, err
			}
			if touches(d.Paths, policy.People) == "" || d.Sum == personSaw {
				break
			}
			stations = append(stations, Station{Name: PeopleStation, Review: ReviewPerson, Back: lastAgent(line.Stations)})
			outputs = append(outputs, "")
			continue
		}
		st := stations[i]
		at := fmt.Sprintf("station %d %s", i, st.Name)
		if n := runs[i]; n > 0 {
			at += fmt.Sprintf(" round %d", n)
		}
		runs[i]++
		res := StationResult{Name: st.Name, Status: StationPassed}
		var output string
		switch {
		case st.Review != "":
			// Its checks first: no one reviews work that does not pass.
			if len(st.Checks) > 0 {
				c, err := loadChecks()
				if err != nil {
					return TurnOutput{}, err
				}
				if err := gateStation(ctx, in.Dir, i, at, c, st.Checks, &res); err != nil {
					return TurnOutput{}, err
				}
			}
			if res.Status != StationPassed {
				break
			}
			v, err := reviewStation(ctx, in, line.Name, st, at, policy, outputs[:i])
			if err != nil {
				return TurnOutput{}, err
			}
			res.Steps, res.Usage, output = v.steps, v.usage, v.text
			if v.pass {
				if v.byPerson {
					personSaw = v.diff
				}
				break
			}
			limit := st.Rounds
			if limit == 0 {
				limit = 1
			}
			if b := stationIndex(stations, st.Back); b >= 0 && sentBack[i] < limit {
				sentBack[i]++
				notes[b] = v.words
				res.Status, res.Why = StationSentBack, v.words
				out.Line = append(out.Line, res)
				out.Steps += res.Steps
				out.Usage.Add(res.Usage)
				outputs[i] = output
				i = b
				continue
			}
			res.Status, res.Why = StationStopped, "sent back, with no round left: "+v.words
		case st.Agent != "":
			text := stationPrompt(line, i, request, outputs[:i], notes[i])
			delete(notes, i)
			h, err := kairo.Call[string](ctx, ActFlowPut, FlowPutInput{Role: string(msg.User), Text: text})
			if err != nil {
				return TurnOutput{}, err
			}
			to, err := kairo.Child[TurnOutput](ctx, Turn, TurnInput{
				Session: in.Session, N: in.N, Dir: in.Dir, Agent: st.Agent, Engine: in.Engine, Model: in.Model,
				Message: h, Sub: true, At: at, Checks: st.Checks, Rounds: st.Rounds,
			})
			var failed *kairo.WorkflowError
			switch {
			case errors.As(err, &failed):
				res.Status, res.Why = StationFailed, failed.Message
			case err != nil:
				return TurnOutput{}, err
			default:
				res.Steps, res.Usage, res.Checks = to.Steps, to.Usage, to.Checks
				if output, err = kairo.Call[string](ctx, ActFlowText, to.Reply); err != nil {
					return TurnOutput{}, err
				}
				if to.Stopped != "" {
					res.Status, res.Why = StationStopped, to.Stopped
				}
			}
		case st.Flow != "":
			args := st.Args
			if args == "" {
				args = request
			}
			fo, err := kairo.Child[FlowOutput](ctx, Flow, FlowInput{
				Session: in.Session, N: in.N, Dir: in.Dir, Agent: in.Agent, Engine: in.Engine, Model: in.Model,
				Call: FlowCall{Name: st.Flow, Args: args}, At: at,
			})
			var failed *kairo.WorkflowError
			switch {
			case errors.As(err, &failed):
				res.Status, res.Why = StationFailed, failed.Message
			case err != nil:
				return TurnOutput{}, err
			default:
				res.Steps, res.Usage, output = fo.Agents, fo.Usage, fo.Message
				if !fo.OK {
					res.Status, res.Why = StationFailed, fo.Message
				}
			}
		}
		// The gate of a flow, or of a station that is its gate alone: run
		// once, sending nothing back (an agent's ran in its turn; a
		// review's, before it).
		if st.Review == "" && st.Agent == "" && len(st.Checks) > 0 && res.Status == StationPassed {
			c, err := loadChecks()
			if err != nil {
				return TurnOutput{}, err
			}
			if err := gateStation(ctx, in.Dir, i, at, c, st.Checks, &res); err != nil {
				return TurnOutput{}, err
			}
		}
		out.Line = append(out.Line, res)
		out.Steps += res.Steps
		out.Usage.Add(res.Usage)
		outputs[i] = output
		if res.Status != StationPassed {
			out.Stopped = StopStation
			break
		}
		i++
	}
	summary := lineSummary(line, stations, out.Line, outputs, out.Stopped == "")
	if out.Stopped == "" {
		// Every station passed: the change goes on the item's branch,
		// ready to merge.
		name := line.Name
		if name == "" {
			name = "line"
		}
		c, err := kairo.Call[LineCommitOutput](ctx, ActLineCommit, LineCommitInput{Session: in.Session, Dir: in.Dir,
			Message: name + ": " + clipLine(firstLineOf(request), 64)})
		if err != nil {
			return TurnOutput{}, err
		}
		if c.Note != "" {
			summary += "\n" + c.Note + "\n"
		}
	}
	reply, err := kairo.Call[string](ctx, ActFlowPut, FlowPutInput{Role: string(msg.Assistant), Text: summary})
	if err != nil {
		return TurnOutput{}, err
	}
	head, err := kairo.Call[string](ctx, ActHistoryExtend, HistoryExtendInput{Prev: in.History, Messages: []string{in.Message, reply}})
	if err != nil {
		return TurnOutput{}, err
	}
	out.Head, out.Reply = head, reply
	return out, nil
}

// gateStation runs station i's checks once, as its gate: res says whether
// they passed. Its first run keeps the key of version 6.
func gateStation(ctx *kairo.Context, dir string, i int, at string, checks *Checks, names []string, res *StationResult) error {
	key := fmt.Sprintf("line%d", i)
	if r := strings.Index(at, " round "); r >= 0 {
		key += "r" + strings.TrimPrefix(at[r:], " round ")
	}
	g, err := newGate(ctx, dir, key, checks, names, 0, false)
	if err != nil {
		res.Status, res.Why = StationFailed, err.Error()
		return nil
	}
	g.at = at
	gr, err := g.run(ctx)
	if err != nil {
		return err
	}
	res.Checks = gr.verdicts
	if !gr.passed {
		res.Status, res.Why = StationStopped, StopChecks
	}
	return nil
}

// verdict is what a review came to: passed, or sent back with words; who
// reviewed; and what it cost.
type verdict struct {
	pass, byPerson bool
	words, text    string
	diff           string // the change reviewed, by its digest
	steps          int
	usage          msg.Usage
}

// reviewStation reviews the item's change so far (ADR 0044): by the model,
// by a person, or by risk (the model, then a person where the change's
// paths or size call for one).
func reviewStation(ctx *kairo.Context, in TurnInput, line string, st Station, at string, policy LinePolicy, outputs []string) (verdict, error) {
	d, err := kairo.Call[LineDiff](ctx, ActLineDiff, LineDiffInput{Session: in.Session, Dir: in.Dir, At: at})
	if err != nil {
		return verdict{}, err
	}
	v := verdict{diff: d.Sum}
	person, why := st.Review == ReviewPerson, ""
	if st.Name == PeopleStation {
		why = "it touches " + touches(d.Paths, policy.People) + ", a path that always needs a person (lines.people)"
	}
	if st.Review == ReviewModel || st.Review == ReviewRisk {
		diff, err := kairo.Call[string](ctx, ActFlowText, d.Diff)
		if err != nil {
			return verdict{}, err
		}
		agent := st.Agent
		if agent == "" {
			agent = "plan"
		}
		h, err := kairo.Call[string](ctx, ActFlowPut, FlowPutInput{Role: string(msg.User), Text: reviewPrompt(st, diff, outputs)})
		if err != nil {
			return verdict{}, err
		}
		to, err := kairo.Child[TurnOutput](ctx, Turn, TurnInput{
			Session: in.Session, N: in.N, Dir: in.Dir, Agent: agent, Engine: in.Engine, Model: in.Model,
			Message: h, Sub: true, At: at + " review",
		})
		var failed *kairo.WorkflowError
		if errors.As(err, &failed) {
			return verdict{words: "the reviewer failed: " + failed.Message}, nil
		} else if err != nil {
			return verdict{}, err
		}
		v.steps, v.usage = to.Steps, to.Usage
		text, err := kairo.Call[string](ctx, ActFlowText, to.Reply)
		if err != nil {
			return verdict{}, err
		}
		q, err := kairo.Call[string](ctx, ActFlowPut, FlowPutInput{Role: string(msg.User),
			Text: "Read this review of a change. Does the reviewer pass the change, or send it back?\n\n<review>\n" + text + "\n</review>"})
		if err != nil {
			return verdict{}, err
		}
		dec, err := kairo.Call[DecideOutput](ctx, ActFlowDecide, DecideInput{At: at + " verdict", Dir: in.Dir, Engine: in.Engine, Model: in.Model,
			Prompt: q, Type: DecideChoice, Choices: []string{"pass", "send back"}})
		if err != nil {
			return verdict{}, err
		}
		v.usage.Add(dec.Usage)
		v.text = "The reviewer (" + agent + "): " + text
		if !dec.OK || string(dec.Value) != `"pass"` {
			v.words = text
			return v, nil // not passed by the model: sent back
		}
		if st.Review == ReviewRisk {
			// A person after the model where the change calls for one.
			if p := touches(d.Paths, policy.People); p != "" {
				person, why = true, "it touches "+p+", a path that always needs a person (lines.people)"
			} else if d.Lines > policy.ReviewLines {
				person, why = true, fmt.Sprintf("it changes %d lines, more than lines.reviewLines (%d)", d.Lines, policy.ReviewLines)
			}
		}
		if !person {
			v.pass = true
			return v, nil
		}
	}
	// A person's review: an approval of kind review, which the process
	// waits for released (invariant 7).
	call := "line_review_" + strings.ReplaceAll(at, " ", "_")
	if _, err := kairo.Call[PermissionOutput](ctx, ActLineReview, LineReviewInput{Session: in.Session, Call: call, Station: st.Name, Line: line,
		Diff: d, Outputs: clipAll(outputs, 2000), Why: why}); err != nil {
		return verdict{}, err
	}
	a, err := kairo.WaitFor[Approval](ctx, SignalApprovePrefix+call)
	if err != nil {
		return verdict{}, err
	}
	v.byPerson = true
	if a.OK {
		v.pass = true
		v.text = strings.TrimSpace(v.text + "\nA person took the change.")
		return v, nil
	}
	v.words = a.Reason
	if v.words == "" {
		v.words = "a person sent it back"
	}
	v.text = strings.TrimSpace(v.text + "\nA person sent it back: " + v.words)
	return v, nil
}

// reviewPrompt is what a reviewer agent is given.
func reviewPrompt(st Station, diff string, outputs []string) string {
	var b strings.Builder
	b.WriteString("You review a change at the station " + strconv.Quote(st.Name) + " of a line. Read it, and say whether it passes, or what must change before it does.\n\n<diff>\n" + diff + "\n</diff>\n")
	for j, o := range outputs {
		if strings.TrimSpace(o) != "" {
			fmt.Fprintf(&b, "\nWhat station %d left:\n<station>\n%s\n</station>\n", j+1, clipLine(o, 4000))
		}
	}
	if p := strings.TrimSpace(st.Prompt); p != "" {
		b.WriteString("\nWhat to look for: " + p + "\n")
	}
	return b.String()
}

// touches is the first of globs one of paths matches ("" none).
func touches(paths, globs []string) string {
	for _, g := range globs {
		for _, p := range paths {
			if globMatch(g, p) {
				return g
			}
		}
	}
	return ""
}

// globMatch matches as the permission rules do: * any run of characters
// (/ among them), ? one.
func globMatch(glob, s string) bool {
	for len(glob) > 0 {
		switch glob[0] {
		case '*':
			for len(glob) > 0 && glob[0] == '*' {
				glob = glob[1:]
			}
			if glob == "" {
				return true
			}
			for k := 0; k <= len(s); k++ {
				if globMatch(glob, s[k:]) {
					return true
				}
			}
			return false
		case '?':
			if s == "" {
				return false
			}
			glob, s = glob[1:], s[1:]
		default:
			if s == "" || s[0] != glob[0] {
				return false
			}
			glob, s = glob[1:], s[1:]
		}
	}
	return s == ""
}

// stationIndex is the place of the station name, -1 for none.
func stationIndex(stations []Station, name string) int {
	if name == "" {
		return -1
	}
	return slices.IndexFunc(stations, func(s Station) bool { return s.Name == name && s.Review == "" })
}

// lastAgent is the name of a line's last agent station ("" none).
func lastAgent(stations []Station) string {
	for k := len(stations) - 1; k >= 0; k-- {
		if stations[k].Agent != "" && stations[k].Review == "" {
			return stations[k].Name
		}
	}
	return ""
}

func clipAll(texts []string, n int) []string {
	out := make([]string, len(texts))
	for k, t := range texts {
		out[k] = clipLine(t, n)
	}
	return out
}

// stationPrompt is what an agent at station i is given: the item's
// request, what the stations before it left, and its own prompt.
func stationPrompt(line *Line, i int, request string, outputs []string, note string) string {
	var b strings.Builder
	name := line.Name
	if name == "" {
		name = "put together for this request"
	}
	fmt.Fprintf(&b, "You are at the station %q of a line (%s), %d of %d, working on this request:\n\n<request>\n%s\n</request>\n",
		line.Stations[i].Name, name, i+1, len(line.Stations), request)
	for j, o := range outputs {
		if strings.TrimSpace(o) == "" {
			continue
		}
		fmt.Fprintf(&b, "\nWhat the station %q before you left:\n\n<station %q>\n%s\n</station>\n", line.Stations[j].Name, line.Stations[j].Name, o)
	}
	if p := strings.TrimSpace(line.Stations[i].Prompt); p != "" {
		fmt.Fprintf(&b, "\nYour part here: %s\n", p)
	} else {
		b.WriteString("\nDo your part of the request here.\n")
	}
	if note = strings.TrimSpace(note); note != "" {
		fmt.Fprintf(&b, "\nYour work was sent back by a review, with these words:\n\n<review>\n%s\n</review>\n", note)
	}
	if len(line.Stations[i].Checks) > 0 {
		fmt.Fprintf(&b, "Your work is done only when these checks pass: %s.\n", strings.Join(line.Stations[i].Checks, ", "))
	}
	return b.String()
}

// lineSummary is a line's turn's reply: how each station it reached ended,
// and what the last one that said anything left.
func lineSummary(line *Line, stations []Station, results []StationResult, outputs []string, done bool) string {
	var b strings.Builder
	name := line.Name
	if name == "" {
		name = "put together on the spot"
	}
	last := results[len(results)-1]
	if done {
		fmt.Fprintf(&b, "The line %s is done: every station passed.\n", name)
	} else {
		fmt.Fprintf(&b, "The line %s stopped at the station %q (%s): %s.\n", name, last.Name, last.Status, firstLineOf(last.Why))
	}
	for _, r := range results {
		fmt.Fprintf(&b, "- %s: %s", r.Name, r.Status)
		for _, c := range r.Checks {
			mark := "passed"
			if !c.Passed {
				mark = "did not pass"
			}
			fmt.Fprintf(&b, "; check %s %s", c.Name, mark)
		}
		b.WriteString("\n")
	}
	// What the last station that said anything left (a gate alone says
	// nothing but its verdicts).
	for j := len(outputs) - 1; j >= 0; j-- {
		if o := strings.TrimSpace(outputs[j]); o != "" {
			fmt.Fprintf(&b, "\nWhat the station %q left:\n\n%s\n", stations[j].Name, o)
			break
		}
	}
	return b.String()
}

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// clipLine is s cut to n characters, marked as cut.
func clipLine(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
