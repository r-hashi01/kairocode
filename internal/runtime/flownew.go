package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/api"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// flowNewPrompt is what /flow new asks the model (ADR 0003): to write a
// flow from the user's words. The user does not write Starlark; the model
// does, checks it with flow_check, and the user sees the flow's chart.
const flowNewPrompt = `Write a flow for this request:

%s

A flow is a Starlark script (Starlark is a small dialect of Python). It runs with /flow <name> [words], survives restarts, and never redoes a step that finished. Write it as a draft with the flow_write tool (the only tool that writes flows; it goes to .kairocode/workflows/drafts/<name>.star): it checks the script first, and if it reports problems, fix them and write it again. To change it, write it whole again with flow_write. A draft does not run: the user tries it with /flow <name> --draft, and publishes it with /flow <name> --publish (or asks you to: then call flow_publish), reviewing it then. Do not run the flow, and do not publish it unless asked.

The script:
- DESCRIPTION = "one line saying what the flow does" at the top.
- ARGS = "<what> [what else]" at the top, if the flow reads input["args"]: what to give it after /flow <name>, as a usage line (shown by /flow <name> --help).
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

With flow_write and flow_publish, give asked: what the user asked for, one thing at a time, as you took it, each with the phase that does it ({"need": "...", "phase": "..."}). The user reviews the flow against it before it is published. Write asked, DESCRIPTION, the questions of ask and the prompts in the language the user wrote the request in; phase names and the flow's name stay short lowercase words.

Choose a short name (lowercase letters, digits and -; not new, graph or runs). When the draft is written, say in one or two sentences what the flow does, and how to try it (/flow <name> --draft) and publish it (/flow <name> --publish).`

var ErrNoFlowRequest = api.ErrNoFlowRequest

// flowFromSession is the request /flow new --from-session makes of a
// session's record (ADR 0036).
const flowFromSession = `Make a flow that repeats what was done in the session whose record is below: what it did, not what it said.
- What would differ from one run to the next (a version, a branch, a file, a name) becomes ARGS.
- Where a person approved something that mattered or chose between options, the flow asks with ask(). Where the model judged, it uses decide() or an agent().
- Steps that went wrong and were done again are left out: keep only the path that worked.
- In asked, list the steps you took from the session, each with the phase that does it, so the user can compare the flow with the session.
%s
The record of session %s (from kairocode log; long parts are cut):

%s`

// flowSourceMax is how much of a record /flow new --from-session gives the
// model (ADR 0036).
const flowSourceMax = 60_000

// fromSessionFlag is the option of /flow new that makes a flow from a
// session's record.
const fromSessionFlag = "--from-session"

// NewFlow starts a turn in which the model writes a flow from the user's
// words (/flow new), or from a session's record (/flow new
// --from-session[=<id>] [words], ADR 0036). The conversation keeps the
// request as it is asked of the model, so the turns after it can change
// the flow too.
func (r *Runtime) NewFlow(ctx context.Context, session, request string) (int, error) {
	request = strings.TrimSpace(request)
	if first, rest, _ := strings.Cut(request, " "); first == fromSessionFlag || strings.HasPrefix(first, fromSessionFlag+"=") {
		from := session
		if id, ok := strings.CutPrefix(first, fromSessionFlag+"="); ok {
			from = "s/" + strings.TrimPrefix(id, "s/")
		}
		var err error
		if request, err = r.flowFromSession(ctx, session, from, strings.TrimSpace(rest)); err != nil {
			return 0, err
		}
	}
	if request == "" {
		return 0, ErrNoFlowRequest
	}
	text := strings.Replace(strings.ReplaceAll(flowNewPrompt, "%%", "%"), "%s", request, 1)
	h, err := r.Store.PutMessage(ctx, msg.Message{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: text}}})
	if err != nil {
		return 0, err
	}
	return r.signal(ctx, session, workflows.SessionEvent{Kind: workflows.EventMessage, Message: h})
}

// flowFromSession is the request to make a flow of from's record, read
// now (so the turn this starts is not in it), for session: from is
// session itself or another of its folder.
func (r *Runtime) flowFromSession(ctx context.Context, session, from, words string) (string, error) {
	dir, err := r.sessionDir(ctx, session)
	if err != nil {
		return "", err
	}
	l, err := r.log(ctx, from, flowSource)
	if errors.Is(err, kairo.ErrUnknownRun) || err == nil && (sameDir(l.Dir) != sameDir(dir) || len(l.Turns) == 0) {
		return "", ErrNoFlowSource
	}
	if err != nil {
		return "", err
	}
	if words != "" {
		words = "\nWhat the user wants of it: " + words + "\n"
	}
	turns := make([]string, len(l.Turns))
	for i, t := range l.Turns {
		turns[i] = l.TurnText(t)
	}
	record := l.Head() + "\n" + strings.Join(fitTurns(turns, flowSourceMax), "\n")
	return fmt.Sprintf(flowFromSession, words, strings.TrimPrefix(from, "s/"), record), nil
}

// fitTurns are turns cut to about max characters: turns in the middle
// left out first (the first request and the last turns matter most), with
// a line saying how many; if the first and last are still too long, each
// is cut.
func fitTurns(turns []string, max int) []string {
	size := 0
	for _, t := range turns {
		size += len([]rune(t))
	}
	n := len(turns)
	lo, hi := n/2, n/2 // turns[lo:hi] are left out
	for size > max && lo+n-hi > 2 {
		// The kept turn nearest the middle: the end of the longer side,
		// never the first or the last.
		if lo >= n-hi && lo > 1 || hi == n-1 {
			lo--
			size -= len([]rune(turns[lo]))
		} else {
			size -= len([]rune(turns[hi]))
			hi++
		}
	}
	out := slices.Clone(turns[:lo])
	if hi > lo {
		out = append(out, fmt.Sprintf("[%d turns in the middle left out]\n", hi-lo))
	}
	out = append(out, turns[hi:]...)
	if size > max {
		for i := range out {
			out[i] = clip(out[i], max/len(out))
		}
	}
	return out
}

var ErrNoFlowSource = api.ErrNoFlowSource
