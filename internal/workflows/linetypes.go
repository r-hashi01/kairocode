package workflows

import "github.com/r-hashi01/kairocode/internal/msg"

// A line (ADR 0043) is the row of stations a work item goes down, from a
// request to a change ready to merge. It is declared in the project
// (.kairocode/lines/<name>.json) or put together on the spot, and recorded
// as the item starts: a line changed afterwards changes no item on it.
type Line struct {
	// Name is the standard line's, "" for one put together on the spot.
	Name        string    `json:"name,omitempty"`
	Description string    `json:"description,omitempty"`
	Stations    []Station `json:"stations"`
}

// Station is a place on a line: one machine, and the gate at its exit.
// With neither Agent nor Flow, the station is its gate alone.
type Station struct {
	Name string `json:"name"`
	// Agent: a subagent, given the item's request, Prompt, and what the
	// stations before it left.
	Agent  string `json:"agent,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	// Flow: a flow of the project's, given Args, else the request.
	Flow string `json:"flow,omitempty"`
	Args string `json:"args,omitempty"`
	// Checks are the gate (ADR 0042): an agent is sent back up to Rounds
	// times; a flow is not, and the item stops at the station.
	Checks []string `json:"checks,omitempty"`
	Rounds int      `json:"rounds,omitempty"`
}

// How a station ended.
const (
	StationPassed  = "passed"
	StationStopped = "stopped" // its gate, or the agent's turn, stopped it
	StationFailed  = "failed"  // its machine failed
)

// StopStation: a line's turn ended with a station that did not pass.
const StopStation = "station"

// StationResult is what a station of an item's line came to.
type StationResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	// Why: of a station that did not pass, why ("checks", a flow's
	// message, an agent's failure).
	Why    string         `json:"why,omitempty"`
	Checks []CheckVerdict `json:"checks,omitempty"`
	Steps  int            `json:"steps,omitempty"`
	Usage  msg.Usage      `json:"usage"`
}

// LineCommitInput: commit the worktree of Session's item, with Message.
type LineCommitInput struct {
	Session string `json:"session"`
	Dir     string `json:"dir"`
	Message string `json:"message"`
}

// LineCommitOutput is the commit made, "" with none (not an item's
// session, or nothing to commit); Note says why or what.
type LineCommitOutput struct {
	Commit string `json:"commit,omitempty"`
	Note   string `json:"note,omitempty"`
}
