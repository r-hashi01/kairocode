---
name: adr
description: Write an Architecture Decision Record for KairoCode in docs/adr (next number, template, index, supersession). Use when a change adds/changes/relaxes an invariant, adds a tool effect type, changes run-ID rules or the kc_message storage format, adds a dependency, deviates from the requirements/design, decides an open question in design section 11, or when the user asks to record a decision.
---

# adr

1. Read `docs/adr/README.md` and the existing ADRs that look related. If the same decision already exists, point to it instead of writing a new one. If the decision is on kairo's side, use `/kairo-gap`.
2. The number is the largest existing number + 1 (4 digits). The file name is `docs/adr/NNNN-slug.md` (the slug is lowercase English letters and hyphens).
3. Write it with the structure of `docs/adr/template.md` (`## Context` / `## Decision` / `## Consequences` / `## Alternatives considered`).
   - In Decision, write where it shows up in the implementation (directories, types, functions).
   - In Consequences, write the tests or hooks that enforce it. If there are none yet, write "None", and ask the user whether to add them.
   - In Alternatives considered, write only the ones actually considered.
   - Set `- Status:` to `Proposed` and the date to today. Only the user sets it to `Accepted`.
4. When overturning an existing decision, write the old number in the new ADR's `- Related:` line, and change only the old ADR's status line to `Superseded by NNNN`. Do not rewrite its body.
5. Add 1 row to the table in `docs/adr/README.md`, and run `scripts/adr-lint.sh`.
6. Tell the user the path of the ADR you wrote and the gist of the decision in 1–2 sentences, and ask for approval.
7. **Do not start implementing until it is approved.** Once it is approved, set the status to `Accepted (date)`, update the index too, and then start implementing.
