# 0024 Limits (tokens, cost, time) and per-provider rate limits

- Status: Proposed (2026-10-22; on hold by the user's decision. To be revisited when circumstances arise such as KairoCode itself billing, or narrowing the models that can be used)
- Date: 2026-10-21
- Related: requirements FR-26 (limits) and FR-27 (rate limits), design section 5 ("budget decisions are made from recorded values") and section 9 (not offered by kairo), invariants 2 and 7

## Context

Requirement FR-26 is "be able to set limits per session on the number of steps, time, tokens and cost. When exceeded, stop and notify a person", and FR-27 is "respect each provider's RPM and TPM".

What exists today is only a limit on the number of steps per turn (`Agent.MaxSteps`, default 50). Usage (`msg.Usage`) is recorded for each model call.

kairo's `RunLimits` cannot be specified from the code-written API (not offered by kairo). The design says "KairoCode decides the budget on its side instead".

## Decision

1. **Settings:** `limits` in `kairocode.json`. Both the user's and the project's are read, and the smaller takes effect (a project cannot loosen a limit).
   - `{"limits": {"steps": 50, "turn_time": "30m", "session_tokens": 2000000, "session_cost": 5.0}}`.
   - `steps`: the number of steps per turn (today's `MaxSteps`).
   - `turn_time`: the time since the turn started.
   - `session_tokens`: the session's total of input and output tokens (cache reads are counted too).
   - `session_cost`: the session's cost (US dollars).
   - For each, if not written, there is no limit (`steps` keeps today's default of 50).
2. **Decisions use only recorded values (invariant 2).**
   - The limit values are recorded in `agent.load`'s output. Even if the settings change during a turn, that turn proceeds with the limits from its start.
   - Tokens and cost: the total usage of the session's earlier turns is passed as the turn's input (`TurnInput.Spent`), and the usage of model responses within the turn is added to it. Both are recorded values. The decision is made before calling the model.
   - Time: compare the turn's start with now using `ctx.Now()` (a recorded time).
   - Time spent waiting for approval is not counted in `turn_time` (so as not to stop on time spent waiting for a person).
3. **When exceeded:** the turn ends with `Stopped: "limit"`, the reply says which limit was exceeded, and the client is notified (`stopReason` in ACP, one line in the terminal UI). It can continue with the next message. When a `session_*` limit is exceeded, the next turn also stops immediately until the limit is raised.
4. **Cost:** computed by multiplying usage by a per-model price table (`internal/provider`). For models not in the table, unless the user's setting (`models.<name>.price`) exists, cost is not counted, and if `session_cost` is set, the user is told so. Prices are not recorded; usage is (fixing the table also fixes the amounts shown when viewing).
5. **CLI engines:** for those whose CLI reports usage (Claude Code's result, ACP's `usage`), it is counted the same way. For those that do not report it, the user is told that the token and cost limits do not apply.
6. **Rate limits (FR-27):** inside the `model` action (`internal/runtime`), each provider's RPM and TPM are respected with a token bucket in the daemon's process.
   - The settings are `providers.<name>.rpm` and `tpm` in `kairocode.json`. If not written, there is no limit.
   - When a provider returns 429, it returns `kairo.Retryable` following `Retry-After` (kairo holds the retry interval).
   - Waiting on the bucket is a short wait inside an action. It does not wait for a person or an outside event, so invariant 7 does not apply. There is one daemon, so no limit across processes is needed (for kairod it is separate, as in design section 1).
7. **Version:** no calls are added for the limit decisions (the decision is a computation inside the workflow). But where it stops changes, so the turn workflow's version is bumped (grouped into the same version 5 as ADR 0020).

Where this shows in the implementation: `internal/workflows/turn.go` (the decision), `internal/workflows/session.go` (passing `Spent`), `internal/runtime` (limits in `agent.load`, the price table, rate limits), `internal/provider` (prices, 429), `internal/acp` and `internal/tui` (notification).

## Consequences

- When a limit is exceeded, it stops and notifies a person. It is decided by recorded values, so on resume it stops at the same point.
- Providers' rate limits are respected.
- Tests (added during implementation)
  - When the fake provider's usage exceeds `session_tokens`, it stops before the next model call. On resume it stops at the same point.
  - Exceeding `turn_time` with `kairotest`'s clock stops it. Time waiting for approval is not counted.
  - `session_cost` is computed with the price table. Models not in the table are not counted, and the user is told.
  - A project's `limits` does not loosen the user's limits.
  - Calls over the RPM are made to wait. A 429 becomes `Retryable`.

## Alternatives considered

- **Wait for kairo's `RunLimits` (not offered by kairo):** kairo's step limit does not know about KairoCode's tokens or cost. Deciding on KairoCode's side fits the requirements better.
- **Decide limits inside the action (inside `model`):** it would stop on an unrecorded value (the total at that moment), and where it stops could change on resume. Decide inside the workflow, from recorded usage.
