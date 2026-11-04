# 0000 Record design decisions as ADRs

- Status: Accepted (2026-10-09)
- Date: 2026-10-08
- Related: kairo's ADR 0000, 0017

## Context

KairoCode is built from the requirements and the design (kairo's `docs/kairocode/`), and agents do much of the implementation. If the reasons for a decision stay only in a conversation, later work repeats the same discussion, or relaxes an invariant without knowing it is one.

## Decision

- Design decisions on KairoCode's side are written in `docs/adr/NNNN-slug.md`, with an index in `docs/adr/README.md`.
- A change that needs an ADR (the "Recording design decisions (ADR)" section of AGENTS.md) is filed as "Proposed", and implementation starts only after the user's approval.
- Decisions on kairo's side are written in kairo's ADRs. KairoCode's ADRs refer to them.

## Consequences

- `scripts/adr-lint.sh` checks for duplicate numbers, required sections and entries missing from the index (it is called from `scripts/check.sh`).
- The `/adr` skill holds the steps for filing one.

## Alternatives considered

- Keep adding to the design (design.md): we did not take this because the time and the reason of each decision are hard to follow, and supersession relationships are not kept.
