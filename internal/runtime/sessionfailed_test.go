package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/provider"
)

// A session that failed (a store's error in it, kairo-gap #16) takes no
// more messages: Send says so, and why, instead of sending one to be lost
// (ADR 0021). The failed run here is a workflow that fails as a session
// does on a store's error.
func TestSendToAFailedSession(t *testing.T) {
	t.Parallel()
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	kairo.Workflow(r.K, "test.fails", func(ctx *kairo.Context, in struct{}) (struct{}, error) {
		return struct{}{}, errors.New("store: database is locked")
	})
	s := "s/fails"
	if _, err := r.K.Submit(ctx, "test.fails", struct{}{}, kairo.WithID(s)); err != nil {
		t.Fatal(err)
	}
	if _, err := kairo.Await[struct{}](ctx, r.K, s); err == nil {
		t.Fatal("the run did not fail")
	}
	for _, send := range []func() (int, error){
		func() (int, error) { return r.Send(ctx, s, "again") },
		func() (int, error) { return r.Compact(ctx, s) },
	} {
		if _, err := send(); !errors.Is(err, ErrSessionFailed) || !strings.Contains(err.Error(), "database is locked") {
			t.Fatalf("to a failed session: %v", err)
		}
	}
}
