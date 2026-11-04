package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// stoppedNote is what the conversation says of a turn the user stopped
// (ADR 0011), in the assistant's place: the next turn's model reads it.
const stoppedNote = "[The user stopped this turn before it finished. Tool calls made in it may have changed the working files; look before going on.]"

// historyCancelled keeps a stopped turn in the conversation: the message
// it was asked with, and the note.
func (r *Runtime) historyCancelled(t *kairo.TaskContext, in workflows.HistoryCancelledInput) (string, error) {
	note, err := r.Store.PutMessage(t, msg.Message{Role: msg.Assistant, Parts: []msg.Part{{Type: msg.Text, Text: stoppedNote}}})
	if err != nil {
		return "", err
	}
	return r.Store.Extend(t, in.Prev, []string{in.Message, note})
}

// lastLink is the one of heads that has all the others before it in its
// links: the conversation's end ("" for none). The links of one session
// form one chain, each head a later end of it.
func (r *Runtime) lastLink(ctx context.Context, heads []string) (string, error) {
	last := ""
	for _, h := range heads {
		if h == "" || h == last {
			continue
		}
		if last == "" {
			last = h
			continue
		}
		later, err := r.linkReaches(ctx, h, last)
		if err != nil {
			return "", err
		}
		if later {
			last = h
		}
	}
	return last, nil
}

// linkReaches says whether target is from or one of its links before it.
func (r *Runtime) linkReaches(ctx context.Context, from, target string) (bool, error) {
	for h := from; h != ""; {
		if h == target {
			return true, nil
		}
		var l store.Link
		if err := r.Store.GetJSON(ctx, h, store.KindHistory, &l); err != nil {
			return false, err
		}
		h = l.Prev
	}
	return false, nil
}

// HistoryLive is the session's conversation as a client shows it again:
// History, then what the turns under way were asked (a turn waiting for an
// approval is not in the history yet; shown without it, the approval would
// come with nothing before it).
func (r *Runtime) HistoryLive(ctx context.Context, session string) ([]msg.Message, error) {
	ms, err := r.History(ctx, session)
	if err != nil {
		return nil, err
	}
	runs, err := r.K.Children(ctx, session)
	if err != nil {
		return nil, err
	}
	type asked struct {
		n   int
		h   string
		run string
	}
	var live []asked
	for _, t := range runs {
		if t.Workflow != workflows.Turn || t.Finished() {
			continue
		}
		var in workflows.TurnInput
		if json.Unmarshal(input(t), &in) != nil || in.Sub || in.Undo != nil || in.Compact || in.Message == "" {
			continue
		}
		live = append(live, asked{in.N, in.Message, t.RunID})
	}
	slices.SortFunc(live, func(a, b asked) int { return a.n - b.n })
	for _, l := range live {
		m, err := r.Store.Message(ctx, l.h)
		if err != nil {
			return nil, err
		}
		ms = append(ms, m)
		so, err := r.turnSoFar(ctx, l.run)
		if err != nil {
			return nil, err
		}
		ms = append(ms, so...)
	}
	return ms, nil
}

// turnSoFar is what a turn under way has done so far, as messages: the
// model's replies and the results of its calls (the API engine's), or its
// calls over MCP and their results (a CLI engine's, whose replies come at
// the turn's end). A call still waiting has its call and no result.
func (r *Runtime) turnSoFar(ctx context.Context, turn string) ([]msg.Message, error) {
	steps, err := r.K.Children(ctx, turn)
	if err != nil {
		return nil, err
	}
	var out []msg.Message
	seen := map[string]bool{}
	add := func(h string) error {
		if h == "" || seen[h] {
			return nil
		}
		seen[h] = true
		m, err := r.Store.Message(ctx, h)
		if err != nil {
			return err
		}
		out = append(out, m)
		return nil
	}
	for _, c := range steps {
		if c.Status != "completed" {
			continue
		}
		var o struct {
			Message string `json:"message"`
		}
		switch plan := strings.TrimPrefix(c.Plan, "kairo.call/"); {
		case plan == workflows.ActModel,
			strings.HasPrefix(plan, workflows.ActToolPrefix) && !strings.HasSuffix(plan, workflows.ActPlanSuffix):
			if json.Unmarshal(c.Output, &o) == nil {
				if err := add(o.Message); err != nil {
					return nil, err
				}
			}
		}
	}
	// A CLI engine's calls, over MCP: a run of their own each (kairo#4), its input the call.
	runs, err := r.Store.MCPRuns(ctx, turn)
	if err != nil {
		return nil, err
	}
	for _, id := range runs {
		info, err := r.K.Get(ctx, id)
		if errors.Is(err, kairo.ErrUnknownRun) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var in workflows.MCPCallInput
		if json.Unmarshal(input(info), &in) != nil {
			continue
		}
		out = append(out, msg.Message{Role: msg.Assistant, Parts: []msg.Part{{Type: msg.ToolUse, ID: id, Name: in.Tool, Input: in.Args}}})
		if info.Status != "completed" {
			continue // still running, or waiting: its call alone
		}
		o, err := kairo.Await[workflows.ToolOutput](ctx, r.K, id)
		if err != nil || o.Message == "" {
			continue
		}
		res, err := r.Store.Message(ctx, o.Message)
		if err != nil {
			return nil, err
		}
		text := ""
		if len(res.Parts) > 0 {
			text = res.Parts[0].Text
		}
		out = append(out, msg.Message{Role: msg.User, Parts: []msg.Part{{Type: msg.ToolResult, ToolUseID: id, Text: text, IsError: o.IsError}}})
	}
	return out, nil
}

// History is the session's conversation as of its last finished turn: what
// a client that comes back shows (ACP's session/load).
func (r *Runtime) History(ctx context.Context, session string) ([]msg.Message, error) {
	if _, err := r.K.Get(ctx, session); err != nil {
		return nil, err
	}
	runs, err := r.K.Children(ctx, session)
	if err != nil {
		return nil, err
	}
	// The conversation's last link: of the last turn completed, or of a
	// turn the user stopped after it (its message and note kept by
	// history.cancelled, ADR 0011). Which came last is read from the links
	// themselves, not a clock: the last one has the others before it.
	lastN, run := -1, ""
	var heads []string
	for _, t := range runs {
		switch {
		case t.Workflow == workflows.Turn && t.Status == "completed":
			var in workflows.TurnInput
			if json.Unmarshal(input(t), &in) == nil && !in.Sub && in.N > lastN {
				lastN, run = in.N, t.RunID
			}
		case t.Plan == "kairo.call/"+workflows.ActHistoryCancelled && t.Status == "completed":
			var h string
			if err := json.Unmarshal(t.Output, &h); err != nil {
				return nil, fmt.Errorf("history.cancelled's link: %w", err)
			}
			heads = append(heads, h)
		}
	}
	if run != "" {
		out, err := kairo.Await[workflows.TurnOutput](ctx, r.K, run)
		if err != nil {
			return nil, err
		}
		heads = append(heads, out.Head)
	}
	head, err := r.lastLink(ctx, heads)
	if err != nil {
		return nil, err
	}
	if head == "" {
		return nil, nil
	}
	hashes, err := r.Store.Conversation(ctx, head)
	if err != nil {
		return nil, err
	}
	return r.Store.Messages(ctx, hashes)
}
