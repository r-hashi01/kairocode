package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// Looking at a session's records (FR-24, ADR 0023): every turn, model
// call, tool call and its result, what waited for a person and how it was
// answered, and what was used. It only reads.

// SessionLog is a session's record.
type SessionLog struct {
	Session string             `json:"session"`
	Dir     string             `json:"dir"`
	Engine  string             `json:"engine,omitempty"`
	From    *workflows.ForkRef `json:"from,omitempty"`
	Usage   msg.Usage          `json:"usage"`
	Turns   []LogEntry         `json:"turns"`
}

// LogEntry is a run of the record: a turn, a call, a wait, a child
// workflow (with its own runs).
type LogEntry struct {
	At     time.Time  `json:"at"`
	End    time.Time  `json:"end,omitzero"`
	Kind   string     `json:"kind"` // turn, model, tool, permission, wait, workflow, step
	Name   string     `json:"name"`
	Status string     `json:"status"`
	Text   string     `json:"text,omitempty"`
	Usage  *msg.Usage `json:"usage,omitempty"`
	Runs   []LogEntry `json:"runs,omitempty"`
}

// logText is how much of a message or result a log line shows.
const logText = 400

// Log is session's record.
func (r *Runtime) Log(ctx context.Context, session string) (SessionLog, error) {
	info, err := r.K.Get(ctx, session)
	if err != nil {
		return SessionLog{}, err
	}
	var in workflows.SessionInput
	if err := json.Unmarshal(input(info), &in); err != nil {
		return SessionLog{}, err
	}
	out := SessionLog{Session: session, Dir: in.Dir, Engine: in.Engine, From: in.From}
	kids, err := r.K.Children(ctx, session)
	if err != nil {
		return SessionLog{}, err
	}
	type turn struct {
		n    int
		info kairo.RunInfo
	}
	var turns []turn
	for _, k := range kids {
		if k.Workflow != workflows.Turn {
			continue
		}
		var tin workflows.TurnInput
		_ = json.Unmarshal(input(k), &tin)
		turns = append(turns, turn{tin.N, k})
	}
	slices.SortFunc(turns, func(a, b turn) int { return a.n - b.n })
	for _, t := range turns {
		e, err := r.logTurn(ctx, t.info)
		if err != nil {
			return SessionLog{}, err
		}
		if e.Usage != nil {
			out.Usage.Add(*e.Usage)
		}
		out.Turns = append(out.Turns, e)
	}
	return out, nil
}

// logTurn is a turn's entry: what was asked, its runs, its reply.
func (r *Runtime) logTurn(ctx context.Context, k kairo.RunInfo) (LogEntry, error) {
	var tin workflows.TurnInput
	_ = json.Unmarshal(input(k), &tin)
	e := LogEntry{At: k.Created, Kind: "turn", Name: fmt.Sprintf("turn %d", tin.N), Status: k.Status}
	if k.Finished() {
		e.End = k.Updated
	}
	switch {
	case tin.Undo != nil && tin.Undo.Fork:
		e.Text = "put the files back as they were after the turn forked from"
	case tin.Undo != nil && tin.Undo.Redo:
		e.Text = "/redo"
	case tin.Undo != nil:
		e.Text = "/undo"
	case tin.Compact:
		e.Text = "/compact"
	case tin.Flow != nil:
		e.Text = "/flow " + tin.Flow.Name
	default:
		e.Text = r.messageText(ctx, tin.Message)
	}
	runs, err := r.logRuns(ctx, k.RunID)
	if err != nil {
		return LogEntry{}, err
	}
	// A CLI engine's tool calls are runs of their own (ADR 0007).
	mcp, err := r.Store.MCPRuns(ctx, k.RunID)
	if err != nil {
		return LogEntry{}, err
	}
	for _, id := range mcp {
		info, err := r.K.Get(ctx, id)
		if err != nil {
			continue // removed (kairo-gap #17)
		}
		me := r.logRun(ctx, info)
		if me.Runs, err = r.logRuns(ctx, id); err != nil {
			return LogEntry{}, err
		}
		runs = append(runs, me)
	}
	slices.SortStableFunc(runs, func(a, b LogEntry) int { return a.At.Compare(b.At) })
	e.Runs = runs
	var out workflows.TurnOutput
	if runOutput(k.Output, &out) == nil {
		u := out.Usage
		e.Usage = &u
		if out.Stopped != "" {
			e.Status += " (stopped: " + out.Stopped + ")"
		}
	}
	return e, nil
}

// logRuns are run's children, oldest first, each with its own.
func (r *Runtime) logRuns(ctx context.Context, run string) ([]LogEntry, error) {
	kids, err := r.K.Children(ctx, run)
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(kids, func(a, b kairo.RunInfo) int { return a.Created.Compare(b.Created) })
	var out []LogEntry
	for _, k := range kids {
		e := r.logRun(ctx, k)
		if k.Workflow != "" {
			if e.Runs, err = r.logRuns(ctx, k.RunID); err != nil {
				return nil, err
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// logRun describes one run by its plan.
func (r *Runtime) logRun(ctx context.Context, k kairo.RunInfo) LogEntry {
	e := LogEntry{At: k.Created, Status: k.Status, Kind: "step", Name: k.Plan}
	if k.Finished() {
		e.End = k.Updated
	}
	call, _ := strings.CutPrefix(k.Plan, "kairo.call/")
	wait, isWait := strings.CutPrefix(k.Plan, "kairo.wait/")
	switch {
	case k.Workflow != "":
		e.Kind, e.Name = "workflow", k.Workflow
		if k.Workflow == workflows.Turn {
			var tin workflows.TurnInput
			_ = json.Unmarshal(input(k), &tin)
			e.Name = "subagent " + tin.Agent
			e.Text = r.messageText(ctx, tin.Message)
		}
	case isWait:
		e.Kind, e.Name = "wait", wait
		var w struct {
			Payload  json.RawMessage `json:"payload"`
			TimedOut bool            `json:"timed_out"`
		}
		if json.Unmarshal(k.Output, &w) == nil && len(w.Payload) > 0 {
			var a workflows.Approval
			switch {
			case w.TimedOut:
				e.Text = "timed out"
			case strings.HasPrefix(wait, workflows.SignalApprovePrefix) && json.Unmarshal(w.Payload, &a) == nil:
				e.Text = "declined"
				if a.OK {
					e.Text = "allowed"
				}
				if a.Reason != "" {
					e.Text += ": " + clip(a.Reason, 200)
				}
			default:
				e.Text = clip(compactJSON(w.Payload), logText)
			}
		}
	case call == workflows.ActModel:
		var in workflows.ModelInput
		var out workflows.ModelReply
		_ = json.Unmarshal(input(k), &in)
		e.Kind, e.Name = "model", in.Model
		if runOutput(k.Output, &out) == nil {
			e.Usage = &out.Usage
			e.Text = clip(r.messageText(ctx, out.Message), logText)
			var names []string
			for _, c := range out.Calls {
				names = append(names, c.Name)
			}
			if len(names) > 0 {
				e.Text += " [calls " + strings.Join(names, ", ") + "]"
			}
			if out.Stop == workflows.StopContext {
				e.Text = "longer than the model's context: compacted first"
			}
		}
	case call == workflows.ActHistoryCompact:
		var out workflows.CompactOutput
		e.Kind, e.Name = "model", "compact"
		if runOutput(k.Output, &out) == nil {
			e.Usage = &out.Usage
			e.Text = "the conversation summarized; the earlier messages are kept"
		}
	case call == workflows.ActPermissionCheck:
		var in workflows.PermissionInput
		var out workflows.PermissionOutput
		_ = json.Unmarshal(input(k), &in)
		e.Kind, e.Name = "permission", in.Tool
		if runOutput(k.Output, &out) == nil {
			e.Text = strings.TrimSpace(out.Action + " " + out.Subject)
			if out.Rule != "" {
				e.Text += " (" + out.Rule + ")"
			}
		}
	case strings.HasPrefix(call, workflows.ActToolPrefix):
		var in workflows.ToolInput
		var out workflows.ToolOutput
		_ = json.Unmarshal(input(k), &in)
		e.Kind, e.Name = "tool", strings.TrimPrefix(call, workflows.ActToolPrefix)
		if args, err := r.toolArgs(ctx, in); err == nil && len(args) > 0 {
			e.Text = clip(compactJSON(args), 200)
		}
		if runOutput(k.Output, &out) == nil && out.Message != "" {
			e.Text += " → " + clip(r.messageText(ctx, out.Message), logText)
			if out.IsError {
				e.Status += " (error)"
			}
		}
	default:
		e.Name = call
	}
	if k.Status == "failed" && k.Error != "" {
		e.Text = strings.TrimSpace(e.Text + " " + clip(k.Error, logText))
	}
	return e
}

// messageText is a stored message's text ("" if it is not there).
func (r *Runtime) messageText(ctx context.Context, h string) string {
	if h == "" {
		return ""
	}
	m, err := r.Store.Message(ctx, h)
	if err != nil {
		return ""
	}
	var parts []string
	for _, p := range m.Parts { // a tool's result too
		if p.Type == msg.Image { // its kind and size, not its bytes (ADR 0026)
			size := "?"
			if _, data, err := r.Store.Get(ctx, p.Ref); err == nil {
				size = fmt.Sprintf("%d KB", (len(data)+1023)/1024)
			}
			parts = append(parts, fmt.Sprintf("[image: %s, %s]", p.MediaType, size))
			continue
		}
		if t := strings.TrimSpace(p.Text); t != "" {
			parts = append(parts, t)
		}
	}
	if len(parts) == 0 {
		return strings.TrimSpace(m.Text())
	}
	return strings.Join(parts, "\n")
}

// compactJSON is v as one line of JSON.
func compactJSON(v any) string {
	if raw, ok := v.(json.RawMessage); ok {
		var x any
		if json.Unmarshal(raw, &x) == nil {
			v = x
		}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// Text is the record as lines to read.
func (l SessionLog) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Session %s in %s", strings.TrimPrefix(l.Session, "s/"), l.Dir)
	if l.Engine != "" {
		fmt.Fprintf(&b, " (%s)", l.Engine)
	}
	if l.From != nil {
		fmt.Fprintf(&b, ", forked from turn %d of %s", l.From.Turn, strings.TrimPrefix(l.From.Session, "s/"))
	}
	fmt.Fprintf(&b, "\nUsed: %s\n", usageText(l.Usage))
	for _, t := range l.Turns {
		b.WriteString("\n" + l.entriesText([]LogEntry{t}))
	}
	return b.String()
}

// entriesText is entries, and the runs in each, as lines to read.
func (SessionLog) entriesText(es []LogEntry) string {
	var b strings.Builder
	var write func(es []LogEntry, depth int)
	write = func(es []LogEntry, depth int) {
		for _, e := range es {
			pad := strings.Repeat("  ", depth)
			line := fmt.Sprintf("%s%s %s %s: %s", pad, e.At.Local().Format("15:04:05"), e.Kind, e.Name, e.Status)
			if e.Usage != nil && (e.Usage.Input > 0 || e.Usage.Output > 0) {
				line += " [" + usageText(*e.Usage) + "]"
			}
			if e.Text != "" {
				line += "\n" + pad + "    " + strings.ReplaceAll(e.Text, "\n", "\n"+pad+"    ")
			}
			b.WriteString(line + "\n")
			write(e.Runs, depth+1)
		}
	}
	write(es, 0)
	return b.String()
}

func usageText(u msg.Usage) string {
	return fmt.Sprintf("in %d, out %d, cache read %d, cache write %d tokens", u.Input, u.Output, u.CacheRead, u.CacheWrite)
}
