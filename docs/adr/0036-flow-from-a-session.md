# 0036 Make a flow from a session that went well

- Status: Proposed
- Date: 2026-11-09
- Related: ADR 0003 (flows), ADR 0017 (try), ADR 0023 (viewing records), ADR 0030 (drafts and publishing), invariant 5, `docs/milestones.md` M9 and "Future work" (the user's idea, 2026-10-22)

## Context

A session that went well holds a procedure: what was asked, which tools were called with what, where a person approved or answered, and what the model decided on the way. Repeating it today means asking again in words and hoping the model takes the same path.

Flows (ADR 0003) are the way to repeat a procedure, and `/flow new <words>` already has the model write one as a draft (ADR 0030), checked and reviewed before it is published. What is missing is giving the model the session as the material, instead of words alone.

The material is the record: `Runtime.Log` (ADR 0023) walks a session's turns with their messages, tool calls and results, waits and answers, and `SessionLog.Text()` renders it. The model of the session may no longer have all of it in its context (compaction, ADR 0020; an engine such as Claude Code holds its own context), so the record is given whole rather than relied on.

## Decision

1. **The command is an option of `/flow new`:** `/flow new --from-session [words]`, in the terminal UI, editors (ACP) and `kairocode run`.
   - Without a value it takes the current session. `--from-session=<id>` takes another session of the same folder (an id from `kairocode sessions`).
   - The words, if any, say what to make of it ("only the release part", "take the version as an argument").
   - `new` is already reserved as a flow name, so no new reserved word is added (ADR 0030 chose options over reserved words).
2. **It runs as `/flow new` does:** a turn of the current session, in which the model writes a draft with `flow_write`. Trying it (`/flow <name> --draft`), reviewing it and publishing it (`flow_publish`) are as in ADR 0030. Nothing new runs a flow or writes files.
3. **The material is the record as text**, read when the command is given and put in the turn's user message (in `kc_message` like any message, invariant 5):
   - From `Runtime.Log` of that session: each turn's user messages, the tool calls with their arguments and permission decisions, the answers to approvals and questions, the flows run, and the model's replies.
   - Long parts are cut: each tool result and reply to its first 2,000 characters, and the whole to 60,000 characters, dropping middle turns first with a note of how many were dropped (the first request and the end matter most).
   - The turn of this command itself is not in it: the record is read before the turn starts.
4. **What the model is asked** (added to `flowNewPrompt`'s instructions): write a flow that repeats what the session did, not what it said.
   - What differs between runs (a version, a branch, a file) becomes `ARGS`.
   - Where a person approved something that matters or chose between options, the flow uses `ask()`. Where the model judged, it uses `decide()` or an `agent()`.
   - Steps that went wrong and were redone are left out; only the path that worked is kept.
   - `asked` (ADR 0030) lists the steps it took from the session, so the review compares the flow with the session.
5. **Secrets:** the text is made from the record, which holds none by invariant 6. It goes to the session's own engine, which saw that conversation already. A record from another session goes to this session's engine; the command says which session it read.

Where this shows in the implementation: `internal/runtime/flownew.go` (`NewFlowFromSession`, the prompt's addition), `internal/runtime/log.go` (a text form cut to size), `internal/runtime/flow.go` and `internal/cli` (the `--from-session` option of `/flow new`), `internal/tui` (completion and help of the option). `internal/workflows`: no change.

## Consequences

- A procedure that worked once can be kept and run again by name, with the same review, try and publish as any flow.
- The flow is the model's reading of the record. It can be wrong; the review against `asked` and the try with `--draft` are where that is caught, as with `/flow new`.
- A long session costs tokens once, for the record in the message.
- Tests (to add):
  - `TestFlowFromSession`: with the fake provider, a session with tool calls and an approval; `/flow new --from-session` sends one message holding the user's request, the tool calls with arguments and the approval's answer; the model's `flow_write` writes a draft, and nothing is published.
  - The record is cut to the limits, and keeps the first request and the last turn.
  - `--from-session=<id>` of another folder's session is refused.
  - No secret in the message (the record's rule, checked with a session that read an environment variable's name).

## Alternatives considered

- **Translate the record into a flow without the model** (each tool call a `tool()` step). Exact, but it copies the session's detours and fixed values, and cannot tell what should become an argument or a question. The model generalizes; the review checks it.
- **Rely on the session's own context, with no record in the message.** Cheaper, but after compaction or with a CLI engine the model no longer sees the whole session, and `--from-session=<id>` would be impossible.
- **A separate command (`/flow save`).** ADR 0030 chose options of `/flow` over new reserved words, which could clash with flow names.
- **Choose turns with an option (`--turns 3-7`).** Left out for now: the words after the option can say which part, and the record of the whole session lets the model see the context of that part.
