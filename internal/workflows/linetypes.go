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
	// Wip: how many of its items may be in progress at once (ADR 0044);
	// 0, only the repository's limit.
	Wip int `json:"wip,omitempty"`
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
	// Review: the station reviews the item's change so far (ADR 0044), by
	// ReviewPerson, ReviewModel (Agent, else "plan", with Prompt) or
	// ReviewRisk. Sent back, the station Back runs again, given the
	// reviewer's words, up to Rounds times (1 if 0).
	Review string `json:"review,omitempty"`
	Back   string `json:"back,omitempty"`
}

// Who reviews at a review station (ADR 0044).
const (
	ReviewPerson = "person"
	ReviewModel  = "model"
	ReviewRisk   = "risk" // the model, then a person where the change's paths or size call for one
)

// PeopleStation is the review a line gets at its end when its change
// touches a path that always needs a person and no person reviewed it.
const PeopleStation = "people"

// LinePolicyInput reads, as an item's line starts, what decides who
// reviews (ADR 0044): recorded with the line's turn.
type LinePolicyInput struct {
	Dir string `json:"dir"`
}

// LinePolicy: the paths that always need a person (lines.people), and the
// size past which a review by risk calls a person (lines.reviewLines).
type LinePolicy struct {
	People      []string `json:"people,omitempty"`
	ReviewLines int      `json:"review_lines"`
}

// LineDiffInput: the change of Session's item so far, in Dir.
type LineDiffInput struct {
	Session string `json:"session"`
	Dir     string `json:"dir"`
	At      string `json:"at"` // the review it is for
}

// LineDiff is a change: its text by hash in the store, the lines it
// changes and its paths.
type LineDiff struct {
	Diff  string   `json:"diff"`
	Lines int      `json:"lines"`
	Paths []string `json:"paths,omitempty"`
	// Sum is a digest of the whole change (the text shown is cut short,
	// and says only that a binary file changed): what a person's yes is
	// kept by.
	Sum string `json:"sum"`
}

// LineReviewInput puts a change to a person as an approval of kind
// review: Call names it, as a tool call names its approval.
type LineReviewInput struct {
	Session string   `json:"session"`
	Call    string   `json:"call"`
	Station string   `json:"station"`
	Line    string   `json:"line,omitempty"`
	Diff    LineDiff `json:"diff"`
	// Outputs: what the stations before it left, a station each, cut short
	// (shown to the person, not given to a model).
	Outputs []string `json:"outputs,omitempty"`
	// Why: why a person reviews (a path that needs one, the size), if not
	// because the station says so.
	Why string `json:"why,omitempty"`
}

// How a station ended.
const (
	StationPassed  = "passed"
	StationStopped = "stopped" // its gate, or the agent's turn, stopped it
	StationFailed  = "failed"  // its machine failed
	// StationSentBack: a review sent the item back to an earlier station
	// (ADR 0044); the line went on from there.
	StationSentBack = "sent back"
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
	// By: of a review station, who came to its verdict last, ReviewPerson
	// or ReviewModel (ADR 0045: kept with the item, for feedback).
	By    string    `json:"by,omitempty"`
	Steps int       `json:"steps,omitempty"`
	Usage msg.Usage `json:"usage"`
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
