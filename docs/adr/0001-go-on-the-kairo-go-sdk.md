# 0001 Write KairoCode in Go and run it on kairo's Go SDK (the embedded runtime)

- Status: Accepted (2026-10-09)
- Date: 2026-10-09 (the 2026-10-08 proposal, rewritten for kairo 0.3.0's Go SDK)
- Related: requirements NFR-2, NFR-3, NFR-4, NFR-8, FR-7, FR-21, FR-24; design sections 1, 2, 8.3, 9 and 11; kairo's ADR 0049, 0051, 0054, 0058, 0059, 0060

## Context

The design (the 2026-10-08 proposal) had KairoCode written in TypeScript, using `kairo-sdk` (npm) embedded (design section 1, "Node 22 or later" in NFR-8). The SDK at that time lacked things that M1 and M2 require (live output, settling a blocked (needs confirmation) call, declaring idempotent retries).

kairo 0.3.0 (2026-10-09) shipped the Go SDK `github.com/r-hashi01/kairo/sdk/go` (kairo's ADR 0058).

- **Workflows written in code:** `kairo.Call`, `Parallel`, `WaitFor` (with a deadline), `Child`, `ctx.Now`, `ctx.Random`, `ctx.Sleep`. Call IDs follow the same rule as TS and Python (`<parent ID>/<first 24 characters of sha256(kind + normalized input)>.<n>`).
- **Embedded runtime:** it calls the core natively and keeps state in `database/sql` (SQLite or PostgreSQL). It has a waiting mode (resident) and a suspend mode (serverless).
- **What KairoCode needs:**
  - live output (`TaskContext.Emit`, `k.Subscribe`; prefix match on the ID, so descendants' output arrives too)
  - settling a blocked (needs confirmation) call (`k.Resolve`, `k.ResolveFailed`)
  - idempotent retries (`IdempotentRetry()`, `TaskContext.IdempotencyKey`)
  - listing child runs (`k.Children`)
  - deletion that does not remove the records of a tree that is waiting (ADR 0054)
  - resuming in another process through a lease on driving (ADR 0059)
  - pinning workflow versions (ADR 0060)
  - a test environment that moves the clock (`kairotest`)

On 2026-10-09 we confirmed the following two things on the published v0.3.0.

- When a turn that waited 48 hours for an approval (finished runs are kept for 24 hours) is resumed in another process, finished real calls are not run again.
- When a real call's handler is SIGKILLed partway, the resume makes it `blocked`, and `Resolve` lets it continue.

## Decision

- KairoCode is written in Go (`github.com/r-hashi01/kairocode`, Go 1.27.1). kairo is used through the embedded runtime (`kairo.Open`) of the Go SDK `github.com/r-hashi01/kairo/sdk/go` (v0.3.0 or later).
- Storage is SQLite (`modernc.org/sqlite`) locally, and PostgreSQL for serverless and teams. The local server runs in waiting mode, and serverless (M4) in suspend mode.
- kairod (`kairo.Connect`) is not used in the first version, because `Resolve`, `Children`, `List` and suspend mode are not available through it (`kairo.ErrNeedsEmbedded`).
- Effect types map as `tools.Read` → `kairo.Unprotected`, `tools.Idempotent` → `kairo.Real` + `IdempotentRetry()`, `tools.Real` → `kairo.Real`.
- For run IDs, only the root (the session) is set with `WithID`. Turns, subagents and plan leaves let `kairo.Child` set them by the SDK's rule. We do not take the `s/<ID>/t/<n>` form of design section 2.1. Instead, we tell children apart by putting the turn number or the position in the plan into the child's input.
- The turn workflow has a version (`WorkflowVersion`). Even when KairoCode is updated, a turn in progress finishes on the version it started with (design section 8.3).
- It is distributed as a single binary (`cmd/kairocode`).
- Requirement NFR-8 is read as "macOS and Linux. A single binary (built with Go 1.27)", and "TypeScript, Node 22 or later, kairo-sdk" in design section 1 is also superseded by this ADR. The text of the requirements and the design lives in kairo's repository, so the user updates it there.
- This ADR does not decide the TUI library (item 2 of design section 11).

## Consequences

- Of the gaps in design section 9, 1 (live output), 2 (settling), 3 (listing children), 5 (large output; `kc_message` keeps inputs and outputs small) and 7 (idempotent retries) are solved by kairo 0.3.0. 4 (run limits) is decided on KairoCode's side from the recorded usage. The remaining gaps are in `docs/kairo-gaps.md`.
- Whether workflows are deterministic is checked with go/ast and go/types (`internal/invariants`). The SDK's packages go into the import allowlist.
- Tests that involve the clock are written with `kairotest` (which moves the clock), and tests that involve forced termination with a child process and a SQLite file.
- Custom tools (FR-9) are added as MCP servers or outside commands. There is no way to write a tool in JavaScript.
- kairo SDK's tables and call IDs are the same across the three languages. That leaves a path to handling the same session from a TS or Python process in the future.

## Alternatives considered

- **Write it in TypeScript as the design said:** in 0.3.0, the TS SDK is also planned to get live output and the rest (item 3 of kairo's ADR 0058), but the Go SDK has them first. Distributing a single binary and not going through WASM are also Go's advantages.
- **Use kairo's engine (`engine.Engine`) directly and build the code-first API on KairoCode's side (this ADR's 2026-10-08 proposal):** we would have had to work around the problem of finished call records disappearing after 24 hours (the former gap #10) ourselves, and the call ID rule could have drifted from the other languages. Now that kairo has a Go SDK, we do not take this.
- **Connect to kairod (`kairo.Connect`):** it can enforce RPM and TPM across processes, but as above, the APIs we need are not available. We will reconsider it for team use (the 4th row of users in the requirements).
