# 0002 機能と設定の形は opencode を手本にし、利用者の側の入口は ACP にする

- 状態: 承認（2026-10-09）
- 日付: 2026-10-09（同日の「opencode のクライアントを KairoCode のサーバーにつなぐ」案を、利用者の判断で ACP に置き換えた）
- 関連: 要件 FR-5、FR-10、FR-11、FR-18、FR-20、FR-21、FR-23、FR-24、設計 7 節・8 節・11 節（2、3）、ADR 0001、0003、0004、0007

## 背景

KairoCode は、opencode と同じ使い方をする道具です（要件 2 節）。利用者は 2026-10-09 に、次の 2 つを選びました。

1. 使い勝手と機能を opencode にそろえる。
2. 利用者の側の入口は、ACP（Agent Client Protocol）にする。最初は「opencode のクライアントを KairoCode のサーバーにつなぐ」案でしたが、ACP に置き換えました。利用者の見立ては「端末で文字を流すより、良い見せ方ができる」です。

2026-10-09 に、opencode と ACP を調べました（ソースと公開の文書を直接読んだ）。

- **opencode**（`github.com/anomalyco/opencode`、v1.18.35、2026-10-06）
  - 権限の規則: `allow`、`ask`、`deny` を、ツールごとか、`bash` のパターンごとに書きます。最後に当たったものが勝ちます。
  - エージェント: Markdown で定義します（`.opencode/agents/`）。
  - コマンド: `.opencode/commands/*.md` です。
  - スキル: `SKILL.md` で、`.opencode/skills/`、`.claude/skills/`、`.agents/skills/` を読みます。
  - LSP: edit の後に、診断をツールの出力に足します。
  - サーバーの HTTP の API: 版の管理がありません。リリースは月に 30 前後あります。
  - opencode 自身も ACP のエージェントとして動きます（`opencode acp`）。opencode の TUI は ACP のクライアントではありません。
- **ACP**（`github.com/agentclientprotocol/agent-client-protocol`、Apache-2.0）
  - 版: 安定版は `protocolVersion: 1` です。schema は v1.10.2（2026-10-01）です。v2 は 2026-07-20 に Draft として公開されています。
  - 運び方: stdio の上の JSON-RPC 2.0 です。
  - 主なメソッド: `session/new`、`session/prompt`、`session/update`（メッセージ、`tool_call`、`tool_call_update`、`plan`）、`session/request_permission`、`session/cancel`、`session/load`、`session/resume` です。
  - 権限の確認の答え方: 選択肢は `allow_once|allow_always|reject_once|reject_always` です。仕様に時間の制限はありません。
  - 対応しているクライアント: Zed、JetBrains、Neovim（CodeCompanion、avante.nvim ほか）、Emacs（agent-shell.el）、VS Code 系の拡張などです。
  - 公式の SDK は Rust と TypeScript だけで、Go はコミュニティ製です（`coder/acp-go-sdk` など）。
  - v2 で変わること:
    - `fs/*` と `terminal/*`（クライアントにファイルや端末の操作を任せる仕組み）が無くなる。
    - `session/load` が `session/resume` と `replayFrom` にまとまる。
    - ターンの状態が `state_update` で伝わる。

## 決定

### 1. 機能と設定の形を opencode にそろえる

KairoCode の設定は KairoCode の置き場（`kairocode.json`、`.kairocode/`）に置きます。そのうえで、opencode の利用者がそのまま移れるように、形と読む場所をそろえます。

- **権限の規則:** opencode と同じ形と意味にします（`allow`、`ask`、`deny`。ツールごとか、パターンごと。最後に当たったものが勝つ。`*` と `?`、`~` の展開）。設計 4 節の「判定も記録する」（`permission.check`）は、この規則で判定します。
- **エージェント:** `.kairocode/agents/*.md` と、opencode の置き場（`.opencode/agents/`、`~/.config/opencode/agents/`）を読みます。欄は opencode と同じにします。
  - KairoCode の欄として `effect_limit` を足します。そのエージェントが使える作用の型の上限（`read` など）です。
  - KairoCode の欄として `engine`（`api` か `claude-code`。ADR 0007）を足します。
  - opencode の組み込みのエージェントのうち、`build`、`plan`、`general`、`explore` を用意します。
- **コマンド:** `.kairocode/commands/*.md` と `.opencode/commands/*.md` を読みます。ワークフロー（ADR 0003）も、コマンド `/<name>` として一覧に並べます。`$ARGUMENTS`、`$1`、`@file` を opencode と同じに展開します。`` !`shell` `` は bash のツールと同じ権限の判定を通します。
- **スキル:** `.kairocode/skills/` に加え、opencode と同じ置き場（`.opencode/skills/`、`.claude/skills/`、`.agents/skills/`、それぞれのホームの側）を読みます。`name` の規則（1〜64 文字の小文字の英数字とハイフンで、ディレクトリの名前と一致する）も同じにします。
- **LSP の診断:** edit と write のツールは、書いた後に LSP の診断を取り、エラーがあればツールの出力に足します。診断を取る処理は、ツールの作用とは別の読むだけのアクションにします。
- **モデル（設計 11 節の 3 を決める）:** 使うのは Claude だけで、エンジンは Claude API（`api`）と Claude Code（`claude-code`、ADR 0007）の 2 つです（利用者の判断、2026-10-09）。プロバイダーのアダプターの境界（`internal/provider`）は残しますが、OpenAI 互換や手元のモデルは足しません。要件 FR-18 の「アダプターで足せる形」は、境界があることで満たします。
- **opencode のプラグインには対応しません。** サーバーのプロセスの中で動く JS なので、Go のサーバーでは動きません。外の道具は、ACP（下の 2）と、ADR 0004 の状態の報告でつなぎます。カスタムツールは MCP で足します（ADR 0001）。

### 2. 利用者の側の入口は ACP にする（設計 11 節の 2 を決める）

- **KairoCode は ACP のエージェントです。** `kairocode acp` を、エディタが stdio で起動します。ACP の v1 を実装し、v2 の変化（`session/resume` と `replayFrom`、`state_update`）に合わせやすい形にしておきます。
  - ACP のプロセスは、ランタイム（kairo と、同じ DB）を埋め込んで開きます。エディタがプロセスを閉じても、セッションは DB に残ります。次に起動されたプロセスが、`session/load` か `session/resume` で続けます。
  - ファイルとコマンドの操作は、KairoCode が自分で行います（kairo の記録、作用の型、sandbox。ADR 0005）。クライアントの `fs/*` と `terminal/*` には頼りません（v2 で無くなるため）。
- **KairoCode の概念を、ACP に写します。**

  | KairoCode | ACP |
  |---|---|
  | セッション（`s/<ID>`） | `session/new`（`cwd`）。ACP の `sessionId` は KairoCode のセッションの ID |
  | ターン | `session/prompt`。ターンの終わりで応答を返す（v2 では `state_update`） |
  | ストリーミング | `session/update` の `agent_message_chunk`（`k.Subscribe` から作る） |
  | ツールの呼び出し | `tool_call` と `tool_call_update`（種類、対象のファイル、差分、状態） |
  | ワークフロー（ADR 0003）の木 | `plan`（各ノードを項目に、状態を `pending`、`in_progress`、`completed` に写す） |
  | 承認の待ち（`kairo.WaitFor`） | `session/request_permission`。選択肢は opencode と同じ意味（今回だけ、以後も許す、拒否）。`allow_always` は、規則として記録する |
  | 要確認（落ちた実のツール） | `session/request_permission`。選択肢は「実行された」（`allow_once` → `k.Resolve`）と「実行されていない」（`reject_once` → `k.ResolveFailed`） |
  | 取り消し | `session/cancel` → ターンの `k.Cancel` |
  | 再開 | `session/load`（記録から `session/update` を送り直す）、`session/resume` |
  | `/undo`、`/redo`、ワークフローのコマンド | ACP のスラッシュのコマンド（`available_commands_update`） |

- **意味を強めるところ:** ACP の権限の確認は、プロセスと接続が生きている間しか保留できません。KairoCode では、承認と要確認の待ちを kairo に記録します。そのため、エディタやプロセスが落ちても失われません。再開したクライアントには、保留中の `session/request_permission` を送り直します。
- **端末で使う画面:** ACP の端末のクライアントで足りるかを、M2 で調べます。足りなければ、最小限の行単位の対話（`kairocode chat`）を、自分の ACP のクライアントとして作ります。`kairocode run`（非対話）は、ACP を通さずに動きます。
- **Go の ACP の実装:** コミュニティの SDK を使うか、仕様の JSON Schema から自分で作るかは、M2 の最初の試作で決めます。SDK を使うなら、依存の追加として利用者の了承を得ます。

## 結果

- 対話の画面は自分で作り込まずに、ACP のクライアント（Zed、JetBrains、Neovim、Emacs など）が持つ表示を使えます。計画、ツールの呼び出し、差分、権限の確認が、構造のある形で出ます。KairoCode に固有の状態（ワークフローの木、承認待ち、要確認）も、`plan` と `session/request_permission` に載せられます。
- opencode のサーバーの API（版の管理が無い）を追いかける手間はかかりません。代わりに、ACP の v1 から v2 への移行に追従します。
- 設計 8.1 節の HTTP の API（`internal/server`）は、内部と自動化のための入口として残します。dspchr（ADR 0004）や CI は、こちらを使います。
- テスト（M2 の受け入れ条件）:
  - ACP の JSON Schema（固定した版）に対する、要求と応答の契約のテスト。
  - 決まった順のメッセージを流す ACP のクライアントを使った結合のテスト（`session/new` → `prompt` → `tool_call` の更新 → `request_permission` → 答え → ターンの終わり）。
  - ACP のプロセスを、承認を待っている間に止めます。次のプロセスで `session/load` すると、保留中の `request_permission` がもう一度届き、答えるとターンが続くこと（`TestACPApprovalSurvivesRestart`）。
  - 少なくとも 1 つのエディタ（Zed）で、手で確かめます。
- 新しく守ること: ACP の層は、KairoCode の概念を ACP の形に写すだけにします。KairoCode の意味（不変条件）を弱めません。

## 検討した代替案

- **opencode のクライアントを、KairoCode のサーバーにつなぐ（この ADR の最初の案）:** opencode の TUI とデスクトップアプリが使えます。ただ、版の管理の無い API（TUI が呼ぶ約 70 の操作）を追いかけ続けることになります。opencode の TUI は ACP のクライアントではないので、ACP とも両立しません。
- **自前の TUI を作る:** 画面の作り込みに時間がかかります。ACP のクライアントの表示より良くなる見込みは薄いです。
- **opencode を fork する:** TypeScript なので、ADR 0001 と逆の方向です。
- **ACP のクライアントの `fs/*` と `terminal/*` に、ファイルと端末の操作を任せる:** v2 で無くなります。作用が kairo の記録と sandbox の外で起きます。
