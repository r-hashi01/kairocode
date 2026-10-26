# 例: Exa の MCP サーバーで web 検索する

KairoCode には web 検索のツールを組み込んでいません。検索は MCP サーバーに任せます（opencode の `websearch` も、Exa などの MCP サーバーを使う形です）。ここでは Exa のリモートの MCP サーバー（`https://mcp.exa.ai/mcp`）を使う 3 つのやり方を示します。

Exa の MCP サーバーのツール（2026-10 時点。<https://exa.ai/docs/reference/exa-mcp>）:

| ツール | 既定 | 中身 |
|---|---|---|
| `web_search_exa` | 有効 | web 検索 |
| `web_fetch_exa` | 有効 | URL の中身を読む |
| `web_search_advanced_exa` | `?tools=` で有効にする | 絞り込みのある検索 |
| `agent_run` | API キーか OAuth のときだけ | 多段の調べもの（Exa の利用料がかかる） |

KairoCode の中では、ツールの名前は `exa_web_search_exa` のように、サーバー（またはプラグイン）の名前が前に付きます。

## 1. キーなしで試す（`kairocode.json`）

Exa は、キーなしでも回数の制限つきで使えます。利用者の `~/.config/kairocode/kairocode.json` に書きます（ADR 0022）。

```json
{
  "mcp": {
    "exa": { "type": "remote", "url": "https://mcp.exa.ai/mcp" }
  },
  "tools": {
    "exa_web_search_exa": { "effect": "read" },
    "exa_web_fetch_exa": { "effect": "read" }
  },
  "permission": {
    "exa_*": "allow"
  }
}
```

- `tools` の `effect: "read"` は、「外に作用しない」という宣言です。宣言のないツールは実（real）として扱われ、途中で落ちたときに人に「実行されたか」を聞きます（不変条件 3・4）。検索と読み込みは何度動いても害がないので、`read` と書きます。
- `permission` で、呼ぶたびの確認を省きます。確認したいときは書かないか、`"ask"` にします。
- `/mcp` で、サーバーがつながっているか、ツールとその作用の型を確かめられます。

プロジェクトの `kairocode.json` に書いた MCP サーバーは、最初のターンで動かし方を聞かれます（プロジェクトが勝手に外につなげないため）。

## 2. API キーで使う（プラグイン）

キーは `kairocode.json` に書かず、プラグインとして入れて、キーを OS の鍵の保管庫に預けます（ADR 0025）。例のプラグインは [`plugins/exa`](plugins/exa/kairocode-plugin.json) です。

```sh
mkdir -p ~/.config/kairocode/plugins
cp -R docs/examples/plugins/exa ~/.config/kairocode/plugins/exa
kairocode plugin connect exa   # API キーを聞かれる（入力は表示されない）
kairocode plugin               # exa: connected と、ツールの作用の型が出る
```

- マニフェストの `"x-api-key": "{secret:api_key}"` に、預けたキーが入ります。キーは記録・ログ・エラーの文面に残りません。
- このプラグインには、検索の使い方を書いたスキル（`skills/web-search/SKILL.md`）も入っています。
- キーが無効になると、`/plugin` に「authentication expired」と出て、モデルには `kairocode plugin connect exa` が要ると返ります。
- プラグインを git のリポジトリに置けば、`kairocode plugin install <git の URL>` で入れられます（コミットで固定され、`kairocode plugin update exa` で更新）。
- 権限の規則は、ほかのツールと同じく `kairocode.json` の `permission` に `"exa_*": "allow"` と書けます。

## 3. OAuth でサインインする（プラグイン）

Exa は OAuth でもサインインできます（`?login`。利用料は Exa のチームに付く）。例は [`plugins/exa-oauth`](plugins/exa-oauth/kairocode-plugin.json) です。

```sh
cp -R docs/examples/plugins/exa-oauth ~/.config/kairocode/plugins/exa
kairocode plugin connect exa   # ブラウザーが開く。サインインすると端末に戻る
```

トークンは保管庫に置かれ、切れる前にデーモンが更新します。

TUI（`kairo`）の中からは、`/plugin connect exa` で同じことができます。
