package server

import (
	"errors"

	"github.com/r-hashi01/kairocode/internal/runtime"
)

// The errors a client tells apart, by the code the API's error carries
// ({"error": "...", "code": "..."}): the daemon's client gives back the
// same error, so errors.Is holds across the socket. One table for both
// sides; a new sentinel a client acts on goes here.
var wireErrors = []struct {
	code string
	err  error
}{
	{"session_failed", runtime.ErrSessionFailed},
	{"no_fork_point", runtime.ErrNoForkPoint},
	{"fork_while_running", runtime.ErrForkWhileRunning},
	{"no_snapshot", runtime.ErrNoSnapshot},
	{"no_undo", runtime.ErrNoUndo},
	{"no_turn_to_cancel", runtime.ErrNoTurnToCancel},
	{"answered", runtime.ErrAnswered},
	{"question", runtime.ErrQuestion},
	{"not_a_flow", runtime.ErrNotAFlow},
	{"trying", runtime.ErrTrying},
	{"no_provider", runtime.ErrNoProvider},
	{"not_installed", runtime.ErrNotInstalled},
	{"unavailable", runtime.ErrUnavailable},
	{"no_command", runtime.ErrNoCommand},
	{"no_flow", runtime.ErrNoFlow},
	{"no_flow_name", runtime.ErrNoFlowName},
	{"no_flow_request", runtime.ErrNoFlowRequest},
	{"no_flow_run", runtime.ErrNoFlowRun},
	{"bad_image", runtime.ErrBadImage},
	{"closing", runtime.ErrClosing},
}

// ErrorCode is err's code on the wire ("" for an error a client does not
// tell apart).
func ErrorCode(err error) string {
	for _, w := range wireErrors {
		if errors.Is(err, w.err) {
			return w.code
		}
	}
	return ""
}

// ErrorOf is the error a code stands for (nil for none known).
func ErrorOf(code string) error {
	for _, w := range wireErrors {
		if w.code == code {
			return w.err
		}
	}
	return nil
}
