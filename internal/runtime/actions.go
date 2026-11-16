package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/r-hashi01/kairocode/internal/config"
	"github.com/r-hashi01/kairocode/internal/provider"
	"os"
	"path/filepath"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/lsp"
	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// toolDefs is every tool this runtime offers.
func (r *Runtime) toolDefs() []tools.Def {
	return append(tools.Builtin(), r.cfg.ExtraTools...)
}

func (r *Runtime) tool(name string) (tools.Def, bool) {
	for _, d := range r.toolDefs() {
		if d.Name == name {
			return d, true
		}
	}
	return tools.Def{}, false
}

func (r *Runtime) observe(o kairo.Observation) {
	if r.cfg.Observe != nil {
		r.cfg.Observe(o)
	}
	r.ev.observe(o)
	r.notifyObserve(o)
	if o.Kind != kairo.ObsRunStarted && o.Kind != kairo.ObsRunSettled {
		return
	}
	if o.Kind == kairo.ObsRunSettled && o.Status == "blocked" && r.mcp != nil {
		r.mcp.blocked(o.Parent)
	}
	if o.Kind == kairo.ObsRunSettled && o.Workflow == workflows.Flow && o.Status != "blocked" {
		run := o.RunID // kept to be looked at later (ADR 0029); kairo's observer must not block
		r.background(func(ctx context.Context) { r.keepSettled(ctx, run) })
	}
	r.mu.Lock()
	close(r.changed)
	r.changed = make(chan struct{})
	r.mu.Unlock()
}

// changes is closed at the next run that starts or settles here.
func (r *Runtime) changes() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.changed
}

// toolArgs is a call's arguments: given (MCP), or read from its message.
func (r *Runtime) toolArgs(ctx context.Context, in workflows.ToolInput) (json.RawMessage, error) {
	if in.Args != nil {
		return in.Args, nil
	}
	call, err := r.toolCall(ctx, in)
	if err != nil {
		return nil, err
	}
	return call.Input, nil
}

// planTool works a write out without making it. It only reads; the content
// it would write is stored by hash, so the record of the plan stays small.
func (r *Runtime) planTool(t *kairo.TaskContext, d tools.Def, in workflows.ToolInput) (workflows.PlanOutput, error) {
	args, err := r.toolArgs(t, in)
	if err != nil {
		return workflows.PlanOutput{}, err
	}
	root, err := os.OpenRoot(in.Dir)
	if err != nil {
		out, err := r.toolResult(t, in.Call, tools.Result{Output: "cannot open the working directory: " + err.Error(), IsError: true})
		return workflows.PlanOutput{Done: true, Result: out}, err
	}
	defer root.Close()
	if d.Name == tools.FlowPublishTool.Name {
		// Only the draft the user decided on (and reviewed) is published:
		// one changed since is not (ADR 0030).
		reviewed := store.Hash(store.KindReviewed, []byte(reviewKey(in, draftHash(in.Dir, args))))
		if _, _, err := r.Store.Get(t, reviewed); errors.Is(err, store.ErrNotFound) {
			out, err := r.toolResult(t, in.Call, tools.Result{IsError: true,
				Output: "not published: the draft changed after it was reviewed (or the user decided on none). Publish it again with flow_publish: the user reviews it as it is now."})
			return workflows.PlanOutput{Done: true, Result: out}, err
		} else if err != nil {
			return workflows.PlanOutput{}, err
		}
	}
	p := d.Plan(t, tools.Env{Root: root, Dir: in.Dir}, args)
	if p.Fail != nil {
		out, err := r.toolResult(t, in.Call, *p.Fail)
		return workflows.PlanOutput{Done: true, Result: out}, err
	}
	if d.Apply != nil {
		apply, err := json.Marshal(workflows.PlanApplyInput{Dir: in.Dir, Call: in.Call, Data: p.Data})
		return workflows.PlanOutput{Action: workflows.ActToolPrefix + d.Name + workflows.ActApplySuffix, Apply: apply}, err
	}
	content, err := r.Store.Put(t, store.KindContent, p.Content)
	if err != nil {
		return workflows.PlanOutput{}, err
	}
	apply, err := json.Marshal(workflows.ApplyInput{
		Dir: in.Dir, Call: in.Call, Path: p.Path, Before: p.Before, After: p.After, Content: content, Summary: p.Summary,
	})
	return workflows.PlanOutput{Action: workflows.ActFileApply, Apply: apply}, err
}

// applyPlan makes a tool's own plan; done again, the tool does not act twice.
func (r *Runtime) applyPlan(t *kairo.TaskContext, d tools.Def, in workflows.PlanApplyInput) (workflows.ToolOutput, error) {
	root, err := os.OpenRoot(in.Dir)
	if err != nil {
		return r.toolResult(t, in.Call, tools.Result{Output: "cannot open the working directory: " + err.Error(), IsError: true})
	}
	defer root.Close()
	res := d.Apply(t, tools.Env{Root: root, Dir: in.Dir,
		Items: itemEnv{r: r, conversation: ownerSession(t.RunID), key: t.IdempotencyKey}}, in.Data)
	if res.Unknown {
		return workflows.ToolOutput{}, kairo.Unknown(errors.New(res.Output))
	}
	out, err := r.toolResult(t, in.Call, res)
	if err != nil {
		return out, kairo.Unknown(fmt.Errorf("the tool acted, its result was not stored: %w", err))
	}
	return out, nil
}

// applyWrite makes a planned write (idempotent: compare, then replace).
func (r *Runtime) applyWrite(t *kairo.TaskContext, in workflows.ApplyInput) (workflows.ToolOutput, error) {
	kind, content, err := r.Store.Get(t, in.Content)
	if err != nil {
		return workflows.ToolOutput{}, err
	}
	if kind != store.KindContent {
		return workflows.ToolOutput{}, fmt.Errorf("%s is %s, not content", in.Content, kind)
	}
	root, err := os.OpenRoot(in.Dir)
	if err != nil {
		return r.toolResult(t, in.Call, tools.Result{Output: "cannot open the working directory: " + err.Error(), IsError: true})
	}
	defer root.Close()
	res := tools.Apply(tools.Env{Root: root, Dir: in.Dir}, in.Path, in.Before, in.After, content, in.Summary)
	if res.Unknown {
		return workflows.ToolOutput{}, kairo.Unknown(errors.New(res.Output))
	}
	if !res.IsError {
		if errs := r.lspErrors(t, in.Dir, in.Path, content); errs != "" {
			res.Output += "\n\n" + errs
		}
	}
	out, err := r.toolResult(t, in.Call, res)
	if err != nil {
		return out, kairo.Unknown(fmt.Errorf("the write was made, its result was not stored: %w", err))
	}
	return out, nil
}

// lspErrors are the errors the language server for the file at path (in
// dir) sees in content, as text for the write's result (ADR 0009). Which
// server, if any, is decided here from the settings, and shows in the
// result, which is recorded. No server, or none in time: "".
func (r *Runtime) lspErrors(ctx context.Context, dir, path string, content []byte) string {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return ""
	}
	servers, err := lsp.Servers(config.UserFile(), config.ProjectFile(real))
	if err != nil {
		r.logf("lsp: %v", err) // what is left out; the servers given are used
	}
	return r.lsp.Errors(ctx, servers, real, filepath.Join(real, filepath.FromSlash(path)), content)
}

// runTool runs one tool call in the working directory and stores its result.
// For a tool that acts (not Read), anything that goes wrong once it may have
// acted is an unknown outcome, never a plain failure (invariant 4): kairo
// stops a real tool for review and checks an idempotent one again.
func (r *Runtime) runTool(t *kairo.TaskContext, d tools.Def, in workflows.ToolInput) (workflows.ToolOutput, error) {
	args, err := r.toolArgs(t, in)
	if err != nil {
		return workflows.ToolOutput{}, err
	}
	root, err := os.OpenRoot(in.Dir)
	if err != nil {
		return r.toolResult(t, in.Call, tools.Result{Output: "cannot open the working directory: " + err.Error(), IsError: true})
	}
	defer root.Close()
	env := tools.Env{Root: root, Dir: in.Dir,
		Jobs:  jobEnv{m: r.jobs, session: ownerSession(t.RunID), dir: in.Dir, key: t.IdempotencyKey},
		Items: itemEnv{r: r, conversation: ownerSession(t.RunID), key: t.IdempotencyKey}}
	if d.Name == tools.FetchTool.Name || d.Name == tools.HTTPTool.Name {
		if env.Net, err = userNet(); err != nil {
			return r.toolResult(t, in.Call, tools.Result{Output: err.Error(), IsError: true})
		}
	}
	res := d.Run(t, env, args)
	if res.Unknown && d.Effect != tools.Read {
		return workflows.ToolOutput{}, kairo.Unknown(errors.New(res.Output))
	}
	out, err := r.toolResult(t, in.Call, res)
	if err != nil && d.Effect != tools.Read {
		return out, kairo.Unknown(fmt.Errorf("the tool ran, its result was not stored: %w", err))
	}
	return out, err
}

// toolCall finds the tool_use the input names in its assistant message.
func (r *Runtime) toolCall(ctx context.Context, in workflows.ToolInput) (msg.Part, error) {
	m, err := r.Store.Message(ctx, in.Message)
	if err != nil {
		return msg.Part{}, err
	}
	for _, p := range m.Parts {
		if p.Type == msg.ToolUse && p.ID == in.Call {
			return p, nil
		}
	}
	return msg.Part{}, fmt.Errorf("no tool call %s in %s", in.Call, in.Message)
}

func (r *Runtime) toolResult(ctx context.Context, id string, res tools.Result) (workflows.ToolOutput, error) {
	h, err := r.Store.PutMessage(ctx, msg.Message{Role: msg.User, Parts: []msg.Part{{
		Type: msg.ToolResult, ToolUseID: id, Text: res.Output, IsError: res.IsError,
	}}})
	return workflows.ToolOutput{Message: h, IsError: res.IsError}, err
}

func (r *Runtime) model(t *kairo.TaskContext, in workflows.ModelInput) (workflows.ModelReply, error) {
	var system string
	if err := r.Store.GetJSON(t, in.System, store.KindSystem, &system); err != nil {
		return workflows.ModelReply{}, err
	}
	var specs []provider.ToolSpec
	if err := r.Store.GetJSON(t, in.Tools, store.KindTools, &specs); err != nil {
		return workflows.ModelReply{}, err
	}
	hashes, err := r.Store.Conversation(t, in.History)
	if err != nil {
		return workflows.ModelReply{}, err
	}
	ms, err := r.Store.Messages(t, append(hashes, in.Turn...))
	if err != nil {
		return workflows.ModelReply{}, err
	}
	if ms, err = r.withImages(t, ms); err != nil {
		return workflows.ModelReply{}, err
	}
	// The text streams to those who watch the session (live output, not a
	// record: the message is recorded once it is whole).
	p, err := r.providerFor(t, in.Engine)
	if err != nil {
		return workflows.ModelReply{}, err
	}
	resp, err := p.Complete(t, provider.Request{Model: in.Model, Effort: in.Effort, System: system, Tools: specs, Messages: ms,
		OnText: func(text string) { t.Emit([]byte(text)) }})
	if in.Engine == "" && errors.Is(err, provider.ErrNoCredentials) {
		r.authSeen(ProviderAnthropic, AuthNeeded, nil)
	} else if in.Engine == "" && err == nil {
		r.authSeen(ProviderAnthropic, AuthSignedIn, nil)
	}
	if errors.Is(err, provider.ErrContextTooLong) {
		// Recorded as such: the turn compacts the conversation and asks
		// again (ADR 0020).
		return workflows.ModelReply{Stop: workflows.StopContext}, nil
	}
	if err != nil {
		return workflows.ModelReply{}, modelErr(t, err)
	}
	h, err := r.Store.PutMessage(t, resp.Message)
	if err != nil {
		return workflows.ModelReply{}, kairo.Retryable(err)
	}
	out := workflows.ModelReply{Message: h, Stop: resp.Stop, Usage: resp.Usage}
	for _, p := range resp.Message.Parts {
		if p.Type == msg.ToolUse {
			out.Calls = append(out.Calls, workflows.ToolCall{ID: p.ID, Name: p.Name})
		}
	}
	return out, nil
}

// turnSettled says whether turn n of a session was recorded cancelled
// (kairo#3). The session has its result, so its run has settled: the
// embedded runtime records a run's end before its parent is given it, and
// a turn is kept as long as its session runs.
func (r *Runtime) turnSettled(t *kairo.TaskContext, in workflows.TurnSettledInput) (workflows.TurnSettledOutput, error) {
	info, ok, err := r.TurnRun(t, in.Session, in.N)
	if err != nil {
		return workflows.TurnSettledOutput{}, kairo.Retryable(err)
	}
	if !ok || !info.Finished() {
		return workflows.TurnSettledOutput{}, fmt.Errorf("turn %d of %s: no settled run", in.N, in.Session)
	}
	return workflows.TurnSettledOutput{Cancelled: info.Status == "cancelled"}, nil
}

// modelErr is how a model call's failure is recorded: as it is when the
// call was cancelled or the provider said it will not go however often it
// is asked (provider.PermanentError); else retryable, a model call being
// safe to repeat (a reply, a summary, a decision).
func modelErr(t *kairo.TaskContext, err error) error {
	var permanent *provider.PermanentError
	if t.Err() != nil || errors.As(err, &permanent) {
		return err
	}
	return kairo.Retryable(err)
}
