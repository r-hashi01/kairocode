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
	chunk := func(call, text string) { // a token of call (<turn>/<call>)
		r.ev.make(item{session: s, chunk: &kairo.Chunk{RunID: call, Data: []byte(text)}})
	}

	turn := s + "/t1"
	chunk(turn+"/m1", "Hello, ")
	r.ev.tellRest(s, turn, "Hello, world.")
	chunk(turn+"/m1", "world.") // late: after the turn's end
	if got := tokens(); got != "Hello, |world.|" {
		t.Fatalf("the reply's end came after it: %q", got)
	}

	// The reply is the last call's words; the tool calls' events come in
	// no set order with the tokens (here after the next call's).
	turn = s + "/t2"
	chunk(turn+"/m1", "Let me look.")
	chunk(turn+"/m2", "Done")
	r.ev.emit(s, Event{Type: EventToolEnd, Turn: turn, Call: "c1", Tool: "read"})
	r.ev.tellRest(s, turn, "Done.")
	chunk(turn+"/m2", ".")
	if got := tokens(); got != "Let me look.|Done|.|" {
		t.Fatalf("after a call: %q", got)
	}

	// Told otherwise than the reply begins (Claude Code's turn tells all
	// its words in one call): nothing held back.
	turn = s + "/t3"
	chunk(turn+"/claude", "Something else")
	r.ev.tellRest(s, turn, "The reply.")
	chunk(turn+"/claude", " and more")
	if got := tokens(); got != "Something else| and more|" {
		t.Fatalf("not the reply's start: %q", got)
	}

	// A turn that told no token (a flow's: its reply is told apart).
	r.ev.tellRest(s, s+"/t4", "The flow's reply.")
	if got := tokens(); got != "" {
		t.Fatalf("a turn with no tokens: %q", got)
	}
}
