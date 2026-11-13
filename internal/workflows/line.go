package workflows

import (
	"errors"
	"fmt"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/msg"
)

// runLineTurn is a work item's first turn (ADR 0043): it runs the item's
// line, its stations in order, each a recorded child (invariant 8: told
// apart by where on the line it is), so a resumed item takes up the
// station it was at and runs no finished one again. The line stops at the
// first station that does not pass. What each came to is the turn's
// output, and a summary its reply.
func runLineTurn(ctx *kairo.Context, in TurnInput) (TurnOutput, error) {
	line := in.Line
	request, err := kairo.Call[string](ctx, ActFlowText, in.Message)
	if err != nil {
		return TurnOutput{}, err
	}
	var out TurnOutput
	var checks *Checks // the project's, read once a gate needs them
	var outputs []string
	for i, st := range line.Stations {
		at := fmt.Sprintf("station %d %s", i, st.Name)
		res := StationResult{Name: st.Name, Status: StationPassed}
		var output string
		switch {
		case st.Agent != "":
			text := stationPrompt(line, i, request, outputs)
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
		// once, sending nothing back (an agent's ran in its turn).
		if st.Agent == "" && len(st.Checks) > 0 && res.Status == StationPassed {
			if checks == nil {
				a, err := kairo.Call[Agent](ctx, ActAgentLoad, AgentLoadInput{Dir: in.Dir, Agent: in.Agent, Engine: in.Engine, Model: in.Model})
				if err != nil {
					return TurnOutput{}, err
				}
				checks = &Checks{}
				if a.Checks != nil {
					checks = a.Checks
				}
			}
			g, err := newGate(ctx, in.Dir, fmt.Sprintf("line%d", i), checks, st.Checks, 0, false)
			if err != nil {
				res.Status, res.Why = StationFailed, err.Error()
			} else {
				g.at = at
				gr, err := g.run(ctx)
				if err != nil {
					return TurnOutput{}, err
				}
				res.Checks = gr.verdicts
				if !gr.passed {
					res.Status, res.Why = StationStopped, StopChecks
				}
			}
		}
		out.Line = append(out.Line, res)
		out.Steps += res.Steps
		out.Usage.Add(res.Usage)
		outputs = append(outputs, output)
		if res.Status != StationPassed {
			out.Stopped = StopStation
			break
		}
	}
	summary := lineSummary(line, out.Line, outputs)
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

// stationPrompt is what an agent at station i is given: the item's
// request, what the stations before it left, and its own prompt.
func stationPrompt(line *Line, i int, request string, outputs []string) string {
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
	if len(line.Stations[i].Checks) > 0 {
		fmt.Fprintf(&b, "Your work is done only when these checks pass: %s.\n", strings.Join(line.Stations[i].Checks, ", "))
	}
	return b.String()
}

// lineSummary is a line's turn's reply: how each station it reached ended,
// and what the last one that said anything left.
func lineSummary(line *Line, results []StationResult, outputs []string) string {
	var b strings.Builder
	name := line.Name
	if name == "" {
		name = "put together on the spot"
	}
	last := results[len(results)-1]
	if last.Status == StationPassed && len(results) == len(line.Stations) {
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
			fmt.Fprintf(&b, "\nWhat the station %q left:\n\n%s\n", results[j].Name, o)
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
