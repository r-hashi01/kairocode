---
name: workflow-change
description: Checklist for changing KairoCode workflow code (internal/workflows - session, turn, agent loop, subagents, dynamic-workflow interpreter, written with kairo's Go SDK) so that it stays deterministic on resume and safe across versions. Use before and while editing internal/workflows or the actions whose results the workflows branch on.
---

# workflow-change

## Decide before the change
- What new value the code branches on, and which action's result records it (settings, permissions and agent definitions always go through an action).
- Whether the order or the input of `kairo.Call` changes. If it does, raise the workflow's version (`kairo.WorkflowVersion`), and keep the previous version with `kairo.Draining()` (kairo's ADR 0060). If you change it without raising the version, an in-progress turn takes a different path on resume, and calls are run anew.
- Start child workflows (turns, subagents, plan leaves) with `kairo.Child`, and leave the ID to the SDK. To tell apart children with the same input, put the turn number or the position in the plan into the input (AGENTS.md, invariant 8).

## Keep to these while implementing
- In `internal/workflows`, do not look outside except through the SDK's `*kairo.Context` (`kairo.Call`, `kairo.Parallel`, `kairo.WaitFor`, `kairo.Child`, `ctx.Now`, `ctx.Random`, `ctx.Sleep`). The hook and `TestWorkflowCodeIsDeterministic` check the import allowlist, clocks, goroutines, `select`, and map iteration.
- Putting I/O or clocks into an allowed import (`internal/msg`) is forbidden in the same way. The checks cannot find it, so check it yourself.
- Do not make results depend on package variables, the order of results inside `Parallel`, or the text of errors.
- Make the model call's input a list of hashes (invariant 5).

## Tests
- Add a test that resumes from the same record and checks that it gives the same sequence of calls. You can see the sequence of calls with `kairotest`'s `env.Calls(id)`. For anything involving the clock, advance it with `env.Advance(d)`.
- If the change involves resuming, add a test that kills the process partway and resumes (`TestCrash...`).

## Finish
- Run `/verify`, and have `durability-reviewer` review the diff.
