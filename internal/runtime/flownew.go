package runtime

import (
	"context"
	"errors"
	"strings"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// flowNewPrompt is what /flow new asks the model (ADR 0003): to write a
// flow from the user's words. The user does not write Starlark; the model
// does, checks it with flow_check, and the user sees the flow's chart.
const flowNewPrompt = `Write a flow for this request:

%s

A flow is a Starlark script (Starlark is a small dialect of Python) at .kairocode/workflows/<name>.star. It runs with /flow <name> [words], survives restarts, and never redoes a step that finished. Write it with the flow_write tool (the only tool that writes flows): it checks the script first, and if it reports problems, fix them and write it again. To change a flow later, write it whole again with flow_write. Do not run the flow.

The script:
- DESCRIPTION = "one line saying what the flow does" at the top.
- LIMITS = {"agents": N, "steps": N, "tokens": N} if the flow needs other limits (all optional).
- def main(input): ... is where it starts; input["args"] is the words after /flow <name>.
- main returns a string (what the flow did), or {"ok": True/False, "message": "..."}.

What it can call (each returns a value with ok, output, error and value, never raises):
- agent(agent = "build", prompt = "...") runs an agent with the prompt; .output is its reply. Agents: build (can change files), plan (reads only), or any of .opencode/agents.
- parallel([{"agent": "...", "prompt": "..."}, ...]) runs agents at once; a list of results, in order.
- tool("bash", command = "..."), tool("read", path = "..."), and the other tools; the user's permission rules apply. Your MCP servers' and plugins' tools too, by the names you have them (tool("github_list_issues", owner = "...", repo = "...")), with the arguments their schemas take; .output is their result's text.
- ask("question", options = ["...", "..."]) asks the user and waits for the answer; .value is the option chosen.
- decide("question", type = "bool" | "number" | "choice", choices = [...]) has the model decide; .value is True/False, a number, or one of the choices.
- phase("name") names the part of the flow that follows. Give each part of the flow a phase.

Starlark, not Python: no f-strings (use "%%s" %% x or "{}".format(x)), no try/except (check .ok), no import, no classes, no recursion; for and while loops, if, def, lists, dicts and comprehensions are there. Bound loops (for round in range(5)).

With flow_write, give asked: what the user asked for, one thing at a time, as you took it, each with the phase that does it ({"need": "...", "phase": "..."}). The user reviews the flow against it before it is written. Write asked, DESCRIPTION, the questions of ask and the prompts in the language the user wrote the request in; phase names and the flow's name stay short lowercase words.

Choose a short name (lowercase letters, digits and -; not new, graph or runs). When it is ready, say in one or two sentences what the flow does and how to run it.`

// ErrNoFlowRequest: /flow new was given nothing to make a flow of.
var ErrNoFlowRequest = errors.New("/flow new needs what the flow is to do: /flow new <what it does>")

// NewFlow starts a turn in which the model writes a flow from the user's
// words (/flow new). The conversation keeps the request as it is asked of
// the model, so the turns after it can change the flow too.
func (r *Runtime) NewFlow(ctx context.Context, session, request string) (int, error) {
	request = strings.TrimSpace(request)
	if request == "" {
		return 0, ErrNoFlowRequest
	}
	text := strings.Replace(flowNewPrompt, "%s", request, 1)
	text = strings.ReplaceAll(text, "%%", "%")
	h, err := r.Store.PutMessage(ctx, msg.Message{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: text}}})
	if err != nil {
		return 0, err
	}
	return r.signal(ctx, session, workflows.SessionEvent{Kind: workflows.EventMessage, Message: h})
}
