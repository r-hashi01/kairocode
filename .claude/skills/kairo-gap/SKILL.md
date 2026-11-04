---
name: kairo-gap
description: Handle something KairoCode needs that kairo does not provide (an API, a guarantee, a mode). Files it as an issue on r-hashi01/kairo instead of editing ../kairo, and marks any workaround in KairoCode with the issue's number. Use when an implementation hits something missing in kairo.
---

# kairo-gap

What kairo is missing is tracked as issues on r-hashi01/kairo, not in this repository.

1. Look for an issue on r-hashi01/kairo that already covers it (`gh issue list -R r-hashi01/kairo --state all --search …`). Read `../kairo/sdk/go/README.md` (`go doc github.com/r-hashi01/kairo/sdk/go` also works) and `../kairo/docs/adr/`, and check that it is really missing: that it cannot be done with kairo's public API.
2. If there is no issue, open one in English (`gh issue create -R r-hashi01/kairo`): a summary, what was observed (the kairo version, a minimal sequence), why KairoCode needs it (the requirement or ADR), KairoCode's workaround if any, and a proposal (a draft of kairo's ADR when it is a design decision). If one exists, add what is new as a comment.
3. Decide whether to work around it on KairoCode's side. If you do, put a `// kairo#N:` comment on the workaround code (N is the issue's number), and say so in the issue. If the workaround relaxes an invariant, do not work around it; stop and consult the user.
4. **Do not edit files in `../kairo`** (the hook refuses it). Changes there are made in the kairo repository, after its own ADR is approved.
5. Tell the user, in 1–2 sentences, the gist of the gap, the issue's link, which milestone it blocks, and whether there is a workaround.
