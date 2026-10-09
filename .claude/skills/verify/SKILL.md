---
name: verify
description: Run the full KairoCode verification (gofmt, vet, build, all tests with -race including the crash/resume tests, the invariant tests, ADR lint) and report the result. Use before declaring any code change done, or when the user asks to check/verify/test the repository.
---

# verify

1. `scripts/check.sh` を実行します（`--quick` ではなく、`-race` と crash のテストを含む全部）。
2. 失敗したら、どの段（gofmt / vet / build / test / adr）で落ちたかを特定し、原因を直してから、もう一度実行します。
3. `internal/invariants` か `TestCrash...` が落ちたら、期待値を緩めてはいけません。AGENTS.md の該当する不変条件を読み、実装の側を直します。
4. `internal/workflows`、`internal/runtime`、`internal/tools` を変えたなら、`durability-reviewer` サブエージェントに差分をレビューさせます。
5. 報告には、実行したコマンド、通ったか落ちたか、落ちたなら失敗の出力の要点を含めます。直せなかったものは、直せなかったと明記します。
