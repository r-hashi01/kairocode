---
name: kairo-gap
description: Handle something KairoCode needs that kairo's Go engine does not provide (the code-first flow API, child-run listing, replay mode, a non-resident embedded mode, keeping finished calls while the parent runs, ...). Records it in docs/kairo-gaps.md and drafts a kairo ADR proposal instead of editing ../kairo. Use when an implementation hits a missing kairo API.
---

# kairo-gap

1. Read `docs/kairo-gaps.md` and check whether the same gap is already there. Read `../kairo/engine/` (`go doc github.com/r-hashi01/kairo/engine` also works) and `../kairo/docs/adr/README.md`, and check that it is really missing (that it cannot be expressed with the engine's public API).
2. If it is missing, add 1 row to the table in `docs/kairo-gaps.md`, or update the existing row (the requirements that use it, how much it is needed, the workaround).
3. Decide whether to work around it on KairoCode's side. If you do, put a `// kairo-gap #N:` comment on the workaround code, and write it in the table's "workaround" column. If the workaround relaxes an invariant, do not work around it; stop and consult the user.
4. Write a draft ADR for kairo's side in the form of kairo's `docs/adr/template.md`, and show it to the user. **Do not edit files in `../kairo`** (the hook refuses it). The user files it in the kairo repository, or in a separate session opened in the kairo repository.
5. Tell the user, in 1–2 sentences, the gist of the gap, which milestone it blocks, and whether there is a workaround.
