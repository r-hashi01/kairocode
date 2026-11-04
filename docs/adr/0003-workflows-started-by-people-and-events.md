# 0003 Workflows are started by people and events. Their body is Starlark, written from words, and shown in the terminal as a flow chart

- Status: Accepted (2026-10-19)
- Date: 2026-10-09 (the same day's proposals "treat them as skills" and "write them in JavaScript" were rewritten after the user pointed out problems). On 2026-10-15, the example of a development workflow (5) was added. On 2026-10-19, how flows are made (made from words, the second half of 1) and how they are shown (the flow chart, 4) were rewritten after the user pointed out problems
- Related: requirements FR-13, FR-14, FR-15, FR-24, FR-26; design section 6; ADR 0001, 0002 (the shape of commands and agents), 0005 (sandbox); kairo's ADR 0049, 0060

## Context

In KairoCode, combinations of several subagents (in parallel, in order, repeated, branching on results) can run as kairo's durable workflows. Even after a crash, finished subagents are not redone, and a workflow can wait for days for a person's approval or an outside event (design section 6).

This ADR was rewritten three times on the same day. Each time it was because of the user's comments.

- **Why we dropped the skill shape (the first proposal):** a skill is something that "is provided but not used". If whether to use it is left to the model's judgment, it gets skipped. In this repository's harness too, "verify before finishing" is always run by the Stop hook.
- **Why we dropped the JSON plan (design section 6.2):** a home-grown format needs extra work to make the model and people understand its content.
- **Why we went through JavaScript to Starlark:** the user wanted the Python way of writing. Real Python can touch the OS, so it needs a sandbox, determinism can only be kept by the environment, and it needs a Python runtime. With Starlark (a dialect of Python), the following properties come from the language itself.

On 2026-10-09 we confirmed the following with `go.starlark.net`.

- **What ran as is:** `for`, `while` (optional), dicts, comprehensions, `def` and `return`, `sorted`, `%` and `.format()`.
- **What becomes an error with line and column at the syntax stage:** f-strings, `try/except`, `class`, `import`, `time`.
- **Determinism:** the language has no clock, random numbers or I/O. The order of iterating a dict is the insertion order, the same every time.
- **How it is stopped:** an infinite loop is stopped by a limit on the number of steps (`SetMaxExecutionSteps`).
- **Dependencies:** it is pure Go, and the only added dependency is `golang.org/x/sys`.

For comparison, we also tried a Go interpreter (`github.com/traefik/yaegi` v0.16.1). We could narrow what is given to the script, but correct generic code failed with type errors.

## Decision

### 1. Do not let the model choose whether to use them. There are three entry points

Workflows live in `.kairocode/workflows/<name>.star` and are started from the following three entry points.

1. **A person calls it (`/flow <name>`).** All flows are grouped under one command, `/flow` (2026-10-19, the user's decision).
   - `/flow <name> [arguments]` runs one.
   - Typing just `/flow` shows the list (names and descriptions).
   - `/flow new <description>` makes one from words (below). So a flow named `new` cannot be made.
   - Names do not collide with project commands (`.opencode/commands`). In ACP's list of slash commands (`available_commands_update`), only one `flow` appears (ADR 0002). In the TUI, flow names are completed after `/flow `.
2. **An event starts it (hook).** `hooks` in `kairocode.json` binds workflows to events. A bound workflow always runs at that point.

   | Event | When | Milestone |
   |---|---|---|
   | `turn.end` | before a turn returns its response | M3 |
   | `tool.after` | after a tool. Narrowed by the tool's name and the shape of its arguments | M3 |
   | `session.start` | at the start of a session | M3 |
   | `github.*`, `ci.result`, `schedule` | outside events (an Issue comment, a CI result, a schedule) | M4 |

   - Workflows for `turn.end` and `tool.after` return `{"ok": bool, "message": str}`.
   - If `ok` is false, `message` is returned to the model and the loop continues. There is a limit on how many times it continues.
3. **Make it the agent's way of working itself.** If an agent definition (ADR 0002) says `workflow: <name>`, that agent's turns proceed with that workflow instead of the default loop.

The path where the model writes a script on the spot and runs it (the `workflow` tool) also stays. In that case, the structure chart (4 below) is shown before running and the user confirms. There is no path where the model picks a saved workflow by name and calls it.

**They are made from words (2026-10-19).** Only some users can write Python, so people are not made to write scripts.

1. The user asks in words (`/flow new <description>`, or in the normal conversation).
2. The model writes a Starlark script. KairoCode checks the syntax and determinism, and if it passes, writes it to `.kairocode/workflows/<name>.star` and shows the flow chart (4 below). If it does not pass, the errors go back to the model with line and column, to fix.
3. There is no confirmation menu. To change something after looking at the chart, there are two ways.
   - **Change the chart directly:** select a box with ↑↓ and press Enter to rewrite its content (prompt, command, question). `d` deletes it, and Shift+↑↓ swaps the order within the same level. A change rewrites only that call's string arguments and the order of statements on the script's syntax tree, and writes the result back to the file.
   - **Ask in words:** type in the usual input box and send with Enter (for example, "add tests after the review"). The model fixes the script and the chart is redrawn. Changes the chart cannot make, such as adding a box or changing a branch, are made this way.
4. It runs with `/flow <name>`. There is no save step. The file as it is is the latest form, and changes are visible in git.

People can use flows without reading the script. For those who want to read it or fix it by hand, the script stays as it is in the `.star` file.

### 2. The body is written in Starlark

```python
DESCRIPTION = "Review the changed files in parallel and fix them, repeating until the tests pass"
INPUT = {"files": list}
LIMITS = {"agents": 12, "tokens": 2000000}

def main(input):
    parallel([
        {"agent": "reviewer", "prompt": "Find and fix type errors in %s" % f}
        for f in input["files"]
    ])
    for round in range(3):
        test = tool("bash", command = "go test ./...")
        if test.exit_code == 0:
            return {"ok": True}
        agent(agent = "build", prompt = "The tests failed. Fix them:\n" + test.output)
    return {"ok": False, "message": "the tests do not pass after 3 fixes"}
```

- **Primitives (the first version):**

  | How it is written | What it does | Its form in kairo |
  |---|---|---|
  | `agent(agent=, prompt=, schema=None)` | One subagent. With `schema`, it returns a typed value | `kairo.Child("agent", …)` |
  | `parallel([spec, …])` | Runs `agent` specs (dicts) in parallel and returns the results in order | `kairo.Parallel` |
  | `decide(prompt=, type=)` | A typed decision (`bool`, `enum`, `number`; design section 6.3) | The model's structured output |
  | `tool(name, **args)` | Calls a tool directly. The permission decision, the effect type and the sandbox (ADR 0005) stay as for the tool | The tool's action |
  | `ask(question=, options=None)` | Asks a person and waits (`session/request_permission` in ACP) | `kairo.WaitFor` |
  | `wait(event, timeout=)` | Waits for an outside event (M4) | `kairo.WaitFor` |
  | `now()`, `random()` | Recorded time and random numbers | `ctx.Now`, `ctx.Random` |

- **Failures are returned as values.** Starlark has no `try/except`. So primitives do not turn expected failures (a tool failure, an agent failure, a refusal) into exceptions; they return a value with `ok`, `output` and `error`. The script branches on it with `if`. Unexpected failures (a limit exceeded, a mistake in the script) stop the workflow.
- **Parallelism runs on the Go side.** What is passed to `parallel` is not functions but an array of specs (dicts). The script's flow stays a single thread.
- **How it runs:**
  - It runs inside the KairoCode process with `go.starlark.net`. No OS sandbox is needed (ADR 0005).
  - On resume, the script runs again from the top, and primitive calls return their results from the record (the same as kairo's code-first API; a call's ID is decided by the order of calls and the input).
  - The syntax is checked before running. Errors go back to the model with line and column, to fix. For differences from Python that are commonly hit, such as f-strings, a way to fix them is added.
- **How determinism is kept:**
  - The only ways out of the language are the primitives above.
  - The script's content is recorded with its content hash when it starts (invariant 2 in AGENTS.md). Even if the file is rewritten while it runs, the running run finishes with the script it started with.
  - There is a limit on the number of steps.
  - **Detecting divergence:** on resume, if the script tries to make a call that differs from the record, it stops without running it. For example, a new call comes while there are still records not yet replayed. Starlark is deterministic, so this should not happen. It is a safety net for cases such as upgrading KairoCode's version.
- **Limits:** `LIMITS` (`agents`, `depth`, `steps`, `tokens`) are counted with recorded values each time a primitive is called, and the run stops when one is exceeded.

### 3. Permissions

- `agent` and `tool` inside a workflow go through the session's permission rules, effect types and sandbox as they are. A workflow cannot widen permissions by itself.
- For a hook placed in a repository, the user is asked once, before it first runs in that repository (which event runs what). The answer is recorded.

### 4. Visible in the terminal as a flow chart

The body is Starlark, but what people see is not code; it is a chart of connected nodes like a Dify workflow. No browser screen is built; it is drawn inside the terminal (the user's choice, 2026-10-19).

- **Nodes:** each primitive call in the script (`agent`, `tool`, `ask`, `decide`, `wait`) becomes one box.
  - A box shows the kind and the gist (the agent's name and the start of the prompt, the tool's command, the question).
  - `phase("review")` gives a phase name to the calls that follow. The chart has a heading for each phase.
- **Shape:** built from the script's syntax tree (`go.starlark.net/syntax`).
  - Order: connected with lines from top to bottom
  - `if`: splits left and right, then joins. The condition is written at the split
  - `parallel`, comprehensions: placed side by side. When the number is not known, one box says `× files`
  - `for`, `while`: shown with an enclosure and `⟳`, with the range (`range(3)`)
  - `return`: an end box
  - A call to a `def` inside the script is drawn with its body expanded (recursion stops at one level). Computation that calls no primitives is not drawn.

```
          ╭─────────────────────╮
          │ ▶ start title,detail│
          ╰──────────┬──────────╯
          ╭──────────┴──────────╮
          │ ✓ plan  agent plan  │ 18s
          ╰──────────┬──────────╯
          ╭──────────┴──────────╮
          │ ✓ confirm  proceed? │──stop──▶ ■ end
          ╰──────────┬──────────╯
       ╭─────────────┼─────────────╮
  ╭────┴─────╮  ╭────┴─────╮  ╭────┴─────╮
  │✓ reviewer│  │⏺ reviewer│  │○ reviewer│
  │  a.go    │  │  b.go    │  │  c.go    │
  ╰────┬─────╯  ╰────┬─────╯  ╰────┬─────╯
       ╰─────────────┼─────────────╯
     ╭ ⟳ for round in range(5)   2/5 ─────╮
     │    ╭──────────┴──────────╮         │
     │    │ ✓ build  agent build│         │
     │    ╰──────────┬──────────╯         │
     │    ╭──────────┴──────────╮         │
     │    │⏺ check bash check.sh│         │
     │    ╰─────────────────────╯         │
     ╰────────────────────────────────────╯
```

- **Colors and marks** (ADR 0010's color scheme)

  | Mark | Color | Meaning |
  |---|---|---|
  | `○` | gray | not yet |
  | `⏺` | yellow | running |
  | `✓` | green | finished |
  | `✗` | red | failed |
  | `⏸` | orange | waiting for a person |

  - The state while running is taken from kairo's record (the tree of calls).
  - Each call records its line in the script, which ties it to a box in the chart.
  - A box inside a loop shows the state of the current round, with `2/5` added.
  - Once the number of parallel items is decided, the box expands to the actual number.
- **When width or height is not enough**
  - Parallel items are folded into one box (`reviewer × 10  ✓6 ⏺3 ✗1`).
  - If the width is still not enough, it switches to a tree-shaped text chart.
  - When the height exceeds the screen, the area around what is running is shown.
- **Where it is shown**
  - When making: once written, the chart with everything `○` is shown above the input box. It is changed by selecting a box in the chart, or by typing words in the input box (1 above). A changed box is marked `~` (an added box `+`) until the next redraw.
  - While running: shown above the TUI's input frame, and redrawn every time the state changes. When it finishes, it is cleared, and only a one-line summary and the failed boxes are written into the terminal's flow.
  - CLI: `kairocode flow graph <name>` (before running) and `kairocode flow show <run> [--follow]` (while running and after) show the same chart.
  - ACP: each node is listed as a `plan` entry, with its state.
  - Usage is shown by `kairocode flow stats` (what ran how many times from which entry point, and how many times it failed).
- The code that builds the chart (making node shapes from the syntax tree) goes in a pure package outside `internal/workflows`. Drawing is done by `internal/tui`.

### 5. Example: a workflow for development itself

This is the user's idea (2026-10-15). The way development proceeds, "file an issue → tests first → implement → review → check → PR", can itself be written as a workflow. It is the same procedure that the `/milestone` skill in KairoCode's harness runs by hand.

There are three reasons to use this as the example.

- **It carries on after a crash.** Finished phases (tests written, a review passed) are not redone.
- **A person's judgment becomes a phase.** Approving the plan and reviewing the PR are `ask` and `wait`. While waiting, no process is held (invariant 7). CI results and review comments start the continuation as events (M4).
- **It is visible.** With the chart in 4 above and `flow show --follow`, you can see which phase it is in and where it waits for a person.

```python
DESCRIPTION = "Take a filed issue, write the tests first, implement it, pass review and checks, and make a PR"
INPUT = {"title": str, "detail": str}
LIMITS = {"agents": 20, "tokens": 5000000}

def main(input):
    plan = agent(agent = "plan", prompt = "Work out a plan for the following issue and tests for its acceptance criteria:\n" + input["detail"],
                 schema = {"plan": str, "tests": list})
    answer = ask(question = "Proceed with this plan?\n" + plan.value["plan"], options = ["proceed", "stop"])
    if answer.value != "proceed":
        return {"ok": False, "message": "the plan was not approved"}

    agent(agent = "build", prompt = "Write the acceptance criteria as tests first (do not implement yet):\n" + str(plan.value["tests"]))
    first = tool("bash", command = "go test ./...")
    if first.exit_code == 0:
        return {"ok": False, "message": "the tests written pass from the start. They do not check the criteria"}

    passed = False
    for round in range(5):
        agent(agent = "build", prompt = "Implement it so the tests pass:\n" + plan.value["plan"])
        if tool("bash", command = "scripts/check.sh").exit_code == 0:
            passed = True
            break
    if not passed:
        return {"ok": False, "message": "the checks do not pass after 5 fixes"}

    for round in range(3):
        review = agent(agent = "durability-reviewer", prompt = "Review the diff",
                       schema = {"findings": list})
        if len(review.value["findings"]) == 0:
            break
        agent(agent = "build", prompt = "Fix the review findings and pass the checks:\n" + str(review.value["findings"]))

    tool("git_commit", message = input["title"])
    pr = tool("bash", command = "gh pr create --fill", sandbox = "escalate",
              justification = "make a PR (network)")
    ci = wait("ci.result", timeout = "2h")              # M4: an outside event
    approved = wait("github.pr_review", timeout = "7d")  # M4: a person's review
    return {"ok": ci.ok and approved.ok, "message": pr.output}
```

- The entry point is a person's command (`/flow develop`) or an outside event (a label put on an Issue, M4). The model never picks it by name and calls it (1 above).
- Permissions are the same as for ordinary tools (3 above). The bash that makes the PR goes outside the sandbox, so the user is asked every time (ADR 0005).
- As a check of M3, we aim to run part of KairoCode's own work from M3 on with this workflow.

### What this ADR does not decide

- A screen viewed in a browser.
- How outside events (`github.*` and so on) are received. Decided in M4.

## Consequences

- `go.starlark.net` is added as a dependency (added to the allowed dependencies in AGENTS.md).
- Invariant 1 in AGENTS.md (deterministic) also extends to the Starlark runtime. When a primitive is added, it must return only recorded values.
- Tests (M3's acceptance criteria):
  - `TestCrashWorkflowScriptResume`: when a script with 10 `agent`s in a `parallel` is forcibly stopped partway and resumed, finished leaves are not redone.
  - `TestWorkflowScriptDivergence`: resuming with a script that makes a call different from the record stops without running it.
  - `TestWorkflowScriptLimits`: it stops at the limits on steps, number of agents and tokens.
  - `TestTurnEndHookBlocks`: when a `turn.end` workflow returns `ok` as false, the message goes back to the model and the loop continues, stopping at the limit on the count.
  - `TestWorkflowGraph`: the flow chart correctly shows order, `if`, `parallel`, `for`, `def` expansion and `phase`. It also covers folding when the width is not enough and switching to the tree chart (comparing against fixed output for fixed input).
  - `TestWorkflowLive`: the state of the run's tree maps to the marks and colors of the chart's boxes and the loop round.
  - `TestDraftFromWords`: a fake model writes a script; when it fails the check the reason comes back; one that passes is written to the file, visible in the chart, and runs with `/flow <name>`. A change asked for from the input box redraws the chart, and the changed boxes are marked.
  - `TestEditOnTheChart`: rewriting, deleting or swapping a box's content on the chart changes only that argument and the order of statements in the script, and no other lines (including comments) change.
  - Even if the script is rewritten while running, the running run finishes with the recorded script.
  - Run the development workflow in 5 above to the end with a fake model and a fake `gh`. Even when it is forcibly stopped partway (after writing the tests, after the review) and resumed, finished phases are not redone. No process is held during `ask` and `wait`.

## Alternatives considered

- **Put them in the skill shape and have the model call them after reading the description:** whether to use them stays with the model's judgment.
- **Make the whole body a Dify-like node definition (JSON) (considered on 2026-10-19):** it maps one to one to the chart. But what the user asked for was the presentation, and there is no reason to lower the body's expressiveness. The chart can be built from Starlark's syntax tree.
- **A JSON or YAML plan:** people would have to learn the building blocks and the template syntax, and trying to express branches and loops would mean inventing something like a language ourselves.
- **JavaScript (goja):** we would have to remove `Date` and `Math.random` ourselves. The user wanted the Python way of writing.
- **Real Python (a process inside the sandbox):** it allows the widest range of what can be written. But it needs a Python runtime (which breaks ADR 0001's single binary), and determinism can only be kept by fixing the environment (`PYTHONHASHSEED`, replacing the time). The syntax tree cannot be read inside Go, so even the static chart would have to be requested from a Python process.
- **Go (yaegi):** it would be the same language as KairoCode, but correct generic code did not run. The interpreter has not caught up with Go's new features. Also, since it reads sources from `GOPATH`, import checking would be needed separately.

## Implementation (2026-10-19, M3's first step)

What went in first is the core that runs a Starlark flow with `/flow <name>` in a form that carries on after a crash.

- **Shape:** `/flow <name> [arguments]` becomes a session turn. Between the snapshots at its start and end, the turn runs a child run `flow` (version 1), and leaves `main`'s result in the conversation as the turn's reply.
- **Recording the script:** at the start of the flow, the action `flow.load` reads the script. Its result is recorded, so even if the file is rewritten while it runs, it finishes with the original script (`TestFlowKeepsItsScript`).
- **Starlark runs inside the workflow.** `starlark`, `starlarkstruct` and `syntax` of `go.starlark.net` (`v0.0.0-20261005163335-bcb1a1a55bf9`) were added to what workflows may import (`internal/invariants`). `while` is allowed, recursion is not. The step limit stops anything that does not end.
- **Primitives (the first version)**

  | How it is written | What it returns | What it is |
  |---|---|---|
  | `agent(agent=, prompt=, model=None)` | A value with `ok`, `output`, `error` | A child turn with no history (`TurnInput.Sub`). No snapshot is taken, and subagents (`mode: subagent`) can be used too |
  | `parallel([{"agent":…, "prompt":…}, …])` | A list of values of the same shape (in order) | `kairo.Parallel` |
  | `tool(name, **args)` | `ok`, `output`, `error` | Goes through the permission rules of the session's agent. Anything that needs confirmation waits for approval, the same as ordinary tools |
  | `phase(name)` | nothing | A phase name (used in the chart) |

  The `input` of `main(input)` is `{"args": "<the words typed after the name>"}`. `main` returns one of `{"ok": …, "message": …}`, a string, or `None`. `test.exit_code` in the ADR's example is now `ok` and `output`.
- **Handling failures:** a mistake in the script, a limit (`agents`, `steps`, `tokens` of `LIMITS`) or a missing flow does not fail the turn; the reply gives "the reason the flow did not succeed" (`TestFlowMistakesAreReplies`). kairo's stops (waits, cancellation) stop it as they are.
- **Tokens:** the words of agents inside a flow are not streamed as the session's reply tokens (only those at the depth `s/<id>/<turn>/<call>` are streamed). The start and end of tools still arrive as before.
- **Entry points:** in ACP (the TUI, Zed), `/flow` alone gives the list and `/flow <name> [arguments]` runs one. Over HTTP it is `GET` and `POST /sessions/{id}/flows`.
- **Compatibility:** everything is separated by new input fields (`TurnInput.Flow`, `Sub`, `AgentLoadInput.Sub`, `SessionEvent.Flow`), so sessions and turns in progress that do not have them make the same calls as before. The version was not raised.
- **Approvals and blocked (needs confirmation) calls (fixed after durability-reviewer's findings):**
  - Approval waits inside a flow, and real calls cut off partway, are deep below the session's turn (the flow's run, and the turns of its agents). `Pending` collects them by walking down from the turn through the flows and turns that have not finished.
  - The approval key is "the ID of the waiting run + `#` + the call's ID", and the answer is sent to that run (`TestFlowAsksForApproval`, `TestFlowCutOffCallIsReviewed`).
  - In ACP, tools the flow called itself (calls whose ID starts with `flow_`) are announced as `tool_call` before asking.
- **The Claude Code engine:** a flow's agent turns do not use the session's Claude Code session. They run in a separate session made from their own turn's run ID. They start with no history, and agents side by side do not stop each other's claude (`TestFlowAgentsOnClaudeCode`).
- **The recorded script:** `TestFlowKeepsItsScript` reopens the process after the rewrite and confirms that even when the flow runs again from the top, it finishes with the recorded script.
- **`ask(question, options=["Yes", "No"])` (the second step):**
  - It records the question with `flow.ask` and then waits for the signal `answer/<id>`. While it waits, no process is needed (`TestFlowAsks` reopens the process while waiting).
  - The question appears in the same list as approvals (`Pending`, `Kind: "question"`, with options) and in events (`question.requested`, `question.resolved`).
  - The answer is one of the options, and only the first answer goes through (`Runtime.Answer`, `POST /questions`, `kairocode answer <key> <answer>`).
  - In ACP it is asked with `session/request_permission`. The title is the question and the kind is `think`. Every option is of the `reject_once` kind. As with blocked (needs confirmation) calls, this keeps a client that allows automatically from answering in the user's place.
  - There are two or more options. Free-text answers do not exist yet.
- **`decide(prompt, type="bool"|"number"|"choice", choices=None)` (the second step):**
  - It makes the model decide with a type (design section 6.3). It does not extract a decision from prose.
  - On the API engine, calling the `decide` tool is forced (`tool_choice`).
  - On the Claude Code engine, it reads `structured_output` of `claude -p --output-format json --json-schema` (no tools, no session kept).
  - An answer that does not fit the type, no answer, or a refusal returns a value with `ok` false (`TestFlowDecide`, `TestFlowDecideOnClaudeCode`, `TestFlowDecideOnClaudeCodeFails`). Even when Claude Code exits non-zero without deciding, the result line is used if it can be read. Temporary API errors are retried, the same as model calls.
  - The text asking for the decision is saved with `flow.put` and passed as a hash (invariant 5; the same as `agent()`).
  - For a number decision, only integers smaller than 2^53 become Starlark integers. Larger ones stay floats (so that conversion to an integer does not vary by machine).
  - The `claude` used for decisions is not covered by the mechanism that stops leftovers after a crash (`SetClaudeProc`). It has no tools and leaves nothing behind, so there is no effect that could happen twice; only the cost grows.
- `kairocode run "/flow <name> …"` also runs a flow non-interactively (`TestRunAFlowThatAsks`).
- **The flow chart (the third step, the chart before running):**
  - `internal/flowchart` builds the shape from the script's syntax tree (from `main`, primitive calls in order; `if`, `for` and `while`, `parallel` (lists and comprehensions), `phase`, `return`, `break`. Functions in the script are drawn expanded in place, and their `return` does not end the flow).
  - `internal/tui` draws it from top to bottom with boxes and lines, like Dify. Branches and agents side by side are placed horizontally and joined, and loops and "for each element at once" are enclosed in a frame. If it does not fit the width, it becomes a tree-shaped text chart.
  - A box's mark and color are decided by its state (`○` not yet, `⏺` running, `✓` finished, `✗` failed, `⏸` waiting for a person). Overlaying the state while running is the next step.
  - The entry points for viewing it are `kairocode flow graph <name>` and the TUI's `/flow graph <name>`. Both draw from the local script without going through the daemon. A flow named `graph` cannot be made.
  - Tests: `TestBuild` (shape), `TestChartLooks`, `TestChartStates`, `TestChartTooNarrow`, `TestFlowGraph`, `TestFlowGraphCommand`.
- **The chart while running (the fourth step):**
  - The flow's version was raised to 2 (version 1 is kept with `Draining`). In version 2, primitive calls carry in their input where in the script they were called (`At`, `"line:column"`; the position of the opening parenthesis that Starlark records for a call). This covers agent turns, tool inputs, `flow.ask` and `flow.decide`.
  - The idea of recording a marker call right before each call was dropped. kairo's list of children is ordered by creation time (milliseconds) and ID, so the order within the same millisecond is not kept.
  - `Runtime.FlowView` builds the chart from the recorded script, and from the states of the flow's child runs (tied to boxes by the `At` in their input) it gives each box's state (running, finished, failed, waiting for a person) and count. Waiting for an approval inside an agent's turn is also "waiting for a person".
  - The event hub emits a `flow` event (the chart and the states) every time the flow progresses. A client that connects later is also sent the current state.
  - In ACP it becomes a `plan` update. The entries are per call, and the chart and the states are in `kairocode/flow` of `_meta`. Zed shows it as a list of entries, and kairo's TUI as a chart.
  - The TUI shows the chart while running above the input frame. If it does not fit the screen, it becomes the tree chart, and if that is still too long, only the area around what is running is shown. When it finishes, the tree chart with marks is left in the terminal's flow.
  - **A note on Bubble Tea's drawing:** if the screen shrinks at the same time as lines are added to the terminal's flow, lines from the previous screen are left behind (because Bubble Tea decides where to add lines by the height it drew). The TUI does not shrink the screen's height while it is adding lines (`TestScreenKeepsItsHeightWhilePrinting`). The leftovers of the trust question had the same cause.
  - The chart is built once per run and kept, and the states are recomputed only when a run under that flow is observed (durability-reviewer's finding; the event hub is a single goroutine, so heavy computation delays every session's events).
  - A flow that started on version 1 continues with version 1's function even on new code, and does not redo finished agents (`TestFlowVersion1GoesOn`; we confirmed it fails when run with version 2's function).
  - Tests: `TestFlowEvents`, `TestFlowAgentWaiting`, `TestFlowVersion1GoesOn`, `TestACPFlowPlan`, `TestClientReadsAFlowPlan`, `TestFlowLive`.
- **`/flow new` (the fifth step, making one from words):**
  - `/flow new <description>` becomes a normal turn, as a request that comes with how to write flows (the primitives, the values they return, the differences between Starlark and Python, where files go) (`Runtime.NewFlow`, `POST /sessions/{id}/flows/new`, `kairocode run "/flow new …"`). The request stays in the conversation, so later turns can fix it with words.
  - **Writing is done by a new tool, `flow_write` (the user's approval, 2026-10-19).** `.kairocode` is a place the model's tools cannot write under ADR 0005 (because the configuration and permission rules are there). We left that as is, and, the same way that only the git tools write `.git`, added a dedicated tool that writes only `.kairocode/workflows/<name>.star`.
    - Before writing, it checks with `flowchart.Check` (syntax, unknown names, whether `main(input)` exists; f-strings, `try` and `import` come with a way to fix them). A script with problems is not written, and the problems go back to the model.
    - There is no default rule, so the user is asked every time it writes (`"*": "ask"`). A project rule can also allow `flow_write`.
    - The effect type is `Idempotent`, and like `write` it plans first and then writes in a "compare and replace" form (design section 3.1). Running it again does not write twice, and a script someone changed in between is not overwritten; it is returned as a conflict. The apply stage writes only when the plan points at `.kairocode/workflows/<name>.star`.
  - `flow_check` is a tool that only checks, without writing (read-only; allowed by default).
  - The TUI compares `.kairocode/workflows` at the start and end of a turn, and draws the charts of flows that were made or changed. Changed boxes are marked `~` and added boxes `+` (`flowchart.Diff`). The same applies when fixing with words.
  - Tests: `TestNewFlow` (a script that cannot be written is fixed, approved, written, and runs with `/flow hello`), `TestFlowWrite`, `TestCheck`, `TestDiff`, `TestFlowsTheTurnMade`.
- **Changing it on the chart (the sixth step):** the TUI's `/flow edit <name>` opens the flow's chart to change it.
  - ↑↓ selects a box, Enter rewrites its words (prompt, command, question) in the input box, `d` deletes it, and Shift+↑↓ swaps within the same level. Finishing with Esc leaves the changed chart in the terminal's flow.
  - Only that part of the script is rewritten (`flowchart.SetText`, `Delete`, `Move`). Words change only the value of that string, delete and swap change only the lines of that statement, and other lines (including comments) stay as they are.
  - If the changed script does not pass the check (`flowchart.Check`), it is not written (for example, deleting a statement that makes a value used later gives "not defined"). Boxes whose words are written as code, and calls that are part of a larger statement, cannot be changed on the chart, and the user is told to ask in words.
  - This is the user's own action, so the TUI writes the script directly (not the model's `flow_write`).
  - Tests: `TestSetText`, `TestDeleteAndMove`, `TestEditOnTheChart` (we confirmed it fails when broken into a form that writes without the check).
- **Hooks and the agent's `workflow:` (the seventh step):**
  - `{"hooks": {"turn.end": ["<flow>", …]}}` in `kairocode.json` binds flows to events. Both the user's configuration (`~/.config/kairocode/kairocode.json`) and the project's configuration are read and recorded with agent.load (invariant 2). The events are `turn.end`, `tool.after` and `session.start` (the ninth step).
  - `turn.end`: once the model has finished answering, they run in order. If one returns `ok: false`, its `message` is returned to the model as the user's message, and the loop continues. It is sent back at most 3 times (`TestTurnEndHookBlocks`). Turns of a flow's agents (`Sub`) have no hooks.
  - **A project's hook asks before it first runs.** It goes through the permission decision with the kind `hook` (the target is the flow's name). A rule can also say `"hook": "allow"` and so on. Once asked and allowed, the folder, the event, the flow and the script's hash are recorded (`hook-trust` in the store; the existing table looked up by content hash, no table added), and it is not asked again while the script is the same. If the script changes, it asks again. A refused hook does not run. It does not run in read-only mode (plan) either.
  - Hooks in the user's own configuration ask too (durability-reviewer's finding). They bind by name, so what runs is the project's script.
  - Only the allowed script runs. The hash of the script at the time it was allowed is passed to the flow (`FlowInput.Expect`), and if it differs from the script read, it does not run. Even if the script changes while waiting for permission, a script that was not allowed does not run (`TestHookRunsTheScriptAllowed`; we confirmed it fails when this check is removed).
  - A hook whose script itself does not run (no script, a mistake, a limit exceeded and so on; `FlowOutput.Broken`) is not sent back to the model, because the model cannot fix it.
  - The input of a hook's flow carries the event, which round, and which position (`FlowInput.Hook`) (invariant 8).
  - If an agent definition (`.opencode/agents/*.md`) says `workflow: <flow>`, that agent's turns proceed with that flow instead of the model loop. The user's message is `input["args"]` (`TestAgentWorkflow`).
  - All of these are separated by new fields in the recorded agent definition, so turns in progress that do not have them make the same calls as before.
- **Detecting divergence and the rest of M3 (the eighth step):**
  - The flow's version was raised to 3 (versions 2 and 1 are kept with `Draining`). Before calling each primitive, `flow.step` records which call number it is and what it does (the kind and the hash of its content).
  - A call resumed with the same content returns its result from the record, so `flow.step`'s handler does not run. It runs only when the content changed (for example, the code changed while the version stayed the same). In that case, if there is a record with different content at the same number, it answers "diverged", and the flow stops without making the call (`TestWorkflowScriptDivergence`; we confirmed that when the check is removed, a new agent runs and the test fails).
  - `kairocode flow show [-follow] <session>` draws the session's latest flow with the states of its calls. With `-follow` it redraws every time the flow progresses and exits when it finishes (`GET /sessions/{id}/flow`, `TestFlowShowFollow`).
  - We confirmed the token limit (`TestFlowTokenLimit`) and that stopping a turn also stops the flow and its agents (`TestCancelStopsTheFlow`).
- **The remaining events, stats, schema, task (the ninth step):**
  - `tool.after`: runs after the result, for each tool call that ran. An element in `kairocode.json` is a flow's name or `{"tool": "<tool>", "flow": "<flow>"}`; with `tool`, it runs only after that tool. The flow's `input["args"]` is the tool's name and what it ran on (the same target as the permission decision: a path or a command). The `message` of `ok: false` is added after the tool's result as the user's message and returned to the model. It does not run after calls that did not run (refused, missing, cut off) (after durability-reviewer's finding, this was fixed to tell them apart by the result of the decision rather than by the prefix of the effect's name).
  - `session.start`: runs at the start of the session's first turn (N = 0). The `message` of `ok: false` is added to the first message as `[What the session's start says: …]`. It does not run on the second and later turns.
  - As with `turn.end`, both ask before they first run, only allowed scripts run, and `Broken` is not returned to the model. The input's `Hook` carries the event, which step and which call number (`tool.after`), and which hook (invariant 8). An agent without hooks makes the same calls as before.
  - Tests: `TestToolAfterAndSessionStartHooks` (with another tool's hook placed first, it fails when the tool check is removed; we confirmed it fails when `session.start` is removed).
  - `kairocode flow stats [-dir DIR]`: from the records of the sessions in that folder, for each flow it shows the number of runs, the entry point (`command`: `/flow`, `hook`, `agent`: the agent's `workflow:`), successes, failures (`ok: false`, a script that does not run, a failure, a cancellation), the number running, and the last time it ran. Flows in the project that have never run are shown too, and flows that no longer exist are marked `(gone)` (`GET /flows/stats?dir=`, `Runtime.FlowStats`). Only the records that remain are counted (trees past `KeepFinished` disappear). Tests: the second half of `TestToolAfterAndSessionStartHooks` (we confirmed it fails when the entry point check is reversed), the second half of `TestFlowShowFollow`.
  - `agent(…, schema = {"name": "string", …})`: after the agent answers, its answer is turned into a dict with fixed fields by the same mechanism as `decide` (a forced tool call). The field types are `"string"`, `"number"`, `"bool"`, `"list"` and `"dict"`. What it returns has the same shape as `decide` (`.ok`, `.value`, `.error`); a value with a missing field or a mismatched type has `ok` false, with the reason in `.error` (`TestAgentSchema`; we confirmed it fails when the type check is removed).
  - The `task` tool (FR-13): the model calls a subagent itself. It is not run as a tool; the turn workflow runs it as a child turn (`Sub`, with `task:<call ID>` in the input's `At`) and makes its answer the call's result. Several `task`s in the same reply run side by side; after a restart, finished subagents are not redone; stopping the parent stops them. The effect type is `Read`; the actual effects are caused by the subagent's tools, each through its own permission decision. `task` is not given to subagents, to read-only mode, or to the Claude Code engine. A call whose arguments cannot be read or whose `prompt` is empty does not fail the turn; it goes back to the model as an error result (`TestTaskTool`; we confirmed it fails when broken so the result is not the child's answer). With two `task`s, when the process is forcibly stopped and resumed after the first finished while the second is running, only the second runs again (`TestCrashTaskResume`; we confirmed that killing it before the first also finishes makes the test detect the first being redone and fail). A flow's `tool("task", …)` is not run; it returns a message to use `agent()` instead.
- **In M4:** `wait` (outside events; built together with the entry point for events). With this, M3 is done (2026-10-19).
- **The flow's shape on the approval screen (2026-10-20, the user's report "I have no idea what kind of flow was made"):**
  - When asking about `flow_write`, the shape of the flow about to be written (`flowchart.Tree`) is put into `permission.check`'s output as `Preview`.
  - `Preview` is carried in `toolCall.content` of ACP's permission request, and the TUI shows it on the approval screen (as much as fits on the screen).
  - Tests: `TestFlowWriteAskShowsTheFlow`, `TestAskShowsWhatItWouldDo`.

## Addendum: a review when a flow is written (2026-10-20, the user's request)

Added after the user's request ("for making workflows, maybe we could have a review layer, on the TUI"). What was chosen is a dedicated review screen in the TUI.

- **The content of the approval is split into sections** (`runtime.PreviewSection`).
  - **Summary:** what each phase does, and the tools it may call. Each tool is marked with what it reaches.
    - MCP and unknown tools: `⚠ an outside service`
    - Real (`Real`): `⚠ cannot be undone`
    - Idempotent (`Idempotent`): `changes files`
  - **Tree:** the tree shape.
  - **Changes:** always shown if a flow with the same name already exists. It is shown even with no diff, so it is clear that this is an overwrite.
    - Added and changed things: shown as a marked tree (`+` and `~`).
    - Removed phases: listed as `- ` lines (`flowchart.Removed`).
    - When the previous version cannot be read (cannot be read as a flow, is not a regular file, is over 1 MiB), it still shows that it will be replaced.
  - **Script:** the full text of the script.
- **Recording and sending**
  - What goes into the record (`permission.check`'s output) is only the summary, the tree, the diff and the script's hash. The full text of the script is read when the list of approvals is built (ADR 0016's addendum).
  - In ACP, each section is sent as a separate content block, and each block's `_meta` (`kairocode/section`) holds the section's name and text. Other clients (Zed and so on) can read it as one continuous text with headings.
- **The TUI's review screen** (`internal/tui/review.go`)
  - Sections are switched with tab (and ←→). Lines are selected with ↑↓, PgUp and PgDn.
  - `c` attaches a comment to the selected line. A script line is recorded with its line number. `x` removes the last comment.
  - The ways to answer are:
    - `a`: allow.
    - `d`: refuse.
    - `r`: return the comments together to the model as the reason for refusing, and have it rewrite (the same mechanism as "Others" in ADR 0016). If there are no comments, type what you want fixed.
- **Opening a phase's content (the user's request "it would be nice to see the details with the arrows and Enter")**
  - Each line of Summary, Tree and Changes holds the script lines of that phase (`flowchart.Lines`).
    - A tree line holds that box's statement. For an if or a loop, its head line.
    - A summary line holds the first step of that phase.
  - Record format: after the section's text, a "first-last" for each line is recorded (`refsSep`). In ACP it goes into `kairocode/refs` of `_meta`.
  - Operation in the TUI
    - Select a line with ↑↓ and press Enter to open that phase's script lines in full, without truncation. Two lines before and after are also shown faintly.
    - Inside the opened view, select a line with ↑↓ and attach a comment with `c`. The comment is attached to the script's line number.
    - esc or Enter goes back.
  - The answer keys changed. Enter now means "open", so the answers are only `a` (allow), `d` (refuse) and `r` (return with comments).
  - Tests: `TestFlowReview` (opening a tree phase, the full prompt, comments inside the opened view, nothing opens on a heading line), `TestLinesOfSteps`.
  - **The screen's height (the user's report "moving left and right makes the window go down"):** each section had a different number of lines, and every time the screen's height changed, its position on the terminal shifted. The review screen now has the same height in every state: section, opened phase, comment, or typing. The number of body lines is decided by subtracting the rest from the screen's height, and missing lines are left blank (`TestFlowReviewKeepsItsHeight`; we confirmed it fails with the previous form).
- **What was fixed after durability-reviewer's findings**
  - **Overwrites:** without diff marks, Changes was not shown, and an overwrite looked like a new flow. Now it is always shown if there is a previous version, and removed phases are listed too.
  - **How the previous version is read:** `Lstat` confirms it is a regular file, and its size is capped (so a FIFO does not hang it).
  - **Withdrawal:** when the approval is answered elsewhere and disappears, a half-typed comment is cleared from the input box (so it is not sent as chat).
  - **ACP:** the section's name goes into `_meta` of the text block. The text is not sent twice.
  - **Mapping lines to phases (opening a phase):** the flow's description (DESCRIPTION) is one line in the tree. Even with newlines or control characters, the mapping between lines and phases does not shift, and no record separator gets in (`TestTreeLinesWithALongDescription`). A mapping that is not to a script line (a negative value and so on) does not open.
- **Tests:** `TestFlowReview` (TUI), `TestPermissionSections` (ACP), `TestRemoved`, `TestFlowWriteAskShowsTheFlow` (sections, overwrites, removed phases).

## Addendum: make the first review screen "what you need to decide" (2026-10-20, the user's request)

The user's comments were "when draft, confirm, notify show up with code, I can't really tell" and "what does everyone want in a review?". After considering it, the first review screen (Overview) puts the following three things on one page. A try run (the other request) is proposed in ADR 0017.

- **Is it what was asked for**
  - `asked` was added to `flow_write`. The model writes how it took the request, as a list `[{need, phase}]`.
  - The list is checked mechanically against the flow's phases.
    - `✓`: there is a matching phase.
    - `✗`: there is no phase with that name in the flow.
    - `+ not asked for`: a phase that matches no request.
  - The instructions for `/flow new` ask for a phase (`phase`) on each part and for `asked` to be written.
  - If it is not written, that is shown ("the model did not say how it took it").
- **What it reaches**
  - Things that send outside or cannot be undone (MCP, unknown tools, `Real`)
    - What it sends with is shown in words. A variable is traced back to the phase that made it (for example, "with the reply of agent plan (in draft)").
    - Whether there is a phase that asks the user before it (`you are asked before it (confirm)`); if not, `⚠ nothing asks you before it` is shown.
  - The files it rewrites.
  - How many times it calls the model (agents and decisions; inside a loop, "grows with each round"), and the agents that can change files.
- **Things to watch**
  - Sending outside without confirmation.
  - `while` loops.
  - Confirmations or tools removed since the previous version.
  - Things asked for that have no phase.
- **How it is built, and the other sections**
  - Everything except the list of requests (the model's words) is built deterministically from the script (`flowchart.Overview`).
  - The list of phases is "Steps" (the tree), followed by Changes and Script. The former "Summary" was replaced by Overview.
  - Each Overview line can also open that phase's script with Enter. ⚠ lines are shown in yellow, ✓ in green and ✗ in red.
- **What was fixed after durability-reviewer's findings**
  - **Whether confirmation exists is checked per branch:** before, if there was an ask anywhere, everything after it was "confirmed". So even a shape that asks in one side of an if and runs `rm -rf` in the other showed "confirmed".
    - Now it is checked per path. After an if, it is "confirmed" only when both paths asked. An ask inside a loop or a parallel does not make what is outside it "confirmed".
  - **What is sent:** the sources of all arguments are shown, not just the first (for `channel = "c", body = x.output`, the agent's reply).
  - **The size of the list of requests:** recorded up to 20 items, up to 200 characters each.
- **Tests:** `TestOverview` (matching requests, the sources of what is sent, confirmation per branch, files, model call counts, things to watch), `TestFlowWriteAskShowsTheFlow` (`asked` reaches the screen).

## Addendum: show the review as items, a chart and diffs, not prose (2026-10-20, the user's request)

The user's comment was "the review's item names and the diff being reviewed are hard to understand". Along with it came three instructions.

- Keep the screen in English, and stop using prose.
- Make the flow a chart like mermaid.
- Show only the parts that changed, and let you select them with the arrows to see their content.

- **Overview drops prose and becomes "label  value" lines** (`flowchart.Overview`).
  - The part headings are `▌ ASKED FOR`, `▌ REACHES` and `▌ WATCH`. In the TUI they are colored headings.
  - Under a tool that goes outside, `sends` (what it sends) and `asks you first` (✓ which phase asks / ⚠ no) are shown.
  - The sources of values were made into short words (`reply of agent plan (draft)`, `your words`).
- **Flow becomes a chart.** The same box chart as the flow run screen (top to bottom, branches side by side) is drawn inside the approval.
  - A box for a tool that goes outside shows `⚠` and `sends …`, and a question box shows its options (`[post | stop]`).
  - In ACP, the chart's shape (`flowchart.Chart`) goes into `kairocode/chart` of the section's `_meta`. Other clients are sent the text of the tree.
- **Changes lists only the phases that changed, in diff form** (`flowchart.Changes`).
  - `+` is an added phase (green), `-` a removed phase (red), and `~` a changed phase (yellow). Unchanged phases are folded as "… N the same".
  - Selecting a line and pressing Enter opens the before (`-`) and after (`+`) of that phase's statements. In ACP they are sent in `kairocode/details` of `_meta`.
  - Comments can be attached inside the opened view too.
- **The record's form**
  - What goes into the record (`permission.check`'s output) is only Overview, plus the hashes of the new script and of the script being replaced (`oldRef`).
  - The chart and the diff are drawn from the two scripts in the store when the list of approvals is built. The record stays small, and changing how things are drawn does not change the record.
  - Approvals recorded before this form are shown with their recorded sections as they are.
- **What was fixed after durability-reviewer's findings**
  - A question's options are also shrunk to one line, like other text (so the chart's boxes do not break).
  - Even when the previous version's diff cannot be drawn (not in the store, cannot be read), "replaces …" is still shown. That it is an overwrite is always visible.
  - A comment attached to the chart put a mark on every line of the chart. Chart comments now get no mark.
  - **Scrolling the chart (the user's report "the flow can't be scrolled"):** a cursor (❯) was placed on each line of the chart too, so pressing ↓ did not move the chart until the cursor passed the middle of the screen. The chart no longer selects lines; the arrows and PgUp/PgDn scroll the chart itself (`TestFlowReviewChartScrolls`; we confirmed it fails with the previous form).
  - What remains: the chart's ⚠ is decided from the list of tools at the time the list of approvals is built. So if tools or MCP servers change in between, it can disagree with REACHES in the recorded Overview. It is display only and is not used for branching.
- **Tests**
  - `TestChanges` (added, removed and changed phases, and their before and after).
  - `TestReviewSections` (the chart's marks, and that marks do not get mixed into the diff).
  - `TestOverview`, `TestFlowWriteAskShowsTheFlow`, `TestFlowReviewChartAndChanges` (drawing the chart, opening a change, headings).

## Addendum: how REACHES is worded, and the reader's language (2026-10-20, the user's request)

- **REACHES** (the user's comment "what does this show?")
  - The verb changes with the kind of tool.
    - bash: `runs a command · cannot be undone`
    - MCP: `sends to <server> · outside service`
    - other real (`Real`): `cannot be undone`
  - The content is shown one line per argument (the argument's name, such as `command`, `text` or `channel`, is the item name).
    - A variable is traced back to the phase that made it, or to the expression assigned (for example, `"curl -X POST " + url`).
  - Whether there is confirmation is `asked before it  ✓ yes, in confirm` or `⚠ no`.
- **The reader's language**
  - The user's decision (2026-10-20): item names (REACHES, models, files, argument names and so on) are close to proper nouns like "Pull Request", so they stay in English. Only the wording of the review's content and the flow's content follow the language.
  - The language is decided in this order (`internal/locale`).
    1. `KAIROCODE_LANG`
    2. `language` in the user's `~/.config/kairocode/kairocode.json`
    3. the terminal's locale (`LC_ALL`, `LC_MESSAGES`, `LANG`)
    - For now, English and Japanese.
  - The replacement happens where KairoCode's agent (ACP) sends an approval. It covers the sections' text, each line's details, and the text of the chart's boxes (`locale.Review`).
    - The record stays in English, so it can be shown in a different language for each reader.
    - The TUI starts KairoCode's agent in its own environment, so it uses the terminal's language.
  - The instructions for `/flow new` have the list of requests (`asked`), `DESCRIPTION`, questions and prompts written in the language the user wrote the request in. Phase names and flow names stay short lowercase English.
- **The Try section** also dropped prose and became lines (`words`, `ended`, `▌ WHAT HAPPENED`, `1 agent plan  replied`, `2 ⚠ write  not called`).
- **Notifying approvals during a try run**
  - Right after a try run was requested, when the wait briefly disappeared and then reappeared as "trying", a client sometimes asked again without Try.
  - An approval that is "trying" is not announced as "requested"; it is announced when the try finishes and its content changes. Because it is announced on the change of content, it is announced even when the try finished without the "trying" state ever being seen.
- **What was fixed after durability-reviewer's findings**
  - **Only KairoCode's own words are replaced.** What is approved must be seen exactly as it will be written and sent. So the following are not replaced.
    - the Script section
    - the before and after of changes
    - lines that contain quotes (the script's strings)
    - indented blocks (an agent's reply, the arguments it would have called with)
    - in the chart's boxes, the script's statements (only "sends …", which KairoCode added, is replaced)
  - **Variables are traced to their sources.**
    - An alias (`text = draft.output`): its source (`reply of agent plan`) is shown.
    - A built-up value: the expression is shown with its sources (`"curl " + url (with … your words)`).
  - What remains: the change to approval notifications (not announced during a try run, announced when the content changes) has no unit test of the events yet. The usual flow is checked by `TestTryFlow` and `TestACPTryFlow`.
- **Tests:** `TestLang`, `TestReview`, `TestLocalizedReview`, `TestOverview` (a line per argument, expressions and sources, aliases).

## Addendum: a more readable chart, and a chart while running too (2026-10-20, the user's request)

The user's comments were "(the tree while running) could be the preview's chart, right?" and "I ran it, but overall it's hard to read".

- **The chart is used while running too.** Before, when the chart did not fit the screen's height, it switched to the tree. Now it stays a chart and shows a screen's height around the running or waiting box (or, if there is none, the last finished box) (`around`). It becomes a tree only when the chart does not fit the terminal's width.
- **The chart itself was made easier to read** (both while running and in the review's Flow; `internal/tui/flowchart.go`)
  - **Guard clauses:** an if that only ends (`if not r.ok: return …`) and an if that only leaves the loop (`if …: break`) are not split into yes and no; they become one box. For example, under `◇ if not r.ok → stop` the text of the ending is shown.
  - **Ends:** return becomes `■ end` or `■ stop` (when `"ok": False`), and the message text is shown instead of code.
  - **Box width:** text wraps at 60 columns and is cut at the second line. A long prompt does not stretch a box across the screen.
  - **Tool boxes:** when an argument is a variable, its source is shown (for example, `"slack-notify " + draft (with …)`). For that, `Node.Args` was put into the JSON (it does not go into the record; only into the run screen's events).
- **Looking around the chart while running** (the user's decision: "no mouse; being able to look with shift + arrows is about right")
  - A chart that does not fit the screen can be moved one line at a time with shift+↑↓. A hint for this is shown below the chart.
  - After moving it, it stays at that position even as the flow progresses. A new flow starts again around the running box.
  - The idea of a mouse and full screen (a draft of ADR 0018) was dropped by the user, so it was not filed.
- **Tests:** `TestChartLooks` (guard clauses, ends, two-way branches, long text, the sources of variables), `TestFlowLive` (stays a chart on a short screen, around the running box, looking around with shift+↑↓ and staying there).
- **Showing that it is moving** (the user's request "it would be nice to see it moving")
  - A running box has a spinning mark (a spinner) and shows the seconds since it started (`agent build ×2 · 37s`).
  - When one box is running, what the agent is doing now is shown faintly inside that box.
    - `▸ go test ./...`: the tool it is calling now.
    - `… Fixing the parser`: the last line of the text it is writing.
  - When two or more boxes are running at the same time (agents in parallel), it cannot be told which box it belongs to, so it is shown below the chart.
  - So that the chart does not shake when the content changes, a box showing what is happening now has its width fixed at 60 columns.
  - During a flow, the agents' text is not shown in the reply area above the chart (it appears in the boxes). As before, it is left in the terminal's history paragraph by paragraph.
  - The screen uses only ACP events (text fragments, tool calls); the record was not changed.
  - Tests: `TestFlowLive` (seconds, the current tool, the last line, finished tools disappear, nothing in the reply area).
- **Showing how the flow itself proceeds** (the user's request "I want to see the flow itself moving")
  - In the chart while running, the path taken so far is green.
    - A line between boxes is green if the run has reached the box after it, and faint if not yet.
    - At an if's fork, the label of the side taken (yes / no) and the lines of the split and join are green.
    - Boxes that do not run (phase headings, ifs, guards, ends) count as "passed" if the run has reached beyond them.
  - Phase headings
    - A finished phase: `── review ✓ ──` (green)
    - The current phase: `── fix ◂ now ──`
    - A phase not yet reached: faint
  - Next to the chart's title, where it is now is shown (`fix · phase 2/2 · 3 done · 1m12s`).
    - the current phase and its position among the phases
    - the number of finished calls
    - the time since it started
  - None of these are shown on the review's chart (a chart that is not running).
  - Tests: `TestFlowLivePath` (green and faint lines, a phase's ✓ and now, the side of an if taken, the progress in the title, and that they do not appear on the review's chart).

Addendum (2026-10-27, the user's request): **write down and show what to pass to a flow.**
- Like `DESCRIPTION`, a flow can have at the top level a one-line usage such as `ARGS = "<owner/repo> [label]"`. It is written for people to read and says what the words in `input["args"]` should be. It is not used when running (and not checked).
- `/flow <name> --help` (and `kairocode run "/flow <name> --help"`) shows the following without starting a turn or a session.
  - the description and the usage (`/flow <name> <ARGS>`)
  - if there is no `ARGS`, the lines in the script that read `input["args"]` (with line numbers). If it does not read them, it says so.
  - the order of phases
  - if there is no published version and only a draft (ADR 0030), it shows the draft and suggests trying it with `--draft`. If both exist, it shows the published version and notes that there is also a draft.
- `/flow` (the list) shows `ARGS` after each name. The model for `/flow new` is told to write `ARGS` for a flow that reads arguments.
- Implementation: `flowchart.Help`, `Chart.Args`, `runtime.FlowHelp`, the ACP agent's `/flow` and the CLI's `run`. Tests: `TestHelp`, `TestRunFlowHelp`.

## Addendum: a flow that went another way also cancels what a run before left running (2026-11-04, Accepted (2026-11-04): issue #6)

- **What was wrong.** When a flow is resumed by code that goes another way (the divergence check above), the flow stops without making the call. But a run before may have left one of its own calls unfinished: for example, its second `agent`'s turn, running when the process stopped. The new code never reaches that `Child` call, so nothing waits for the turn. kairo still resumes it from the record, and it went on after the flow had stopped. That turn can be a subagent that edits files, while the user is told that the flow stopped. Found when the tests went parallel (issue #6). On kairo's side, nothing defines what happens to a run whose parent has ended (r-hashi01/kairo#2).
- **Decision.** When `flow.step` answers "went another way", it first cancels the flow run's child workflows that have not finished (`Runtime.cancelLeft`: `k.Children`, then `k.Cancel` for each `kairo.workflow` run that is not finished). The flow is stopping, so nothing of it should go on. `flow.step` stays unprotected: cancelling a run that has already settled does nothing, so running it again is safe.
- **Except a child that holds a call whose outcome may be unknown.** A child is left as it is when anything under it is an unfinished call other than a model call, for example a tool's call that is running or blocked (needs confirmation), or an MCP call of a CLI engine's turn, which is a run of its own (r-hashi01/kairo#4) (`Runtime.openCall`). Cancelling it would drop the question whether that call took effect, which invariant 4 does not allow without the user. ADR 0011's cancel may drop it because the user stops the turn; here nobody chose to stop. Such a child goes on as before this addendum, and the user is asked about the call as usual. A pending approval is no reason to keep a child: the call it waits for has not run.
- **Its review is listed.** Nothing above such a child waits for it any more: the flow and its turn end. So `Pending`, which walks the waits down from the session's turns, would never reach the child's review. `flow.step` therefore notes each child it keeps in the store (table `kc_kept`: the run, its session and its turn), and `Pending` lists what those runs wait for, forgetting each once it has finished. It costs one query per `Pending`, plus one per run noted. A run is forgotten once it is gone, or has finished with nothing left to answer (a finished turn may still hold an MCP call to review).
- **What it does not cover.** Calls of the flow that are not workflows (its own `tool()` calls) cannot be cancelled one by one through kairo's API. A real one that was cut off still becomes blocked (needs confirmation), as for any call (invariant 4). Left to r-hashi01/kairo#2 (`// kairo#2` on `cancelLeft`). Nor are the check and the cancel one step: a tool's call that starts between them (the turn's model call returns, and its tool call is made, in that moment) is cancelled with the turn, not left to be reviewed. Closing that window needs kairo: a cancel that leaves a real call whose outcome is unknown to be reviewed (proposed in r-hashi01/kairo#2). And if noting a kept child in the store keeps failing (a write retried until `flow.step` gives up), the child is still kept, its call still blocked in kairo, but `Pending` does not list it.
- **A turn left by a run before is resumed when the runtime opens, and runs until the flow reaches the diverged call**: in that window it may make model calls and run tools as its rules allow. Cancelling it ends that window early; it does not close it. The diverged call itself never runs.
- **Tests.** `TestWorkflowScriptDivergence` checks two things. The diverged call's prompt never reaches the model. And what the run before left running (a turn held in its model call) ends cancelled, with no workflow left running under the session. Without the cancel, the test fails: the turn stays running. It runs in parallel again. The test used to require that the model is not called at all; that cannot hold, because the turn left by the run before may be resumed before the flow replays. `TestCrashDivergedFlowKeepsAnOpenCall`: the flow's second agent is killed in its `bash`. Resumed by code that goes another way, the flow stops, the agent's turn is not cancelled, and the `bash` waits to be reviewed (listed by `Pending`). Without the exception, the test fails: the review is gone.
