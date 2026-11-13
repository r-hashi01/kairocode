package api

import (
	"time"

	"github.com/r-hashi01/kairocode/internal/msg"
)

// The states of a work item (ADR 0043).
const (
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
	Station string    `json:"station,omitempty"`
	Why     string    `json:"why,omitempty"`
	Waiting int       `json:"waiting,omitempty"`
	Steps   int       `json:"steps,omitempty"`
	Usage   msg.Usage `json:"usage"`
	Started time.Time `json:"started"`
	// Stale: done or stopped for more than a week, neither taken nor
	// dropped: to be looked at.
	Stale bool `json:"stale,omitempty"`
}

// MaxItemsRunning is how many items run at once in a repository (ADR
// 0043): a stop-gap until a queue replaces it.
const MaxItemsRunning = 4
