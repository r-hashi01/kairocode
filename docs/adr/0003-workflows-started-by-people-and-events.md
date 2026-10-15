# 0003 ワークフローは、人と出来事が始める。中身は Starlark で書き、TUI と CLI で見える

- 状態: 提案
- 日付: 2026-10-09（同日の「スキルとして扱う」案と「JavaScript で書く」案を、利用者の指摘で書き直した）。2026-10-15 に、開発のワークフローの例（5）を足した
- 関連: 要件 FR-13、FR-14、FR-15、FR-24、FR-26、設計 6 節、ADR 0001、0002（コマンドとエージェントの形）、0005（sandbox）、kairo の ADR 0049、0060

## 背景

KairoCode では、複数のサブエージェントの組み合わせ（並列、順番、反復、結果での分岐）を、kairo の耐久のワークフローとして動かせます。落ちても終わったサブエージェントはやり直さず、人の承認や外の出来事を何日でも待てます（設計 6 節）。

この ADR は、同じ日に 3 回書き直しました。いずれも利用者の指摘によるものです。

- **スキルの形（最初の案）をやめた理由:** スキルは「用意したけど使われない」ものです。使うかどうかをモデルの判断に任せると、飛ばされます。このリポジトリのハーネスでも、「終える前に検証する」は Stop hook で必ず流しています。
- **JSON の計画（設計 6.2 節）をやめた理由:** 独自の形は、中身をモデルと人に理解させる工夫が要ります。
- **JavaScript を経て Starlark にした理由:** 利用者は Python の書き方を望みました。本物の Python は、OS を触れるので sandbox が要り、決定性も環境でしか守れず、Python の実行環境も要ります。Starlark（Python の方言）にすると、次の性質が言語の側で手に入ります。

2026-10-09 に、`go.starlark.net` で次のことを確かめました。

- **そのまま動いたもの:** `for`、`while`（オプション）、辞書、内包表記、`def` と `return`、`sorted`、`%` と `.format()`。
- **構文の段階で、行と列つきのエラーになるもの:** f-string、`try/except`、`class`、`import`、`time`。
- **決定性:** 時計、乱数、I/O は言語にありません。辞書を走査する順番は、入れた順で毎回同じです。
- **止める仕組み:** 無限ループは、ステップ数の上限（`SetMaxExecutionSteps`）で止まります。
- **依存:** 純粋な Go で、増える依存は `golang.org/x/sys` だけです。

比較のため、Go のインタプリタ（`github.com/traefik/yaegi` v0.16.1）も試しました。スクリプトに渡すものを絞ることはできましたが、正しいジェネリクスのコードが型のエラーで落ちました。

## 決定

### 1. 使うかどうかをモデルに選ばせない。入口は 3 つ

ワークフローは `.kairocode/workflows/<name>.star` に置き、次の 3 つの入口から始めます。

1. **人が呼ぶ（コマンド）。** ワークフローは、そのままコマンド `/<name>` になり、ACP のスラッシュのコマンドの一覧（`available_commands_update`）に並びます（ADR 0002）。
2. **出来事が始める（hook）。** `kairocode.json` の `hooks` で、出来事にワークフローを結びつけます。結びつけたものは、その時点で必ず走ります。

   | 出来事 | 時点 | 段階 |
   |---|---|---|
   | `turn.end` | ターンが応答を返す前 | M3 |
   | `tool.after` | ツールの後。ツールの名前と引数の形で絞る | M3 |
   | `session.start` | セッションの始め | M3 |
   | `github.*`、`ci.result`、`schedule` | 外の出来事（Issue のコメント、CI の結果、定時） | M4 |

   - `turn.end` と `tool.after` のワークフローは、`{"ok": bool, "message": str}` を返します。
   - `ok` が偽なら、`message` をモデルに返してループを続けます。続ける回数には上限があります。
3. **エージェントの進め方そのものにする。** エージェントの定義（ADR 0002）に `workflow: <name>` を書くと、そのエージェントのターンは、既定のループの代わりにそのワークフローで進みます。

モデルがその場でスクリプトを書いて動かす道（`workflow` のツール）も残します。その場合は、動かす前に構造の図（下の 4）を見せて、利用者に確認します。保存されたワークフローを、モデルが名前で選んで呼ぶ道は作りません。

### 2. 中身は Starlark で書く

```python
DESCRIPTION = "変えたファイルを並列にレビューして直し、テストが通るまで繰り返す"
INPUT = {"files": list}
LIMITS = {"agents": 12, "tokens": 2000000}

def main(input):
    parallel([
        {"agent": "reviewer", "prompt": "%s の型の誤りを探して直す" % f}
        for f in input["files"]
    ])
    for round in range(3):
        test = tool("bash", command = "go test ./...")
        if test.exit_code == 0:
            return {"ok": True}
        agent(agent = "build", prompt = "テストが落ちた。直して:\n" + test.output)
    return {"ok": False, "message": "3 回直してもテストが通らない"}
```

- **プリミティブ（最初の版）:**

  | 書き方 | 働き | kairo での形 |
  |---|---|---|
  | `agent(agent=, prompt=, schema=None)` | サブエージェント 1 つ。`schema` を付けると型付きの値で返る | `kairo.Child("agent", …)` |
  | `parallel([spec, …])` | `agent` の指定（辞書）を並列に動かし、結果を順に返す | `kairo.Parallel` |
  | `decide(prompt=, type=)` | 型付きの判断（`bool`、`enum`、`number`。設計 6.3 節） | モデルの構造化された出力 |
  | `tool(name, **args)` | ツールを直接呼ぶ。権限の判定、作用の型、sandbox（ADR 0005）はツールのまま | ツールのアクション |
  | `ask(question=, options=None)` | 人に訊いて待つ（ACP では `session/request_permission`） | `kairo.WaitFor` |
  | `wait(event, timeout=)` | 外の出来事を待つ（M4） | `kairo.WaitFor` |
  | `now()`、`random()` | 記録される時刻と乱数 | `ctx.Now`、`ctx.Random` |

- **失敗は値で返します。** Starlark には `try/except` がありません。そのため、プリミティブは予期した失敗（ツールの失敗、エージェントの失敗、拒否）を例外にせず、`ok`、`output`、`error` を持つ値で返します。スクリプトの中で `if` で分岐します。予期しない失敗（上限を超えた、スクリプトの誤り）は、ワークフローを止めます。
- **並列は Go の側で動かします。** `parallel` に渡すのは関数ではなく、指定（辞書）の配列です。スクリプトの流れは 1 本のままです。
- **実行の仕組み:**
  - `go.starlark.net` で、KairoCode のプロセスの中で動かします。OS の sandbox は要りません（ADR 0005）。
  - 再開するとスクリプトを頭から動かし直し、プリミティブの呼び出しは記録から結果を返します（kairo のコードで書く API と同じ。呼び出しの ID は、呼び出しの順と入力から決まる）。
  - 構文の検査は、動かす前に行います。エラーは行と列つきでモデルに返し、直させます。f-string など、Python との違いでよく当たるものには、直し方を添えます。
- **決定性の守り方:**
  - 言語の外に出る道は、上のプリミティブだけです。
  - スクリプトの中身は、始めたときに内容のハッシュとともに記録します（AGENTS.md の不変条件 2）。走っている途中でファイルを書き換えても、走っている実行は始めたときのスクリプトで終わります。
  - ステップ数の上限を設けます。
  - **ずれの検出:** 再開のときに、スクリプトが記録と違う呼び出しをしようとしたら、実行せずに止めます。たとえば、まだ読み直していない記録が残っているのに、新しい呼び出しが来た場合です。Starlark は決定的なので、本来は起きません。KairoCode の版を上げたときなどに備える安全網です。
- **上限:** `LIMITS`（`agents`、`depth`、`steps`、`tokens`）は、プリミティブを呼ぶたびに記録された値で数え、超えたら止めます。

### 3. 権限

- ワークフローの中の `agent` と `tool` は、セッションの権限の規則、作用の型、sandbox をそのまま通ります。ワークフローが自分で権限を広げることはできません。
- リポジトリに置かれた hook は、そのリポジトリで初めて走る前に、一度だけ利用者に確認します（何の出来事で何が走るか）。答えは記録します。

### 4. TUI と CLI で見える

ブラウザの画面は作りません。見え方は 2 つです。

- **動かす前（静的な図）:**
  - スクリプトを構文の木（`go.starlark.net/syntax`）に読み、プリミティブの呼び出しが `for`、`while`、`if` の中でどう並ぶかを、木の形の文字の図にします。
  - 繰り返しには範囲（`for f in input["files"]`、`range(3)`）を、分岐には条件の文を添えます。
  - 見せる場所:
    - `kairocode workflow graph <name>` で出します。
    - モデルがその場で書いたワークフローを動かす前の確認で、同じ図を見せます。
    - ACP のクライアントでは、ツールの出力（テキスト）として出ます。

  ```
  review-and-fix
  ├─ parallel   for f in input["files"]
  │   └─ agent reviewer   "%s の型の誤りを探して直す"
  └─ loop   for round in range(3)
      ├─ tool bash   "go test ./..."
      ├─ if test.exit_code == 0 → return
      └─ agent build   "テストが落ちた。直して:…"
  ```

- **動いている最中と後（実際の木）:**
  - kairo の記録（呼び出しの木、`k.Children`）から、各ノードの状態（実行中、終わり、承認待ち、要確認、失敗）、トークン、時間を出します。
  - 各呼び出しにスクリプトの行番号を記録し、静的な図のどこを動いているかを重ねて示します。
  - 見せる場所:
    - `kairocode workflow show <実行>` で出します。`--follow` を付けると、更新し続けます。
    - ACP のクライアントでは、ワークフローの各ノードが `plan` の項目として、状態つきで見えます（ADR 0002）。
- **使われ方:** `kairocode workflow stats` で、どのワークフローがどの入口から何回走り、何回失敗したかを出します。使われていないものも、それと分かるようにします。

### 5. 例: 開発そのもののワークフロー

利用者の発案（2026-10-15）です。「起票 → テストを先に → 実装 → レビュー → 検査 → PR」という開発の進め方そのものを、ワークフローとして書けます。KairoCode のハーネスの `/milestone` スキルが手で回している手順と同じです。

これを例にする理由は 3 つあります。

- **落ちても続く。** 終わった段（書いたテスト、通ったレビュー）はやり直しません。
- **人の判断が段になる。** 計画の承認や PR のレビューは `ask` と `wait` です。待っている間はプロセスを持ちません（不変条件 7）。CI の結果やレビューのコメントが、出来事として続きを始めます（M4）。
- **見える。** 上の 4 の図と `workflow show --follow` で、どの段にいて、どこで人を待っているかが分かります。

```python
DESCRIPTION = "起票した課題を、テストを先に書いて実装し、レビューと検査を通して PR にする"
INPUT = {"title": str, "detail": str}
LIMITS = {"agents": 20, "tokens": 5000000}

def main(input):
    plan = agent(agent = "plan", prompt = "次の課題の計画と、受け入れ条件のテストを考える:\n" + input["detail"],
                 schema = {"plan": str, "tests": list})
    answer = ask(question = "この計画で進めますか？\n" + plan.value["plan"], options = ["進める", "やめる"])
    if answer.value != "進める":
        return {"ok": False, "message": "計画が承認されなかった"}

    agent(agent = "build", prompt = "受け入れ条件をテストとして先に書く（まだ実装しない）:\n" + str(plan.value["tests"]))
    first = tool("bash", command = "go test ./...")
    if first.exit_code == 0:
        return {"ok": False, "message": "書いたテストが最初から通る。条件を確かめていない"}

    passed = False
    for round in range(5):
        agent(agent = "build", prompt = "テストが通るように実装する:\n" + plan.value["plan"])
        if tool("bash", command = "scripts/check.sh").exit_code == 0:
            passed = True
            break
    if not passed:
        return {"ok": False, "message": "5 回直しても検査が通らない"}

    for round in range(3):
        review = agent(agent = "durability-reviewer", prompt = "差分をレビューする",
                       schema = {"findings": list})
        if len(review.value["findings"]) == 0:
            break
        agent(agent = "build", prompt = "レビューの指摘を直し、検査を通す:\n" + str(review.value["findings"]))

    tool("git_commit", message = input["title"])
    pr = tool("bash", command = "gh pr create --fill", sandbox = "escalate",
              justification = "PR を作る（ネットワーク）")
    ci = wait("ci.result", timeout = "2h")              # M4: 外の出来事
    approved = wait("github.pr_review", timeout = "7d")  # M4: 人のレビュー
    return {"ok": ci.ok and approved.ok, "message": pr.output}
```

- 入口は、人のコマンド（`/develop`）か、外の出来事（Issue に付いたラベル、M4）です。モデルが名前で選んで呼ぶことはありません（上の 1）。
- 権限は、ふだんのツールと同じです（上の 3）。PR を作る bash は sandbox の外に出るので、そのたびに利用者に確認します（ADR 0005）。
- M3 の確かめとして、KairoCode の M3 以降の作業の一部を、このワークフローで回すことを目指します。

### この ADR で決めないこと

- ブラウザで見る画面。
- 外の出来事（`github.*` など）の受け方。M4 で決めます。

## 結果

- 依存に `go.starlark.net` を足します（AGENTS.md の許可済みの依存に加える）。
- AGENTS.md の不変条件 1（決定的）は、Starlark の実行環境の側にも及びます。プリミティブを足すときは、それが記録された値だけを返すことを守ります。
- テスト（M3 の受け入れ条件）:
  - `TestCrashWorkflowScriptResume`: 10 個の `agent` を `parallel` で並べたスクリプトを途中で強制終了して再開すると、終わった葉はやり直さない。
  - `TestWorkflowScriptDivergence`: 記録と違う呼び出しをするスクリプトで再開すると、実行せずに止まる。
  - `TestWorkflowScriptLimits`: ステップ数、エージェントの数、トークンの上限で止まる。
  - `TestTurnEndHookBlocks`: `turn.end` のワークフローが `ok` を偽で返すと、メッセージがモデルに返ってループが続き、上限の回数で止まる。
  - `TestWorkflowGraph`: 静的な図が、`for`、`if`、`parallel` の入れ子を正しく表す（決まった入力に対する出力を固定して比べる）。
  - 走っている途中でスクリプトを書き換えても、走っている実行は記録したスクリプトで終わる。
  - 上の 5 の開発のワークフローを、偽のモデルと偽の `gh` で最後まで流す。途中（テストを書いた後、レビューの後）で強制終了して再開しても、終わった段をやり直さない。`ask` と `wait` の間はプロセスを持たない。

## 検討した代替案

- **スキルの形で置き、モデルが説明を見て呼ぶ:** 使うかどうかがモデルの判断に残ります。
- **JSON か YAML の計画:** 構成要素とテンプレートの書き方を覚えてもらう必要があり、分岐や繰り返しを表そうとすると、言語のようなものを自分で作ることになります。
- **JavaScript（goja）:** `Date` や `Math.random` を自分で外す必要があります。利用者は Python の書き方を望みました。
- **本物の Python（sandbox の中のプロセス）:** 書ける範囲は一番広いです。ただし、Python の実行環境が要り（ADR 0001 の単一のバイナリから外れる）、決定性は環境の固定（`PYTHONHASHSEED`、時刻の差し替え）でしか守れません。構文の木を Go の中で読めないので、静的な図も Python のプロセスに頼むことになります。
- **Go（yaegi）:** KairoCode と同じ言語にできますが、正しいジェネリクスのコードが動きませんでした。インタプリタが Go の新しい機能に追いついていません。また、`GOPATH` からソースを読み込む作りなので、import の検査が別に要ります。
