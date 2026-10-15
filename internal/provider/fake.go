package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
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
	// HoldAt is the reply index at which the fake writes HoldFile and then
	// waits until its context ends, so a test can kill the process mid-call.
	// It applies only when HoldFile is set.
	HoldAt   int    `json:"hold_at"`
	HoldFile string `json:"hold_file,omitempty"`
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
	if f.HoldFile != "" && i == f.HoldAt {
		if err := os.WriteFile(f.HoldFile, nil, 0o600); err != nil {
			return Response{}, err
		}
		if err := f.hold(ctx); err != nil {
			return Response{}, err
		}
	}
	if i >= len(f.Script) {
		return Response{}, fmt.Errorf("fake provider: no reply %d in a script of %d", i, len(f.Script))
	}
	s := f.Script[i]
	m := msg.Message{Role: msg.Assistant}
	if s.Text != "" {
		m.Parts = append(m.Parts, msg.Part{Type: msg.Text, Text: s.Text})
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
