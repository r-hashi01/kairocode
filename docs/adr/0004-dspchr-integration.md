# 0004 Become an agent that behaves well inside dspchr

- Status: Proposed (2026-10-27; on hold by the user's decision. Revisit when dspchr is published, or when a reason to integrate comes up)
- Date: 2026-10-09
- Related: requirements FR-2, FR-11, FR-24, NFR-4; ADR 0002 (clients), 0003 (the workflow tree); dspchr's ADR-039, 042, 050, 055, 066, 068; REQUIREMENTS §8, §24

## Context

dspchr is a TUI multiplexer that runs CLI agents such as Claude Code and Codex inside a PTY, makes them visible as processes, and lets you operate them (`../dspchr`). It guesses their state from CPU and the screen, but if an agent reports its state itself, it does not guess (dspchr's ADR-039, `dspchr state <state>`).

dspchr's requirements put "building our own coding agent" out of scope (REQUIREMENTS §24). So KairoCode is not absorbed into dspchr; it becomes one of the agents that run inside dspchr.

KairoCode can provide information other agents cannot.

- A state that needs no guessing, including waiting for approval and blocked (needs confirmation) (a real tool that crashed).
- A resume that continues from where it was after a crash.
- Recorded usage.
- The tree of subagents and workflows (ADR 0003).

On the other hand, a KairoCode session holds no process while it waits for an approval (NFR-4). dspchr today cannot list things that have no process.

## Decision

Integrate in two stages. Stage 1 can be done entirely on KairoCode's side. Stage 2 needs changes to dspchr, so it is proposed as a dspchr ADR (`docs/proposals/dspchr-kairocode.md`). dspchr's repository is not edited from this repository.

### Stage 1: what can be done on KairoCode's side alone (M2)

- **Reporting state:** the KairoCode launcher that runs inside the pane (a terminal chat like `kairocode chat`; ADR 0002) subscribes to the session's events. If the environment variable `DSPCHR_SESSION` exists and `dspchr` is on the PATH, it starts `dspchr state <state>` at each change of state.
  - The mapping matches the one dspchr uses for other agents (dspchr's `hooks.cpp`).

    | KairoCode event | dspchr state |
    |---|---|
    | a turn started | `working` |
    | a turn ended and it is waiting for the next message | `waiting` |
    | asking for approval, stopped at blocked (needs confirmation) | `waiting` |
    | a turn failed, stopped at a limit | `error` |
    | the session was closed | `idle` |

  - Reports are started without waiting for them, and failures are ignored (not having dspchr is not an error). Reporting happens outside the workflow (on the client's side) and is not recorded.
- **A hint for resume:** the launcher puts the session ID in its arguments (`kairocode --session <ID>`). dspchr reads claude's `--session-id` from its arguments (dspchr's resume). With the same shape, dspchr can read it from the arguments in stage 2.
- **Usage:** `kairocode usage --session <ID> --json` gives the input, output and cache token counts. The fields are the same as dspchr's usage type (`agent::Usage`).
- **dspchr's skill:** dspchr teaches agents its own commands (`send`, `read`, `wait`) as a skill (dspchr's `skill.cpp`). The places KairoCode reads skills from (ADR 0002) are aligned with where dspchr can write.

### Stage 2: dspchr talks to KairoCode's server (proposed as a dspchr ADR)

- dspchr's daemon becomes a client of KairoCode's server. It is the same idea as the mechanism that lists sessions of another dspchr daemon in the same list (dspchr's ADR-042).
- Sessions that hold no process are listed too (for example, a session waiting for approval).
- Approvals and blocked (needs confirmation) calls are answered in dspchr's central dialog. The answers are sent to KairoCode's `POST /approvals/{key}` and `POST /reviews/{key}`.
- The tree of subagents and workflows is shown in the sidebar.
- What KairoCode's side provides:
  - How to find the local server: the address and token are written to `~/.local/share/kairocode/server.json`. The file's permissions are 0600.
  - An API listing sessions: it returns the state, usage, what is being waited for (approval keys, keys of blocked (needs confirmation) calls), and the tree of children.
  - SSE of events.

## Consequences

- Stage 1 alone makes KairoCode's state show accurately in dspchr's sidebar. For users who do not use dspchr, nothing happens.
- State is reported outside the workflow. It does not touch determinism (invariants 1 and 2 in AGENTS.md).
- Tests:
  - A test of the table mapping events to states.
  - A test that replaces `dspchr` with a fake command (a script on the PATH that only records its arguments) and checks that reports are started.
  - A test that nothing is started when `DSPCHR_SESSION` is absent.
- Stage 2 proceeds after it is approved on dspchr's side. KairoCode's API (listing sessions, SSE, answering approvals and blocked (needs confirmation) calls) is in design section 8.1; the only new things for stage 2 are the file for finding the server and the fields of the list.

## Alternatives considered

- **Build a KairoCode conversation screen into dspchr:** dspchr is a multiplexer that runs processes in panes and has no conversation screen. It would also go against dspchr's requirements (no coding agent of its own).
- **Ride on dspchr's hook mechanism (writing commands in a configuration file):** KairoCode is our own code, so it can report directly without going through a configuration file. No rewriting of configuration and no versioning are needed.
- **KairoCode's server reports:** the server runs outside the pane and does not know `DSPCHR_SESSION`. If the launcher inside the pane reports, dspchr's resolution of the destination (the environment variable) can be used as it is.
