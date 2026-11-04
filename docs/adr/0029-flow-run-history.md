# 0029 Flow run history: keep finished runs, and look back at them later

- Status: Accepted (2026-10-27)
- Date: 2026-10-27
- Related: ADR 0003 (flows), ADR 0017 (try), ADR 0023 (browsing the record), kairo's ADR 0054 (deleting finished trees)

## Context

There are only 3 ways to look back at a flow later.

- `kairocode flow show <session>`: the diagram of that session's latest flow, and the state of each step.
- `kairocode log <session>`: the whole record of the session.
- `kairocode flow stats`: run counts per flow.

There is no way to pick a past run from a list and look at it. Also, the record itself disappears. kairo deletes the record of a tree whose root has finished after `KeepFinished` (24 hours by default). Conversation sessions keep waiting, so they stay, but `kairocode run "/flow …"` (CI, hooks, scheduled starts) closes its session, so after a day nothing of that run's record remains.

## Decision

1. **When a flow run finishes, save that run on KairoCode's side.** In a new table `kc_flow_run`, one run is kept as one row.
   - Columns: run ID (the kairo ID of the flow run), working folder, flow name, start, end, how it ended, body (JSON).
   - Body:
     - Who started it and from where (the conversation's `/flow`, an agent, a hook)
     - Arguments, session, turn
     - How it ended (succeeded, failed, broken, cancelled), the text the flow returned, the number of agents, usage
     - The flow's diagram and the state of each step (`flowchart.View`)
     - The timestamped sequence of each step (entries in the same form as `kairocode log`: agents, tool calls and results, approvals and questions and who answered how, model calls and usage)
   - The length of the body is the same as `kairocode log` (replies and results up to 400 characters). The full text can be seen with `kairocode log` while the record remains.
   - No secrets go in, as with other records (invariant 6; nothing that is not in the record is written).
2. **Saving happens on the daemon's side, and the workflow is not changed.** Of the notices kairo sends that "a run finished" (`ObsRunSettled`), those for flow runs are saved on the spot. Runs that were missed because the daemon crashed are saved at the next start, from what remains in kairo's record. Writing the same run twice still leaves one row.
3. **How much is kept:** the latest 200 runs per folder and flow, and within 90 days. On each save, older ones beyond that are deleted. This can be changed with `flowHistory` (`{"keep": 200, "days": 90}`) in the user's `kairocode.json`.
4. **Ways to look back**
   - `kairocode flow runs [-dir DIR] [-flow name] [-n N] [-json]`: a list of past runs (newest first). Runs in progress are also listed, read from kairo.
   - `kairocode flow show <run ID>`: that run's diagram (with the state of each step) and its timestamped sequence. When given a `<session>`, it shows the latest flow as before, and a run in progress can be followed with `-follow`.
   - The terminal UI: `/flow runs` shows the list, and choosing one shows that run's diagram and sequence. Like the review, it is shown on a separate screen (alt screen). The ACP agent's extension methods (`_kairocode/flow_runs`, `_kairocode/flow_run`) supply it.
   - API: `GET /flows/runs`, `GET /flows/run?id=…`.

Where this shows up in the implementation: `internal/store` (`kc_flow_run`), `internal/runtime/flowruns.go` (save, list, one run, delete), `internal/runtime/runtime.go` (saving from `observe`, saving at start), `internal/server` and `internal/daemon` (API and client), `internal/cli` (`flow runs`, `flow show`), `internal/acp` (extension methods), `internal/tui` (`/flow runs`).

Addendum (2026-10-27, the user's request): **tries are also kept, as tries.** A try before writing (ADR 0017) could no longer be seen once the approval was answered.
- Tries are also saved in `kc_flow_run`, marked with the `try` column (and `try` in the body). For a table created by the earlier version, the column is added when it is opened.
- The count and day limits (`flowHistory`) are counted separately for tries and real runs. This is so that trying many times does not push out the history of real runs.
- `kairocode flow runs` lists both, showing tries as "name (try)". `-tries` shows only tries, `-no-tries` only runs. The API and the ACP extension methods filter with `only` (`tries`, `runs`).
- `kairocode flow show <ID>` and the terminal's `/flow runs` show tries the same way, with diagram and sequence (in the terminal, with the `try` mark and the heading "try before writing").
- Tests: `TestTryFlowWithPluginTools` (a try is kept as a try), `TestFlowTriesCountedApart`, `TestFlowRunsScreen`.

## Consequences

- Even for flows run with `kairocode run`, how they ran can be looked back at after they finish, however many days later.
- Tests
  - When a flow finishes it is saved, and after kairo's record is deleted, the list and one run's diagram and sequence can still be seen.
  - Runs the daemon missed saving are saved at the next start. Writing the same run twice leaves one row.
  - Old ones are deleted by the count and day limits.
  - The terminal's `/flow runs` shows the list and one run on a separate screen.

## Alternatives considered

- **Extend kairo's `KeepFinished`:** all records would keep remaining, and the DB would grow. We only want to look back at flow runs, but we would be holding the detailed records of turns and model calls too.
- **Add a saving action at the end of the flow workflow:** reliable, but the order of the workflow's calls changes, and the version must be bumped. The finish notice plus picking up again at start is enough.
- **Save the full text:** keeping long model replies and tool results would make rows large. When the full text is needed, it can be seen with `kairocode log` while the record remains.
