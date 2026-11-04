package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/r-hashi01/kairocode/internal/api"
	"slices"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/workflows"
)

type TryTarget = api.TryTarget

var (
	ErrNotAFlow = api.ErrNotAFlow
	ErrTrying   = api.ErrTrying
)

// canTry says why the flow of turn's approval cannot be tried, if it
// cannot: its agents would run on an engine that may act without asking
// (an ACP agent with tools of its own, as codex): only the API, Claude
// Code (run with KairoCode's tools alone) and copilot (given them alone)
// run a try (ADR 0017).
func (r *Runtime) canTry(ctx context.Context, turn string) error {
	info, err := r.K.Get(ctx, turn)
	if err != nil {
		return err
	}
	var in workflows.TurnInput
	if err := runInput(info.Input, &in); err != nil {
		return err
	}
	engine := in.Engine
	if engine == "" {
		engine = r.cfg.Engine
	}
	if !dryEngine(engine) {
		return fmt.Errorf("a try runs the flow's agents with %s, which may change things without asking: switch to the Claude API or Claude Code (/provider) to try it", registryAgent(engine))
	}
	return nil
}

// TryFlow tries the flow an approval would write (ADR 0017), with args as
// the words after /flow <name>: the call keeps waiting, and the try runs
// below it; tools that do more than read are not called. The approval is
// asked again when the try ends, with what it did.
func (r *Runtime) TryFlow(ctx context.Context, key, args string) error {
	session := sessionOf(key)
	if session == "" {
		return fmt.Errorf("not an approval key: %q", key)
	}
	r.approveMu.Lock()
	defer r.approveMu.Unlock()
	pending, err := r.Pending(ctx, session)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(pending, func(p Pending) bool { return p.Key == key })
	if i < 0 {
		return ErrAnswered
	}
	switch {
	case pending[i].Trying:
		return ErrTrying
	case pending[i].Try == nil:
		return ErrNotAFlow
	}
	if err := r.canTry(ctx, pending[i].Turn); err != nil {
		return err
	}
	run, call, _ := strings.Cut(key, "#")
	t := pending[i].Try
	return r.K.Signal(ctx, run, workflows.SignalApprovePrefix+call,
		workflows.Approval{Try: &workflows.FlowTry{Name: t.Name, Script: t.Script, Args: strings.TrimSpace(args)}})
}

// lastTry is the last try, among calls, of the flow whose script is
// script, as a section: what it was given, how it ended, and in order what
// its agents said and what it would have done (the calls it did not make,
// with all they would have been given).
func (r *Runtime) lastTry(ctx context.Context, calls []kairo.RunInfo, script, call string) (PreviewSection, bool) {
	var last *kairo.RunInfo
	var in workflows.FlowInput
	for i, c := range calls {
		if c.Workflow != workflows.Flow || !c.Finished() {
			continue
		}
		var fi workflows.FlowInput
		if runInput(c.Input, &fi) != nil || !fi.Dry || fi.Script != script || fi.For != call {
			continue
		}
		if last == nil || c.Created.After(last.Created) {
			last, in = &calls[i], fi
		}
	}
	if last == nil {
		return PreviewSection{}, false
	}
	var b strings.Builder
	row := func(label, value string) {
		fmt.Fprintf(&b, "  %s%s%s\n", label, strings.Repeat(" ", max(26-len([]rune(label)), 2)), value)
	}
	words := in.Call.Args
	if words == "" {
		words = "none"
	}
	row("words", words)
	var fo workflows.FlowOutput
	switch {
	case last.Status != "completed" || runOutput(last.Output, &fo) != nil:
		row("ended", "✗ stopped: "+firstLineOf(last.Error))
	case fo.OK:
		row("ended", "✓ "+firstLineOf(fo.Message))
	default:
		row("ended", "✗ "+firstLineOf(fo.Message))
	}
	b.WriteString("\n▌ WHAT HAPPENED\n")
	n := 0
	var walk func(id string, depth int)
	walk = func(id string, depth int) {
		if depth > 8 {
			return
		}
		kids, err := r.K.Children(ctx, id)
		if err != nil {
			return
		}
		// A CLI engine's tool calls are runs of their own (kairo#4).
		if mcp, err := r.Store.MCPRuns(ctx, id); err == nil {
			for _, m := range mcp {
				if info, err := r.K.Get(ctx, m); err == nil {
					kids = append(kids, info)
				}
			}
		}
		slices.SortStableFunc(kids, func(a, b kairo.RunInfo) int { return a.Created.Compare(b.Created) })
		for _, k := range kids {
			switch {
			case k.Plan == "kairo.call/"+workflows.ActFlowWould && k.Status == "completed":
				var w workflows.WouldOutput
				var wi struct {
					In workflows.WouldInput `json:"in"`
				}
				if json.Unmarshal(k.Output, &w) != nil || w.Run || json.Unmarshal(k.Input, &wi) != nil {
					continue
				}
				args, _ := r.toolArgs(ctx, wi.In.In)
				n++
				row(fmt.Sprintf("%d ⚠ %s", n, wi.In.Tool), "not called")
				b.WriteString(indent(prettyJSON(args), "        ") + "\n")
			case k.Workflow == workflows.Turn && k.Status == "completed":
				var ti workflows.TurnInput
				var to workflows.TurnOutput
				if runInput(k.Input, &ti) != nil || runOutput(k.Output, &to) != nil {
					continue
				}
				walk(k.RunID, depth+1) // what the agent would have done, before its reply
				reply := ""
				if m, err := r.Store.Message(ctx, to.Reply); err == nil {
					reply = m.Text()
				}
				n++
				row(fmt.Sprintf("%d agent %s", n, ti.Agent), "replied")
				b.WriteString(indent(clipLines(reply, 30), "        ") + "\n")
			case k.Workflow != "":
				walk(k.RunID, depth+1)
			}
		}
	}
	walk(last.RunID, 0)
	if n == 0 {
		row("✓ nothing", "")
	}
	return PreviewSection{Name: "Try", Text: strings.TrimRight(b.String(), "\n")}, true
}
