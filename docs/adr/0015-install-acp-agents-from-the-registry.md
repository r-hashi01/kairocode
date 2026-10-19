# 0015 ACP のエージェントを、登録簿からチャット欄で入れて使う（/provider）

- 状態: 承認（2026-10-19）
- 日付: 2026-10-19
- 関連: ADR 0014（プロバイダーの切り替え、ACP のエンジン）、0002（ACP）、0005（sandbox）、0007（Claude Code のエンジン）、AGENTS.md の不変条件 3・4・6・7

## 背景

利用者の要望（2026-10-19）は、チャット欄で ACP のエージェントを入れられるようにすることです。その役割を `/provider` に持たせます。

ADR 0014 では、codex と copilot を、利用者が自分で入れたコマンド（`codex-acp`、`copilot --acp`）として決め打ちで起動しています。

ACP には、エージェントの登録簿があります（`https://cdn.agentclientprotocol.com/registry/v1/latest/registry.json`）。2026-10-19 に取得したところ、版 1.0.0 で 42 のエージェントがありました。たとえば `claude-acp`、`codex-acp`、`github-copilot-cli`、`gemini`、`opencode`、`cursor`、`goose` です。

各エージェントの `distribution` は、次のどれかです。

- `npx`: npm のパッケージ。版が固定され（`@agentclientprotocol/codex-acp@2.2.2` など）、`args` と `env` を持ちます。
- `uvx`: PyPI のパッケージ。
- `binary`: 機種（`darwin-aarch64` など）ごとのアーカイブの URL、`cmd`、`args`、`sha256` を持ちます。

## 決定

1. **`/provider` の一覧は、登録簿から作ります。**
   - 並びは次のとおりです。
     - 組み込みのもの: `anthropic`（API）、`claude-code`（ADR 0007 の `claude -p`）。
     - 登録簿のエージェント: 名前、版、説明、入っているか。
   - 登録簿は、デーモンが取りに行き、データの置き場に 1 時間置きます（`<data>/agents/registry.json`）。取れないときは、置いてあるものを使います。
   - この手元で動かせないもの（`binary` に今の機種が無い、`npx` に node が無い、`uvx` に uv が無い）は、理由を添えて「使えない」と出します。
2. **入れるのは、利用者が選んだときだけです。**
   - 入っていないエージェントを `/provider` で選ぶと、TUI が確認を出します。確認には、何を、どこから、どの版で入れるか（パッケージの名前か URL）を示します。「入れる」を選んだときだけ、デーモンが入れます。
   - 入れるのはモデルではなく利用者の操作なので、kairo のアクションにはしません（設定の作業です）。ネットワークを使い、sandbox の外で動かします。
   - **入れ方**
     - `npx`: `npm install --prefix <data>/agents/<id>/<版> <パッケージ@版>` で入れます。起動するのは、そのパッケージの `bin` です。
     - `uvx`: `uv tool install` で、`<data>/agents/<id>/<版>` に入れます。
     - `binary`: アーカイブを取って `sha256` を確かめ、`<data>/agents/<id>/<版>` に展開します。合わなければ入れません。
   - 入れたものは `<data>/agents/installed.json` に記録します（ID、版、起動するコマンドと引数、環境変数）。
   - 版を上げるのも、利用者が選んだときだけです（`/provider` の一覧に「新しい版がある」と出す）。
3. **入れたエージェントは、ADR 0014 の ACP のエンジンで動かします。**
   - エンジンの名前は `acp:<登録簿の ID>` です（例: `acp:codex-acp`）。
   - ADR 0014 の `codex` と `copilot` は、それぞれ `acp:codex-acp`、`acp:github-copilot-cli` の別名として残します。
   - 起動するのは、入れたもののコマンドです。入っていなければ、PATH にある同じ名前のコマンドを使います。今までどおり `KAIROCODE_CODEX`、`KAIROCODE_COPILOT` でも指定できます。
4. **エージェント自身のツールは、利用者の承認を通します。**
   - 多くのエージェントは、組み込みのツールを外せません。ADR 0014 の扱いは次のとおりです。
     - KairoCode のツール（`kairocode-<名前>` か `mcp__kairocode__<名前>`、組み込みのツールを外した copilot では名前だけ）は許します。
     - それ以外は断ります。
   - このままだと、多くのエージェントが動けません。そこで、それ以外の `session/request_permission` は、KairoCode の承認（人に聞く）に回します。
     - その場では `reject_once` を答え、聞くことを記録して、ターンの回を終えます。
     - ワークフローが承認を待ちます（ADR 0007 の MCP の「聞く」と同じ。`kairo.WaitFor`、不変条件 7）。
     - 許されたら、次の回で「許された呼び出しをもう一度」と伝えます。同じ呼び出し（題と入力のハッシュが同じ）の求めには、その回だけ `allow_once` を答えます。
     - 許した記録は、ADR 0007 の `mcp.grant` と同じく記録に残します（不変条件 2）。
   - **限界:** 許されたエージェント自身のツールの作用は、エージェントの中で起きます。KairoCode の作用の型、sandbox、「結果が不明なら人に聞く」（不変条件 4）は効きません。承認の画面に「KairoCode の外で動く」と示します。
   - ファイルの読み書きと端末を KairoCode に頼むエージェント（`fs/*`、`terminal/*`）の作用は、ADR 0014 のとおり KairoCode のツールを通ります。
5. **秘密**
   - 入れるときも動かすときも、KairoCode の秘密（環境変数のキーなど）を渡しません（`cliEnv`）。
   - 各エージェントの認証は、そのエージェント自身のものです（ACP の `authenticate` は使わず、エージェントのサインインに任せる）。認証が要ると返されたら、その旨とエージェントのサインインの方法を利用者に伝えます。

## 結果

- 利用者は、チャット欄の `/provider` から、登録簿の任意の ACP のエージェントを入れて、会話の途中で切り替えられます。
- 入れるのは、利用者の確認の後だけです。バイナリはハッシュで確かめます。
- エージェント自身のツールは、毎回（同じ呼び出しはその回だけ）人に聞きます。KairoCode の外で動くことは画面に出します。
- テスト（実装のとき足す）
  - 偽の登録簿（`httptest`）と偽の配り物で、次のことを確かめます。
    - 一覧の作り方。
    - 入れる確認。
    - バイナリの `sha256` の照合（合わなければ入れない）。
    - 入れた記録。
    - `acp:<id>` での起動。
  - エージェント自身のツールの承認を求める偽のエージェントで、次のことを確かめます。
    - 承認を待ってプロセスを手放す。
    - 許されたら次の回に同じ呼び出しだけが許される。
    - 断られたら動かない。
    - 落ちて再開しても、許しが二重にならない。
  - TUI: `/provider` で、入っていないものを選ぶと確認が出て、「入れる」で入れること。

## 検討した代替案

- **入れるのは利用者に任せ、PATH にあるものだけ使う（今の ADR 0014）:** 利用者の要望（チャット欄で入れる）に合いません。
- **`npx -y <パッケージ@版>` で毎回起動する（入れる手順を持たない）:** 手順は減ります。ただし、初回のダウンロードがターンの中で起き、時間の上限やネットワークの失敗がターンの失敗になります。何をいつ入れたかも記録に残りません。
- **エージェント自身のツールの求めを、すべて許す:** 作用が KairoCode の外で、確認なしに起きます（ADR 0014 のレビューで退けた形）。
- **エージェント自身のツールの求めを、すべて断る:** 安全ですが、組み込みのツールを外せない多くのエージェントが動けません。

## 実装（2026-10-19）

- **`internal/agents`**
  - 登録簿の取得（`Manager.Agents`。`<data>/agents/registry.json` に 1 時間置く）。
  - 一覧（`List`）: この機種で動かせるか、何を取るかを付けます。
  - 入れる（`Install`）と、入れた記録（`installed.json`、`Command`）。
- **入れ方**
  - `binary`: アーカイブを取って `sha256` を確かめ、`os.OpenRoot` で `<data>/agents/<id>/<版>` の外に出ないように展開します。`.tar.gz`、`.tgz`、`.tar.bz2`、`.zip` を開けます。
  - `npx`: `npm install --prefix` で入れ、パッケージの `bin` を起動します。
  - `uvx`: `UV_TOOL_DIR`、`UV_TOOL_BIN_DIR` をその置き場にして `uv tool install` で入れます。
  - 入れるときの環境変数からは、秘密らしきものを外します。
  - 入れるコマンドを起動できるのは `internal/agents/` だけで、`internal/invariants` の `commandStarters` に足しました。
  - 失敗したら、その置き場を消します。
- **エンジン**
  - `acp:<id>` は、`acp.turn`（`ClaudeTurnInput.Engine` に `acp:<id>`。新しいアクションなので、進行中の呼び出しの ID には影響しない）で、ADR 0014 の ACP のエンジンを使います。
  - ADR 0014 の `codex` と `copilot` は、登録簿の `codex-acp` と `github-copilot-cli` に当たります。登録簿から入れたものがあればそれを、無ければ今までどおり PATH の `codex-acp`、`copilot --acp` を起動します。
  - `github-copilot-cli` は、どちらで起動しても `--available-tools` で KairoCode のツールだけにし、名前だけの承認の求めを KairoCode のツールとみなします。
- **プロバイダーの一覧と、デーモンの API**
  - `Runtime.Providers` は、`anthropic`、`claude-code` に、登録簿の各エージェントを足します。各エージェントには、使える・入れられる（入れたら何を取るか）・使えない理由のどれかが付きます。
  - デーモンの API は `POST /providers/{id}/install` です。入っていないものへの切り替えは `ErrNotInstalled`（409）です。
  - デーモンは `KAIROCODE_ACP_REGISTRY` で登録簿を変えられます（`off` で読まない）。テストは `off` にします（不変条件 9）。ランタイムは、URL を渡されなければ登録簿を読みません。
- **ACP と TUI**
  - KairoCode の ACP のエージェントは、入っていないプロバイダーの値の `_meta` に `kairocode/install`（取るものと版）を付けます。入れるのは、拡張のメソッド `_kairocode/install_provider` か、打ち込んだ `/provider install <id>` です。
  - TUI は、`/provider` の一覧で入っていないものを選ぶと、取るものと「sandbox の外で、利用者として動く」ことを示して確認します。Enter で入れてから切り替え、esc で一覧に戻ります。
- **エージェント自身のツール**
  - KairoCode のツールでない承認の求めは、その回では断り、`AskRequest`（ツールは求めの題、対象は入力、規則は「エージェントの中で、KairoCode の sandbox と記録の外で動く」）として記録します。
  - ワークフローは、ADR 0007 と同じく、プロセスを手放して承認を待ちます。答えは `mcp.grant` で記録されます。
  - 次の回で、同じ呼び出し（題と入力のハッシュが同じ）の求めに、答えが 1 回だけ使われます（`Store.UseGrant`）。答えの効く範囲はターンの中です。
  - codex は組み込みのツールを外せないので、名前だけの `write` も codex 自身のツールとして聞きます。
- **テスト**
  - `internal/agents`: `TestInstallBinary`、`TestInstallNpm`、`TestInstallAgainKeepsTheOldOne`、`TestCommandOutsideTheInstall`。`sha256` の照合を外すと落ちることを確かめました。
  - `internal/runtime`: `TestInstallProviderFromRegistry`、`TestAgentOwnToolAsksTheUser`（聞かずに許す形に壊すと落ちることを確かめた）、`TestSwitchProviderAndModel`、`TestCrashACPWrite`。
  - `internal/tui`: `TestProviderAndModel`（入れる確認、esc で戻る、Enter で入れてから切り替え）。
- **durability-reviewer の指摘で直したこと**
  - **TUI:** 入れる確認の画面では、Enter（確認したものを入れる）と esc（一覧に戻る）だけを受けます。矢印や数字で別のものを選べないようにしました（`TestProviderAndModel`）。
  - **許しの記録:** 許しを使った記録の名前を、エージェントの決める呼び出しの ID ではなく、その回の試行ごとに新しい MCP の口のトークンと内容のハッシュから作るようにしました。落ちた後に、セッションを読み直したエージェントが同じ ID で同じ求めを出しても、使われた許しは使われず、もう一度聞きます（`TestCrashAgentToolAnswerUsedOnce`。エージェントの ID で記録する形に戻すと、聞き直さずに落ちることを確かめた）。
  - **codex と copilot の名前だけの求め:** 確かめるテストを戻しました。codex は聞き（`TestCodexBareNameIsItsOwn`）、copilot は許します（`TestSwitchProviderAndModel`）。
  - **入れ直し:** 入れるたびに、新しい置き場（`<id>/<版>-<乱数>`）に入れます。前に入れたもの（動いているかもしれない）は消しません。
  - **記録の書き込み:** `installed.json` と `registry.json` は、一時ファイルに書いてから rename します。
  - **版のずれ:** 確かめた版を一緒に渡し（`version`）、登録簿の版が違えば入れません（`ErrChanged`）。
  - **起動するコマンド:** 登録簿の `cmd` と npm の `bin` が、入れたものの外を指せないようにしました（`filepath.IsLocal`）。
- **まだ:** 版を上げる（新しい版があると出す）こと。認証が要るエージェントへの案内（ACP の `authenticate`）。古い版の置き場の片付け。
