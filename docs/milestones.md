# 進め方（M0〜M4）

設計 10 節の段階を、受け入れ条件のテストに落としたものです。段階の作業は `/milestone` スキルで進めます。受け入れ条件は、実装の前にテストとして書きます（最初は落ちる）。すべて通ったら、その段階の「状態」を「完了（日付）」にします。

今の段階: **M2（作業中）**

## M0 骨組み

状態: 完了（2026-10-09）

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

状態: 完了（2026-10-09）

M0 のレビューからの持ち越し（`durability-reviewer`、2026-10-09）:

- 実のツールの handler で、作用の後に起きたエラー（tool_result の保存の失敗など）は `kairo.Unknown` で包む。`tools.Def.Run` から「効いたかわからない」を返せるようにする（不変条件 4）。
- 書くツールは、`resolve` の後の差し替え（TOCTOU）に備え、`os.OpenRoot` か `O_NOFOLLOW` でたどる。
- `kairocode serve` が自動で作ったトークンを stderr に出している。ADR 0004 の `server.json`（0600）に置き換える。 → M2 の始めに、ADR 0008 のデーモンで解決（`kairocode serve` は無くなり、トークンは `server.json` に置く）。

中身: edit、write、bash、git（commit、push）、作用の型、権限の規則と判定（`permission.check`）、承認の待ち、要確認、コマンドの sandbox（ADR 0005）。

作業の順番（2026-10-09）:

1. ツールの契約: 「効いたかわからない」を返せるようにする。作用の後のエラーは `kairo.Unknown` で包む。
2. ファイルの操作を `os.Root` に移す（TOCTOU を塞ぐ）。
3. edit と write: 「計画」（読むだけ。書く前と書いた後のハッシュを記録する）と「適用」（冪等。比べて置き換える）の 2 段にする（設計 3.1 節）。`TestCrashWriteOnce`。
4. 権限の規則、`permission.check`、承認の待ち、plan のモード（ADR 0002 の第 1 部の承認が要る）。
5. sandbox（ADR 0005）。macOS と Linux。
6. bash と要確認。`TestCrashBashNeedsReview`。
7. git の commit と push。`TestCrashGitCommitOnce`。
8. Claude Code のエンジンで、実のツールを MCP に通す。自前の表に結果を記録する（kairo の不足 #17 の手当て）。前のプロセスで終わった実の呼び出しと同じものが来たら、人に確かめる。
9. `TestResumeAfterLongApproval`。サーバーのトークンを `server.json` に移す。

受け入れ条件:

- [x] `TestCrashWriteOnce`: write と edit の最中に強制終了して再開しても、ファイルの書き込みは 1 回分の結果になる。人が途中で書き換えた場合は、衝突としてモデルに返る。
- [x] `TestCrashGitCommitOnce`: git commit の最中に強制終了して再開しても、commit は 1 つだけできる。
- [x] `TestCrashBashNeedsReview`: bash（実）の最中に強制終了すると、再開で「要確認」になり、答え（実行された／されなかった）が `k.Resolve` / `k.ResolveFailed` で返り、続く。コマンドは確かめずに再実行されない。
- [x] `TestApprovalFromAnotherProcess`: 承認を待っているセッションに、別のプロセスから承認を出すと、セッションが続く。
- [x] `TestPermissionRecordedAtTurnStart`: 承認待ちの間に権限の設定を変えても、再開したターンは開始時の判定で進む。
- [x] plan のモードでは、`tools.Read` のツールしか呼ばれない。
- [x] sandbox（ADR 0005）: `TestSandboxDeniesWriteOutsideWorkspace`、`TestSandboxProtectsGitAndConfig`、`TestSandboxDeniesNetwork`、`TestSandboxStripsSecrets`、`TestSandboxIgnoresModelCwdOutsideRoot`、`TestSandboxFailsClosed` が、macOS と Linux（Docker）の両方で通る。
- [x] `TestEscalationAlwaysAsks`（bash の昇格。手順 6 で）。
- [x] Claude Code のエンジンで、実のツールを MCP に通す（ADR 0007）: `TestClaudeEngineAsksForApproval`、`TestClaudeEngineRepeatAfterInterruptionAsks`、`TestCrashClaudeEngine`。
- [x] 24 時間を超えて承認を待ったターンが落ちて再開しても、終わった呼び出しをやり直さない（`TestResumeAfterLongApproval`。kairo 0.3.0 で解けたことを、KairoCode のターンで確かめ続ける）。

## M2 クライアント、ストリーミング、取り消し、dspchr

状態: 作業中

中身: ACP のエージェント（ADR 0002。`kairocode acp`、v1）、端末の対話（`kairocode chat`、ACP のクライアントとして）、Claude Code のエンジン（ADR 0007）、トークンのストリーミング（`TaskContext.Emit` と `k.Subscribe`）、スナップショットと `/undo`・`/redo`、LSP の診断、エージェントとコマンドの Markdown の定義、dspchr との統合の段階 1（ADR 0004）。

前提: ADR 0002、0004、0007、0008 の承認。

受け入れ条件:

- [x] データの置き場ごとのデーモン（ADR 0008）: 2 つ目は起動しない（`TestSecondDaemonDoesNotStart`）。クライアントが自動で起動し、`server.json` は 0600（`TestClientStartsTheDaemon`）。別のクライアントからの承認で、待っているターンがすぐ続く（`TestApprovalFromAnotherClient`。借用の期間は既定の 30 秒のまま、数ミリ秒で続く）。落ちたデーモンを起動し直すと、借用の期間を待たずにターンが続く（`TestCrashDaemonRestart`）。何もしないと止まり（`TestIdleDaemonStops`）、クライアントが待っている間は止まらない（`TestDaemonStaysWhileAClientWaits`）。終わる途中のデーモンの後に起動したデーモンは、ロックを待って引き継ぐ（`TestDaemonWaitsForTheOneOnItsWayOut`）。
- [x] デーモンの版の確かめ（ADR 0008 の追記）: 版の違うクライアントは、ステップが走っておらずほかに使われていないデーモンを入れ替え（`TestOtherBuildReplacesTheDaemon`）、そうでなければ使い続けて利用者に伝える（`TestOtherBuildKeepsABusyDaemon`、`TestOtherBuildKeepsADaemonThatRuns`）。
- [x] エンジンとモデルをセッションごとに選ぶ（ADR 0008 の追記）: `TestEngineAndModelPerSession`。
- [x] `kairocode daemon stop`: デーモンがデータの置き場を手放してから応答する（`TestStopTheDaemon`）。走っているステップは、最大 30 秒、終わるまで待ってから止まる（kairo 0.4.1 の `Drain`、`TestStopLetsTheRunningStepFinish`）。
- [x] 待っている承認を、複数のクライアントで扱う（ADR 0008 の追記）: 同じセッションを見ているクライアント全員に承認の求めが届き、最初の答えが通り、ほかのクライアントには「答えが出た」が届く。後からつないだクライアント（ACP の `session/load` を含む）には、待っている承認が送り直される。 → SSE の側は済み（`TestApprovalToManyClients`、`TestEventsReplayPendingOnJoin`、`TestCrashDaemonEventsAfterRestart`）。ACP の側は `TestACPApprovalSurvivesRestart`、`TestACPPermissionAnsweredElsewhere`。

- [x] SSE で、トークン、ツールの開始と終了、承認の求め、要確認、ターンの終わりが届く（`TestSSEDeliversTurnEvents`、`TestEventsOfATurn`、`TestEventsStreamTokens`、`TestEventsOfAClaudeTurn`、`TestCrashEventsAfterRestart`）。
- [x] `TestACPContract`: 実装した ACP のメソッドの要求と応答が、固定した版の ACP の JSON Schema（v1、`schema-v1.26.0`）に合う。
- [x] 決まった順のメッセージを流す ACP のクライアントで、`session/new` → `prompt` → `tool_call` の更新 → `request_permission` → 答え → ターンの終わりが通る（`TestACPTurn`、`TestACPCommand`）。要確認も `request_permission` で答えられる（`TestCrashACPReview`）。
- [x] 端末の対話（`kairocode chat`、引数なしの `kairo`）を、自分の ACP のクライアントとして作る（ADR 0002）: 返答を届いたそばから書き、ツールの呼び出しを示し、権限を端末で聞き、`-session` で会話を表示し直して続ける（`TestChat`、`TestChatAsks`、`TestNoCommandIsTheChat`、`TestClientKeepsTheOrderOfUpdates`、`TestClientPermission`）。
- [x] `kairo` は端末では TUI（ADR 0010）: 見出し、入力の枠、状態の行、返答とツールの行、矢印で選ぶ承認（要確認は何も選んでいない状態から）、取り下げ、コマンドの補完、Ctrl-C 2 回で抜ける（`TestTurn`、`TestAsk`、`TestKeys`、`TestQueuedDuringATurn`、`TestHelp`、`TestReplyWraps`、`TestNoSize`）。初めてのフォルダは信頼を聞き、「いいえ」なら何も読まない（`TestTrustQuestion`、`TestChatInAFolderNotTrusted`、`TestTrust`）。Esc でターンを止める（ADR 0011）。
- [x] `TestACPApprovalSurvivesRestart`: 承認を待っている間に ACP のプロセスを止めても、次のプロセスで `session/load` すると保留中の `request_permission` がもう一度届き、答えるとターンが続く。
- [ ] Claude Code のエンジン（ADR 0007）: `TestClaudeEngineResumeDoesNotRepeatARealCall`（偽の `claude` で、落ちた後に新しい ID で呼び直された実のツールが二重に動かない）。本物の `claude` では手で確かめる。 → テストは通る（`TestClaudeEngineResumeDoesNotRepeatARealCall`、`TestCrashClaudeEngine`）。本物の `claude` での確認が残り。
- [x] ターンの取り消し（ADR 0011、#11）: モデルの呼び出しの最中に止めるとターンが cancelled で終わり、次のターンの履歴に利用者のメッセージと書き添えが入る。承認の待ちの最中に止めると待ちが消え、ツールは動かない。取り消しの後に落ちて再開しても書き添えは二重にならない。版 4 のセッションは止めたターンを捨てる。ACP の `session/cancel` と TUI の Esc で止まる。（`TestCancelTurn`、`TestCancelWhileAwaitingApproval`、`TestCancelStopsARunningCommand`、`TestCancelInAVersion4Session`、`TestCrashCancel`、`TestCancelOverHTTP`、`TestACPCancel`、`TestEscStopsTheTurn`。セッションの版 5）
- [ ] edit のあと、LSP の診断のエラーがツールの出力に足される。
- [x] `.opencode/agents/*.md`、`.opencode/commands/*.md`、`.opencode/skills/` を、opencode と同じ意味で読む。（`TestAgents`、`TestCommands`、`TestExpand`、`TestSkills`、`TestOpencodeAgent`、`TestOpencodeAgentRefused`、`TestOpencodeSkills`、`TestOpencodeCommand`、`TestACPProjectCommand`）
- [x] `/undo` で作業ファイルが前のターンの状態に戻り、`/redo` で戻る。記録は消えない。（`TestUndoRedo`、`TestUndoThenNewTurnClearsRedo`、`TestUndoAfterRestart`、`TestCrashUndo`、`TestACPUndoRedo`。セッションとターンの版 4）
- [ ] `TestDspchrStateReports`: `DSPCHR_SESSION` があるとき、ターンの始まり・終わり・承認の求め・要確認・失敗・閉じるときに、`dspchr state` が ADR 0004 の表のとおりに起動される（`dspchr` は引数を記録する偽のコマンドに差し替える）。`DSPCHR_SESSION` が無ければ何も起動しない。
- [x] `kairocode usage --session <ID> --json` が、記録された使用量（入力・出力・キャッシュ）を返す。（`TestUsageOfASession`、`TestRunWithFakeProvider`）
- [ ] 普段の作業（バグの修正、機能の追加、リファクタリング）を、ACP のクライアント（Zed）から、opencode と同等の手間でこなせる（手で確かめる）。

## M3 サブエージェントと、人と出来事が始めるワークフロー

状態: 作業中

中身: task（サブエージェント）。Starlark のフロー（ADR 0003: `.kairocode/workflows/<name>.star`、プリミティブ `agent`・`parallel`・`decide`・`tool`・`ask`・`phase`、ずれの検出、上限）。入口は `/flow <name>`、hook（`turn.end`、`tool.after`、`session.start`）、エージェントの `workflow:`。言葉から作る（`/flow new <説明>`）。端末のフローの図（作るとき、動いている間、`kairocode flow graph`・`show`・`stats`）と、図の上での変更。

前提: ADR 0003 の承認（2026-10-19）、ADR 0011（親を止めると子も止まる）。

受け入れ条件:

- [x] `TestCrashWorkflowScriptResume`: 10 個の `agent` を `parallel` で並べたスクリプトを途中で強制終了して再開すると、終わった葉はやり直さない。
- [ ] `TestWorkflowScriptDivergence`: 記録と違う呼び出しをするスクリプトで再開すると、実行せずに止まる。
- [ ] `TestWorkflowScriptLimits`: ステップ数、エージェントの数、トークンの上限で止まる。 → ステップ数とエージェントの数は `TestFlowMistakesAreReplies`。トークンが残り。
- [ ] `TestTurnEndHookBlocks`: `turn.end` のフローが `ok` を偽で返すと、メッセージがモデルに返ってループが続き、上限の回数で止まる。
- [x] `TestWorkflowGraph`: フローの図が、順番、`if`、`parallel`、`for`、`def` の展開、`phase` を正しく表し、幅が足りないときはまとめ、木の図に切り替える。（`TestBuild`、`TestChartLooks`、`TestChartStates`、`TestChartTooNarrow`、`TestFlowGraph`）
- [ ] `TestWorkflowLive`: 実行の木の状態が、図の箱の印と色、繰り返しの回に写る。`kairocode flow show <実行> --follow` が同じ図を出す。 → TUI と ACP は済み（`TestFlowEvents`、`TestFlowAgentWaiting`、`TestFlowLive`、`TestACPFlowPlan`）。`kairocode flow show --follow` が残り。
- [ ] `TestDraftFromWords`: `/flow new` で、モデルが書いたスクリプトが検査を通ってファイルになり、図で見え、`/flow <name>` で動く。入力欄から頼んだ変更で図が描き直される。
- [ ] `TestEditOnTheChart`: 図の上で箱の中身を書き換える、消す、入れ替えると、スクリプトのその引数と文の並びだけが変わる。
- [x] 走っている途中でスクリプトを書き換えても、走っている実行は記録したスクリプトで終わる。（`TestFlowKeepsItsScript`）
- [ ] リポジトリの hook が初めて走る前に、利用者に確認する。
- [ ] エージェントの定義に `workflow:` を書くと、そのエージェントのターンがそのフローで進む。
- [ ] 親を止めると、子も止まる（ADR 0011）。

## M4 非対話とサーバーレス

状態: 未着手

中身: PostgreSQL、サスペンドモード（`kairo.Suspend`、`k.Handler()` の `/tick`）、HTTP(S) のツール、GitHub からの起動。

受け入れ条件:

- [ ] Issue のコメントから起動し、承認と CI の結果を待つ間、プロセスが残らない。
- [ ] `kairocode run` が承認で中断し、`kairocode resume` で続く。

## 段階に割り当てていない今後の作業

- **承認の auto モード**（利用者の要望、2026-10-09）: 規則に `auto` を足し、判定用のアクション（`permission.judge`）が許可・拒否・人に訊く、のどれかを理由つきで返す。
  - 判定は型付きの値で受ける（kairo の ADR 0055 の型付きの判断か、構造化された出力）。判定は記録されるので、再開しても同じ判定で進む（不変条件 2）。
  - 人に訊く、と判定されたら、今の承認の待ちに落とす。
  - sandbox の外で動かす昇格と `git_push` は、auto でも人に訊く、を既定にする（設定で変えられる）。
  - 判定にモデルを使うか、どのモデルを使うかなどを、作るときに ADR で決める。
