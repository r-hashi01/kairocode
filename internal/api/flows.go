package api

import (
	"fmt"
	"github.com/r-hashi01/kairocode/internal/flowchart"
	"github.com/r-hashi01/kairocode/internal/msg"
	"strings"
	"time"
)

import ()

// FlowInfo is a flow a session can run.
type FlowInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Draft: a draft, which runs only as a try (/flow <name> --draft, ADR
	// 0030).
	Draft bool `json:"draft,omitempty"`
	// Args: what it is to be given (its ARGS).
	Args string `json:"args,omitempty"`
}

// Flow runs' statuses, besides running.
const (
	FlowRunOK        = "ok"
	FlowRunFailed    = "failed"
	FlowRunBroken    = "broken" // the script could not run
	FlowRunCancelled = "cancelled"
	FlowRunRunning   = "running"
)

// FlowRun is a flow's run: where it ran and from what, how it ended, its
// chart with each step's state, and its steps with their times.
type FlowRun struct {
	ID      string `json:"id"` // the flow's run (kairo's)
	Session string `json:"session"`
	Turn    int    `json:"turn"`
	Dir     string `json:"dir"`
	Name    string `json:"name"`
	Args    string `json:"args,omitempty"`
	From    string `json:"from"` // FromCommand, FromAgent, FromHook
	// Try: a try of the flow before it was written (ADR 0017): what it
	// would have done, kept apart from its runs.
	Try     bool      `json:"try,omitempty"`
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
	if f.Try {
		from = "a try, before it was written"
	}
	name := f.Name
	if f.Try {
		name += " (try)"
	}
	return fmt.Sprintf("%s  %s%s  %s%s  (%s, session %s turn %d)", f.Started.Local().Format("2006-01-02 15:04"),
		name, args, f.Status, took, from, strings.TrimPrefix(f.Session, "s/"), f.Turn)
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

// How a flow came to run.
const (
	FromCommand = "command" // /flow <name>
	FromHook    = "hook"    // a hook (turn.end, tool.after, session.start)
	FromAgent   = "agent"   // an agent's workflow:
)

// FlowStat is what the records say of one flow of a project: how often it
// ran, from where, and how it ended. A flow of the project that never ran
// has no runs.
type FlowStat struct {
	Name   string `json:"name"`
	Runs   int    `json:"runs"`
	OK     int    `json:"ok"`
	Failed int    `json:"failed"` // said no, could not run, failed or was cancelled
	// Runs that are neither OK, Failed nor Running ended in a way that is no
	// longer kept.
	Running int            `json:"running"`
	From    map[string]int `json:"from,omitempty"`
	Last    time.Time      `json:"last,omitzero"`
	// Defined: the project has its script now.
	Defined bool `json:"defined"`
}

// States of a call of a flow, as its chart shows them.
const (
	FlowRunning = "running"
	FlowDone    = "done"
	FlowFailed  = "failed"
	FlowWaiting = "waiting" // for the user: an approval, a review, a question
)
