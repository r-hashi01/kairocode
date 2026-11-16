package api

import (
	"time"

	"github.com/r-hashi01/kairocode/internal/msg"
)

// The states of a work item (ADR 0043).
const (
	ItemQueued  = "queued" // past a limit on items in progress: starts in its turn (ADR 0044)
	ItemRunning = "running"
	ItemWaiting = "waiting" // for a person: its requests are listed with the conversation's
	ItemDone    = "done"    // every station passed: its branch is ready to merge
	ItemStopped = "stopped" // at a station that did not pass
	ItemFailed  = "failed"
	ItemTaken   = "taken"   // its branch merged, and it was cleaned up
	ItemDropped = "dropped" // dropped by the person, and cleaned up
	ItemClosed  = "closed"  // its session was closed (or its record let go of) with the item neither taken nor dropped
)

// Item is a work item a conversation sent down a line (ADR 0043): a
// session of its own, in a worktree of its own.
type Item struct {
	ID   string `json:"id"` // its session
	Of   string `json:"of"` // the conversation's
	Name string `json:"name"`
	// Line is the standard line's name, "" for one put together on the
	// spot; Stations, its stations' names.
	Line     string   `json:"line,omitempty"`
	Stations []string `json:"stations"`
	Request  string   `json:"request"`
	Dir      string   `json:"dir,omitempty"`
	Branch   string   `json:"branch,omitempty"`
	State    string   `json:"state"`
	// Station is where it is, or where it stopped; Why, why it did not
	// pass there.
	Station string `json:"station,omitempty"`
	Why     string `json:"why,omitempty"`
	Waiting int    `json:"waiting,omitempty"`
	// Position: of an item queued, how many are before it.
	Position int       `json:"position,omitempty"`
	Steps    int       `json:"steps,omitempty"`
	Usage    msg.Usage `json:"usage"`
	Started  time.Time `json:"started"`
	// Stale: done or stopped for more than a week, neither taken nor
	// dropped: to be looked at.
	Stale bool `json:"stale,omitempty"`
}

// DefaultItemsWip is how many items are in progress at once in a
// repository when lines.wip does not say (ADR 0044): more wait, queued.
const DefaultItemsWip = 4

// LineInfo is a line of the project, and how it stands (ADR 0044).
type LineInfo struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Stations    []string `json:"stations"`
	// Error: the line's file could not be read, and why.
	Error string `json:"error,omitempty"`
	// Stopped: it stopped itself, its items stopping one after another;
	// Why says which, and where. A person starts it again.
	Stopped bool   `json:"stopped,omitempty"`
	Why     string `json:"why,omitempty"`
	// InProgress and Queued: its items running or waiting, and waiting
	// their turn; Wip, its limit (0: the repository's alone).
	InProgress int `json:"in_progress"`
	Queued     int `json:"queued"`
	Wip        int `json:"wip,omitempty"`
}
