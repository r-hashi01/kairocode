# 進め方（M0〜M4）

設計 10 節の段階を、受け入れ条件のテストに落としたものです。段階の作業は `/milestone` スキルで進めます。受け入れ条件は、実装の前にテストとして書きます（最初は落ちる）。すべて通ったら、その段階の「状態」を「完了（日付）」にします。

今の段階: **M1（未着手）**

## M0 骨組み

状態: 完了（2026-10-09）。ただし会話の保存の形（ADR 0006）が承認待ち

中身: `go.mod` に kairo の SDK（`github.com/r-hashi01/kairo` v0.3.0 以上）と `modernc.org/sqlite` を足す。`internal/runtime`（`kairo.Open`、ツールの作用の写し）。サーバー、CLI、セッションとターンのワークフロー、モデル（Anthropic と fake）、読むツール（read、glob、grep、list）、メッセージの保存（`kc_message`）。

前提: ADR 0001 の承認。

決めること: セッションとターンの形（設計 2.1 節）。セッション（根）が `kairo.WaitFor("message")` と `kairo.Child("turn", …)` を繰り返すか、ターンを根の実行として `WithID` で始めるか。前者は木の単位の削除と取り消しの伝わり方を使えるが、再開でセッションの関数がターンの数だけ記録を読み直す。

受け入れ条件:


- [x] `TestCrashTurnResume`: ターンの途中（モデルの 2 回目の呼び出しの最中）でプロセスを強制終了し、再開すると同じ最終応答まで進む。終わったモデルの呼び出しは、もう一度は呼ばれない（fake の呼び出し回数で確かめる）。
- [x] `TestHistoryStoredOnce`: 10 ターンの会話で、`kc_message` の行の数がメッセージの数と等しい。モデルの呼び出しの入力にメッセージの本文が入っていない（NFR-5）。
- [x] `TestWorkflowIDs`: セッションの ID が `s/<ID>` で、失敗したターンの後に同じメッセージを送り直しても、ターンが別の実行になる（子の入力にターンの番号が入る）。
- [x] `kairocode run "<依頼>"` が、fake のプロバイダーで最後まで動く。
- [x] `internal/invariants` の検査が、実際のワークフローとツールに対して通る。

## M1 書くツールと承認

状態: 未着手

M0 のレビューからの持ち越し（`durability-reviewer`、2026-10-09）:

- 実のツールの handler で、作用の後に起きたエラー（tool_result の保存の失敗など）は `kairo.Unknown` で包む。`tools.Def.Run` から「効いたかわからない」を返せるようにする（不変条件 4）。
- 書くツールは、`resolve` の後の差し替え（TOCTOU）に備え、`os.OpenRoot` か `O_NOFOLLOW` でたどる。
- `kairocode serve` が自動で作ったトークンを stderr に出している。ADR 0004 の `server.json`（0600）に置き換える。

中身: edit、write、bash、git（commit、push）、作用の型、権限の規則と判定（`permission.check`）、承認の待ち、要確認、コマンドの sandbox（ADR 0005）。

受け入れ条件:

- [ ] `TestCrashWriteOnce`: write と edit の最中に強制終了して再開しても、ファイルの書き込みは 1 回分の結果になる。人が途中で書き換えた場合は、衝突としてモデルに返る。
- [ ] `TestCrashGitCommitOnce`: git commit の最中に強制終了して再開しても、commit は 1 つだけできる。
- [ ] `TestCrashBashNeedsReview`: bash（実）の最中に強制終了すると、再開で「要確認」になり、答え（実行された／されなかった）が `k.Resolve` / `k.ResolveFailed` で返り、続く。コマンドは確かめずに再実行されない。
- [ ] `TestApprovalFromAnotherProcess`: 承認を待っているセッションに、別のプロセスから承認を出すと、セッションが続く。
- [ ] `TestPermissionRecordedAtTurnStart`: 承認待ちの間に権限の設定を変えても、再開したターンは開始時の判定で進む。
- [ ] plan のモードでは、`tools.Read` のツールしか呼ばれない。
- [ ] sandbox（ADR 0005）: `TestSandboxDeniesWriteOutsideWorkspace`、`TestSandboxProtectsGitAndConfig`、`TestSandboxDeniesNetwork`、`TestSandboxStripsSecrets`、`TestSandboxIgnoresModelCwdOutsideRoot`、`TestSandboxFailsClosed`、`TestEscalationAlwaysAsks` が、macOS と Linux の両方で通る。
- [ ] 24 時間を超えて承認を待ったターンが落ちて再開しても、終わった呼び出しをやり直さない（`TestResumeAfterLongApproval`。kairo 0.3.0 で解けたことを、KairoCode のターンで確かめ続ける）。

## M2 クライアント、ストリーミング、取り消し、dspchr

状態: 未着手

中身: ACP のエージェント（ADR 0002。`kairocode acp`、v1）、端末の対話（`kairocode chat`、ACP のクライアントとして）、Claude Code のエンジン（ADR 0007）、トークンのストリーミング（`TaskContext.Emit` と `k.Subscribe`）、スナップショットと `/undo`・`/redo`、LSP の診断、エージェントとコマンドの Markdown の定義、dspchr との統合の段階 1（ADR 0004）。

前提: ADR 0002、0004、0007 の承認。

受け入れ条件:

- [ ] SSE で、トークン、ツールの開始と終了、承認の求め、要確認、ターンの終わりが届く。
- [ ] `TestACPContract`: 実装した ACP のメソッドの要求と応答が、固定した版の ACP の JSON Schema に合う。
- [ ] 決まった順のメッセージを流す ACP のクライアントで、`session/new` → `prompt` → `tool_call` の更新 → `request_permission` → 答え → ターンの終わりが通る。要確認も `request_permission` で答えられる。
- [ ] `TestACPApprovalSurvivesRestart`: 承認を待っている間に ACP のプロセスを止めても、次のプロセスで `session/load` すると保留中の `request_permission` がもう一度届き、答えるとターンが続く。
- [ ] Claude Code のエンジン（ADR 0007）: `TestClaudeEngineResumeDoesNotRepeatARealCall`（偽の `claude` で、落ちた後に新しい ID で呼び直された実のツールが二重に動かない）。本物の `claude` では手で確かめる。
- [ ] edit のあと、LSP の診断のエラーがツールの出力に足される。
- [ ] `.opencode/agents/*.md`、`.opencode/commands/*.md`、`.opencode/skills/` を、opencode と同じ意味で読む。
- [ ] `/undo` で作業ファイルが前のターンの状態に戻り、`/redo` で戻る。記録は消えない。
- [ ] `TestDspchrStateReports`: `DSPCHR_SESSION` があるとき、ターンの始まり・終わり・承認の求め・要確認・失敗・閉じるときに、`dspchr state` が ADR 0004 の表のとおりに起動される（`dspchr` は引数を記録する偽のコマンドに差し替える）。`DSPCHR_SESSION` が無ければ何も起動しない。
- [ ] `kairocode usage --session <ID> --json` が、記録された使用量（入力・出力・キャッシュ）を返す。
- [ ] 普段の作業（バグの修正、機能の追加、リファクタリング）を、ACP のクライアント（Zed）から、opencode と同等の手間でこなせる（手で確かめる）。

## M3 サブエージェントと、人と出来事が始めるワークフロー

状態: 未着手

中身: task（サブエージェント）。Starlark のワークフロー（ADR 0003: `.kairocode/workflows/<name>.star`、プリミティブ `agent`・`parallel`・`decide`・`tool`・`ask`、ずれの検出、上限）。入口はコマンド（`/<name>`）、hook（`turn.end`、`tool.after`、`session.start`）、エージェントの `workflow:`。モデルがその場で書くスクリプト（`workflow` のツール）。`kairocode workflow graph`・`show`・`stats`。

前提: ADR 0003 の承認。

受け入れ条件:

- [ ] `TestCrashWorkflowScriptResume`: 10 個の `agent` を `parallel` で並べたスクリプトを途中で強制終了して再開すると、終わった葉はやり直さない。
- [ ] `TestWorkflowScriptDivergence`: 記録と違う呼び出しをするスクリプトで再開すると、実行せずに止まる。
- [ ] `TestWorkflowScriptLimits`: ステップ数、エージェントの数、トークンの上限で止まる。
- [ ] `TestTurnEndHookBlocks`: `turn.end` のワークフローが `ok` を偽で返すと、メッセージがモデルに返ってループが続き、上限の回数で止まる。
- [ ] `TestWorkflowGraph`: 静的な図が、`for`、`if`、`parallel` の入れ子を正しく表す。
- [ ] `kairocode workflow show <実行> --follow` が、各ノードの状態とスクリプトの行を示す。
- [ ] 走っている途中でスクリプトを書き換えても、走っている実行は記録したスクリプトで終わる。
- [ ] リポジトリの hook が初めて走る前に、利用者に確認する。
- [ ] エージェントの定義に `workflow:` を書くと、そのエージェントのターンがそのワークフローで進む。
- [ ] 親を止めると、子も止まる。

## M4 非対話とサーバーレス

状態: 未着手

中身: PostgreSQL、サスペンドモード（`kairo.Suspend`、`k.Handler()` の `/tick`）、HTTP(S) のツール、GitHub からの起動。

受け入れ条件:

- [ ] Issue のコメントから起動し、承認と CI の結果を待つ間、プロセスが残らない。
- [ ] `kairocode run` が承認で中断し、`kairocode resume` で続く。
