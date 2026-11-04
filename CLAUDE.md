@AGENTS.md

## Notes for Claude Code

- After every edit, a hook runs gofmt, go vet and `go test -short` (for that package). If you change `internal/workflows` or `internal/tools`, the invariant checks run too. Failures come back on the next turn; fix them right away.
- Before a turn ends, the Stop hook runs `scripts/check.sh --quick`. If it fails, you cannot finish: fix it, or tell the user why you cannot.
- If you change `internal/workflows`, `internal/runtime` or `internal/tools`, have the `durability-reviewer` subagent review the diff before you finish.
- Use the skills for work with a set procedure: `/verify`, `/milestone`, `/tool-change`, `/workflow-change`, `/kairo-gap`, `/adr`.
