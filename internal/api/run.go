package api

import "github.com/r-hashi01/kairocode/internal/msg"

// RunResult is how `kairocode run -json` and `resume -json` say a turn
// ended, for another program to read (ADR 0041). Status says the same
// as the exit code.
type RunResult struct {
	Session string `json:"session"`
	// Turn is the turn followed; absent when none was started.
	Turn   *int   `json:"turn,omitempty"`
	Status string `json:"status"`
	// Stopped is why the turn ended before the model was done (the turn's
	// own reason), with StatusStopped.
	Stopped string `json:"stopped,omitempty"`
	// Reply, Steps and Usage are there when the turn ended.
	Reply *string    `json:"reply,omitempty"`
	Steps *int       `json:"steps,omitempty"`
	Usage *msg.Usage `json:"usage,omitempty"`
	// Waiting is what waits for a person, with StatusWaiting.
	Waiting []Pending `json:"waiting,omitempty"`
	// Error is what failed, with StatusError.
	Error string `json:"error,omitempty"`
}

// The statuses of a RunResult, each with its exit code.
const (
	StatusDone        = "done"        // 0: the model was done
	StatusError       = "error"       // 1: the turn failed, or the command did
	StatusWaiting     = "waiting"     // 3: -detach, and the turn waits for a person
	StatusStopped     = "stopped"     // 4: the turn stopped before the model was done
	StatusCancelled   = "cancelled"   // 5: the turn was cancelled
	StatusInterrupted = "interrupted" // 130: the command was; the turn goes on
)
