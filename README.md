# KairoCode

落ちても、待っても、並べても壊れないコーディングエージェントです。

KairoCode は、セッションを [kairo](https://github.com/r-hashi01/kairo)（耐久ワークフローのランタイム）のワークフローとして動かします。Claude Code や opencode と同じ種類の道具ですが、次のことを約束します。

- **落ちても続く。** プロセスを `kill -9` しても、次に起きたところから続きます。終わったモデルの呼び出しやツールの実行は、やり直しません。
- **二重に起きない。** `git push` や HTTP の POST のような「実の」作用は、結果が分からなくなったら勝手にやり直さず、人に「実行されたか」を聞きます。ファイルの書き込みは、内容のハッシュで確かめてから進みます。
- **何時間でも待てる。** 承認の待ちはプロセスを手放して待ちます。答えは、端末、エディタ、CLI のどこからでも、後から出せます。
- **どのエージェントの上でも同じ。** Claude API のほか、手元の Claude Code・Codex・GitHub Copilot の CLI や ACP のエージェントをエンジンにでき、権限の規則・sandbox・記録は KairoCode の側で揃えます。
- **手順を、壊れないフローにする。** 決まった手順を Starlark のフロー（`.kairocode/workflows/*.star`）に書くか、言葉から作らせ、図で見て、試運転してから使えます。途中で落ちても、終わったところはやり直しません。

> まだ公開前の版です。使い方や設定の形は変わることがあります。

## 対応する環境

- macOS（`sandbox-exec` を使います。追加で入れるものはありません）
- Linux（[bubblewrap](https://github.com/containers/bubblewrap) の `bwrap` が要ります。user namespace を使えること）
- Windows には対応していません（sandbox がありません）。

## 入れる

Go 1.27.2 以上で、ソースからビルドします。

```sh
git clone https://github.com/r-hashi01/kairocode
cd kairocode
make build          # bin/kairocode と、同じコマンドの bin/kairo ができる
```

`bin/` を `PATH` に入れるか、`go install ./cmd/kairocode` で入れてください。`kairocode version` で版を確かめられます。

## 最初の設定

モデルのエンジンを 1 つ選びます。

| エンジン | 使うもの | 選び方 |
|---|---|---|
| `anthropic`（既定） | Claude API。`ANTHROPIC_API_KEY` など、Anthropic の SDK が読む認証 | 何もしない |
| `claude-code` | 自分の Claude Code（サブスクリプションでサインイン済み） | `/provider` か `KAIROCODE_ENGINE=claude-code` |
| `codex` | 自分の Codex CLI | 同上 |
| `copilot` | 自分の GitHub Copilot CLI | 同上 |

チャット欄の `/provider` で、ACP のエージェントを登録簿から入れて使うこともできます。モデルは `/model`（または `KAIROCODE_MODEL`）で選びます。

## 使う

```sh
kairo                     # 端末の対話（TUI）。初めてのフォルダでは、信頼するかを聞きます
kairocode run "テストを直して"         # 1 つの依頼を対話なしでこなして終わる
kairocode run -detach "…"            # 人の承認が要るところで止めて終わる（終了コード 3）
kairocode resume <session>           # その続き
kairocode approvals <session>        # 待っている承認の一覧
kairocode approve [-deny] <key>      # 承認に答える（どの端末からでも）
kairocode log <session>              # セッションの記録（ターン、モデルの入出力、ツール、待ち、使用量）
kairocode sessions                   # このフォルダのセッション
```

`kairocode help` にすべてのコマンドがあります。セッションは、データの置き場（既定は `~/.local/share/kairocode`）ごとに 1 つのデーモンが動かします。デーモンはコマンドが自動で起動し、使われなければ 10 分で止まります。

### 端末の対話で使えるもの

- `/help`、`/model`、`/provider`、`/effort`、`/login`
- `/undo`、`/redo`: 作業ファイルを前のターンの状態に戻す・戻したのを戻す（記録は消えません）
- `/compact`: 履歴を要約して続ける（文脈の上限が近づくと自動でも行います）
- `/fork`: 過去のターンから、別のセッションとして続ける
- `/flow`: フロー（下を参照）
- `/plugin`、`/skills`、`/mcp`: プラグイン、スキル、MCP サーバー
- `/log`、`/resume`、`/review`
- Shift+Tab: 承認のモード（manual・auto・plan）を切り替える
- Esc: 今のターンを止める。Ctrl-C を 2 回で抜ける

### エディタから使う（ACP）

`kairocode acp` は、標準入出力で話す [ACP](https://agentclientprotocol.com) のエージェントです。Zed などの ACP に対応したエディタで、コマンドに `kairocode`、引数に `acp` を指定してください。

## フロー

決まった手順を、`.kairocode/workflows/<name>.star` に Starlark で書きます。

```python
DESCRIPTION = "変更をレビューしてから要約する"

def main(input):
    phase("review")
    r = agent(agent = "plan", prompt = "今の差分をレビューして: " + input["args"])
    if not r.ok:
        return {"ok": False, "message": r.error}
    return {"ok": True, "message": r.output}
```

- 使えるもの: `agent`（サブエージェント）、`parallel`、`tool`、`ask`（人に聞く）、`decide`（型付きの判断）、`phase`
- `/flow <name> <引数>` で動かします。`/flow new <説明>` で言葉から作らせ、図で確かめ、試運転してから保存できます。
- `kairocode flow graph <name>` でフローの図、`kairocode flow show -follow <session>` で動いている様子、`kairocode flow runs` で過去の実行が見られます。
- 途中で落ちても、終わった `agent` や `tool` はやり直しません。

## 権限と sandbox

`kairocode.json`（利用者の分は `~/.config/kairocode/kairocode.json`、プロジェクトの分はリポジトリの直下）に、ツールごとの規則を書きます。形は opencode と同じで、後に書いたものが勝ちます。

```json
{
  "permission": {
    "bash": { "*": "ask", "git status*": "allow", "rm -rf*": "deny" },
    "edit": "allow"
  }
}
```

KairoCode が動かすコマンドは、OS の sandbox の中で動きます。書けるのは作業ディレクトリと一時ディレクトリだけで、その中でも `.git`・`.kairocode`・`.claude` などは守られます。sandbox の外で動かすときは、既定では毎回人に聞きます。

プロジェクトの設定（権限、hook、MCP サーバー）は、そのフォルダを信頼すると答えるまで読みません。

## 開発

作業の決まりは [AGENTS.md](AGENTS.md)、設計判断は [docs/adr](docs/adr/README.md)、進み具合は [docs/milestones.md](docs/milestones.md) にあります。

```sh
scripts/check.sh            # gofmt、vet、build、全テスト（-race、crash のテストを含む）、不変条件、ADR
scripts/check.sh --quick    # 軽い版
```

## ライセンス

[MIT](LICENSE)。ただし `third_party/` の中のものは、それぞれのライセンスに従います（macOS の sandbox の方針 `third_party/codex-sandbox` は OpenAI Codex 由来で、Apache License 2.0。[LICENSE](third_party/codex-sandbox/LICENSE)、[NOTICE](third_party/codex-sandbox/NOTICE)）。
