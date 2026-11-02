package daemon

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/r-hashi01/kairocode/internal/runtime"
	"github.com/r-hashi01/kairocode/internal/server"
)

// An error a client tells apart is the same error on the client's side of
// the socket: errors.Is holds for what the daemon returned (the server
// says its code, the client gives it back), wrapped or not, whatever the
// status. One the table does not know is a plain error with its message.
func TestErrorsCrossTheSocket(t *testing.T) {
	for _, want := range []error{
		runtime.ErrForkWhileRunning, runtime.ErrNoSnapshot, runtime.ErrNoForkPoint, runtime.ErrNoUndo,
		runtime.ErrSessionFailed, runtime.ErrAnswered, runtime.ErrUnavailable, runtime.ErrNotInstalled,
	} {
		sent := fmt.Errorf("fork: %w", want)
		rec := httptest.NewRecorder()
		server.Fail(rec, http.StatusInternalServerError, sent)
		got := responseError(rec.Code, rec.Body.Bytes())
		if !errors.Is(got, want) || got.Error() != sent.Error() {
			t.Errorf("%v: came back as %v", want, got)
		}
	}
	rec := httptest.NewRecorder()
	server.Fail(rec, http.StatusInternalServerError, errors.New("disk full"))
	if got := responseError(rec.Code, rec.Body.Bytes()); got.Error() != "disk full" || errors.Unwrap(got) != nil {
		t.Errorf("an error not known: %v", got)
	}
}
