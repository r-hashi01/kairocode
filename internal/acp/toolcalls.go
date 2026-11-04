package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/flowchart"
	"github.com/r-hashi01/kairocode/internal/locale"
	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// ask asks the client for permission (or, for a review, whether the call
// took effect) and gives the answer to the daemon. The request waits in
// kairo, not here: if the client goes away unanswered, the next session/load
// asks again (ADR 0002).
func (a *agent) ask(session string, p api.Pending) {
	a.mu.Lock()
	if a.asks[p.Key] != nil {
		a.mu.Unlock()
		return // asked already, and not answered yet
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.asks[p.Key] = cancel
	a.mu.Unlock()
	// answers: what each option offered means (Approve's ok). Any other
	// answer is not taken: the request still waits in kairo.
	options := []map[string]any{
		{"optionId": "allow_once", "name": "Allow", "kind": "allow_once"},
		{"optionId": "reject_once", "name": "Reject", "kind": "reject_once"},
	}
	answers := map[string]bool{"allow_once": true, "reject_once": false}
	title := strings.TrimSpace(p.Tool + " " + p.Subject)
	ruleText := p.RuleText() // the rule, as the title says it
	question := p.Kind == api.KindQuestion
	kind := toolKind(p.Tool)
	if question {
		// A flow's question (ADR 0003): its options, each of a reject kind
		// for the reason a review's are (a client that allows by itself
		// must not answer for the user: a hook's too, nothing is run before
		// the user chose). Its first line is the title, the question whole
		// its content (a title is cut); a hook's has the script it runs.
		options, answers = nil, nil
		for i, o := range p.Options {
			id := fmt.Sprintf("answer_%d", i)
			options = append(options, map[string]any{"optionId": id, "name": o, "kind": "reject_once"})
		}
		first, _, _ := strings.Cut(strings.TrimSpace(p.Subject), "\n")
		if h, err := os.UserHomeDir(); err == nil && h != "" {
			first = strings.ReplaceAll(first, h+"/", "~/")
		}
		if r := []rune(first); len(r) > 100 {
			first = string(r[:100]) + "…"
		}
		title, kind = first, "other"
	} else if p.Kind == "review" {
		// Neither option is of an allow kind: a client that allows tool
		// calls by itself must not say a call took effect (invariant 4).
		options = []map[string]any{
			{"optionId": "did_not_take_effect", "name": "It did not take effect", "kind": "reject_once"},
			{"optionId": "took_effect", "name": "It took effect", "kind": "reject_once"},
		}
		answers = map[string]bool{"took_effect": true, "did_not_take_effect": false}
		title = api.ReviewQuestion + " " + title
	} else if p.Rule != "" {
		if p.Try != nil { // a flow: it can be tried first (ADR 0017)
			options = append(options, map[string]any{"optionId": TryOption, "name": "Try it first", "kind": "reject_once"})
		}
		rule, _, _ := strings.Cut(ruleText, "\n") // one line: the title is not a page
		if len([]rune(rule)) > 120 {
			rule = string([]rune(rule)[:120]) + "…"
		}
		title += " (" + rule + ")"
	}
	call, known := "", false
	if _, c, ok := strings.Cut(p.Key, "#"); ok && !strings.HasPrefix(c, workflows.MCPCallPrefix) && !strings.HasPrefix(c, workflows.FlowCallPrefix) {
		call, known = c, true // the model's tool call, as in its tool_call
	}
	if !known {
		// A review, Claude Code's call or a flow's own: no tool_call has
		// this id yet.
		call = p.Key
		a.update(session, map[string]any{"sessionUpdate": "tool_call", "toolCallId": call, "title": title,
			"kind": kind, "status": "pending"})
	}
	toolCall := map[string]any{"toolCallId": call, "title": title, "kind": kind, "status": "pending"}
	switch {
	case len(p.Sections) > 0: // what it would do, a block a section (SectionMeta names it)
		sections := make([]api.PreviewSection, len(p.Sections))
		for i, sec := range p.Sections {
			sections[i] = localized(a.lang, sec)
		}
		toolCall["content"] = reviewContent(sections, p.Try, a.dirOf(session))
	case p.Preview != "": // what it would do: a flow's shape
		toolCall["content"] = []any{map[string]any{"type": "content", "content": textBlock(p.Preview)}}
	case question: // the question whole; a hook's, with what it runs
		text := p.Subject
		if strings.Contains(p.Key, "#hook_") {
			text += hookScripts(p.Subject, a.dirOf(session)) +
				"\n\nNot to read Claude Code's hooks at all: \"agents\": {\"claudeHooks\": \"off\"} in ~/.config/kairocode/kairocode.json."
		}
		toolCall["content"] = []any{map[string]any{"type": "content", "content": textBlock(text)}}
	case !question && p.Kind != "review" && !strings.Contains(title, ruleText):
		// The reason, cut short in the title: all of it, to decide on.
		toolCall["content"] = []any{map[string]any{"type": "content", "content": textBlock(ruleText)}}
	}
	params := map[string]any{
		"sessionId": session,
		"toolCall":  toolCall,
		"options":   options,
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer func() {
			a.mu.Lock()
			delete(a.asks, p.Key)
			a.mu.Unlock()
			cancel()
		}()
		var resp struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
			Meta struct {
				Reason string `json:"kairocode/reason"`
			} `json:"_meta"`
		}
		if err := a.c.Call(ctx, "session/request_permission", params, &resp); err != nil {
			return // withdrawn, or the client went away: it still waits in kairo
		}
		if resp.Outcome.Outcome != "selected" {
			return
		}
		// A call only this request made (a question, a review, Claude
		// Code's or a flow's call) is settled with the answer: a client
		// marks the call by the option's kind (Zed, a reject kind as
		// refused), and the call says nothing more. It is settled before
		// the answer is given, so it shows before what the answer leads to;
		// an answer not taken asks again, and the call is pending again.
		settle := func(status, text string) {
			if !known || question { // a question's call is its own too
				a.update(session, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": call, "status": status,
					"content": []any{map[string]any{"type": "content", "content": textBlock(text)}},
					"_meta":   map[string]any{SettledMeta: true}})
			}
		}
		if question {
			var i int
			if _, err := fmt.Sscanf(resp.Outcome.OptionID, "answer_%d", &i); err != nil || i < 0 || i >= len(p.Options) {
				return // not an answer offered: it still waits in kairo
			}
			settle("completed", "Answered: "+p.Options[i])
			if err := a.b.Answer(a.ctx, p.Key, p.Options[i]); err != nil && !errors.Is(err, api.ErrAnswered) {
				a.o.Logger.Warn("acp: answering", "key", p.Key, "err", err)
			}
			return
		}
		if resp.Outcome.OptionID == TryOption && p.Try != nil {
			// The words to try it with come as the reason; the approval
			// is asked again when the try ends.
			settle("in_progress", "Trying it first: it is asked again when the try ends")
			if err := a.b.TryFlow(a.ctx, p.Key, resp.Meta.Reason); err != nil && !errors.Is(err, api.ErrAnswered) {
				a.o.Logger.Warn("acp: trying the flow", "key", p.Key, "err", err)
			}
			return
		}
		ok, offered := answers[resp.Outcome.OptionID]
		if !offered {
			return
		}
		// A refusal may say what to do instead: the model is told.
		switch {
		case p.Kind == "review" && ok:
			settle("completed", "It took effect")
		case p.Kind == "review":
			settle("completed", "It did not take effect")
		case ok:
			settle("completed", "Allowed")
		default:
			settle("failed", strings.TrimSpace("Refused "+resp.Meta.Reason))
		}
		if err := a.b.Approve(a.ctx, p.Key, ok, resp.Meta.Reason); err != nil && !errors.Is(err, api.ErrAnswered) {
			a.o.Logger.Warn("acp: answering", "key", p.Key, "err", err)
		}
	}()
}

// SettledMeta marks, in a tool_call_update's _meta, the call a request
// made settled with its answer: for a client that marks the call by the
// option chosen. kairo's terminal shows the answer its own way.
const SettledMeta = "kairocode/settled"

// FlowMeta is the key of a plan update's _meta that holds the flow's chart
// and states (flowchart.View), for a client that draws it (kairo's TUI).
const FlowMeta = "kairocode/flow"

// flowPlan is a running flow as an ACP plan (ADR 0003): an entry per call
// of its script, in order, with its state. A client that knows the chart
// finds it in _meta.
func flowPlan(v *flowchart.View) map[string]any {
	entries := []any{}
	var walk func([]flowchart.Node)
	walk = func(nodes []flowchart.Node) {
		for _, n := range nodes {
			if n.Line != 0 && n.Kind != flowchart.Parallel {
				content := flowchart.Label(n)
				if n.Text != "" {
					content += ": " + n.Text
				}
				status := "pending"
				switch v.States[n.Key()] {
				case api.FlowRunning, api.FlowWaiting:
					status = "in_progress"
				case api.FlowDone, api.FlowFailed:
					status = "completed"
				}
				entries = append(entries, map[string]any{"content": content, "priority": "medium", "status": status})
			}
			walk(n.Then)
			walk(n.Else)
			walk(n.Body)
			for _, b := range n.Branches {
				walk(b)
			}
		}
	}
	walk(v.Chart.Nodes)
	return map[string]any{"sessionUpdate": "plan", "entries": entries, "_meta": map[string]any{FlowMeta: v}}
}

// toolKind is ACP's kind of a tool.
func toolKind(tool string) string {
	switch tool {
	case "read":
		return "read"
	case "edit", "write", "apply_patch":
		return "edit"
	case "glob", "grep", "list":
		return "search"
	case "bash":
		return "execute"
	case "webfetch", "fetch", "http":
		return "fetch"
	}
	return "other"
}

// title names a call for the user: its tool and what it acts on.
func title(tool string, input json.RawMessage) string {
	var args map[string]any
	_ = json.Unmarshal(input, &args)
	keys := []string{"path", "command", "pattern", "url"}
	if tool == "fetch" || tool == "http" { // judged by their URL alone (ADR 0013)
		keys = []string{"url"}
	}
	if tool == tools.FlowWriteTool.Name || tool == tools.FlowPublishTool.Name { // the flow's name
		keys = []string{"name"}
	}
	for _, k := range keys {
		if v, ok := args[k].(string); ok && v != "" {
			return tool + " " + v
		}
	}
	if tool == "" {
		return "tool call"
	}
	return tool
}

func toolCall(id, tool string, input json.RawMessage, status string) map[string]any {
	u := map[string]any{"sessionUpdate": "tool_call", "toolCallId": id, "title": title(tool, input),
		"kind": toolKind(tool), "status": status}
	if len(input) > 0 {
		u["rawInput"] = input
	}
	return u
}

func toolResult(id, tool, output string, isError bool) map[string]any {
	status := "completed"
	if isError {
		status = "failed"
	}
	u := map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": id, "status": status,
		"content": []any{map[string]any{"type": "content", "content": textBlock(output)}}}
	if tool != "" {
		u["kind"] = toolKind(tool)
	}
	return u
}

// localized is a section of a flow's review in lang: KairoCode's own
// words in it, and in its chart's boxes what KairoCode says a call sends.
// The script, and a change's before and after, are as written.
func localized(lang string, s api.PreviewSection) api.PreviewSection {
	if s.Name == "Script" {
		return s
	}
	s.Text = locale.Review(lang, s.Text)
	if len(s.Chart) > 0 {
		var c flowchart.Chart
		if json.Unmarshal(s.Chart, &c) == nil {
			var walk func([]flowchart.Node)
			walk = func(nodes []flowchart.Node) {
				for i := range nodes {
					if strings.HasPrefix(nodes[i].Text, "sends ") { // KairoCode's, not the script's
						nodes[i].Text = locale.Review(lang, nodes[i].Text)
					}
					walk(nodes[i].Then)
					walk(nodes[i].Else)
					walk(nodes[i].Body)
					for _, b := range nodes[i].Branches {
						walk(b)
					}
				}
			}
			walk(c.Nodes)
			if b, err := json.Marshal(c); err == nil {
				s.Chart = b
			}
		}
	}
	return s
}
