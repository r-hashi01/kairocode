# 0014 プロバイダーとモデルを、会話の途中で切り替える（/provider、/model）

- 状態: 提案
- 日付: 2026-10-19
- 関連: 設計 11 節の 2（最初のプロバイダー）、ADR 0002（opencode を手本にする）、0007（Claude Code のエンジン）、0008（エンジンとモデルはセッションごと）、0010（TUI）、AGENTS.md の不変条件 2・4・6・8

## 背景

利用者の要望（2026-10-19）は、opencode のように、Claude、Codex、Copilot を切り替え、モデルも切り替えることです。チャット欄では `/provider` と `/model` で切り替えます。

利用者が選んだ形は次の 2 つです。

- 動かし方は「両方」です。まず、利用者の CLI（`codex`、`copilot`）をエンジンにして使えるようにし（ADR 0007 と同じ形）、後で、KairoCode が API を直接呼ぶ形も足します。
- 切り替えは、会話の途中でも、次のターンから効かせます。

今は、エンジン（`api`、`claude-code`）とモデルが、セッションを作るときに `SessionInput` で決まり、途中では変わりません（ADR 0008 の追記）。

この手元で確かめた CLI（2026-10-19）:

- `codex` 0.163.0
  - `codex exec [--json] [-m <モデル>] [-s read-only] [-C <dir>] [-c mcp_servers.<名前>.url=…] <プロンプト>` と、`codex exec resume <id>` があります。
  - セッションの ID は Codex が決め、`--json` の出来事で返します。
  - 組み込みのシェルは外せません。sandbox（`-s read-only`）で書き込みを止められます。
- `copilot` 1.0.54
  - `copilot -p <プロンプト> --output-format json [--model <モデル>] --session-id <uuid> --additional-mcp-config <json> --available-tools <ツール>…` があります。
  - `--available-tools` で、KairoCode の MCP のツールだけに絞れます。
  - `--session-id` で、新しいセッションの UUID を KairoCode が決められます（claude と同じ）。

## 決定

### 1. プロバイダーの一覧

| ID | 動かし方 | 認証 | 段 |
|---|---|---|---|
| `anthropic` | API（今の `api` のエンジン） | API キー（環境変数） | 済み |
| `claude-code` | CLI のエンジン（ADR 0007） | 利用者の Claude Code | 済み |
| `codex` | CLI のエンジン（`codex exec`） | 利用者の Codex（ChatGPT のサインインか API キー） | 1 |
| `copilot` | CLI のエンジン（`copilot -p`） | 利用者の Copilot CLI（GitHub のサインイン） | 1 |
| `openai` | API（OpenAI の Responses API） | API キー（`OPENAI_API_KEY`） | 2 |
| `github-copilot` | API（Copilot の chat completions） | GitHub のサインイン（認証の仕方は、段 2 の前に ADR の追記で決める） | 2 |

- 今の `engine`（`api`、`claude-code`）は、そのままプロバイダーの ID の別名として受けます。`api` は `anthropic` と同じです。今までの記録と設定は、そのまま読めます。
- モデルは、プロバイダーの中の名前です。`/model` は今のプロバイダーのモデルを選びます。設定では `<プロバイダー>/<モデル>` の形でも書けます（opencode と同じ）。

### 2. 切り替えは、セッションへの出来事として記録する（不変条件 2）

- `/provider <ID>` と `/model <名前>` は、セッションへの新しい出来事 `EventSettings{Provider, Model}` です。セッションのワークフローは、それを記録された値として受け、次のターンからの `TurnInput` の `Engine` と `Model` を変えます。
- 進行中のターンは、始めたときの値のまま終わります。
- セッションのワークフローの版を上げ（6）、今の版 5 は `kairo.Draining()` で残します。
- ターンの子の ID は、入力に `Engine` と `Model` が入るので、切り替えの前後で別のものになります。これは不変条件 8 のとおりです（記録された値だけが入る）。

### 3. エンジンをまたいで会話を引き継ぐ

- API のエンジン（`anthropic`、後の `openai`、`github-copilot`）は、会話を `kc_message` の鎖で持ちます（ADR 0006）。
- CLI のエンジンは、会話をそれぞれの CLI の記録（transcript）で持ちます。KairoCode の鎖には、各ターンの利用者の発言と最終の応答が残ります（ADR 0007）。
- **CLI のエンジンに切り替えたとき**は、その CLI の新しいセッションを始めます。最初のプロンプトの前に、それまでの会話（鎖の利用者の発言と応答の文字だけ）を「ここまでの会話」として付けます。ツールの呼び出しの中身は付けません。
  - CLI のセッションの ID は、KairoCode のセッションの ID、プロバイダー、何回目の切り替えか、から決めます（不変条件 8）。同じ CLI に戻ったときも、新しいセッションにします。
- **API のエンジンに切り替えたとき**は、今までどおり鎖をそのまま使います。CLI のエンジンだった間のターンは、鎖に利用者の発言と応答だけがあるので、そのまま続きます。
- モデルだけを変えたとき（同じプロバイダー）は、会話はそのままです。CLI のエンジンでは `--model` を変えて、同じ CLI のセッションを続けます。

### 4. codex と copilot のエンジン（段 1）

ADR 0007 の `claude-code` と同じ作りです。ターンは 1 つのアクション（`codex.turn`、`copilot.turn`。作用は `Unprotected`）で、KairoCode のツールは MCP で出します。MCP の呼び出しの実行の ID は不変条件 8 のとおりです。

- **`copilot`**
  - 起動の形: `copilot -p <プロンプト> --output-format json --session-id <uuid> [--model …] --additional-mcp-config <KairoCode の MCP> --available-tools <kairocode の各ツール> --disable-builtin-mcps --no-custom-instructions --no-ask-user --allow-tool kairocode`
  - 2 回目からのターンは、同じ `--session-id` で続けます。
- **`codex`**
  - 起動の形: `codex exec --json -s read-only -C <dir> --skip-git-repo-check -c mcp_servers.kairocode.url=… -c approval_policy="never" [-m …] <プロンプト>`
  - 2 回目からのターンは `codex exec resume <id>` で続けます。
  - Codex のセッションの ID は、最初のターンの出来事から読み、`kc_engine_session` に記録します。
  - 組み込みのシェルは外せないので、sandbox を `read-only` にします。シェルでの読み取りは記録されませんが、作用はありません。書き込みは KairoCode の MCP のツールだけが通ります（不変条件 3・4 の作用の型と承認が効く）。
- **共通**
  - ライブの出力は、各 CLI の JSON の出来事を `TaskContext.Emit` で流します。
  - 落ちたときの再開は ADR 0007 と同じ考えです。CLI の記録にターンの目印があれば、プロンプトを送り直さず「続けて」を送ります。
  - 子の環境から、KairoCode の秘密と、入れ子を拒ませる変数を外します。
  - 起動するのは `internal/runtime` の各エンジンのファイルだけです（`internal/invariants` の `commandStarters` に足す）。
- 各 CLI のフラグと出力の形は、実装のときに実際の CLI で確かめ、合わないところはこの ADR に追記します。

### 5. チャット欄の `/provider` と `/model`（ADR 0010 の TUI、ACP）

- `/provider` は一覧（ID、動かし方、使えるか）を出し、矢印で選びます。`/provider <ID>` で直接選びます。
  - 「使えるか」は、CLI のエンジンならコマンドがあるか、API ならキーがあるかです。
- `/model` は、今のプロバイダーのモデルの一覧を出して選びます。`/model <名前>` で直接選びます。
  - 一覧は、デーモンの `GET /providers` と `GET /providers/{id}/models` から取ります。
  - API のプロバイダーは、その API のモデルの一覧です。
  - CLI のエンジンは、KairoCode が持つ既知の一覧に、自由入力を足したものです。
- 状態の行に、今のプロバイダーとモデルを出します。
- ACP では、セッションの設定（`session/set_model` と、モード）に写します。
- `kairocode run` と `chat` の `-provider`、`-model` で、セッションの始めの値を決めます。環境変数 `KAIROCODE_ENGINE`、`KAIROCODE_MODEL` も今までどおり使えます。

### 6. 段 2: API のプロバイダー

- `openai`: OpenAI の Responses API を `net/http` で直接呼びます（依存は足さない）。キーはアクションの中で環境変数から読みます（不変条件 6）。
- `github-copilot`: Copilot の API を呼びます。GitHub での認証の仕方は、利用規約と、どの OAuth のアプリで認可するかを確かめてから、この ADR の追記で決めます。
- どちらも `internal/provider` のアダプターです。ツールの呼び出しを、今の `msg` の形に写します。

## 結果

- 会話の途中で、Claude、Codex、Copilot と、それぞれのモデルを切り替えられます。切り替えは記録されるので、落ちて再開しても同じプロバイダーとモデルで進みます。
- CLI のエンジンでは、各サブスクリプションをそのまま使えます。ただし、ループの中のモデルの呼び出しは KairoCode の記録に残りません（ツールの呼び出しは残る。ADR 0007 と同じ）。
- テスト（実装のとき足す）
  - 偽の `codex` と偽の `copilot`（テストのバイナリを子として起動し、決まった JSON の出来事を出し、MCP のツールを呼ぶ）で、ターンが通ること。
  - 書くツールが MCP を通って 1 回だけ動くこと（落ちて再開しても。`TestCrash...`）。
  - `/provider` と `/model` の後のターンが、新しいエンジンとモデルで動くこと。進行中のターンは、始めの値で終わること。
  - 切り替えが記録され、再開で同じになること。版 5 のセッションが終わりまで動くこと。
  - エンジンをまたいだとき、前の会話の文字が新しい CLI のセッションの最初のプロンプトに入ること。
  - TUI の `/provider` と `/model` の選択、状態の行の表示。

## 検討した代替案

- **切り替えは新しいセッションから:** 作りは小さくなります。ただし、利用者は会話の途中での切り替えを選びました。
- **CLI のエンジンのとき、毎ターン会話の全体を渡す（CLI のセッションを続けない）:** CLI の文脈の圧縮や記録を使えず、長い会話で遅く、高くなります。
- **codex を MCP のサーバー（`codex mcp-server`）として使う:** 対話の形が違い、ツールを KairoCode の MCP に向ける設定が `exec` より複雑になります。
- **Copilot と OpenAI を最初から API で呼ぶ:** 認証（とくに Copilot の OAuth）を先に決める必要があります。利用者の選んだ順（CLI が先）にしました。
