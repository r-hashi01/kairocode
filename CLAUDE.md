@AGENTS.md

## Claude Code 向けの補足

- 編集のたびに hook が gofmt、go vet、`go test -short`（そのパッケージ）を実行します。`internal/workflows` と `internal/tools` を変えたら、不変条件の検査も走ります。失敗の内容は次のターンに返ってくるので、その場で直してください。
- ターンを終える前に、Stop hook が `scripts/check.sh --quick` を実行します。失敗すると終了できないので、直すか、直せない理由を利用者に伝えてください。
- `internal/workflows`、`internal/runtime`、`internal/tools` を変えたら、終える前に `durability-reviewer` サブエージェントで差分をレビューさせてください。
- 手順が決まっている作業にはスキルを使います: `/verify`、`/milestone`、`/tool-change`、`/workflow-change`、`/kairo-gap`、`/adr`。
