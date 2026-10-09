---
name: tool-change
description: Checklist for adding or changing a KairoCode tool (built-in, MCP or custom) - effect type, how an unknown outcome is checked, permission rule shape, and the crash/resume test. Use before and while editing internal/tools.
---

# tool-change

## 変更前に決めること
- 作用の型は `tools.Read`、`tools.Idempotent`、`tools.Real` のどれか（設計 3 節の表）。迷ったら `Real` です。ゼロ値は `Real` ですが、`tools.Def` には必ず明示します（`TestToolsDeclareEffect`）。
- `Idempotent` なら、何をもって「同じ入力で同じ結果」とするか。結果が不明なとき、何を見て確かめるか（ファイルの内容のハッシュ、git の tree / commit、ブランチの PR の有無）。確かめた結果が「すでに済んでいる」「まだ」「衝突」のどれになるか。
- `Real` なら、結果が不明なときに人に何を聞くか（「このコマンドは実行されたか」）。答えは `k.Resolve` か `k.ResolveFailed` で kairo に返します。handler の失敗は、効いたかわからないなら `kairo.Unknown(err)`、効いていないと言えるときだけ `kairo.Retryable(err)` で包みます。
- 権限の規則の対象になる「引数の形」は何か（`bash: git push*` のような照合に使う文字列）。plan のモードで使えるか（`Read` だけが使える）。
- 結果の大きさ。大きくなりうるなら、本文は `kc_message` に入れ、記録にはハッシュだけを残します。

## 実装中に守ること
- 1 ファイル 1 ツールで、`internal/tools/<name>.go` に `tools.Def` を置きます。kairo への写し方は AGENTS.md の不変条件 3 のとおりで、`internal/runtime` が行います。`Idempotent` のツールは、`TaskContext.IdempotencyKey` を外への操作の冪等キーに使えます。
- パスは作業ディレクトリの中に正規化し（シンボリックリンクも解決する）、外に出るものは確定した失敗にします。
- 衝突や不正な入力は、Go のエラーではなく結果としてモデルに返します。
- API キーや秘密を、入力・結果・エラーの文面に入れません。

## テスト
- ツールの単体テスト（`internal/tools/<name>_test.go`）。
- `Idempotent` と `Real` のツールは、強制終了と再開のテスト（`TestCrash...`）を足します。作用（ファイル、git の記録、handler の呼び出し回数）が 1 回分だけになることを確かめます。`Idempotent` は、途中で人が書き換えた場合の衝突も確かめます。

## 仕上げ
- `/verify` を実行し、`durability-reviewer` に差分をレビューさせます。
