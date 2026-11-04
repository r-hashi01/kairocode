# Draft ADR for kairo: make completion and cancellation of code-written workflows exclusive

From KairoCode's kairo-gap #22. This is a draft to file as an ADR in the kairo repository. This repository does not edit `../kairo`.

---

# 0061. For code-written workflows, only whichever of completion and cancellation is recorded first takes effect

- Status: Proposed
- Date: 2026-10-19 (on 2026-10-28, confirmed it is still not fixed in kairo's `00376a0` (after v0.4.1). The number is the next number at that time)
- Related: ADR 0026 (cancellation of a running task), 0058 (the Go SDK), 0059 (driving workflows), 0060

## Context

In the Go SDK, `runAs` (`sdk/go/kairo.go`), which drives a code-written workflow, works in this order.

1. Call the workflow function.
2. If the function returned an error: if the run has been cancelled (`cancelled()`), return `ErrCancelled`.
3. If the function returned success: without looking at `cancelled()`, record the `done` signal with `runs.signal`, and return the value.

If `k.Cancel(id)` is recorded between the function returning and `done` being recorded, the following happens.

- The run settles as `cancelled`.
- The later `done` is ignored by the core as an event for a settled run (`res.Ignored`). It is not an error.
- In live execution, the parent's `Child` gets the function's value. When it is read again (the parent resumes), `done()` sees `Status == "cancelled"` and returns `ErrCancelled`.

The relevant code as of 2026-10-28:

- `runAs` in `sdk/go/kairo.go`: the success path (`k.runs.signal(..., "done", {"ok": true, ...})`) does not look at `cancelled()`. The failure path discards the `signal` error with `_ =`.
- `runtime.signal` in `sdk/go/runtime.go`: when the core ignores the event (`res.Ignored`), it records nothing and returns `nil`.
- `k.Cancel` returns `k.runs.cancel` as is, and does not say whether the run had already settled.

When the function returns failure too, if the goroutine watching `cancelled()` has not yet noticed the cancellation, it is likewise a failure in live execution and `ErrCancelled` on resume.

The parent workflow uses the value in live execution and takes a different path with an error on resume, so the input and IDs of the calls after that drift. As a result, it can run a finished call again.

In KairoCode, the user's action to stop a turn (Esc, ACP's `session/cancel`) is built as a `k.Cancel` on the turn's run. Stopping it at the moment the turn finishes falls into this gap.

## Decision (draft)

- In `runAs`, when the function returns success, check the result of recording `done`. If `done` was ignored (the run had already settled), follow the recorded outcome. If it was cancelled, return `ErrCancelled` and do not return the value.
  - For that, `runs.signal` (`process`) returns to the caller that the event was ignored (for example, `ErrRunSettled`).
- Conversely, if `done` was recorded first, `k.Cancel` does nothing (as now; a cancellation of a settled run is ignored). It would be good for `Cancel` to be able to return that "it had already settled" (`ErrRunSettled`), so the caller can know.
- Provide a hook in `kairotest` that can insert a cancellation right after the function returns, and add a test that checks that `Child`'s result is the same in live execution and on resume.

## Consequences

- Only whichever of completion and cancellation is recorded first takes effect, and the result the parent sees matches in live execution and on resume.
- The caller of `Cancel` can know whether the cancellation took effect. KairoCode can tell the user "the turn had already finished" when it did not.

## Alternatives considered

- **The caller (KairoCode) checks that the run has not finished before cancelling:** It can finish between the check and the cancellation, so the gap only narrows; it does not go away.
- **Make cancellation a signal inside the workflow (checked between steps):** It cannot stop a running model call or command on the spot (an alternative in KairoCode's ADR 0011).
