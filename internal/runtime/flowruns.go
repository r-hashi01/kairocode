package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/flowchart"
	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// Flows' runs, kept once they end (ADR 0029): kairo removes a finished
// tree's record after KeepFinished; what a flow's run did is kept here, in
// kc_flow_run, to be looked at later (kairocode flow runs, flow show).
// Kept as kairo says a flow's run settled, and, for those a process missed
// (it stopped), as the runtime opens.

// Flow runs' statuses, besides running.
const (
	FlowRunOK        = "ok"
	FlowRunFailed    = "failed"
	FlowRunBroken    = "broken" // the script could not run
	FlowRunCancelled = "cancelled"
	FlowRunRunning   = "running"
)

// Defaults of how many of a flow's runs are kept (flowHistory).
const (
	flowRunsKeep = 200
	flowRunsDays = 90
)

// FlowRun is a flow's run: where it ran and from what, how it ended, its
// chart with each step's state, and its steps with their times.
type FlowRun struct {
	ID      string    `json:"id"` // the flow's run (kairo's)
	Session string    `json:"session"`
	Turn    int       `json:"turn"`
	Dir     string    `json:"dir"`
	Name    string    `json:"name"`
	Args    string    `json:"args,omitempty"`
	From    string    `json:"from"`           // FromCommand, FromAgent, FromHook
	Hook    string    `json:"hook,omitempty"` // the hook that ran it
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended,omitzero"`
	Status  string    `json:"status"`
	Message string    `json:"message,omitempty"` // what the flow said it did
	Agents  int       `json:"agents,omitempty"`
	Usage   msg.Usage `json:"usage"`
	// View and Steps: not in a list (FlowRuns).
	View  *flowchart.View `json:"view,omitempty"`
	Steps []LogEntry      `json:"steps,omitempty"`
}

// isTry says a flow's run is a try of a flow not written yet (ADR 0017):
// not one of the flow's runs.
func isTry(k kairo.RunInfo) bool {
	var fi workflows.FlowInput
	return json.Unmarshal(input(k), &fi) != nil || fi.Dry || fi.Call.Name == ""
}

// flowRunOf is a flow's run as kairo has it.
func (r *Runtime) flowRunOf(ctx context.Context, k kairo.RunInfo, whole bool) (FlowRun, error) {
	var fi workflows.FlowInput
	_ = json.Unmarshal(input(k), &fi)
	f := FlowRun{ID: k.RunID, Session: fi.Session, Turn: fi.N, Dir: sameDir(fi.Dir), Name: fi.Call.Name, Args: fi.Call.Args,
		Hook: fi.Hook, Started: k.Created, Status: FlowRunRunning}
	switch {
	case fi.Hook != "":
		f.From = FromHook
	default:
		f.From = FromAgent
		if t, err := r.K.Get(ctx, k.Parent); err == nil {
			var ti workflows.TurnInput
			if json.Unmarshal(input(t), &ti) == nil && ti.Flow != nil {
				f.From = FromCommand
			}
		}
	}
	if k.Finished() {
		f.Ended = k.Updated
		out, err := kairo.Await[workflows.FlowOutput](ctx, r.K, k.RunID)
		switch {
		case k.Status == "cancelled":
			f.Status = FlowRunCancelled
		case err == nil && out.Broken:
			f.Status, f.Message = FlowRunBroken, out.Message
		case err == nil && out.OK:
			f.Status, f.Message = FlowRunOK, out.Message
		case err == nil:
			f.Status, f.Message = FlowRunFailed, out.Message
		default:
			f.Status, f.Message = FlowRunFailed, k.Error
		}
		if err == nil {
			f.Agents, f.Usage = out.Agents, out.Usage
		}
	}
	if !whole {
		return f, nil
	}
	v, err := r.FlowView(ctx, k, nil)
	if err != nil {
		return FlowRun{}, err
	}
	f.View = v
	if f.Steps, err = r.logRuns(ctx, k.RunID); err != nil {
		return FlowRun{}, err
	}
	return f, nil
}

// keepFlowRun keeps a flow's run that ended (once: kept again, it is the
// same), and lets go of the oldest of that flow's beyond flowHistory.
func (r *Runtime) keepFlowRun(ctx context.Context, run string) error {
	k, err := r.K.Get(ctx, run)
	if err != nil {
		return err
	}
	if k.Workflow != workflows.Flow || !k.Finished() || isTry(k) {
		return nil
	}
	f, err := r.flowRunOf(ctx, k, true)
	if err != nil {
		return err
	}
	body, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := r.Store.PutFlowRun(ctx, store.FlowRunRow{ID: f.ID, Dir: f.Dir, Name: f.Name, Status: f.Status,
		Started: f.Started, Ended: f.Ended, Body: body}); err != nil {
		return err
	}
	keep, days := flowHistory()
	return r.Store.PruneFlowRuns(ctx, f.Dir, f.Name, keep, time.Now().AddDate(0, 0, -days))
}

// keepSettled keeps the flow's run kairo says settled here (observe).
func (r *Runtime) keepSettled(ctx context.Context, run string) {
	if err := r.keepFlowRun(ctx, run); err != nil && ctx.Err() == nil {
		r.logf("keeping the flow's run %s: %v", run, err)
	}
}

// flowsUnder calls f with each flow's run under run (a session, a turn, a
// flow and its agents', at any depth), until f says to stop. Only the
// runs not finished are gone into if open.
func (r *Runtime) flowsUnder(ctx context.Context, run string, open bool, f func(kairo.RunInfo) bool) bool {
	kids, _ := r.K.Children(ctx, run)
	for _, k := range kids {
		if ctx.Err() != nil {
			return false
		}
		if k.Workflow != workflows.Turn && k.Workflow != workflows.Flow {
			continue
		}
		if open && k.Finished() {
			continue
		}
		if k.Workflow == workflows.Flow && !f(k) {
			return false
		}
		if !r.flowsUnder(ctx, k.RunID, open, f) {
			return false
		}
	}
	return true
}

// keepMissed keeps the flows' runs that ended while no process kept them
// (it stopped first), as long as kairo still has them; not one that would
// be let go at once (past flowHistory: kept, then removed, it is not made
// again at each start).
func (r *Runtime) keepMissed(ctx context.Context) {
	sessions, err := r.K.List(ctx, kairo.Filter{Workflow: workflows.Session})
	if err != nil {
		return
	}
	keep, days := flowHistory()
	oldest := time.Now().AddDate(0, 0, -days)
	for _, s := range sessions {
		if !r.flowsUnder(ctx, s.RunID, false, func(k kairo.RunInfo) bool {
			if !k.Finished() || isTry(k) || k.Created.Before(oldest) {
				return true
			}
			if kept, err := r.Store.HasFlowRun(ctx, k.RunID); err != nil || kept {
				return err == nil
			}
			f, err := r.flowRunOf(ctx, k, false)
			if err != nil {
				return true
			}
			if newer, err := r.Store.FlowRunsSince(ctx, f.Dir, f.Name, f.Started); err != nil || newer >= keep {
				return err == nil
			}
			if err := r.keepFlowRun(ctx, k.RunID); err != nil && ctx.Err() == nil {
				r.logf("keeping the flow's run %s: %v", k.RunID, err)
			}
			return ctx.Err() == nil
		}) {
			return
		}
	}
}

// running are the flows' runs not ended, of dir ("" any), of flow name
// ("" any).
func (r *Runtime) running(ctx context.Context, dir, name string) ([]FlowRun, error) {
	sessions, err := r.K.List(ctx, kairo.Filter{Workflow: workflows.Session})
	if err != nil {
		return nil, err
	}
	var out []FlowRun
	for _, s := range sessions {
		if s.Finished() {
			continue
		}
		r.flowsUnder(ctx, s.RunID, true, func(k kairo.RunInfo) bool {
			if isTry(k) {
				return true
			}
			f, err := r.flowRunOf(ctx, k, false)
			if err == nil && (dir == "" || f.Dir == dir) && (name == "" || f.Name == name) {
				out = append(out, f)
			}
			return true
		})
	}
	return out, ctx.Err()
}

// FlowRuns are the flows' runs of dir ("" any), of flow name ("" any), the
// latest first, at most limit (0: all): those running, and those kept.
// Each without its chart and steps (FlowRunByID gives them).
func (r *Runtime) FlowRuns(ctx context.Context, dir, name string, limit int) ([]FlowRun, error) {
	if dir != "" {
		dir = sameDir(dir)
	}
	out, err := r.running(ctx, dir, name)
	if err != nil {
		return nil, err
	}
	rows, err := r.Store.FlowRuns(ctx, dir, name, limit)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		var f FlowRun
		if json.Unmarshal(row.Body, &f) != nil {
			continue
		}
		f.View, f.Steps = nil, nil
		out = append(out, f)
	}
	slices.SortStableFunc(out, func(a, b FlowRun) int { return b.Started.Compare(a.Started) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *Runtime) keptFlowRun(ctx context.Context, id string) (FlowRun, error) {
	row, err := r.Store.FlowRun(ctx, id)
	if err != nil {
		return FlowRun{}, err
	}
	var f FlowRun
	return f, json.Unmarshal(row.Body, &f)
}

// ErrNoFlowRun: no flow's run of that id, running or kept.
var ErrNoFlowRun = errors.New("no flow run of that id (kept runs: kairocode flow runs)")

// FlowRunByID is a flow's run with its chart and steps: as it goes, if it
// runs; as it was kept, if it ended.
func (r *Runtime) FlowRunByID(ctx context.Context, id string) (FlowRun, error) {
	f, err := r.keptFlowRun(ctx, id)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return FlowRun{}, err
	}
	k, err := r.K.Get(ctx, id)
	if err != nil || k.Workflow != workflows.Flow {
		return FlowRun{}, ErrNoFlowRun
	}
	return r.flowRunOf(ctx, k, true)
}

// flowHistory is how many of a flow's runs are kept, and for how many
// days: the user's kairocode.json, {"flowHistory": {"keep": 200, "days": 90}}.
func flowHistory() (keep, days int) {
	keep, days = flowRunsKeep, flowRunsDays
	b, err := os.ReadFile(filepath.Join(userConfigDir(), ConfigFile))
	if errors.Is(err, fs.ErrNotExist) || err != nil {
		return keep, days
	}
	var cfg struct {
		FlowHistory struct {
			Keep int `json:"keep"`
			Days int `json:"days"`
		} `json:"flowHistory"`
	}
	if json.Unmarshal(b, &cfg) == nil {
		if cfg.FlowHistory.Keep > 0 {
			keep = cfg.FlowHistory.Keep
		}
		if cfg.FlowHistory.Days > 0 {
			days = cfg.FlowHistory.Days
		}
	}
	return keep, days
}

// Line is the run in a line of a list.
func (f FlowRun) Line() string {
	took := ""
	if !f.Ended.IsZero() {
		took = " in " + f.Ended.Sub(f.Started).Round(time.Second).String()
	}
	args := ""
	if f.Args != "" {
		args = " " + f.Args
	}
	from := f.From
	if f.Hook != "" {
		from += " " + f.Hook
	}
	return fmt.Sprintf("%s  %s%s  %s%s  (%s, session %s turn %d)", f.Started.Local().Format("2006-01-02 15:04"),
		f.Name, args, f.Status, took, from, strings.TrimPrefix(f.Session, "s/"), f.Turn)
}

// Text is the run's steps, with their times, as lines to read (its chart
// is drawn apart).
func (f FlowRun) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", f.Line())
	if f.Message != "" {
		fmt.Fprintf(&b, "said: %s\n", f.Message)
	}
	if f.Usage.Input > 0 || f.Usage.Output > 0 {
		fmt.Fprintf(&b, "used: %s, %d agents\n", usageText(f.Usage), f.Agents)
	}
	fmt.Fprintf(&b, "id: %s\n", f.ID)
	if len(f.Steps) > 0 {
		b.WriteString("\nSteps:\n")
		b.WriteString(SessionLog{}.entriesText(f.Steps))
	}
	return b.String()
}
