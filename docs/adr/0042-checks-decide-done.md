# 0042 Checks decide when work is done: gates on a flow's agents, and after a turn

- Status: Accepted (2026-11-13)
- Date: 2026-11-13
- Related: `docs/factory.md`, ADR 0003 (flows, `turn.end` hooks), 0017 (trying a flow), 0018 (command hooks, Claude Code's Stop), 0029 (flow run history), 0030 (flow drafts), 0036 (a flow from a session), 0005 and 0031 (sandbox), 0039 (limits), 0041 (results other programs read), invariants 1, 2, 3, 5 and 8, M13

## Context

The user wants KairoCode to become a software factory you talk to (2026-11-13, `docs/factory.md`). Behind the conversation, work goes down lines: rows of stations where machines (flows, agents, checks, tools) and people work on it. The question that tells a factory from a long chat is **who decides that the work is done, and what happens when that decision is wrong**.

A flow is one machine on a line, not the line (the user, 2026-11-13). Lines come in a later step. What every station needs first, and what neither a flow nor a plain turn has today, is a gate: the point where work that does not meet the bar is sent back or stopped, not passed on. This ADR adds gates where work is done today: on a flow's agents and on a plain turn. Lines will place the same gates at their stations' exits.

Today the model decides:

- `turn.end` hooks (flows, ADR 0003; commands, Claude Code's Stop, ADR 0018) send the model back with what failed, at most 3 times. After the third, the turn **ends as if done** (`Stopped == ""`): nothing records that the check never passed.
- A flow's agents (`agent()`, `parallel()`; `Sub` turns) run no hooks at all. The agents a flow runs, often side by side, are exactly the work nothing checks.
- A command hook is a real action (`hook.command`): if the process dies while it runs, the turn stops and asks a person whether it ran. That is right for a hook that sends a message. It is wrong for a test run, which can simply run again.
- The agent can change what checks it. Whatever the gate looks at, the agent works to satisfy, and editing the test is the cheapest way.

kairo's workflows are what make a gate worth having here:

- Each check run is a recorded call, so a crash during the tests resumes by running the tests again, not the model.
- The rounds spent are recorded, so a restart does not give the agent fresh tries.
- The verdict is in the record, so `kairocode log` and the flow's run history show why a work item passed a gate.

## Decision

1. **Checks are the project's instruments, declared by name in `kairocode.json`** (the user's and the project's, read by `agent.load` and recorded, invariant 2). Where they are used is chosen where the work is described: by a flow for its agents (2), by the project for a plain turn (3), and later by a line for its stations.

   ```json
   "checks": {
     "test": {"command": "go test ./...", "timeout": "10m", "protect": ["**/*_test.go", "testdata/**"]},
     "lint": {"command": "scripts/check.sh --quick"}
   }
   ```

   - A check is a command run in KairoCode's sandbox (offline, writes only in the project, ADR 0005; `"sandbox": "loopback"` as ADR 0031 allows). It passes on exit 0. When it fails, the last part of its output, up to a bound, is what the model is given.
   - The project's checks are asked about before they first run, as a project's command hooks are (ADR 0018), and the answer is remembered.
   - `protect`: paths the agent's writing tools may not change while the check gates its work (a denied call, with "this file is checked by `test`"). This makes the gate harder to game. It is not a wall: a `bash` command can still write them. So work that changed a protected path by any means fails the gate on that alone (the files' hashes before and after, from the turn's snapshots).
2. **A flow gates its agents** (`internal/workflows`, the flow builtins):
   - `agent(..., checks = ["test", "lint"], rounds = 3)`, and the same in `parallel()`'s specs, gates that station. After the agent's model is done, the checks run (independent ones at once, `kairo.Parallel`). A failure sends the agent back with the failed checks' names and output, up to `rounds` times (default 3).
   - When the rounds are spent and a check still fails, `agent()` returns `.ok == False`, `.fail == "checks"`, and `.checks`, each check's last verdict. The flow decides what follows: another agent to fix it, a person (`ask`), or stopping (`fail()`).
   - `check(name)` runs one check as a step of its own, and returns `.ok` and `.output`.
   - Both are recorded flow steps; their names and positions are in the child's input (invariant 8). A gate's verdicts are kept in the flow's run history (ADR 0029), so `kairocode flow stats` can say how often each gate passed at first, after rounds, or not at all.
3. **A plain turn can be gated too.** `"checks": {"turn": ["lint"]}` in `kairocode.json` names the checks a plain turn (chat, `kairocode run`) must pass, with the same rounds and send-back. When they are spent, the turn ends with `Stopped: "checks"`, and `TurnOutput.Checks` holds the verdicts (each check's name, whether it passed, and the hash of its output in `kc_message`, invariant 5). No `turn` list: a plain turn is as today, and its `Checks` is empty. "Not checked" is never shown as "passed".
4. **A check is an action that may run again**: `check.run`, `kairo.Real` with `IdempotentRetry` (invariant 3's mapping for an idempotent effect). A check observes. If the process dies while it runs, it runs again when the work resumes. A check that does more than observe is a hook's job, not a check's.
5. **The turn workflow's version and the flow's version are raised** (`kairo.WorkflowVersion`), and the current ones kept with `kairo.Draining()` (kairo's ADR 0060), since gates add calls. A turn or flow run under way finishes on the version it started on.
6. **What is shown.**
   - The terminal UI and ACP show each check as it runs, and its verdict.
   - `kairocode log` and `kairocode flow show` show the verdicts per turn and per station.
   - `TurnOutput.Checks` is what ADR 0041's `-json` will carry (`status: "stopped"`, `stopped: "checks"`).

## Consequences

- Work that has a gate is taken as done only when its checks pass, and the record says so. Work that could not get them to pass comes back as not done, and what does it (the flow, the turn's caller, later the line) decides what that means.
- A flow becomes a machine whose result can be trusted: plan, then agents gated by checks, then review. A crash at any point resumes without redoing work that already passed.
- The flow's run history shows each gate's yield: how much work passed each gate at first, how much after rounds, and how much was stopped.
- A gated station costs its checks' time. They run after the agent is done, not after each step.
- Not covered here, and built on this:
  - lines (standard ones, and ones put together in the conversation), whose stations carry these gates;
  - work items sent down lines from the conversation, each in its own worktree;
  - people at stations, and where a person must always stand;
  - intake from outside.
- Tests (added with the implementation):
  - Flows: `TestFlowAgentChecks` (passes; sends back; rounds spent gives `.fail == "checks"`), `TestFlowParallelChecks`, `TestFlowCheckStep`, `TestFlowStatsGates`.
  - A plain turn: `TestChecksTurn` (pass, send back, `Stopped: "checks"`), `TestChecksNoneDeclared`.
  - Running and guarding the checks: `TestChecksParallel`, `TestChecksProtect` (a write tool denied; a `bash` write fails the gate), `TestChecksProjectAsked`.
  - Resuming: `TestChecksReplay` (the same calls from the record), and `TestCrashDuringCheck`. Killed while a check runs, the check runs again on resume, the model is not called again, and the rounds spent are not reset.
  - The workflow versions test.

## Alternatives considered

- **Only fix `turn.end` hooks** (a verdict after the third round, hooks for flow agents). Hooks are Claude Code's Stop, kept the same across agents (ADR 0018). Making them a gate would change what those settings mean, and a hook may do things a check must not (send, write). The two stay apart: checks decide done, hooks react.
- **Gates declared only in `kairocode.json`, for every turn.** Which gate goes where is part of how the work is described, not a project-wide setting: a planning agent needs no tests, an implementing one does. The project names its instruments, and the flow (later the line) places them.
- **Gates only in flows, none for a plain turn.** Most work is done in plain turns, and will stay so for small work. The same gate applies there.
- **Let the model run the tests and say they passed.** That is what a turn does today. The point is that the decision is not the model's.
- **Unlimited rounds until the checks pass.** A gate that cannot fail can loop on a check the agent cannot fix. Rounds and the turn's limits bound it, and the result says it stopped.
