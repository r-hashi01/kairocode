package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// Compacting the conversation (ADR 0020).

// summaryLead begins the message that stands for a compacted conversation.
const summaryLead = "[A summary of the conversation before this point. The earlier messages are not shown.]\n\n"

// modelContext is model's context in tokens: the user's kairocode.json
// (models.<name>.context), else the provider's.
func modelContext(model string) (int, error) {
	path := filepath.Join(userConfigDir(), ConfigFile)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return provider.ContextOf(model), nil
	}
	if err != nil {
		return 0, err
	}
	var cfg struct {
		Models map[string]struct {
			Context int `json:"context"`
		} `json:"models"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	if m, ok := cfg.Models[model]; ok && m.Context > 0 {
		return m.Context, nil
	}
	return provider.ContextOf(model), nil
}

// historyCompact summarizes the conversation (up to in.History, and the
// turn so far) and returns one that starts from the summary: a Base link
// whose Prev is the whole conversation, which is kept. A conversation too
// long to summarize whole is summarized from its later part.
func (r *Runtime) historyCompact(t *kairo.TaskContext, in workflows.CompactInput) (workflows.CompactOutput, error) {
	var specs []provider.ToolSpec
	if err := r.Store.GetJSON(t, in.Tools, store.KindTools, &specs); err != nil {
		return workflows.CompactOutput{}, err
	}
	hashes, err := r.Store.Conversation(t, in.History)
	if err != nil {
		return workflows.CompactOutput{}, err
	}
	ms, err := r.Store.Messages(t, append(hashes, in.Turn...))
	if err != nil {
		return workflows.CompactOutput{}, err
	}
	if ms, err = r.withImages(t, ms); err != nil {
		return workflows.CompactOutput{}, err
	}
	var resp provider.Response
	for {
		resp, err = r.cfg.Provider.Complete(t, provider.Request{Model: in.Model, Effort: in.Effort, System: provider.CompactSystem,
			Tools: specs, NoTools: true, Messages: withAsk(ms)})
		if !errors.Is(err, provider.ErrContextTooLong) {
			break
		}
		later := laterHalf(ms)
		if len(later) == 0 || len(later) == len(ms) {
			return workflows.CompactOutput{}, err
		}
		ms = later
	}
	if err != nil {
		var permanent *provider.PermanentError
		if t.Err() != nil || errors.As(err, &permanent) {
			return workflows.CompactOutput{}, err
		}
		return workflows.CompactOutput{}, kairo.Retryable(err) // a summary is safe to ask for again
	}
	text := ""
	for _, p := range resp.Message.Parts {
		if p.Type == msg.Text {
			text += p.Text
		}
	}
	if text == "" {
		return workflows.CompactOutput{}, kairo.Retryable(errors.New("the summary is empty"))
	}
	if resp.Stop == provider.StopLength {
		text += "\n\n[The summary was cut off here.]"
	}
	summary, err := r.Store.PutMessage(t, msg.Message{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: summaryLead + text}}})
	if err != nil {
		return workflows.CompactOutput{}, kairo.Retryable(err)
	}
	whole := in.History
	if len(in.Turn) > 0 {
		if whole, err = r.Store.Extend(t, in.History, in.Turn); err != nil {
			return workflows.CompactOutput{}, kairo.Retryable(err)
		}
	}
	head, err := r.Store.Compacted(t, whole, summary)
	if err != nil {
		return workflows.CompactOutput{}, kairo.Retryable(err)
	}
	return workflows.CompactOutput{Head: head, Usage: resp.Usage}, nil
}

// withAsk is the conversation, ending with the request to summarize it: in
// the last message if it is the user's (a turn's tool results), so the
// roles still alternate.
func withAsk(ms []msg.Message) []msg.Message {
	ask := msg.Part{Type: msg.Text, Text: provider.CompactAsk}
	out := append([]msg.Message(nil), ms...)
	if n := len(out); n > 0 && out[n-1].Role == msg.User && out[n-1].Native == nil {
		last := out[n-1]
		last.Parts = append(append([]msg.Part(nil), last.Parts...), ask)
		out[n-1] = last
		return out
	}
	return append(out, msg.Message{Role: msg.User, Parts: []msg.Part{ask}})
}

// laterHalf is the later half of a conversation, from a message of the
// user's own words (not a tool's result), so that no tool call is cut from
// its result.
func laterHalf(ms []msg.Message) []msg.Message {
	for i := len(ms) / 2; i < len(ms); i++ {
		if ownWords(ms[i]) {
			return ms[i:]
		}
	}
	return nil
}

func ownWords(m msg.Message) bool {
	if m.Role != msg.User {
		return false
	}
	for _, p := range m.Parts {
		if p.Type == msg.ToolResult {
			return false
		}
	}
	return true
}
