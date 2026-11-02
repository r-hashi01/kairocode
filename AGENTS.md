# AGENTS.md

このリポジトリで作業するコーディングエージェント（と人）向けの作業指示です。

- 要件: `../kairo/docs/kairocode/requirements.md`
- 設計: `../kairo/docs/kairocode/design.md`（言語と kairo の使い方は ADR 0001 で置き換え）
- kairo の Go の SDK: `../kairo/sdk/go/README.md`、`go doc github.com/r-hashi01/kairo/sdk/go`
- 進め方と各段階の受け入れ条件: `docs/milestones.md`
- kairo の側に足りないもの: `docs/kairo-gaps.md`
- KairoCode の側の設計判断: `docs/adr/`

要件と設計は kairo のリポジトリにあります。隣に `kairo` が clone されている前提です（`../kairo`）。

## これは何か

KairoCode は、kairo（耐久ワークフローのランタイム）の上に作るコーディングエージェントです。opencode や Claude Code と同じ種類の道具ですが、セッションを kairo のワークフローとして動かします。目標は「落ちても、待っても、並べても壊れない」ことです。

- Go 1.27.2。kairo は、Go の SDK `github.com/r-hashi01/kairo/sdk/go`（v0.4.1 以上、パッケージ名 `kairo`）の埋め込みのランタイム（`kairo.Open`）で使います（ADR 0001）。
- 保存先は、手元では SQLite（`modernc.org/sqlite`）、サーバーレスとチームでは PostgreSQL です。手元のサーバーは待ちモード、サーバーレスはサスペンドモードです。
- サーバー（HTTP + SSE）と、それにつなぐクライアント（TUI と CLI）に分けます。配り方は単一のバイナリ（`cmd/kairocode`）です。

## パッケージと責務の境界

| パッケージ | 責務 | I/O・goroutine |
|---|---|---|
| `internal/workflows` | kairo のワークフロー（`session`、`turn`、`flow`（Starlark のフローをワークフローの中で動かす。ADR 0003））。エージェントのループの制御 | **一切禁止。** SDK の `*kairo.Context`（`kairo.Call`、`Parallel`、`WaitFor`、`Child`、`ctx.Now`、`ctx.Random`、`ctx.Sleep`）を通してだけ外を見る |
| `internal/runtime` | `kairo.Open`、ワークフローとアクションの宣言。アクションの実装（`agent.load`、`model`、`history.extend`、`tool.<name>`。後で `permission.check`、`snapshot`、`restore`）。ツールの作用の型から kairo の `Effect` への写し。サーバーと CLI に向けた小さな API（`CreateSession`、`Send`、`WaitTurn`） | ここで I/O する |
| `internal/tools` | 組み込みのツール。1 ファイル 1 ツールで、`tools.Def` に作用の型を書く | ここで I/O する |
| `internal/provider` | モデルのプロバイダーのアダプター（Anthropic と、テスト用の fake） | ここで I/O する |
| `internal/plugins` | プラグインとマーケットプレイス（ADR 0025、0027）: 入れる・更新する・消す、一覧、プラグインの定義（`kairocode-plugin.json`、Claude Code のプラグイン）を読む。サーバーにつなぐのと、鍵を使うのは runtime | ここで I/O する（入れるときに git を動かす） |
| `internal/secrets` | プラグインの鍵の保存（ADR 0025）: macOS のキーチェーン、Linux の Secret Service、なければ利用者だけが読めるファイル。値を記録に残さない（不変条件 6） | ここで I/O する |
| `internal/store` | メッセージの保存（`kc_message`、内容のハッシュで引く。1 度だけ書く） | ここで I/O する |
| `internal/daemon` | データの置き場ごとに 1 つのデーモン（ADR 0008）。ロック、`server.json`、Unix ソケットの上の HTTP、何もしないときの終了。ほかのプロセスが使うクライアントと、デーモンの自動の起動 | ここで I/O する |
| `internal/api` | デーモンとクライアントのあいだの型（セッション、出来事、承認、プロバイダー、プラグイン、フローの実行）と、名前の付いたエラー。runtime はこれを別名で使い、tui と acp は runtime ではなくこれを使う（`TestClientsDoNotImportTheRuntime`） | なし（型と小さな関数だけ） |
| `internal/server` | HTTP + SSE の API（設計 8.1 節）。ライブの出力は `k.Subscribe(<セッションの ID>)` から流す（ID の前方一致で、子孫の出力も届く） | ここで I/O する |
| `internal/opencode` | opencode のエージェント・コマンド・スキルの定義（`.opencode/agents`、`commands`、`SKILL.md`）を、opencode と同じ意味で読み、コマンドを展開する。frontmatter の YAML は小さな自前のパーサーで読む | ファイルを読むだけ（コマンドの `` !`…` `` は呼び出し側が sandbox で動かす） |
| `internal/acp` | ACP のエージェント（ADR 0002）と、端末の対話が使う ACP のクライアント。stdio の JSON-RPC、デーモンのセッション・出来事・承認を ACP に写す。待ちの状態を持たない（承認の待ちは kairo にある）。runtime は import しない（型は `internal/api`） | ここで I/O する |
| `internal/jsonrpc` | 改行で区切った JSON の上の JSON-RPC 2.0（ACP の stdio）。`internal/acp`（KairoCode が ACP のエージェント）と、`internal/runtime` の ACP のエンジン（KairoCode が codex・copilot の ACP のクライアント。ADR 0014）が使う | 渡された読み書きの口だけ |
| `internal/cli` | `kairocode chat`（引数なしの `kairo` も。自分の ACP のクライアントとして話す端末の対話）、`run / approvals / approve / daemon / acp / usage`、後で `replay`。DB を自分で開かず、デーモンにつなぐ | ここで I/O する |
| `internal/flowfile` | プロジェクトのフローのスクリプト（`.kairocode/workflows/<名前>.star` と下書き）の場所、一覧、読み書き。作業ディレクトリの中（`os.Root`）でだけ読み書きする | フローのファイルだけ |
| `internal/flowchart` | フローの形（ADR 0003）。Starlark のスクリプトの構文の木から、プリミティブの呼び出しを、順番・分岐・繰り返し・並列の形で読む。木の形の文字の図も | なし（読むだけ） |
| `internal/tui` | `kairo` の端末の画面（ADR 0010、Bubble Tea v2）。ACP のクライアントの表示と入力を持つ。フローの図と編集のため、フローのスクリプトを `internal/flowfile` で読み書きする。runtime は import しない（型は `internal/api`） | 端末の入出力と、`internal/flowfile` を通したフローのファイルだけ |
| `internal/clipboard` | クリップボードの画像を読む OS のコマンド（ADR 0026）と、貼った画像の保存。利用者がキーを押したときだけ動く | ここで I/O する |
| `internal/tui/screen` | 端末の画面の下の領域を描き、会話をその上に書く（ADR 0033。Codex と同じ仕組み）。生モードと端末の設定 | 端末の入出力だけ |
| `internal/lsp` | 書いたファイルの、言語サーバーの診断のエラー（ADR 0009）。サーバーは sandbox の中で起動し、デーモンの中で持ち続け、使われなければ止める | ここで I/O する |
| `internal/config` | 設定の置き場所（利用者の `$XDG_CONFIG_HOME/kairocode` と、プロジェクトの `kairocode.json`）と、その読み出し（利用者の分、プロジェクトの分の順の層）。各節の意味は、読む側が決める | 設定のファイルを読むだけ |
| `internal/locale` | 読む人の言語（`KAIROCODE_LANG`、利用者の `kairocode.json`、ロケール）と、フローのレビューの言い回しの置き換え（項目名とスクリプトは置き換えない） | 環境変数と利用者の設定を読むだけ |
| `internal/trust` | 信頼したフォルダの一覧（ADR 0010。データの置き場の `trust.json`）。クライアントだけが読み書きする | ここで I/O する |
| `internal/invariants` | 下の不変条件のうち、ソースから読めるものの検査（go/ast、go/types） | なし |
| `cmd/kairocode` | エントリーポイント | — |

ワークフローが import してよいパッケージは、`internal/invariants/invariants.go` の許可リストにあるものだけです（kairo の SDK、Starlark（`go.starlark.net`。ADR 0003）、`internal/msg`（メッセージの型。純粋なパッケージ）、決定的な標準ライブラリ）。ほかの内部のパッケージの型が要るときは、純粋なパッケージに分けてから、リストに足します。

## 破ってはいけない不変条件

破ると壊れるのは、要件の中心（落ちても壊れない、二重に起きない）です。どれも自動テストか hook で守ります。

1. **ワークフローのコードは決定的である。** `internal/workflows` の非テストのコードでは、時計（`time.Now` など）、乱数、環境変数、ファイル・ネットワーク・プロセス、goroutine、`select`、map の走査（`maps.Keys` などの反復子を含む）を使いません。時刻と乱数は `ctx.Now()` と `ctx.Random()`、並列は `kairo.Parallel`、それ以外はすべてアクションを通します。map は `slices.Sorted(maps.Keys(m))` で走査します。→ `TestWorkflowCodeIsDeterministic`（import の許可リスト、型による map の検出）、hook（`scripts/workflow-forbidden.txt`）
2. **ワークフローの分岐に使う値は、記録された値だけである。** 設定（権限の規則、エージェントの定義、モデルの選択）は、ワークフローの中で直接読まず、アクション（`agent.load`、`permission.check`）の結果として記録してから使います。パッケージの変数やキャッシュも読みません。設定を変えた後に再開しても、進行中のターンは開始時の値で進みます（設計 4・5 節）。
3. **すべてのツールは作用の型を宣言する。宣言がなければ「実」として扱う。** 型は `tools.Read`、`tools.Idempotent`、`tools.Real` で、ゼロ値が `Real` です（kairo の `ir.Effect` と同じ規則）。`tools.Def` のリテラルには、`Real` でも `Effect` を明示します。kairo への写し方は `Read` → `kairo.Unprotected`、`Idempotent` → `kairo.Real` + `kairo.IdempotentRetry()`、`Real` → `kairo.Real` です（`internal/runtime`）。MCP とカスタムのツールも同じです。→ `TestToolsDeclareEffect`
4. **結果が不明な実のツールを、成功扱いしない。** `Idempotent` のツールは、自動で確かめてからやり直します（edit と write は「書く前・書いた後のハッシュ」で比べて置き換える。設計 3.1 節）。`Real` のツールは kairo の「要確認」（blocked）で止め、人に「実行されたか」を聞き、答えを `k.Resolve` か `k.ResolveFailed` で返します。handler が「効いたかわからない」失敗を返すときは `kairo.Unknown(err)`、効いていないと言えるときだけ `kairo.Retryable(err)` で包みます。確かめずにもう一度実行してはいけません。→ crash のテスト
5. **モデルの呼び出しの入力は、ハッシュの列にする。** 履歴の本文を `model` の呼び出しの入力に入れません。メッセージは `kc_message` に内容のハッシュを鍵として 1 度だけ書き、書き換えません（設計 2.2 節、NFR-5）。
6. **秘密を記録に残さない。** API キーは、アクションの中で環境変数か設定から読みます。ワークフローの入力、アクションの入出力、`kc_message`、ログ、エラーの文面、テストの記録（testdata）に入れません。→ `TestNoSecretsInSources`、hook
7. **待つときはプロセスを手放す。** 承認や外部の出来事は `kairo.WaitFor`（kairo のシグナル）で待ちます。ポーリング、`time.Ticker`、プロセスの中に待ちの状態を持つ実装をしません（NFR-4）。
8. **実行の ID は、決まった規則で作る。** 根（セッション）だけを `kairo.WithID("s/<ID>")` で決めます。ターン、サブエージェント、計画の葉は `kairo.Child` で始め、ID は SDK の規則（`<親の ID>/<sha256(種類 + 入力) の先頭 24 文字>.<n>`）に任せます。同じ入力の子を区別するため、子の入力にターンの番号や計画の中の位置を入れます。子の入力に、記録されていない値（乱数、時刻、設定）を入れません。 Claude Code のエンジン（ADR 0007）の MCP の呼び出しの実行は、`<ターンの ID>/mcp/<ツールの名前と正規化した引数のハッシュ>.<何回目か>` です（tool_use の ID は使わない。落ちた後に呼び直されると変わるため）。
9. **テストは本物のモデルを呼ばない。** プロバイダーは fake か、記録した応答を返すものを使います。ネットワークにつながずに、すべてのテストが通るようにします。

## 設計判断の記録（ADR）

- KairoCode の側の判断は `docs/adr/` に書きます（`/adr` スキル）。kairo の ADR 0017 と同じく、**ADR が要る変更は、利用者の承認を得てから実装に入ります。** 自分で起票した ADR を自分で承認してはいけません。
- ADR が要る変更: 上の不変条件を足す・変える・緩める、ツールの作用の型を増やす、実行の ID の規則を変える、`kc_message` などの保存の形式を変える、依存を足す（下の「許可済みの依存」以外）、要件・設計から外れる、設計 11 節の未決事項を決める。
- **kairo の側に足りないものは、kairo の ADR として提案します（`/kairo-gap` スキル）。** このリポジトリから `../kairo` を編集してはいけません（hook が拒否します）。回避策を KairoCode の側に置く場合は、`docs/kairo-gaps.md` に書き、コードに `// kairo-gap #N:` のコメントを付けます。

## 作業の進め方

```sh
scripts/check.sh           # 完了の条件: gofmt、vet、build、全テスト（-race、crash を含む）、不変条件、ADR の検査
scripts/check.sh --quick   # hook が使う軽量版（go test -short。crash のテストは -short で自分を飛ばす）
make check | quick | build
```

- **完了の条件は `scripts/check.sh` が通ることです。** 通らない状態で終わるなら、何が失敗していて、なぜ直せないのかを明記してください。
- **`scripts/check.sh` は、エージェントの sandbox の外で動かします。** テストは `127.0.0.1` と Unix ソケットで待ち受け（偽のサーバー、デーモン）、KairoCode の sandbox でコマンドを起動します。エージェントの sandbox の中ではそれができないので、検査はテストの前に `== env` で止まり、何ができないかを言います（合格にはしません）。
  - KairoCode: `bash` を `sandbox: "loopback"`（sandbox の中で、手元の通信だけを許す。ADR 0031）で呼んで動かします。承認は要りません（`bash` の規則に従う）。sandbox を起動するテストは、入れ子にできないので自分を飛ばし、検査は「一部」の合格（`partial: N skipped`）になります。全部を動かすときだけ、`sandbox: "escalate"`（利用者の承認で sandbox の外）を使います。`KAIROCODE_CHECK_FULL=1` なら、「一部」は合格にせず終了コード 3 で止まります。
  - Codex: `scripts/check.sh` は、sandbox の外での実行の許可（escalated）を求めて動かします。`network_access` を許すだけでは足りません（macOS は、sandbox の中で別の sandbox を始められない）。
  - `== env` で止まったときの終了コードは 3 です。Stop hook は、そのとき「直せ」ではなく「外で動かせ」と返します。外で通れば、`check.sh` が通ったこと（`.claude/.cache/last-green`）を書くので、ソースが変わるまで Stop hook は検査を繰り返しません。
  - 外で動かす許可が得られないなら、`== env` の出力を添えて、利用者に外で動かしてもらってください。
- 段階（M0〜M4）の作業は `/milestone` スキルで進めます。受け入れ条件をテストとして先に書き、それが通ったら段階を終えます。
- 実のツール、ワークフロー、再開に関わる変更では、**プロセスを強制終了して再開するテスト**（crash のテスト）を書きます。名前は `TestCrash...` にし、先頭で `if testing.Short() { t.Skip(...) }` とします。テストのバイナリを子のプロセスとして起動し直し（`os.Args[0]` と `-test.run`、環境変数で役を渡す）、途中で `SIGKILL` して、同じ DB で再開します。「二重に起きない」は、ツールの handler が呼ばれた回数か、外から見える作用（ファイル、git の記録）の回数で確かめます。
- テストの閾値や期待値を緩めて通してはいけません。待ち合わせは固定の `time.Sleep` でなく、条件を見て待ちます。
- 時間の予算（NFR-2、NFR-3）は、テストで合否を決めません。計測して前後で比べます（kairo の ADR 0056 と同じ）。

## 変更の種類ごとの手順

- **ツールを足す・変える。** `/tool-change` スキル（作用の型、結果が不明なときの確かめ方、権限の規則の形、crash のテスト）。
- **ワークフロー（`internal/workflows`）を変える。** `/workflow-change` スキル（決定性、記録された値、ID の規則、版の扱い）。
- **kairo の API が足りない。** `/kairo-gap` スキル。
- **依存を足す。** 許可済みの依存は、`github.com/r-hashi01/kairo`（と `sdk/go/pgnotify`）、`modernc.org/sqlite`、`github.com/jackc/pgx/v5`、`github.com/anthropics/anthropic-sdk-go`、`github.com/modelcontextprotocol/go-sdk`、`go.starlark.net`（ADR 0003、承認済み）、`github.com/elastic/go-seccomp-bpf` と `golang.org/x/net`（ADR 0005。`x/net/bpf` を Linux の sandbox だけが使う）、`charm.land/bubbletea/v2`・`charm.land/bubbles/v2`・`charm.land/lipgloss/v2`（ADR 0010。`internal/tui` だけが使う）、`golang.org/x/oauth2`（ADR 0025。MCP の SDK の `auth` の型。プラグインの OAuth だけが使う）です。それ以外は、利用者の了承と ADR が要ります（hook が確認を求めます）。

## コーディング規約

- コメントと識別子は英語、ドキュメントは日本語です。周りのコードの密度と書き方に合わせます。
- gofmt に従います（編集後に hook が整形します）。
- エラーは握りつぶさず、戻り値で返すか記録します。ツールの「確定した失敗」（衝突、作業ディレクトリの外など）は、エラーではなく結果としてモデルに返します。
- ファイルの操作は、作業ディレクトリ（セッションのリポジトリ）の外に出ないようにします（パスを正規化し、シンボリックリンクも解決して確かめる）。
- goroutine を起こしたら、止める経路（`context` の取り消し）を必ず持たせます。

## 落とし穴

- `kairo.Call` は、kairo の 1 つの実行です。再開するとワークフローの関数を頭から実行し、同じ ID の呼び出しは記録から結果を返します。呼び出しの順番や入力を変えると、別の呼び出しとして新しく実行されます。ターンのワークフローの中身を変えるときは、`kairo.WorkflowVersion` で版を上げ、前の版を `kairo.Draining()` で残します（kairo の ADR 0060）。前の版は、`k.List` でその版の実行が残っていないことを確かめてから消します。
- 終わった木の記録は、根が終わってから `KeepFinished`（既定 24 時間）が過ぎると消えます。待っている木は消えません（kairo の ADR 0054）。
- 実の呼び出しは、意図が DB に確定してから動きます。プロセスが落ちると、借用（`Options.Lease`、既定 30 秒）が切れてから、別のプロセスか次の `Tick` が拾います。実の呼び出しは `blocked` に、ワークフローの関数は最初から動かし直しになります（kairo の ADR 0059）。
- 時計に関わるテスト（長い承認の待ち、期限、眠り）は `kairotest.New(t)` と `env.Advance(d)` で書きます。実時間では待ちません。
- kairo の実のステップは、意図が耐久化してから動きます。テストで「1 回だけ動いた」を確かめるときは、handler の中で数えて、ファイルか DB に残します（プロセスを殺すので、メモリの数は残らない）。
- kairo の側の不変条件（純粋なコア、実の命令は耐久化した意図から、など）は `../kairo/AGENTS.md` にあります。SDK の約束は `../kairo/sdk/go/README.md` と kairo の ADR 0058〜0060 で確かめます。

## このリポジトリのエージェント用ハーネス

- `.claude/settings.json` の hooks
  - `SessionStart`: ブランチ、未コミットの数、直近の検証結果、今の段階（`docs/milestones.md`）を表示します。
  - `PreToolUse`（Edit/Write）: `internal/workflows` への禁止パターン、API キーらしき文字列、ランタイムのデータ（`*.db`）の編集、`../kairo` の編集を拒否します。許可済み以外のモジュールを `go.mod` に足すときは確認を求めます。
  - `PostToolUse`（Edit/Write）: gofmt、パッケージの go vet と `go test -short` を実行します。`internal/workflows`、`internal/tools` を変えたら不変条件の検査も走らせます。
  - `Stop`: `scripts/check.sh --quick` が通らないと終了させません。ソースが前回の成功時から変わっていなければ省きます。
- スキル: `/verify`、`/milestone`、`/tool-change`、`/workflow-change`、`/kairo-gap`、`/adr`
- サブエージェント: `durability-reviewer`（差分を、不変条件と設計の観点でレビューする。読み取り専用）。`internal/workflows`、`internal/runtime`、`internal/tools` を変えたら、終える前に使います。
- `scripts/check.sh` は `scripts/adr-lint.sh` も実行します。

hook やテストを無効化して回避しないでください。誤検出だと思ったら、hook や検査の側を直す変更として提案してください。
