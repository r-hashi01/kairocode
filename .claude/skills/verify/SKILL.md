---
name: verify
description: Run the full KairoCode verification (gofmt, vet, build, all tests with -race including the crash/resume tests, the invariant tests, ADR lint) and report the result. Use before declaring any code change done, or when the user asks to check/verify/test the repository.
---

# verify

1. Run `scripts/check.sh` (not `--quick`; the full run, including `-race` and the crash tests).
2. If it fails, find which step (gofmt / vet / build / test / adr) failed, fix the cause, and run it again.
3. If `internal/invariants` or a `TestCrash...` fails, do not relax the expected values. Read the matching invariant in AGENTS.md and fix the implementation.
4. If you changed `internal/workflows`, `internal/runtime` or `internal/tools`, have the `durability-reviewer` subagent review the diff.
5. In the report, include the commands you ran, whether they passed or failed, and, if they failed, the gist of the failure output. State plainly anything you could not fix.
