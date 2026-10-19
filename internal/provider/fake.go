package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/r-hashi01/kairocode/internal/msg"
)

// Fake answers from a script, so tests never call a real model (AGENTS.md,
// invariant 9). The reply is chosen by how many assistant messages the request
// already holds, so it depends only on the conversation: a call repeated after
// a crash gets the same reply.
type Fake struct {
	Script []FakeReply `json:"script"`
	// CallLog, if set, gets one line per call ("<index>\n"), appended, so a
	// test can count calls across processes.
	CallLog string `json:"call_log,omitempty"`
	// PromptLog, if set, gets one line per call: the request's first user
	// text on one line, so a test can tell whose calls were made (the
	// subagent given a prompt).
	PromptLog string `json:"prompt_log,omitempty"`
	// HoldAt is the reply index at which the fake writes HoldFile and then
	// waits until its context ends, so a test can kill the process mid-call.
	// It applies only when HoldFile is set.
	HoldAt   int    `json:"hold_at"`
	HoldFile string `json:"hold_file,omitempty"`
	// HoldOn, if set, holds instead the calls whose last user message has
	// it in its text (one of agents run at once).
	HoldOn string `json:"hold_on,omitempty"`
	// Replies answers, in place of the script, a request whose first user
	// message has the key in its text (a subagent given that prompt).
	Replies map[string]FakeReply `json:"replies,omitempty"`
	// Decide, if set, answers a request that forces a tool (Request.Force):
	// it calls that tool with this input, whatever the script says.
	Decide json.RawMessage `json:"decide,omitempty"`
	// ReleaseFile, if set, ends the hold when it appears: the call then
	// answers from the script, so a test can let a held call finish.
	ReleaseFile string `json:"release_file,omitempty"`
}

type FakeReply struct {
	Text  string     `json:"text,omitempty"`
	Tools []FakeTool `json:"tools,omitempty"`
	// Stop overrides the stop reason (e.g. StopRefusal, StopLength).
	Stop string `json:"stop,omitempty"`
}

type FakeTool struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// LoadFake reads a Fake from a JSON file.
func LoadFake(path string) (*Fake, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f := &Fake{HoldAt: -1}
	if err := json.Unmarshal(b, f); err != nil {
		return nil, fmt.Errorf("fake provider %s: %w", path, err)
	}
	return f, nil
}

func (f *Fake) Complete(ctx context.Context, r Request) (Response, error) {
	i := 0
	for _, m := range r.Messages {
		if m.Role == msg.Assistant {
			i++
		}
	}
	if f.CallLog != "" {
		if err := appendLine(f.CallLog, strconv.Itoa(i)); err != nil {
			return Response{}, err
		}
	}
	if f.PromptLog != "" {
		if err := appendLine(f.PromptLog, strings.Join(strings.Fields(firstUserText(r.Messages)), " ")); err != nil {
			return Response{}, err
		}
	}
	if f.HoldFile != "" && (f.HoldOn == "" && i == f.HoldAt || f.HoldOn != "" && strings.Contains(lastUserText(r.Messages), f.HoldOn)) {
		if err := os.WriteFile(f.HoldFile, nil, 0o600); err != nil {
			return Response{}, err
		}
		if err := f.hold(ctx); err != nil {
			return Response{}, err
		}
	}
	if r.Force != "" && len(f.Decide) > 0 {
		return Response{Message: msg.Message{Role: msg.Assistant, Parts: []msg.Part{
			{Type: msg.ToolUse, ID: "toolu_force", Name: r.Force, Input: f.Decide}}}, Stop: StopToolUse,
			Usage: msg.Usage{Input: 10, Output: 5}}, nil
	}
	s, found := FakeReply{}, false
	if first := firstUserText(r.Messages); first != "" {
		for _, key := range slices.Sorted(maps.Keys(f.Replies)) {
			if strings.Contains(first, key) {
				s, found = f.Replies[key], true
				break
			}
		}
	}
	if !found {
		if i >= len(f.Script) {
			return Response{}, fmt.Errorf("fake provider: no reply %d in a script of %d", i, len(f.Script))
		}
		s = f.Script[i]
	}
	m := msg.Message{Role: msg.Assistant}
	if s.Text != "" {
		m.Parts = append(m.Parts, msg.Part{Type: msg.Text, Text: s.Text})
		if r.OnText != nil {
			for _, piece := range strings.SplitAfter(s.Text, " ") { // word by word, as a stream
				r.OnText(piece)
			}
		}
	}
	for j, t := range s.Tools {
		in := t.Input
		if len(in) == 0 {
			in = json.RawMessage(`{}`)
		}
		m.Parts = append(m.Parts, msg.Part{Type: msg.ToolUse, ID: fmt.Sprintf("toolu_%d_%d", i, j), Name: t.Name, Input: in})
	}
	stop := StopEnd
	if len(s.Tools) > 0 {
		stop = StopToolUse
	}
	if s.Stop != "" {
		stop = s.Stop
	}
	return Response{Message: m, Stop: stop, Usage: msg.Usage{Input: 10, Output: 5}}, nil
}

func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// hold waits until ctx ends (its error) or ReleaseFile appears (nil). A
// test fake: it looks for the file every few milliseconds.
func (f *Fake) hold(ctx context.Context) error {
	if f.ReleaseFile == "" {
		<-ctx.Done()
		return ctx.Err()
	}
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for {
		if _, err := os.Stat(f.ReleaseFile); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// lastUserText is the text of the last user message.
func lastUserText(ms []msg.Message) string {
	for i := len(ms) - 1; i >= 0; i-- {
		if ms[i].Role != msg.User {
			continue
		}
		var b strings.Builder
		for _, p := range ms[i].Parts {
			if p.Type == msg.Text {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

// firstUserText is the text of the first user message.
func firstUserText(ms []msg.Message) string {
	for _, m := range ms {
		if m.Role != msg.User {
			continue
		}
		var b strings.Builder
		for _, p := range m.Parts {
			if p.Type == msg.Text {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}
