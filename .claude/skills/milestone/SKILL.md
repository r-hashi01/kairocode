---
name: milestone
description: Work through a KairoCode milestone (M0-M4 in docs/milestones.md) acceptance-test first - pick the current milestone, write its acceptance criteria as failing tests, implement until they pass, review, and mark it done. Use when starting or continuing implementation work, or when the user says "next milestone", "M0", "M1", etc.
---

# milestone

1. Read "Current stage" in `docs/milestones.md`, and that milestone's contents and acceptance criteria. Also read the related sections of the design (`../kairo/docs/kairocode/design.md`) and the matching FR / NFR of the requirements.
2. List any decisions in the milestone that need an ADR (open questions in design section 11, adding a dependency, anything that falls under "Recording design decisions (ADR)" in AGENTS.md). If there are any, write them first with `/adr` and wait for the user's approval. While waiting, work only on the parts that do not need an ADR.
3. Check whether any of the things missing on kairo's side (`docs/kairo-gaps.md`) are required for this milestone. If so, decide how to handle them with `/kairo-gap`.
4. Write the acceptance criteria as tests first. Check that they fail (if they do not fail, the test does not check the criterion).
   - Write "does not happen twice" and "can resume" as `TestCrash...`. Start with `if testing.Short() { t.Skip(...) }`, start the test binary again as a child process (`os.Args[0]`, `-test.run`, pass the role in an environment variable), `SIGKILL` it partway, and resume with the same DB (`t.TempDir()`). Decide the moment to kill it by having the fake provider or the tool's handler send a signal (a file or a line on standard output) (not with a fixed sleep).
   - Count in the handler, keep the count in the DB or a file, and check it in the parent process.
5. Implement in small units, and pass the acceptance tests one at a time. For tools, follow `/tool-change`; for workflows, follow `/workflow-change`.
6. Run `/verify`, have `durability-reviewer` review the diff, and fix what it points out.
7. Check off the acceptance criteria in `docs/milestones.md`. When all pass, set the milestone's status to "done (date)", and move "Current stage" to the next one.
8. Report briefly to the user: the acceptance criteria that passed, the ADRs you wrote and the kairo gaps, and the remaining issues.
