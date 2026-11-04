---
name: tool-change
description: Checklist for adding or changing a KairoCode tool (built-in, MCP or custom) - effect type, how an unknown outcome is checked, permission rule shape, and the crash/resume test. Use before and while editing internal/tools.
---

# tool-change

## Decide before the change
- Which effect type it is: `tools.Read`, `tools.Idempotent` or `tools.Real` (the table in design section 3). If unsure, it is `Real`. The zero value is `Real`, but always state it explicitly in `tools.Def` (`TestToolsDeclareEffect`).
- If `Idempotent`: what counts as "the same input gives the same result". When the outcome is unknown, what to look at to check it (the hash of the file's contents, the git tree / commit, whether the branch has a PR). Whether the check's result is "already done", "not yet" or "conflict".
- If `Real`: what to ask a person when the outcome is unknown ("was this command run?"). The answer goes back to kairo with `k.Resolve` or `k.ResolveFailed`. Wrap a handler failure in `kairo.Unknown(err)` when you do not know whether it took effect, and in `kairo.Retryable(err)` only when you can say it did not take effect.
- What "shape of the arguments" the permission rules match against (the string used for matching like `bash: git push*`). Whether it can be used in plan mode (only `Read` can).
- The size of the result. If it can get large, put the body in `kc_message` and keep only the hash in the record.

## Keep to these while implementing
- One tool per file: put the `tools.Def` in `internal/tools/<name>.go`. The mapping to kairo is as in invariant 3 of AGENTS.md, and `internal/runtime` does it. An `Idempotent` tool can use `TaskContext.IdempotencyKey` as the idempotency key for outside operations.
- Normalize paths to inside the working directory (resolving symbolic links too), and make anything that goes outside a definite failure.
- Return conflicts and invalid input to the model as results, not as Go errors.
- Do not put API keys or secrets in the input, the result, or the text of errors.

## Tests
- Unit tests for the tool (`internal/tools/<name>_test.go`).
- For `Idempotent` and `Real` tools, add a kill-and-resume test (`TestCrash...`). Check that the effect (files, git records, the number of handler calls) happens exactly once. For `Idempotent`, also check the conflict when a person changed things partway.

## Finish
- Run `/verify`, and have `durability-reviewer` review the diff.
