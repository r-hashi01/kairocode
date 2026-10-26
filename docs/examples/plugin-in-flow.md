# 例: フローからプラグインのツールを呼ぶ（GitHub）

プラグイン（ADR 0025）のツールは、フロー（ADR 0003）の `tool(...)` から、会話で使うときと同じ名前（`<プラグイン>_<ツール>`）で呼べます。フローの `agent(...)` で動くエージェントも、同じツールを使えます。

## 1. プラグインを入れる

```sh
kairocode plugin install github   # 続けてトークンを聞かれる（gh auth token の値）
```

## 2. 権限の規則

フローの中の呼び出しにも、会話と同じ権限の規則が効きます。何も書かなければ、どのツールも呼ぶ前に聞かれます。読むだけのものを聞かずに通すなら、`~/.config/kairocode/kairocode.json` に書きます。

```json
{
  "permission": {
    "github_list_*": "allow",
    "github_get_*": "allow",
    "github_search_*": "allow",
    "github_issue_read": "allow",
    "github_pull_request_read": "allow"
  }
}
```

これは同梱の github のプラグインが「読むだけ」と宣言しているツールと同じ並びです。ほかのツール（`github_add_issue_comment` など、書き込むもの）は、既定の規則（`"*": "ask"`）のとおり聞かれます。

## 3. フロー

`.kairocode/workflows/triage.star`:

```python
DESCRIPTION = "開いている issue を読み、優先して直すものを選んで、その方針をコメントする"

def main(input):
    repo = input["args"] or "r-hashi01/kairocode"
    owner, name = repo.split("/")

    phase("read")
    issues = tool("github_list_issues", owner = owner, repo = name, state = "OPEN")
    if not issues.ok:
        return {"ok": False, "message": issues.error}

    phase("choose")
    pick = agent(agent = "plan", prompt = "次の issue から、先に直すべきものを 1 つ選び、番号と理由を書いて:\n" + issues.output)

    phase("comment")
    number = decide("選ばれた issue の番号は？\n" + pick.output, type = "number")
    if not number.ok:
        return {"ok": False, "message": number.error}
    done = tool("github_add_issue_comment", owner = owner, repo = name,
                issue_number = number.value, body = pick.output)
    return {"ok": done.ok, "message": done.output or done.error}
```

```sh
kairocode run "/flow triage r-hashi01/kairocode"   # か、会話で /flow triage r-hashi01/kairocode
```

- `github_list_issues` は読むだけなので、規則のとおり聞かれずに動きます。
- `github_add_issue_comment` は書き込むので、承認を聞かれます。承認を待つ間はプロセスを手放し、落ちても、もう一度コメントすることはありません（結果がわからないときは、人に「されたか」を聞きます）。
- 引数の名前は、そのツールのスキーマのとおりです。`/plugin` の詳細でツールの名前を、会話でモデルに聞けば引数を確かめられます。

## 4. 言葉から作る

`/flow new 開いている issue から先に直すものを選んでコメントする` と頼むと、モデルが入っているプラグインのツールを使ってフローを書きます。書く前の試し（ADR 0017）では、MCP サーバーとプラグインを起動しません。そのツールは読むものも書くものも呼ばず、「何を渡して呼んだはずか」を試しの結果に出して、フローは先に進みます（その呼び出しは成功として返り、`.output` は「試しなので呼んでいない」という文です）。
