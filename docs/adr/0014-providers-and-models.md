# 0014 プロバイダーとモデルを、会話の途中で切り替える（/provider、/model）

- 状態: 承認（2026-10-19）
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

### 4. codex と copilot のエンジン（段 1）: KairoCode が ACP のクライアントになって呼ぶ

（2026-10-19 に利用者の判断で書き換えました。最初の案は、各 CLI を非対話で起動し（`codex exec`、`copilot -p`）、その JSON の出力を読む形でした。下の追記を参照。）

KairoCode が ACP（ADR 0002）のクライアントになり、各エージェントを子のプロセスとして起動して、ACP で話します。

| プロバイダー | 起動するもの | 備考 |
|---|---|---|
| `codex` | `codex-acp`（Zed の ACP のアダプター。`KAIROCODE_CODEX` で変えられる） | 利用者が入れる（npm） |
| `copilot` | `copilot --acp`（`KAIROCODE_COPILOT`） | Copilot CLI がそのまま話す |

`claude-code` は、今までどおり ADR 0007 の形（`claude -p` と MCP）のままです。Claude の ACP のアダプター（`claude-code-acp`）を使う形は、必要になったら足します。

- **ターン**
  - ターンは 1 つのアクション（`codex.turn`、`copilot.turn`。作用は `Unprotected`）です。
  - 中身は次の順です。エージェントを起動し、`initialize` を送ります。セッションを作るか読み込み（`session/new` か `session/load`）、モデルを選び（`session/set_config_option`）、`session/prompt` を送ります。答え（`agent_message_chunk`）を `TaskContext.Emit` で流しながら集め、終わったら記録します。
- **セッション**
  - 最初のターンで `session/new` を送り、返ったセッションの ID を `kc_engine_session` に、KairoCode が決めた名前（セッション、プロバイダー、何回目の切り替えか。不変条件 8）で記録します。
  - 次のターンからは `session/load` で続けます。エージェントが `loadSession` を持たなければ、毎ターン新しいセッションにして、それまでの会話を付けて渡します（3）。
- **作用は KairoCode のツールを通す**
  - KairoCode は、クライアントの能力として `fs.readTextFile`、`fs.writeTextFile`、`terminal` を出します。
  - エージェントからのファイルの書き込み（`fs/write_text_file`）と端末（`terminal/create`）は、KairoCode の `write` と `bash` のツールとして実行します。ADR 0007 の MCP の呼び出しと同じ仕組みで、実行の ID は不変条件 8 のとおり、作用の型、権限の判定、承認、要確認、sandbox もそのまま効きます。
  - 読み取り（`fs/read_text_file`）は、作業ディレクトリの中だけを、その場で読みます。
  - KairoCode の MCP のサーバーも `session/new` の `mcpServers` で渡します（そのほかのツールのため）。
- **承認**
  - エージェントの `session/request_permission` には、KairoCode のツールの呼び出しなら、その場で `allow_once` を答えます。実際の作用は、上のとおり KairoCode のツールを通るところで判定されるからです。ほかの呼び出しは断ります（実装の節の追記）。
  - そこで「聞く」になった呼び出しは、ADR 0007 と同じく、呼び出しを承認待ちとしてエージェントに返します。ターンの回を終え、ワークフローで承認を待ち、答えを次の回のプロンプトで伝えます。
- **落ちたときの再開**
  - プロンプトの末尾に、ターンの目印を付けます。
  - 再開で `session/load` が返す履歴（`user_message_chunk`）に目印があれば、プロンプトを送り直さず「続けて」を送ります。
- **限界**
  - エージェントが、クライアントに頼まずに自分でファイルを書いたりコマンドを動かしたりするなら、その作用は KairoCode の記録に残りません。
  - 各エージェントが頼む形で動くかは、実装のときに実際のエージェントで確かめ、この ADR に追記します。
- **起動するファイル**
  - 起動するのは `internal/runtime` の ACP のエンジンのファイルだけです（`internal/invariants` の `commandStarters` に足す）。
  - JSON-RPC は、KairoCode の ACP のエージェント（`internal/acp`）と同じものを、`internal/jsonrpc` に分けて使います。

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

## 追記（2026-10-19）: CLI の起動から ACP のクライアントへ

- 最初の決定 4 は、各 CLI を非対話で起動し、その JSON の出力を読む形でした。2026-10-19 に、手元の `codex` 0.163.0 と `copilot` 1.0.54 で、次のことを確かめました。
  - `codex exec --json` は、`thread.started`、`item.completed`（`agent_message`）、`turn.completed`（`usage`）を出します。
  - `copilot -p --output-format json` は、`assistant.message_delta`、`assistant.message`、`result`（`sessionId`）を出します。
  - この版の copilot は `--session-id` を受けず、セッションの名前（`--name`、`--resume=<名前か ID>`）で続けます。
- 利用者から「KairoCode から ACP で claude、codex、copilot を呼ぶ構図」という指示があり、決定 4 を ACP の形に書き換えました。
  - ACP にすると、エージェントのファイルの書き込みとコマンドを、クライアントとして KairoCode のツールで実行できます。承認もそこで扱えます。
  - CLI ごとに出力を読み分ける必要もなくなります。


## 実装（2026-10-19、段 1）

- **切り替え**
  - `SessionEvent.Switch`（`workflows.Switch{Engine, Model}`）を足し、セッションのワークフローを版 6（`RunSession`）に上げました。版 5 は `RunSessionV5` で残します（`Draining`）。
  - エンジンが変わるたびに `TurnInput.Epoch` が 1 つ増えます。CLI のエンジンのターンに会話（`ClaudeTurnInput.History`）を渡すのは、`Epoch > 0` のときだけです。最初の時期の Claude Code のターンは入力が変わらないので、進行中のターンの呼び出しの ID も変わりません。
  - `/provider` と `/model` の選択は、デーモンのメモリに置き（`Runtime.Switch`）、そのセッションへの次の出来事（`signal`）に付けて送ります。記録されるのはそのときです（不変条件 2）。送る前にデーモンが止まると、選択は失われます。
  - `Runtime.Settings` は、最新のターンの入力と、まだ送っていない選択から、次のターンのプロバイダーとモデルを出します。
  - デーモンの API は `GET /providers`、`GET /providers/{id}/models`、`GET|POST /sessions/{id}/settings` です。
- **ACP のエンジン**（`internal/runtime/acpengine.go`）
  - `codex.turn` と `copilot.turn` は、エージェントを起動し、`internal/jsonrpc`（`internal/acp` から分けた JSON-RPC）で話します。
  - エージェントからの `fs/write_text_file` と `terminal/create` は、`mcpServer.call`（ADR 0007 の MCP の呼び出しと同じ、`<ターン>/mcp/<内容のハッシュ>.<n>` の実行）で、KairoCode の `write` と `bash` として実行します。`fs/read_text_file` は、作業ディレクトリの中だけをその場で読みます。
  - `session/request_permission` には、KairoCode のツールの呼び出しだけ `allow_once` を答えます（下の「直したこと」）。
  - エージェントのセッションの ID は、`kc_engine_session` に名前で記録します。次のターンでは `session/load` で続けます。
  - 指定したモデルをエージェントが受けないときは、ターンを止めず、エージェント自身のモデルで答え、そのことを答えの頭に書き添えます。2026-10-19 に `copilot --acp`（1.0.54）で確かめたところ、セッションのモデルの一覧はアカウントの状態で変わりました（`gpt-4.1` が通った後、`auto` だけになった）。
- **本物のエージェントでの確認**（2026-10-19、`copilot --acp`）
  - ファイルの作成は KairoCode の `write` を、`ls` は KairoCode の `bash` を通りました（copilot は、渡した MCP の KairoCode のツールを使った）。
  - `codex-acp` は、この手元に入っていないので、まだ確かめていません。
- **画面**
  - ACP のエージェント（`internal/acp`）は、`session/new`・`session/load` の応答と `config_option_update` で、`provider` と `model` の設定を出し、`session/set_config_option` を受けます。打ち込んだ `/provider`・`/model` も扱います。
  - TUI は、`/provider`・`/model` で一覧を出して矢印で選ぶか、名前で直接選びます。状態の行に今の値を出します。
  - `run` と `chat` に `-provider`、`-model` を足しました。
- **テスト**
  - `TestSwitchProviderAndModel`: 偽の ACP のエージェントで、API → Codex → Copilot → API と切り替えます。次のことを確かめます（切り替えを送らない、会話を渡さない、書き込みを直接する、の 3 つの壊し方で、それぞれ落ちることを確かめた）。
    - 会話の引き継ぎ
    - `session/load` での継続
    - 書き込みが KairoCode のツールの実行として記録されること
  - `TestProviderAndModel`（TUI）。
- **durability-reviewer の指摘で直したこと**
  - **承認:** `session/request_permission` は、KairoCode のツールの呼び出しだけを許し、ほかは断ります。KairoCode のツールかどうかは、題で判断します。
    - 題が `kairocode-<名前>` か `mcp__kairocode__<名前>` なら、KairoCode のツールです。
    - 名前だけの題は、組み込みのツールを外した copilot のときだけ、KairoCode のツールとみなします。組み込みのツールを外せない codex では、同じ名前の codex 自身のツールがあり得るので断ります（再レビューの指摘）。copilot は `--available-tools=kairocode-<ツール>,…` で起動し、copilot 自身の組み込みのツール（bash、apply_patch、view など）を使えなくします。copilot 1.0.54 で、組み込みのツールが外れ、作業が KairoCode のツールで進むことを確かめました。copilot の承認の求めは、MCP のツールでも題がツールの名前だけ（`write`、`bash`）で来るので、組み込みのツールを外すことが要ります。`codex-acp` での形はまだ確かめていません。
  - **Claude Code への切り替え:** `Epoch > 0` のターンは、`(セッション, Epoch)` から作った新しい Claude Code のセッションで始めます。会話も付けます。最初の時期のターンは、ID も入力も今までどおりです（`TestSwitchToClaudeCode`）。
  - **会話の引き継ぎ:** codex と copilot のターンには、最初の時期から会話を渡します。`session/load` ができないエージェントでも、新しいセッションに会話を付けて渡せます。新しいアクションなので、進行中の呼び出しの ID には影響しません。
  - **プロセスの扱い:**
    - エージェントの stderr は、書かれている間に読んでも安全な形にしました。
    - 出力を読み切るのを待ってから `Wait` します。
    - 止めるときはプロセスグループごと止め、`WaitDelay` は 5 秒です。
    - 答えの文字は、ロックの中で読みます。
    - 出力を読み切るのを待つのは 5 秒までです。グループの外に出た子孫が出力を開いたままでも、止まり続けません。
  - **crash のテスト:** `TestCrashACPWrite`。エージェントの書き込みが済み、その結果を記録する前に強制終了します。再開すると、セッションを読み込み、「続けて」に応えてエージェントが同じ書き込みをもう一度頼みますが、書き込みの実行は 1 回だけです（実行を再利用しない形に壊すと 2 回になって落ちることを確かめた）。
  - **コマンドを起動するファイル:** `internal/runtime/acpengine.go`（エージェント）と、`internal/runtime/cliengines.go` です。後者は、モデルの一覧を読むために `codex debug models` と `copilot help config` を、sandbox の外で起動します。どちらも読むだけのコマンドです。
  - **残した点:** 残ったエージェントを止める判定は、記録した PID のコマンドの名前（`codex-acp`、`copilot`）で行います。PID が使い回されていると、利用者の別の copilot を止め得ます（ADR 0007 の claude は、セッションの ID で見分けている）。
- **まだ:** 段 2（`openai`、`github-copilot` の API）。`codex-acp` での確認。

## 追記（2026-10-19）: 切り替えたときのモデル、effort

利用者の要望（2026-10-19）は、次の 2 つです。

- モデルは、そのプロバイダーの既定か、前のセッションで使っていたものにする。
- effort があるものは、effort も変えられるようにする。

- **モデルと effort を覚える**
  - プロバイダーごとに、最後に選んだモデルと effort を、新しい表 `kc_provider_pref` に残します。残るのは、`/model`、`/effort`、`/provider <id>` で明示して選んだときです。
  - モデルや effort を言わずに、あるプロバイダーへ切り替えたときは、残っているものを使います。無ければ、そのプロバイダーの既定です。これは、別のセッションで選んだものでも同じです。
  - モデルや effort を言わずにセッションを作るときも同じで、残っているものを `SessionInput` に入れます。
  - 値は、切り替えの出来事かセッションの入力として記録されてから使われます（不変条件 2）。
- **effort**
  - `Switch.Effort`、`SessionInput.Effort`、`TurnInput.Effort`、`AgentLoadInput.Effort`、`Agent.Effort`、`ModelInput.Effort`、`ClaudeTurnInput.Effort` を足しました。どれも空なら JSON に出ないので、今までの記録の呼び出しの ID は変わりません。
  - プロバイダーを替えると、effort は空（既定）に戻ります。言わなければ、そのプロバイダーで覚えているものを使います。
  - 渡し方は、プロバイダーごとに次のとおりです。
    - API: リクエストの `output_config.effort`（`provider.Request.Effort`。無ければプロバイダーの既定の `high`）。
    - Claude Code: `claude --effort`。
    - copilot: 起動の `--reasoning-effort`（ACP のセッションの設定には無い）。
    - そのほかの ACP のエージェント: セッションの設定のうち、分類が `thought_level`（または ID に `effort` を含む）ものに `session/set_config_option` で渡します。codex-acp は `reasoning_effort` です。
  - 選べる値は、次のとおりです。
    - API と Claude Code は `low`、`medium`、`high`、`xhigh`、`max` です。
    - ACP のエージェントは、`session/new` で知らせてきたモデルと effort の一覧を `kc_provider_pref` に残し、それを使います（一度動かすまでは、codex と copilot は決めた一覧）。
  - effort が無いプロバイダーには、effort の選択肢を出しません。
- **画面:** ACP の設定に `effort`（分類 `thought_level`）を足し、`/effort` で選べるようにしました。TUI の状態の行は「プロバイダー · モデル · effort」です。
- **テスト:**
  - `TestModelAndEffortKept`: 次のことを確かめます。切り替え先で覚えているものを使う処理を外すと、落ちることを確かめました。
    - API と codex で選んだものが、戻ったときと、新しいセッションで使われること。
    - effort が API の呼び出しと codex のセッションに届くこと。
    - codex が知らせた一覧が使われること。
  - `TestProviderAndModel`（`/effort`）。
- **レビューで直したこと:**
  - 今と同じプロバイダーを選び直したときは、切り替えの出来事を送りません。既定のエンジンのセッションで、画面の effort と次のターンの effort が食い違うのを防ぎます。
  - そのプロバイダーの effort の一覧に無い値は受けず、覚えもしません。打ち間違いが、ほかのセッションに残り続けないようにするためです。
  - claude には `--effort=<値>` の形で渡します。
  - `task` のサブエージェントにも effort を渡します。フローのエージェントには渡しません（フローの入力に effort は無い）。
- **残した点:** 設定を知らせない ACP のセッションには、`reasoning_effort` を送ってみて、失敗しても知らせません。
- **TUI（2026-10-20、利用者の要望）:** shift+Tab で、effort を次の値に切り替えます（最後の次は最初。Claude Code と同じ）。状態の行には effort を常に出します（`effort high`。既定なら `effort default`、effort が無いプロバイダーなら `effort none`）。テスト: `TestProviderAndModel`。
- **`/model` はプロバイダーから（2026-10-20、利用者の要望）**
  - すべてのプロバイダーのモデルを一度に並べると見づらいので、`/model` は 2 段にしました。まずプロバイダーを選び、次にそのプロバイダーのモデルを選びます（最初はそのプロバイダーの既定）。
  - プロバイダーの並びは、使えるもの（入っているもの）が先、入っていないものが後です。入っていないものは名前を灰色にし、「not installed」と取るものを添えます。
  - 入っていないものを選ぶと入れる確認を出し、入れた後にモデルを選びます。
  - 今と違うプロバイダーのモデルを選ぶと、プロバイダーとモデルを一度に切り替えます。
  - ほかのプロバイダーのモデルは、ACP の拡張のメソッド `_kairocode/models` で読みます。
  - `/model <名前>` と打てば、今のプロバイダーのモデルを直接選べます。
  - テスト: `TestModelProviderFirst`。

## 追記（2026-11-02、利用者の了承）: まだ送っていない選択を DB に置く

実装（段 1）では、`/provider`、`/model`、`/mode` の選択を、次の出来事で送るまでデーモンのメモリに置いていました。送る前にデーモンが止まる（何もしないときの終了、更新、落ちる）と、選択は黙って失われ、画面に出ていた設定と次のターンが食い違いました。

- まだ送っていない選択は、`kc_switch`（セッションごとに 1 行。選択の JSON と、変わった回数 `seq`）に置きます（`internal/store`）。`Runtime.SetProvider`、`SetMode` が書き、`Settings` と `signal` が読みます。
- `signal` は、選択を出来事に付けて送り、送れたら、読んだときの `seq` の行だけを消します。送る間に選択が変わったら、変わった方が次の出来事を待ちます。
- 送ってから消すまでの間にデーモンが止まると、同じ選択がもう一度送られます。セッションはその値をすでに持っているので、何も変わりません（プロバイダーが同じなら `Epoch` も進まない）。記録の形（`SessionEvent.Switch`）は変わりません。
- セッションを閉じると、待っていた選択も消します（次の出来事が無いので）。
- テスト: `internal/runtime`（`TestSwitchSurvivesAReopen`、`TestSwitchSentAgainChangesNothing`）、`internal/store`（`TestSwitchClearedAsSent`）。

## 追記（2026-11-02、利用者の了承）: スキーマで答えられないエンジンの decide

フローの `decide` は、codex と copilot のセッションでは、API に判断させ、API が無ければ Claude Code に判断させていました。ACP のレジストリのエージェントのセッションでは、そのエージェントのモデルの名前のまま API に聞いていました（API が無ければ動かない）。auto モードの判定役は、API が無いか、API が答えない（キーが無いなど）ときに、Claude Code に切り替えていました。

- 両方とも同じ順にそろえます（`Runtime.decideBy`）: プロバイダーがあれば API（フローは既定のモデル、判定役は `judgeModel`）、無いか API が答えなければ Claude Code。両方とも答えなければ、両方の理由を返します。API がやり直しても答えない種類の失敗（キーが無いなど）なら、やり直しません。
- ACP のレジストリのエージェントのセッションも、codex と同じく扱います。前は、そのエージェントのモデルの名前のまま API に聞いていました。
- どのエンジンがスキーマで答えるか、試しに流せるか、どのアクションがそのターンを動かすかは、`internal/runtime/engines.go` の `factsOf` の 1 か所にまとめました。
- テスト: `internal/runtime`（`TestFlowDecideFallsBackToClaudeCode`、`TestEngineFacts`）。
