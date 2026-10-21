# KairoCode の設計判断（ADR）

KairoCode の側の設計判断の記録です。kairo の ADR 0000 と同じ運用です。状態は「提案」「承認（日付）」「却下」「NNNN により置き換え」のどれかで、「承認」にするのは利用者です。

kairo の側の判断は、kairo の `docs/adr/` にあります。

| 番号 | 題 | 状態 |
|---|---|---|
| [0000](0000-record-decisions.md) | 設計判断を ADR として記録する | 承認（2026-10-09） |
| [0001](0001-go-on-the-kairo-go-sdk.md) | KairoCode を Go で書き、kairo の Go の SDK（埋め込みのランタイム）の上で動かす | 承認（2026-10-09） |
| [0002](0002-opencode-features-and-acp.md) | 機能と設定の形は opencode を手本にし、利用者の側の入口は ACP にする | 承認（2026-10-09） |
| [0003](0003-workflows-started-by-people-and-events.md) | ワークフローは、人と出来事が始める。中身は Starlark で、言葉から作り、端末にフローの図として見せる | 承認（2026-10-19） |
| [0004](0004-dspchr-integration.md) | dspchr の中でよく振る舞うエージェントになる | 提案 |
| [0005](0005-sandbox-for-processes.md) | KairoCode が動かすプロセスは、Codex にならった OS の sandbox の中で動かす | 承認（2026-10-09） |
| [0006](0006-conversation-storage.md) | 会話は、内容のハッシュで引く 1 つの表と、ターンごとのリンクの鎖で持つ。セッションはターンを子として順に動かす | 承認（2026-10-09） |
| [0007](0007-claude-code-engine-for-local-use.md) | 手元では、利用者自身の Claude Code（サブスクリプション）をモデルのエンジンとして使える | 承認（2026-10-09） |
| [0008](0008-one-daemon-per-data-directory.md) | データの置き場ごとに、kairo を動かすデーモンを 1 つだけ置く。ほかはすべてクライアントにする | 承認（2026-10-09） |
| [0009](0009-lsp-diagnostics-after-edits.md) | ファイルを書いたツールの結果に、言語サーバーの診断のエラーを足す | 承認（2026-10-19） |
| [0010](0010-terminal-ui.md) | 端末の対話を、claude と同じ形の TUI にする（Bubble Tea）。初めてのフォルダは信頼を聞く | 承認（2026-10-19） |
| [0011](0011-cancel-a-turn.md) | ターンの取り消し: kairo の取り消しで止め、止めたことを会話に残す | 承認（2026-10-19） |
| [0012](0012-github-and-serverless.md) | GitHub から起動し、待つ間はプロセスを持たない（サーバーレス）。作業ディレクトリはブランチで持ち越す | 提案 |
| [0013](0013-http-tools.md) | HTTP(S) のツール: GET の `fetch` と、POST の `http` | 承認（2026-10-21） |
| [0014](0014-providers-and-models.md) | プロバイダーとモデルを、会話の途中で切り替える（/provider、/model）。codex と copilot は CLI のエンジンから | 承認（2026-10-19） |
| [0015](0015-install-acp-agents-from-the-registry.md) | ACP のエージェントを、登録簿からチャット欄で入れて使う（/provider）。エージェント自身のツールは人に聞く | 承認（2026-10-19） |
| [0016](0016-auto-mode.md) | auto モード: 人に聞く呼び出しを、判定のモデルが決める（manual・auto・plan を shift+Tab で） | 承認（2026-10-20） |
| [0017](0017-try-a-flow-before-it-is-written.md) | 書く前のフローを試す（試運転）: 外への作用は止め、「するはずだったこと」を見せる | 承認（2026-10-20） |
| [0018](0018-same-across-agents.md) | エージェントが変わっても同じことができる: KairoCode の約束、足りないものを補う、フックは KairoCode の sandbox で（外は承認） | 承認（2026-10-21） |
| [0019](0019-daemon-updates-itself.md) | デーモンを自動で新しい版にする: ディスクの版を正とし、ステップが途切れたら替わる。クライアントはつなぎ直す | 承認（2026-10-21） |
| [0020](0020-compact-the-history.md) | 履歴の圧縮: 文脈の上限が近づいたら要約して続け、前の記録は残す | 承認（2026-10-21） |
| [0021](0021-fork-a-session.md) | セッションの分岐: 過去のターンから、別のセッションとして続ける | 提案 |
| [0022](0022-mcp-servers-custom-tools-todo.md) | 利用者の MCP サーバー、カスタムツール、todo | 提案 |
| [0023](0023-view-and-replay-records.md) | 記録の閲覧と再生 | 提案 |
| [0024](0024-limits-and-rate-limits.md) | 上限（トークン、金額、時間）と、プロバイダーごとの頻度の制限 | 提案 |
