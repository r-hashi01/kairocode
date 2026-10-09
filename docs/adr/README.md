# KairoCode の設計判断（ADR）

KairoCode の側の設計判断の記録です。kairo の ADR 0000 と同じ運用です。状態は「提案」「承認（日付）」「却下」「NNNN により置き換え」のどれかで、「承認」にするのは利用者です。

kairo の側の判断は、kairo の `docs/adr/` にあります。

| 番号 | 題 | 状態 |
|---|---|---|
| [0000](0000-record-decisions.md) | 設計判断を ADR として記録する | 承認（2026-10-09） |
| [0001](0001-go-on-the-kairo-go-sdk.md) | KairoCode を Go で書き、kairo の Go の SDK（埋め込みのランタイム）の上で動かす | 承認（2026-10-09） |
| [0002](0002-opencode-features-and-acp.md) | 機能と設定の形は opencode を手本にし、利用者の側の入口は ACP にする | 承認（2026-10-09） |
| [0003](0003-workflows-started-by-people-and-events.md) | ワークフローは、人と出来事が始める。中身は Starlark で書き、TUI と CLI で見える | 提案 |
| [0004](0004-dspchr-integration.md) | dspchr の中でよく振る舞うエージェントになる | 提案 |
| [0005](0005-sandbox-for-processes.md) | KairoCode が動かすプロセスは、Codex にならった OS の sandbox の中で動かす | 承認（2026-10-09） |
| [0006](0006-conversation-storage.md) | 会話は、内容のハッシュで引く 1 つの表と、ターンごとのリンクの鎖で持つ。セッションはターンを子として順に動かす | 承認（2026-10-09） |
| [0007](0007-claude-code-engine-for-local-use.md) | 手元では、利用者自身の Claude Code（サブスクリプション）をモデルのエンジンとして使える | 承認（2026-10-09） |
