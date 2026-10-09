---
name: workflow-change
description: Checklist for changing KairoCode workflow code (internal/workflows - session, turn, agent loop, subagents, dynamic-workflow interpreter, written with kairo's Go SDK) so that it stays deterministic on resume and safe across versions. Use before and while editing internal/workflows or the actions whose results the workflows branch on.
---

# workflow-change

## 変更前に決めること
- 新しく分岐に使う値は何か。それはどのアクションの結果として記録されるか（設定、権限、エージェントの定義は、必ずアクションを通す）。
- `kairo.Call` の順番か入力が変わるか。変わるなら、ワークフローの版（`kairo.WorkflowVersion`）を上げ、前の版を `kairo.Draining()` で残します（kairo の ADR 0060）。版を上げずに変えると、進行中のターンが再開で別の道に入り、呼び出しが新しく実行されます。
- 子のワークフロー（ターン、サブエージェント、計画の葉）は `kairo.Child` で始め、ID は SDK に任せます。同じ入力の子を区別するため、入力にターンの番号や計画の中の位置を入れます（AGENTS.md、不変条件 8）。

## 実装中に守ること
- `internal/workflows` では、SDK の `*kairo.Context`（`kairo.Call`、`kairo.Parallel`、`kairo.WaitFor`、`kairo.Child`、`ctx.Now`、`ctx.Random`、`ctx.Sleep`）以外で外を見ません。hook と `TestWorkflowCodeIsDeterministic` が、import の許可リスト、時計、goroutine、`select`、map の走査を検査します。
- 許可された import 先（`internal/msg`、`internal/plan`）に I/O や時計を入れるのも同じく禁止です。検査では見つけられないので、自分で確かめます。
- パッケージの変数、`Parallel` の中での結果の順番、エラーの文面に結果を依存させません。
- モデルの呼び出しの入力は、ハッシュの列にします（不変条件 5）。

## テスト
- 同じ記録から再開して、同じ呼び出しの列になることを確かめるテストを足します。`kairotest` の `env.Calls(id)` で呼び出しの列を見られます。時計に関わるものは `env.Advance(d)` で進めます。
- 再開に関わる変更なら、途中で強制終了して再開するテスト（`TestCrash...`）を足します。

## 仕上げ
- `/verify` を実行し、`durability-reviewer` に差分をレビューさせます。
