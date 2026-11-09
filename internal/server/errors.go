package server

import (
	"errors"

	"github.com/r-hashi01/kairocode/internal/api"
)

// The errors a client tells apart, by the code the API's error carries
// ({"error": "...", "code": "..."}): the daemon's client gives back the
// same error, so errors.Is holds across the socket. One table for both
// sides; a new sentinel a client acts on goes here.
var wireErrors = []struct {
	code string
	err  error
}{
	{"session_failed", api.ErrSessionFailed},
	{"no_fork_point", api.ErrNoForkPoint},
	{"fork_while_running", api.ErrForkWhileRunning},
	{"no_snapshot", api.ErrNoSnapshot},
	{"no_undo", api.ErrNoUndo},
	{"no_turn_to_cancel", api.ErrNoTurnToCancel},
	{"answered", api.ErrAnswered},
	{"question", api.ErrQuestion},
	{"not_a_flow", api.ErrNotAFlow},
	{"trying", api.ErrTrying},
	{"no_provider", api.ErrNoProvider},
	{"not_installed", api.ErrNotInstalled},
	{"unavailable", api.ErrUnavailable},
	{"no_command", api.ErrNoCommand},
	{"no_flow", api.ErrNoFlow},
	{"no_flow_name", api.ErrNoFlowName},
	{"no_flow_request", api.ErrNoFlowRequest},
	{"no_flow_source", api.ErrNoFlowSource},
	{"no_flow_run", api.ErrNoFlowRun},
	{"bad_image", api.ErrBadImage},
	{"closing", api.ErrClosing},
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
