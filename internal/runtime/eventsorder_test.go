package runtime

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/provider"
)

// A turn's tokens come apart from its end and may come after it. What the
// tokens have not told of the reply is told before the end, and the tokens
// that come after it are not told again: a client does not see the end of
// a reply after the turn's end (in the next turn's reply).
func TestReplyToldWholeBeforeTheTurnEnds(t *testing.T) {
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	s := "s/order"
	w := r.ev.watch(s)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		r.ev.mu.Lock()
		ready := w.ready
		r.ev.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the watch did not start")
		}
	}
	tokens := func() string {
		var b strings.Builder
		for {
			select {
			case ev := <-w.ch:
				if ev.Type == EventToken {
					b.WriteString(ev.Text + "|")
				}
			default:
				return b.String()
			}
		}
	}
	late := func(turn, text string) {
		r.ev.make(item{session: s, chunk: &kairo.Chunk{RunID: turn + "/call", Data: []byte(text)}})
	}

	turn := s + "/t1"
	r.ev.emit(s, Event{Type: EventToken, Turn: turn, Text: "Hello, "})
	r.ev.tellRest(s, turn, "Hello, world.")
	late(turn, "world.")
	if got := tokens(); got != "Hello, |world.|" {
		t.Fatalf("the reply's end came after it: %q", got)
	}

	// What came before the turn's last call is not the reply's start.
	turn = s + "/t2"
	r.ev.emit(s, Event{Type: EventToken, Turn: turn, Text: "Let me look."})
	r.ev.emit(s, Event{Type: EventToolStart, Turn: turn, Call: "c1", Tool: "read"})
	r.ev.emit(s, Event{Type: EventToken, Turn: turn, Text: "Done"})
	r.ev.tellRest(s, turn, "Done.")
	late(turn, ".")
	if got := tokens(); got != "Let me look.|Done|.|" {
		t.Fatalf("after a call: %q", got)
	}

	// Told otherwise than the reply begins: nothing held back.
	turn = s + "/t3"
	r.ev.emit(s, Event{Type: EventToken, Turn: turn, Text: "Something else"})
	r.ev.tellRest(s, turn, "The reply.")
	late(turn, " and more")
	if got := tokens(); got != "Something else| and more|" {
		t.Fatalf("not the reply's start: %q", got)
	}

	// A turn that told no token (a flow's: its reply is told apart).
	r.ev.tellRest(s, s+"/t4", "The flow's reply.")
	if got := tokens(); got != "" {
		t.Fatalf("a turn with no tokens: %q", got)
	}
}
