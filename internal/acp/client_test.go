package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeAgent is the agent's end of a client's pipes: the test reads the
// client's requests and writes the agent's messages.
type fakeAgent struct {
	requests *bufio.Scanner
	w        io.Writer
}

func dialFake(t *testing.T, ctx context.Context, h ClientHandler) (*Client, *fakeAgent) {
	t.Helper()
	toAgentR, toAgentW := io.Pipe()
	fromAgentR, fromAgentW := io.Pipe()
	t.Cleanup(func() { toAgentR.Close(); fromAgentW.Close() })
	return Dial(ctx, fromAgentR, toAgentW, h), &fakeAgent{requests: bufio.NewScanner(toAgentR), w: fromAgentW}
}

// next is the client's next message.
func (a *fakeAgent) next(t *testing.T) message {
	t.Helper()
	if !a.requests.Scan() {
		t.Fatal("the client wrote nothing more")
	}
	var m message
	if err := json.Unmarshal(a.requests.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func (a *fakeAgent) send(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(a.w, line+"\n"); err != nil {
		t.Fatal(err)
	}
}

// Updates reach the handler in the order the agent sent them, all before
// the prompt's answer that follows them: a reply's chunks are not shuffled,
// and none is written after the turn has ended.
func TestClientKeepsTheOrderOfUpdates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var got []string // written by the handler only, read after Prompt
	cl, agent := dialFake(t, ctx, ClientHandler{Update: func(u Update) {
		time.Sleep(time.Duration(len(u.Text)%3) * time.Microsecond)
		got = append(got, u.Text)
	}})
	const n = 300
	go func() {
		m := agent.next(t)
		for i := range n {
			agent.send(t, fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s/x","update":`+
				`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"%d"}}}}`, i))
		}
		agent.send(t, `{"jsonrpc":"2.0","id":`+string(m.ID)+`,"result":{"stopReason":"end_turn"}}`)
	}()
	stop, err := cl.Prompt(ctx, "s/x", "hi")
	if err != nil || stop != "end_turn" {
		t.Fatal(stop, err)
	}
	if len(got) != n {
		t.Fatalf("%d updates before the answer, want %d", len(got), n)
	}
	for i, text := range got {
		if text != strconv.Itoa(i) {
			t.Fatalf("update %d is %q: %v", i, text, got)
		}
	}
}

// A request for permission withdrawn by the agent ($/cancel_request: it was
// answered elsewhere) ends the handler's ctx, and is answered "cancelled";
// one answered gives the option chosen.
func TestClientPermission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	asked := make(chan PermissionRequest, 2)
	_, agent := dialFake(t, ctx, ClientHandler{Permission: func(ctx context.Context, p PermissionRequest) PermissionAnswer {
		asked <- p
		if p.Title == "withdrawn" {
			<-ctx.Done()
			return PermissionAnswer{}
		}
		return PermissionAnswer{ID: p.Options[1].ID}
	}})
	ask := func(id, title string) {
		agent.send(t, `{"jsonrpc":"2.0","id":"`+id+`","method":"session/request_permission","params":{"sessionId":"s/x",`+
			`"toolCall":{"toolCallId":"c","title":"`+title+`","kind":"edit"},"options":[`+
			`{"optionId":"allow_once","name":"Allow","kind":"allow_once"},{"optionId":"reject_once","name":"Reject","kind":"reject_once"}]}}`)
	}
	ask("p1", "withdrawn")
	if p := <-asked; p.Title != "withdrawn" || p.ToolKind != "edit" || len(p.Options) != 2 || p.Options[0].Kind != "allow_once" {
		t.Fatalf("asked %+v", p)
	}
	agent.send(t, `{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":"p1"}}`)
	if m := agent.next(t); string(m.ID) != `"p1"` || !strings.Contains(string(m.Result), `"outcome":"cancelled"`) {
		t.Fatalf("the withdrawn request: %s %s", m.ID, m.Result)
	}
	ask("p2", "write x")
	<-asked
	if m := agent.next(t); string(m.ID) != `"p2"` || string(m.Result) != `{"outcome":{"optionId":"reject_once","outcome":"selected"}}` {
		t.Fatalf("the answer: %s %s", m.ID, m.Result)
	}
}

// A plan of KairoCode's carries its flow in _meta: the client gives it to
// the handler (Update.Flow); a plan of another agent has none.
func TestClientReadsAFlowPlan(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got := make(chan Update, 2)
	_, agent := dialFake(t, ctx, ClientHandler{Update: func(u Update) { got <- u }})
	agent.send(t, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s/x","update":{"sessionUpdate":"plan","entries":[],`+
		`"_meta":{"kairocode/flow":{"turn":"s/x/t","chart":{"name":"fix","nodes":[{"kind":"agent","title":"build","line":2,"col":10}]},"states":{"2:10":"running"}}}}}}`)
	agent.send(t, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s/x","update":{"sessionUpdate":"plan","entries":[]}}}`)
	if u := <-got; u.Flow == nil || u.Flow.Chart.Name != "fix" || u.Flow.States["2:10"] != "running" {
		t.Fatalf("flow %+v", u.Flow)
	}
	if u := <-got; u.Flow != nil {
		t.Fatalf("a plan with no flow: %+v", u.Flow)
	}
}
