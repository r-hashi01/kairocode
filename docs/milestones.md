# 進め方（M0〜M9）

設計 10 節の段階を、受け入れ条件のテストに落としたものです。段階の作業は `/milestone` スキルで進めます。受け入れ条件は、実装の前にテストとして書きます（最初は落ちる）。すべて通ったら、その段階の「状態」を「完了（日付）」にします。

今の段階: **M6（公開の準備）を作業中（2026-10-27）**。M5 は完了（2026-10-22）。M3 も完了。M2 の残りは、dspchr（ADR 0004 の承認待ち）、本物の `claude` と Zed での手での確認です。M4 の残り（PostgreSQL、サスペンドモード、GitHub から起動）は ADR 0012 と一緒に保留です。

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
- [x] edit のあと、LSP の診断のエラーがツールの出力に足される。（ADR 0009。`TestWriteGetsLSPErrors`、`TestCrashWriteOnceWithLSP`、`internal/lsp` のテスト）
- [x] `.opencode/agents/*.md`、`.opencode/commands/*.md`、`.opencode/skills/` を、opencode と同じ意味で読む。（`TestAgents`、`TestCommands`、`TestExpand`、`TestSkills`、`TestOpencodeAgent`、`TestOpencodeAgentRefused`、`TestOpencodeSkills`、`TestOpencodeCommand`、`TestACPProjectCommand`）
- [x] `/undo` で作業ファイルが前のターンの状態に戻り、`/redo` で戻る。記録は消えない。（`TestUndoRedo`、`TestUndoThenNewTurnClearsRedo`、`TestUndoAfterRestart`、`TestCrashUndo`、`TestACPUndoRedo`。セッションとターンの版 4）
- [ ] `TestDspchrStateReports`: `DSPCHR_SESSION` があるとき、ターンの始まり・終わり・承認の求め・要確認・失敗・閉じるときに、`dspchr state` が ADR 0004 の表のとおりに起動される（`dspchr` は引数を記録する偽のコマンドに差し替える）。`DSPCHR_SESSION` が無ければ何も起動しない。
- [x] `kairocode usage --session <ID> --json` が、記録された使用量（入力・出力・キャッシュ）を返す。（`TestUsageOfASession`、`TestRunWithFakeProvider`）
- [ ] 普段の作業（バグの修正、機能の追加、リファクタリング）を、ACP のクライアント（Zed）から、opencode と同等の手間でこなせる（手で確かめる）。

## M3 サブエージェントと、人と出来事が始めるワークフロー

状態: 完了（2026-10-19）

中身: task（サブエージェント）。Starlark のフロー（ADR 0003: `.kairocode/workflows/<name>.star`、プリミティブ `agent`・`parallel`・`decide`・`tool`・`ask`・`phase`、ずれの検出、上限。外の出来事を待つ `wait` は M4）。入口は `/flow <name>`、hook（`turn.end`、`tool.after`、`session.start`）、エージェントの `workflow:`。言葉から作る（`/flow new <説明>`）。端末のフローの図（作るとき、動いている間、`kairocode flow graph`・`show`・`stats`）と、図の上での変更。

前提: ADR 0003 の承認（2026-10-19）、ADR 0011（親を止めると子も止まる）。

受け入れ条件:

- [x] `TestCrashWorkflowScriptResume`: 10 個の `agent` を `parallel` で並べたスクリプトを途中で強制終了して再開すると、終わった葉はやり直さない。
- [x] `TestWorkflowScriptDivergence`: 記録と違う呼び出しをするスクリプトで再開すると、実行せずに止まる。
- [x] `TestWorkflowScriptLimits`: ステップ数、エージェントの数、トークンの上限で止まる。（`TestFlowMistakesAreReplies`、`TestFlowTokenLimit`）
- [x] `TestTurnEndHookBlocks`: `turn.end` のフローが `ok` を偽で返すと、メッセージがモデルに返ってループが続き、上限の回数で止まる。
- [x] `TestWorkflowGraph`: フローの図が、順番、`if`、`parallel`、`for`、`def` の展開、`phase` を正しく表し、幅が足りないときはまとめ、木の図に切り替える。（`TestBuild`、`TestChartLooks`、`TestChartStates`、`TestChartTooNarrow`、`TestFlowGraph`）
- [x] `TestWorkflowLive`: 実行の木の状態が、図の箱の印と色、繰り返しの回に写る。`kairocode flow show <実行> --follow` が同じ図を出す。（`TestFlowEvents`、`TestFlowAgentWaiting`、`TestFlowLive`、`TestACPFlowPlan`、`TestFlowShowFollow`）
- [x] `TestDraftFromWords`: `/flow new` で、モデルが書いたスクリプトが検査を通ってファイルになり、図で見え、`/flow <name>` で動く。入力欄から頼んだ変更で図が描き直される。（`TestNewFlow`、`TestFlowWrite`、`TestCheck`、`TestDiff`、`TestFlowsTheTurnMade`）
- [x] `TestEditOnTheChart`: 図の上で箱の中身を書き換える、消す、入れ替えると、スクリプトのその引数と文の並びだけが変わる。（`TestSetText`、`TestDeleteAndMove`、`TestEditOnTheChart`）
- [x] 走っている途中でスクリプトを書き換えても、走っている実行は記録したスクリプトで終わる。（`TestFlowKeepsItsScript`）
- [x] リポジトリの hook が初めて走る前に、利用者に確認する。（`TestTurnEndHookBlocks`。台本が変わったらまた聞く）
- [x] エージェントの定義に `workflow:` を書くと、そのエージェントのターンがそのフローで進む。（`TestAgentWorkflow`）
- [x] 親を止めると、子も止まる（ADR 0011）。（`TestCancelStopsTheFlow`）

## M4 非対話とサーバーレス

状態: 作業中（ADR 0012 は利用者の判断で保留（2026-10-21）。HTTP(S) のツールは済み（ADR 0013））

中身: フローの `wait(event, timeout=)`（ADR 0003）、PostgreSQL、サスペンドモード（`kairo.Suspend`、`k.Handler()` の `/tick`）、HTTP(S) のツール、GitHub からの起動。

受け入れ条件:

- [ ] Issue のコメントから起動し、承認と CI の結果を待つ間、プロセスが残らない。
- [x] HTTP(S) のツール（ADR 0013）: `fetch`（GET、読むだけ）と `http`（POST、実）。POST の途中で落ちても送り直さず、人に聞く。利用者の設定の認証は記録に残らない。（`TestFetch`、`TestHTTPPost`、`TestHTTPUnknown`、`TestNetAuth`、`TestNetPrivateRefused`、`TestNetRedirects`、`TestNetSettingsAreTheUsers`、`TestCrashHTTPPost`）
- [x] `kairocode run` が承認で中断し、`kairocode resume` で続く。（`kairocode run -detach` が人を待つところで止まり（終了コード 3）、`kairocode resume <session>` が最新のターンを最後まで追う。Ctrl-C で止めたときも続け方を示す。`TestRunDetachAndResume`）

## M5 長く使い続ける: 圧縮と分岐、ツールの広がり、記録

状態: 完了（2026-10-22）。ADR 0024（上限と頻度の制限）は利用者の判断で保留

中身（利用者が選んだもの、2026-10-21。M4 の残りは ADR 0012 と一緒に保留）:

- 履歴の圧縮（FR-3）と分岐（FR-4）。ADR 0020、0021。
- 利用者の MCP サーバーのツール（FR-8）、カスタムツール（FR-9）、todo（FR-5）。ADR 0022。
- 記録の閲覧（FR-24）。ADR 0023。再生（FR-25）は、利用者の判断で作らない（2026-10-22、ADR 0023 の実装の 2）。
- 上限（FR-26）と頻度の制限（FR-27）は、利用者の判断で保留（2026-10-22）。KairoCode 自身が課金する、使えるモデルを絞る、といった事情が出たときに、ADR 0024（提案のまま）で入れる。

受け入れ条件:

- [x] 文脈の上限に近づいた会話が、要約して続く。要約の前の記録は消えず、閲覧できる。落ちて再開しても、要約は 1 度だけ（ADR 0020。`TestCompactNearTheContext`、`TestCompactAtTheTurnsStart`、`TestCompactWhenTooLong`、`TestCrashCompact`）。
- [x] 過去のターンから分岐したセッションが、そのターンまでの会話で続く。元のセッションは変わらない（ADR 0021。`TestForkFromATurn`、`TestForkWithFiles`、`TestForkOnClaudeCode`）。
- [x] 利用者の MCP サーバーのツールを、宣言した作用の型で使える。宣言のないツールは実として扱い、結果が不明なら人に聞く。プロジェクトが定める MCP サーバーとカスタムツールは、承認なしに sandbox の外で動かない（ADR 0022。どのエンジンでも。`TestMCPServerTool`、`TestCrashMCPServerTool`、`TestCrashMCPServerToolOnClaudeCode`、`TestProjectMCPServerAsks`、`TestProjectMCPServerChanged`、`TestCodexOwnMCPServersOff`）。
- [x] todo の一覧が、ターンをまたいで続き、クライアントに見える（ADR 0022。`TestTodo`、`TestACPTodoPlan`）。
- [x] セッションのすべてのターン、モデルの入出力、ツールの呼び出しと結果、待ち、使用量を見られる（`kairocode log`、`/log`。金額は ADR 0024 の後。`TestSessionLog`）。

## M6 公開の準備（個人向けの OSS として出す）

状態: 作業中（2026-10-27 に計画）

中身（利用者が選んだもの、2026-10-27: 個人向けの OSS として出す）: ライセンス、版の表示、CI、リリース、README、手での確認。ここまでで公開する（上限は 2026-10-30 に公開の後へ回した）。

受け入れ条件:

- [x] LICENSE（MIT）を置く（2026-10-27）。`third_party/codex-sandbox` の Apache-2.0 と NOTICE はそのまま残す。README から辿れるようにするのは、README の項目で。
- [x] kairo の SDK を、外の人が `replace` なしで取れる（proxy.golang.org に v0.4.1 がある。2026-10-27 に確かめた）。
- [x] `kairocode version` が、ビルドのときに埋めた版とコミットを出す（埋めていなければ Go のビルド情報から。`TestVersionLine`、`TestVersionCommand`）。
- [x] CI（GitHub Actions、macOS と Linux）で `scripts/check.sh` が通る（2026-10-28、PR #12 の `341af18`。sandbox を起動するテストも含めて、両方で通った）。
- [ ] タグからリリースのバイナリ（macOS と Linux、amd64 と arm64）ができ、`go install` と Homebrew の tap で入る。 → 公開まで保留（利用者の判断、2026-10-27）。
  - 2026-10-30: 仕組みは用意した。`v*` のタグで、検査を通してから 4 つのバイナリ・チェックサム・Homebrew の formula を下書きのリリースに載せる（`.github/workflows/release.yml`、`scripts/release-build.sh`）。公開の手順は `docs/release.md`。実際のタグと tap は公開のときに。
- [ ] 利用者向けの README（入れ方、最初の設定、`kairo` の使い方、`/flow`、sandbox、対応 OS は macOS と Linux、第三者のライセンス）。 → 下書きは書いた（`README.md`、2026-10-27）。公開の前に、手での確認（下の関門）の結果に合わせて直す。
- [x] 公開の前の関門: Zed と TUI で、普段の作業（バグの修正、機能の追加、リファクタリング）を手でこなす（M2 の残り）。出た不具合を直してから公開する（2026-11-04、利用者が確かめた）。
  - 2026-10-28、TUI で使って出た不具合を直した: 画面のチラつき（返事を 1 行ずつ上に書く）、同じコマンドの 2 回目を「中断の後」と聞く（ADR 0007 の追記）、終わるときの検査が sandbox の中で通らず質問を押し流す（ADR 0018 の 7・8 節）。足したもの: クリップボードの画像（ADR 0026 の追記）、端末の通知（ADR 0032）、失敗したセッションから続ける（ADR 0021 の追記）。
  - 2026-10-30、TUI: 窓の大きさを変えると入力欄の古い線が残る不具合を、tmux で再現して直した（ADR 0033 の追記）。見た目を KairoCode の縦の線でそろえ、見出しの下にフォルダーで続いているものを出した。貼り付けのラベルに頭を出し、Ctrl+O で開ける。
  - 2026-10-30、Zed: 承認を待ったまま Zed を終了し、開き直すと、頼んだメッセージと同じ承認が出て、許すと続きから進み、答えが返った（dify-oss、Claude Code のエンジン）。途中で直した: 質問（hook）が答えられる形で出ない、セッションを作った直後のコマンドの一覧が捨てられる、新しい版への交代でターンの待ちが失敗に見える、開き直したときに進行中のターンのメッセージが出ない、表示名（Claude Code、既定のモデルとエフォート、既定の規則）。hook の質問には、動かすスクリプトの中身を添える。
  - 2026-11-02: 開き直したとき、進行中のターンですでに動いた呼び出しも再生する（PR #17）。Zed で確かめた。
  - 2026-11-02、公開の前のコードの整理（PR #17〜#21）: 大きなファイルを仕事ごとに分け、`internal/config`・`api`・`plugins`・`secrets`・`clipboard`・`flowfile` を足した。tui と acp は runtime を import しない（`TestClientsDoNotImportTheRuntime`）。途中で直したもの: `sandbox.loopback` をターンの始めの値で決める（ADR 0031 追記）、まだ送っていない `/provider`・`/mode` の選択をデーモンが止まっても残す（ADR 0014 追記）、ホームが無いときに利用者の設定を相対パスで読まない、フローの `decide` を API・Claude Code の順にそろえる（ADR 0014 追記）。
  - 2026-11-04、手での確認で出たものを直した（PR #23〜#26）: TUI の `/flow` だけでフローを選ぶ画面（動かす・図で変える・実行を見る・新しく作る）、コマンドの名前の後にそのコマンドが取るもの（`/plugin install`、`/flow new`、フローの名前、モデルなど）を出す。Zed: 質問に答えるとどれを選んでも却下（×）に見える、`flow_write` の下書きが見えない（差分で出す）、フローのレビューが崩れる（節を Markdown に、スクリプトを差分に）。
  - 残り: なし（公開の手順は上の 2 項目）。

## M7 強みを見せる

状態: 未着手

- [ ] 耐久性のデモ: `kill -9` しても続き、何も二重に起きない様子の録画と、再現の手順（`docs/durability.md`）。README の冒頭に置く。
- [ ] 承認の求めを手元の外（Slack など）に知らせる。hook に「承認の求め」の出来事を足し、フローで送る（ADR が要る）。外から答えるのは後で別に決める。
- [ ] 他社のエージェント（Claude Code、codex）をエンジンにしても同じ保証が保たれることの説明とデモ。本物の `claude` での確認（M2 の残り）もここで済ませる。

## M8 入口の格差を埋める

状態: 未着手

- [ ] OpenAI 互換の API のアダプター（手元のモデルを含む）。標準ライブラリの HTTP で書く（ADR 0014 の追記か新しい ADR）。
- [x] 画像の入力（ADR 0026）。ファイル（`@<パス>`、端末に落としたファイル）、ACP、API から（2026-10-27）。
- [x] クリップボードの画像を TUI に貼る（ADR 0026 の追記、2026-10-28）。Linux は手での確認が残り。
- [ ] `/init`: リポジトリを読んで AGENTS.md の下書きを作る。

## M9 差別化を深める

状態: 未着手

- [ ] うまくいったセッションからフローを作る（下の「今後の作業」の案。ADR が要る）。
- [ ] worktree で並べるセッションと、その一覧の画面（ADR が要る）。

M6〜M9 の後に回すもの: 上限（トークンと金額。ADR 0024、保留のまま。2026-10-30 に利用者の判断で公開の後へ）、VS Code の拡張、Windows、GitHub とサーバーレス（ADR 0012）、セッションの共有、外から承認に答える、検査の報告が回答の後に割り込んで回答を押し流す問題（Stop hook。急ぐなら M6 に入れる）。

## 段階に割り当てていない今後の作業

- **セッションの内容からフローを作る**（利用者の発案、2026-10-22）: うまくいったセッションの記録（頼んだこと、呼んだツール、承認したところ）から、同じ手順を繰り返せるフロー（`.kairocode/workflows`）を書き起こす。
  - 材料は閲覧の記録（`Runtime.Log`、`kairocode log -json`）です。モデルに読ませて、`/flow new` と同じ仕組み（レビューの画面、試運転）で書かせる形を考えている。
  - 作るときに ADR で決める。

- **（済み: ADR 0016）承認の auto モード**（利用者の要望、2026-10-09）: 規則に `auto` を足し、判定用のアクション（`permission.judge`）が許可・拒否・人に訊く、のどれかを理由つきで返す。
  - 判定は型付きの値で受ける（kairo の ADR 0055 の型付きの判断か、構造化された出力）。判定は記録されるので、再開しても同じ判定で進む（不変条件 2）。
  - 人に訊く、と判定されたら、今の承認の待ちに落とす。
  - sandbox の外で動かす昇格と `git_push` は、auto でも人に訊く、を既定にする（設定で変えられる）。
  - 判定にモデルを使うか、どのモデルを使うかなどを、作るときに ADR で決める。
