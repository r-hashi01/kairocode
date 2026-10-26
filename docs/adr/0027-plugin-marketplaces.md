# 0027 プラグインのマーケットプレイス: 同梱のカタログと、足せるカタログ

- 状態: 承認（2026-10-26）
- 日付: 2026-10-26
- 関連: ADR 0025（プラグイン）、ADR 0010（端末の画面）、ADR 0002（ACP）

## 背景

ADR 0025 で、プラグインは `kairocode plugin install <git の URL か手元のパス>` で入れるようにしました。ただ、入れられるものの一覧がどこにもなく、URL を知らない利用者は、何を入れればよいかわかりません。何も入っていないときの `/plugin` も、「install で入れられる」と言うだけです。

Codex（0.163.0 で確かめた）と Claude Code は、どちらも「マーケットプレイス」を通して入れさせています。
- マーケットプレイスは、プラグインの名前と置き場所を並べたカタログ（`marketplace.json`）です。
- 製品に同梱のものが最初から入っていて、設定しなくても選べます。Codex の `openai-bundled` などです。
- 利用者は、git のリポジトリか手元のフォルダーを、カタログとして足せます（`codex plugin marketplace add`）。
- 入れるのは `codex plugin add <名前>@<カタログ>` か、画面の `/plugins` からです。
- カタログの各行は、認証のタイミング（入れるとき／初めて使うとき）を持ちます。

また、ADR 0025 は会話の中の `/plugin install`・`remove` を決めていましたが、実装は CLI だけでした。

## 決定

1. **カタログの形（`kairocode-marketplace.json`）:** git のリポジトリかフォルダーの最上位に置きます。
   ```json
   {
     "name": "acme",
     "description": "Acme のプラグイン",
     "plugins": [
       {"name": "tracker", "description": "Acme の課題管理", "source": "./plugins/tracker", "connect": "install"},
       {"name": "docs", "source": "https://github.com/acme/docs-plugin.git"}
     ]
   }
   ```
   - `source` が `./` で始まれば、カタログの中のフォルダーです。カタログの外（`..`、外へのシンボリックリンク）は断ります。それ以外は git の URL です。
   - `connect` は `install`（入れたらすぐ connect に進む）か `use`（初めて使うときに案内する。既定）です。
   - カタログとプラグインの名前は、フォルダーの名前にできるもの（`/` や `\` を含まない、`.` で始まらない）だけです。
2. **同梱のカタログ `kairocode`:** バイナリに埋め込みます（`internal/runtime/catalog`、`go:embed`）。初めは次の 2 つです。マニフェストは、私たちが本物のサーバーで確かめたものです。
   - `github`: GitHub の MCP サーバー（キー。`gh auth token` の値か、個人のアクセストークン）。
   - `exa`: Exa のウェブ検索（キー）。
3. **カタログを足す:** `kairocode plugin marketplace add|list|update|remove`。
   - `add <git の URL、owner/repo、手元のフォルダー>`。手元のフォルダーは、その場所をそのまま読みます（作りながら試すため）。git のものは、利用者の設定の置き場の `marketplaces/<名前>` に clone し、そのコミットで固定します。足したものの一覧は `marketplaces.json` です。
   - `update <名前>` で、git のものを最新のコミットにします。同梱のものは、バイナリと一緒に変わります。
   - 名前 `kairocode` は同梱のものの名前なので、足せません。
4. **入れる:** `kairocode plugin install <名前>` か `<名前>@<カタログ>`。
   - 名前だけなら、同梱、足した順の順にカタログを探します。2 つ以上にあれば、`@` で選ぶよう求めます。
   - カタログの中のプラグインは、利用者のプラグインの置き場にコピーします。どこから入れたか（カタログと名前）を `.kairocode-source` に残します。git の URL のものは、これまでどおり clone します。
   - git の URL と手元のパスも、これまでどおり受けます。手元のフォルダーが git のリポジトリでなければ、コピーします（これまでは clone しようとして失敗していた）。
   - `connect` が `install` で、プラグインに認証が要り、端末から動かしたときは、続けて connect に進みます。そうでなければ `kairocode plugin connect <名前>` を案内します。
   - `update <名前>` は、カタログから入れたものなら、カタログを（git のものは最新にしてから）読み直して入れ替えます。
5. **画面:** `/plugin` と `kairocode plugin list` は、入っているものに続けて、まだ入れていないものをカタログごとに見せます（名前、説明、認証の種類）。
   - 会話の中の `/plugin install`・`update`・`remove`・`marketplace` を足します（ADR 0025 の実装漏れ）。ACP のエージェントが直接行います（秘密は通らない）。
   - `kairo` の画面の `/plugin install` は、`connect` と同じく端末を `kairocode plugin install` に渡します（入れたらすぐ connect に進むため）。
6. **リモートのカタログ（サーバーに置くもの）は、この ADR では決めない。** 置き場所のサーバーが要るので、要るときに別に決めます。

実装に現れるところ: `internal/runtime/marketplace.go`（カタログの読み書き、入れる・入れ替える・外す）、`internal/runtime/catalog/`（同梱のカタログ）、`internal/cli/plugin.go`、`internal/acp/agent.go`（`/plugin` の副コマンド）、`internal/tui`（`/plugin install` を端末で）。

実装で決めたこと（決定の趣旨は変えない）:
- **版の指紋:** コピーで入れたプラグインには、ファイル（パスと中身）のハッシュの先頭 12 文字を `.kairocode-commit` に書きます。clone したものは、これまでどおりコミットです。これがサーバーの指紋（ADR 0025 の `Spec`）に入るので、版を上げずに中身が変わっても、進行中のターンは変わったサーバーを呼びません。
- **目印は KairoCode のもの:** プラグインが持ってきた `.kairocode-source`・`.kairocode-commit` は使わず、入れるときに書き直します。プラグインの中のシンボリックリンクは、コピーしません。
- **カタログの `source`:** `./` で始まらないものは、`https://`・`http://`・`ssh://`・`git://`・`git@` の git の URL だけです。カタログが、利用者の手元のフォルダーを指すことはできません。
- **ACP の `/plugin install`・`marketplace add` の手元のパス:** 絶対パスだけを受けます（相対パスは、セッションのフォルダーでなく、ACP のエージェントのプロセスの場所になってしまうため）。
- **URL の資格情報:** `https://user:token@…` のような URL は、表示（`From`、`marketplace list`）では `user:token@` を落とします。保存はそのままです（git が使うため）。
- テスト: `TestCatalog`、`TestMarketplaceInstall`、`TestMarketplaceGit`、`TestCopiedPluginIsAtItsFiles`、`TestUpdateFromAMarketplaceGoneBad`、`TestPluginChange`（ACP）、`TestPluginCommand`（端末の画面）。

## 結果

- 利用者は、何も設定しなくても `/plugin` で入れられるものを見て、`/plugin install github` で入れられます。会社や仲間のカタログも足せます。
- カタログから入れたものは、そのときの中身で固定され、`update` で入れ替えるまで変わりません。進行中のターンは、ADR 0025 のとおり始めの版のまま進みます。
- テスト
  - 同梱のカタログの全部のマニフェストが読めて、作用の型が宣言されている（`TestCatalog`）。
  - 手元のカタログを足し、名前だけ・`@` 付きで入れ、2 つにある名前は選ばせ、`update` で入れ替わり、`remove` で消える。カタログの外を指す `source` は断る（`TestMarketplaceInstall`）。
  - git のカタログを足して固定し、`update` で進む。
  - git でない手元のフォルダーを、そのまま入れられる。
  - `/plugin` が、まだ入れていないものをカタログごとに見せる。

## 検討した代替案

- **同梱の一覧だけ（カタログを足せない）:** 実装は一番小さいです。ただ、会社や仲間のプラグインを配る手段がなく、結局 URL を知らせて回ることになります。
- **Codex・Claude Code のカタログの形（`.agents/plugins/marketplace.json`、`.claude-plugin/marketplace.json`）をそのまま読む:** 中身のプラグインの形（MCP の設定、作用の型の宣言）が違うので、読めてもそのまま動きません。プラグインの形を `kairocode-plugin.json` にした ADR 0025 に合わせ、カタログも自分の形にします。
- **リモートのカタログを最初から:** 公式のカタログをサーバーに置く形です。置き場所と運用が要るので、今は同梱で足ります。
