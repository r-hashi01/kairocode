package api

import (
	"encoding/json"
	"github.com/r-hashi01/kairocode/internal/flowchart"
	"github.com/r-hashi01/kairocode/internal/msg"
)

import ()

// Pending is what waits for the user: a tool call, for approval before it
// runs (Kind "approval") or, cut off while it ran, to be told whether it
// took effect (Kind "review", invariant 4); or a flow's question (Kind
// "question": Subject is the question, Options its answers, ADR 0003).
type Pending struct {
	Kind string `json:"kind"`
	// Key names the request: give it to Approve, or a question's to
	// Answer.
	Key     string   `json:"key"`
	Turn    string   `json:"turn"`
	Tool    string   `json:"tool"`
	Subject string   `json:"subject"`
	Rule    string   `json:"rule"`
	Options []string `json:"options,omitempty"`
	// Preview is what the call would do (a flow's shape), to show; and in
	// its sections, for a client that shows them apart.
	Preview  string           `json:"preview,omitempty"`
	Sections []PreviewSection `json:"sections,omitempty"`
	// Try, for a flow's write: the flow, to try it first (ADR 0017).
	// Trying: a try of it is under way; an answer given now is taken when
	// it ends.
	Try    *TryTarget `json:"try,omitempty"`
	Trying bool       `json:"trying,omitempty"`
}

// KindQuestion is a Pending that is a flow's question.
const KindQuestion = "question"

// Kinds of Event.
const (
	EventToken             = "token"
	EventTurnStart         = "turn.start"
	EventTurnEnd           = "turn.end"
	EventToolStart         = "tool.start"
	EventToolEnd           = "tool.end"
	EventApprovalRequested = "approval.requested"
	EventApprovalResolved  = "approval.resolved"
	EventReviewRequested   = "review.requested"
	EventReviewResolved    = "review.resolved"
	// A flow's question (ADR 0003): Pending has it, and its options.
	EventQuestionRequested = "question.requested"
	EventQuestionResolved  = "question.resolved"
	// A flow ran on (ADR 0003): Flow is its chart and its calls' states.
	EventFlow = "flow"
)

// Event is something that happened in a session, told to those who watch
// it (issue #1). Events are not records: they are made from the records
// and the runtime's observations as things happen, and carry nothing that
// is not recorded (invariant 6). They come from this process's
// observations, so they tell of what runs here: all of it while one daemon
// drives the data directory (ADR 0008). A watcher that misses some (it joined
// late, was too slow, or the daemon restarted) is told first what waits
// for a person when it watches again; the rest it reads from the records.
type Event struct {
	// Seq orders a session's events while this process runs.
	Seq  int64  `json:"seq"`
	Type string `json:"type"`
	// Turn is the turn's run id; N its number (turn.start, turn.end).
	Turn string `json:"turn,omitempty"`
	N    int    `json:"n"`
	// Call names a tool call: the model's tool_use id, or the MCP call's run
	// id (ADR 0007).
	Call    string          `json:"call,omitempty"`
	Tool    string          `json:"tool,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	Output  string          `json:"output,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
	// Text is a token's text.
	Text string `json:"text,omitempty"`
	// Pending is what waits (approval.requested, review.requested); Key
	// what was answered (approval.resolved, review.resolved), and OK the
	// answer when it was given in this process: the call may run, or (a
	// review) it took effect.
	Pending *Pending `json:"pending,omitempty"`
	Key     string   `json:"key,omitempty"`
	OK      *bool    `json:"ok,omitempty"`
	// turn.end
	Reply   string     `json:"reply,omitempty"`
	Steps   int        `json:"steps,omitempty"`
	Stopped string     `json:"stopped,omitempty"`
	Usage   *msg.Usage `json:"usage,omitempty"`
	Error   string     `json:"error,omitempty"`
	// Flow: a flow's chart and how far it has gone (flow).
	Flow *flowchart.View `json:"flow,omitempty"`
}

// TryTarget is the flow an approval would write, to try it first (ADR
// 0017): its name and its script's hash in the store.
type TryTarget struct {
	Name   string `json:"name"`
	Script string `json:"script"`
}

// PreviewSection is a named part of what a call would do: Overview,
// Flow, Changes, Script, Try. Refs: for each line of Text, the script's
// lines it is ({0, 0} for none), to show them; Details: for each line,
// more to show of it ("" for none: a change's before and after). Chart:
// the flow's chart (flowchart.Chart), for a client that draws it (Text is
// its tree).
type PreviewSection struct {
	Name    string          `json:"name"`
	Text    string          `json:"text"`
	Refs    [][2]int        `json:"refs,omitempty"`
	Details []string        `json:"details,omitempty"`
	Chart   json.RawMessage `json:"chart,omitempty"`
}

// RuleText is the rule that asks about p, as said to a person: the
// catch-all ("*: ask") in words.
func (p Pending) RuleText() string {
	if p.Rule == "*: ask" {
		return "asked by default: no rule allows it"
	}
	return p.Rule
}

// ReviewQuestion is what a review asks of a call cut off while it ran
// (invariant 4): whether it took effect.
const ReviewQuestion = "Cut off while it ran — did it take effect?"
