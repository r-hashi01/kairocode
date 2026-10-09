# 0001 KairoCode を Go で書き、kairo の Go の SDK（埋め込みのランタイム）の上で動かす

- 状態: 承認（2026-10-09）
- 日付: 2026-10-09（2026-10-08 の案を、kairo 0.3.0 の Go の SDK に合わせて書き直した）
- 関連: 要件 NFR-2、NFR-3、NFR-4、NFR-8、FR-7、FR-21、FR-24、設計 1 節・2 節・8.3 節・9 節・11 節、kairo の ADR 0049、0051、0054、0058、0059、0060

## 背景

設計（2026-10-08 の案）は、KairoCode を TypeScript で書き、`kairo-sdk`（npm）を埋め込みで使う形でした（設計 1 節、NFR-8 の「Node 22 以上」）。当時の SDK には、M1 と M2 に必須のもの（ライブの出力、要確認の決着、冪等なやり直しの宣言）がありませんでした。

kairo 0.3.0（2026-10-09）で、Go の SDK `github.com/r-hashi01/kairo/sdk/go` が出ました（kairo の ADR 0058）。

- **コードで書くワークフロー:** `kairo.Call`、`Parallel`、`WaitFor`（期限つき）、`Child`、`ctx.Now`、`ctx.Random`、`ctx.Sleep`。呼び出しの ID は TS・Python と同じ規則（`<親の ID>/<sha256(種類 + 正規化した入力) の先頭 24 文字>.<n>`）です。
- **埋め込みのランタイム:** コアをネイティブで呼び、状態は `database/sql`（SQLite か PostgreSQL）に置きます。待ちモード（常駐）とサスペンドモード（サーバーレス）があります。
- **KairoCode に要るもの:**
  - ライブの出力（`TaskContext.Emit`、`k.Subscribe`。ID の前方一致で、子孫の出力も届く）
  - 要確認の決着（`k.Resolve`、`k.ResolveFailed`）
  - 冪等なやり直し（`IdempotentRetry()`、`TaskContext.IdempotencyKey`）
  - 子の実行の一覧（`k.Children`）
  - 待っている木の記録を消さない削除（ADR 0054）
  - 駆動の借用による別のプロセスでの再開（ADR 0059）
  - ワークフローの版の固定（ADR 0060）
  - 時計を動かすテストの環境（`kairotest`）

2026-10-09 に、公開された v0.3.0 で次の 2 つを確かめました。

- 承認を 48 時間待ったターン（終わった実行を残す期間は 24 時間）を別のプロセスで再開しても、終わった実の呼び出しは再実行されない。
- 実の呼び出しの handler の途中で SIGKILL すると、再開で `blocked` になり、`Resolve` で続く。

## 決定

- KairoCode を Go で書きます（`github.com/r-hashi01/kairocode`、Go 1.27.1）。kairo は、Go の SDK `github.com/r-hashi01/kairo/sdk/go`（v0.3.0 以上）の埋め込みのランタイム（`kairo.Open`）で使います。
- 保存先は、手元では SQLite（`modernc.org/sqlite`）、サーバーレスとチームでは PostgreSQL です。手元のサーバーは待ちモード、サーバーレス（M4）はサスペンドモードで動かします。
- kairod（`kairo.Connect`）は、最初の版では使いません。`Resolve`、`Children`、`List`、サスペンドモードが使えないためです（`kairo.ErrNeedsEmbedded`）。
- 作用の型の写し方は、`tools.Read` → `kairo.Unprotected`、`tools.Idempotent` → `kairo.Real` + `IdempotentRetry()`、`tools.Real` → `kairo.Real` です。
- 実行の ID は、根（セッション）だけを `WithID` で決め、ターン・サブエージェント・計画の葉は `kairo.Child` に SDK の規則で決めさせます。設計 2.1 節の `s/<ID>/t/<n>` という形はとりません。代わりに、子の入力にターンの番号や計画の中の位置を入れて区別します。
- ターンのワークフローには版（`WorkflowVersion`）を付けます。KairoCode を更新しても、進行中のターンは始めたときの版で終わります（設計 8.3 節）。
- 配り方は、単一のバイナリ（`cmd/kairocode`）です。
- 要件 NFR-8 は「macOS と Linux。単一のバイナリ（Go 1.27 でビルドする）」に読み替え、設計 1 節の「TypeScript、Node 22 以上、kairo-sdk」も、この ADR で置き換えます。要件と設計の本文は kairo のリポジトリにあるので、利用者がそちらで更新します。
- TUI のライブラリ（設計 11 節の 2）は、この ADR では決めません。

## 結果

- 設計 9 節の不足のうち、1（ライブの出力）、2（決着）、3（子の一覧）、5（大きな出力。`kc_message` で入力と出力を小さく保つ）、7（冪等なやり直し）は、kairo 0.3.0 で解けています。4（実行の上限）は、KairoCode の側で記録された使用量から判定します。残る不足は `docs/kairo-gaps.md` にあります。
- ワークフローが決定的かどうかは、go/ast と go/types で検査します（`internal/invariants`）。import の許可リストには、SDK のパッケージを入れます。
- テストは、時計に関わるものを `kairotest`（時計を動かす）で、強制終了に関わるものを、子のプロセスと SQLite のファイルで書きます。
- カスタムツール（FR-9）は、MCP のサーバーか外のコマンドとして足します。JavaScript でツールを書く道はありません。
- kairo の SDK の表と呼び出しの ID は、3 つの言語で同じです。そのため、将来 TS や Python のプロセスから同じセッションを扱う道も残ります。

## 検討した代替案

- **設計どおり TypeScript で書く:** 0.3.0 では、TS の SDK にもライブの出力などが入る予定ですが（kairo の ADR 0058 の 3 項）、Go の SDK が先に持っています。単一のバイナリで配れることと、WASM を通さないことも Go の利点です。
- **kairo のエンジン（`engine.Engine`）を直接使い、コードで書く API を KairoCode の側に作る（この ADR の 2026-10-08 の案）:** 終わった呼び出しの記録が 24 時間で消える問題（旧の不足 #10）を自分で回避する必要があり、呼び出しの ID の規則もほかの言語とずれるおそれがありました。kairo の側に Go の SDK ができたので、採りません。
- **kairod につなぐ（`kairo.Connect`）:** RPM と TPM をプロセスをまたいで守れますが、上のとおり必要な API が使えません。チームでの利用（要件の利用者の 4 行目）のときに改めて検討します。
