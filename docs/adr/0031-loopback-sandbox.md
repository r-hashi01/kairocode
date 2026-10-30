# 0031 手元の通信だけを許す sandbox: 検査を sandbox の外に出さずに動かす

- 状態: 承認（2026-10-27）
- 日付: 2026-10-27
- 関連: ADR 0005（sandbox）、ADR 0028（裏で動くコマンド）、AGENTS.md「作業の進め方」

## 背景

`scripts/check.sh` は、エージェントの sandbox の外で動かすことになっています。KairoCode では `bash` を `sandbox: "escalate"`（利用者の承認で sandbox の外）で呼びます。

外で動かすと、`go test` を通して、エージェントがいま書いたテストのコードが制限なしで動きます。「check.sh だけは聞かずに外へ出す」という許可も、中身は「何でも外で動かせる」と同じです。そのため、外に出すことを自動にはできず、長いループで 1 つのエージェントに任せるとき、検査のたびに人の承認を待つことになります。

KairoCode の sandbox（macOS、`WorkspaceWrite`）の中で検査を動かして、止められるものを確かめました（2026-10-27）。

1. **`127.0.0.1` での待ち受け**（偽のサーバー）: `bind: operation not permitted`。
2. **Unix ソケット**（デーモン）: 一時フォルダーの中でも `bind: operation not permitted`。
3. **sandbox の入れ子**（KairoCode の sandbox でコマンドを起動するテスト）: `sandbox_apply: Operation not permitted`。macOS では、sandbox の中で別の sandbox を始められません。
4. **Go のビルドのキャッシュ**（`~/Library/Caches/go-build`）: 書けないので、`go test` はビルドの段階で止まります。

試作で、1 と 2 だけを許す規則を足しました。また、ビルドのキャッシュをリポジトリの中に置きました。すると、`internal/flowchart`、`internal/server` は中で通りました。`internal/daemon` も、3 に当たる 1 件（`TestCrashDaemonEventsAfterRestart`。bash を sandbox で動かす）を除いて通りました。外へのネットワーク（`curl https://example.com`）と、許した場所の外の Unix ソケットは、止められたままでした。

## 決定

1. **sandbox の規則に「手元の通信」を足す。** `sandbox.Policy` に `Loopback bool` を足します。`Loopback` のときに許すのは、次の 2 つだけです。
   - **localhost の TCP と UDP:** 待ち受け、受ける、つなぐ（macOS の規則では `(local ip "localhost:*")` と `(remote ip "localhost:*")`）。
   - **Unix ソケット:** そのコマンドのためだけの一時フォルダー（`/private/tmp/kc-XXXX`、モード 0700）の中でだけ、作る・つなぐことを許します。
     - コマンドの `TMPDIR` を、そのフォルダーにします。テストの `t.TempDir()` も、デーモンのソケットの置き場（ADR 0008）もこの中に入ります。
     - 一時フォルダー全体（`$TMPDIR`、`/tmp`）のソケットは許しません。そこには、本物のデーモンのソケット（`$TMPDIR/kairocode-<uid>/`）、tmux や herdr のソケットがあります。つなげると、自分で承認したり、端末に打ち込んだりして、sandbox の外に出られます。
     - `loopback` のときに書ける一時フォルダーは、この専用のフォルダーだけです（`/tmp` と `$TMPDIR` は読むだけ）。書けると、他人のソケットやそのフォルダーを専用のフォルダーへ `mv` して、つなげてしまうからです。専用のフォルダーの外のソケットは、動かすことも消すこともできません。
     - フォルダーは、コマンドが終わったら消します。裏で動くコマンド（ADR 0028）は、その仕事が終わったときに消します。
   - ほかは `WorkspaceWrite` と同じです。外へのネットワーク、書ける場所の外への書き込み、守る名前（`.git`、設定の置き場）は許しません。
   - Linux（bubblewrap）は、ネットワークを切ると、`--unshare-net` でそのコマンドだけの localhost を持ちます。そのため TCP の扱いは今のままです。Unix ソケットの一時フォルダーは、macOS と同じにします。
   - 実装: `internal/sandbox`（`Policy.Loopback`、`seatbelt.go`、`platform_linux.go`、一時フォルダーの作成と後始末）。
2. **`bash` の `sandbox` に `"loopback"` を足す。**
   - 承認は、既定では、ふつうの `bash` と同じ規則に従います。`bash` が聞かずに通る設定なら、`loopback` も聞かずに通ります。ここが `escalate`（毎回聞く）との違いです。
   - **設定で変えられます。** `kairocode.json` の `sandbox.loopback` に、次のどれかを書きます。
     - `"allow"`（既定）: `bash` と同じ規則に従う。
     - `"ask"`: `loopback` の呼び出しは毎回聞く。
     - `"deny"`: `loopback` は使えない（`escalate` だけが残る）。
   - 利用者の設定はどれにでもできます。プロジェクトの設定（リポジトリの中）は、利用者の設定より厳しくする方向（`ask`、`deny`）にだけ効きます。手に入れたリポジトリが、勝手にゆるめられないようにするためです。
   - 設定は `permission.check` の中で読み、その判定として記録します（不変条件 2）。
   - sandbox が止めたらしい失敗のとき、モデルへの案内を「手元の通信が要るなら `loopback`、それ以外で要るなら `escalate`」にします。
   - 実装: `internal/tools/bash.go`、`internal/tools/jobs.go`、`internal/runtime/permission.go`。
3. **入れ子の sandbox は、使えないものとして扱う。**
   - macOS の `sandbox.Available()` は、`sandbox-exec` があるかだけでなく、一度実際に起動して確かめます（結果は覚えておく。Linux はすでにそうしている）。
   - 入れ子のときは `ErrUnavailable` を返します。本番の `bash` は、これまでどおり sandbox なしでは動かずに失敗します（ゆるめる方向には倒しません）。
   - sandbox を起動するテストは、すでに「no sandbox」で自分を飛ばす形になっています。まだ飛ばさないもの（`TestCrashDaemonEventsAfterRestart` など）にも、同じ飛ばし方を足します。
4. **`scripts/check.sh` を sandbox の中でも通す。**
   - `KAIROCODE_SANDBOX` があるときは、`GOCACHE` を `.cache/go-build`（git の対象外）にします。
     - 共有のキャッシュに sandbox から書けるようにはしません。書けると、sandbox の中のコマンドが、後で sandbox の外のビルドが使う中身を置けてしまうからです。
   - envcheck は、入れ子の sandbox が使えないことを、止める理由から外します。代わりに「sandbox を起動するテストは飛ばした」と言います。localhost と Unix ソケットが使えないときは、これまでどおり止めます（終了コード 3）。
   - 入れ子のために飛ばしたテストがあれば、終わりに「N 件を飛ばした。外（CI か手元の端末）で全部を動かすこと」と出します。
   - 既定では、そのときの終了コードは 0 にします。ただし `last-green` には「一部」と印を付けます。Stop hook は「一部」でも通し、コミットもできます。全部の合格は、外での実行（CI、push の前の手元）で確かめます。
   - **設定で変えられます。** 環境変数 `KAIROCODE_CHECK_FULL=1` のときは、飛ばしたテストがあれば、終了コード 3（外で動かせ）で終わります。「一部」の合格を認めない人や機械（CI など）は、これを設定します。`.claude/settings.json` の `env` や、KairoCode のエージェントの設定に書けます。
5. **AGENTS.md:** KairoCode では、`scripts/check.sh` を `sandbox: "loopback"` で動かすと書き換えます。`escalate` は、全部を動かすときだけに使います。

実装で決めたこと（決定の趣旨は変えない）:
- **macOS の「localhost」は、この機械の自分のアドレス全部です。** sandbox の規則の `localhost` には、LAN の IP など、この機械のほかのアドレスへの接続も含まれます（試して確かめた）。外の機械のアドレス（`8.8.8.8` など）へは止められます。届くのはこの機械の中のサービスだけで、決定の範囲と同じです。
- **sandbox の中のビルドのキャッシュは、`.cache/go-build`（リポジトリ直下。git の対象外）です。** `.claude` は sandbox が守る名前なので、中からは書けません。同じ理由で、sandbox の中の `check.sh` は `last-green` と「一部」の印も書けません（書けなくても合格は合格）。印は外で動かしたときにだけ残ります。
- **Linux の Unix ソケット（2026-10-27、CI で見つけて直した）。** bubblewrap の sandbox では、読み取り専用のファイルにしても、ネットワークの名前空間を分けても、パスの上の Unix ソケットにはつなげました。loopback に限らず、ネットワークのない普段の sandbox でもそうでした。デーモンのソケット（普段はデータの置き場、`~/.local/share/kairocode/kairocode.sock`）にもつなげたので、sandbox の中のコマンドが自分の呼び出しを承認できたことになります。CI の Linux で `TestSandboxLoopback` が落ちて見つかりました。直し方は次のとおりです。
  - ネットワークのない普段の sandbox: seccomp で `socket(AF_UNIX)` を拒否します（macOS と同じく Unix ソケットを作れない）。どこにもつながらない `socketpair` は残します。seccomp のライブラリは引数を 64 ビットで比べ、ほかのアーキテクチャ（amd64 の上の i386 の呼び出し）を素通しするので、その前に自前の命令を置きます（ほかのアーキテクチャの呼び出しはすべて拒否し、ソケットの種類は下位 32 ビットで見る。`TestSeccompFilter`）。
  - `loopback`: `/tmp`、`/var/tmp`、`TMPDIR`、`$XDG_RUNTIME_DIR` と、デーモンのデータの置き場（`sandbox.HideFromLoopback`。デーモンと、裏で動くコマンドの runner がそれぞれ言う）を空の tmpfs で覆い、`/run` の直下のソケット（docker の）を空のファイルで覆います。書ける場所はその後で結び直し、書ける場所の下にある覆う場所（ホームで始めたセッションの、データの置き場）は、結び直した後でもう一度覆います。覆う場所そのものが作業フォルダーなら（`/tmp` で始めたセッション）、`loopback` は使えません。`/tmp` の下のプログラムは、`loopback` の中からは見えません。
  - ホームの下のほかのソケット（gpg-agent など）には、Linux の `loopback` の中からつなげます。利用者の判断で、これは塞がないことにしました（下の追記）。
- **残ること:** 作業フォルダーの中にあるソケット（そのプロジェクトのプロセスのもの）は、ソケットを含むフォルダーごと専用のフォルダーへ動かせば、つなげます。作業フォルダーは書ける場所だからです。プロジェクトの外のソケット（デーモン、tmux、herdr）には届きません。
- **消し残し:** 専用のフォルダーは、コマンドの後に消します。デーモンや裏の仕事の runner が強制終了されたときは、`/private/tmp/kc-*` が残ります（OS の一時フォルダーの掃除に任せる）。
- 飛ばしたテストの数え方: sandbox を起動するテストは、`sandboxtest.Require(t)`（`internal/sandbox/sandboxtest`）で自分を飛ばします。そのとき、環境変数 `KAIROCODE_SKIPPED` のファイルにテストの名前を書きます。`check.sh` は、それを数えます。
- 入れ子のときに自分を飛ばすようにしたテスト: 裏で動くコマンド、Claude Code のフック、ファイルのスナップショット（undo、fork）、`escalate` の承認、`kairocode jobs run` ほか（`--quick` で 45 件）。

## 追記（2026-10-27、利用者の判断）: `loopback` が約束すること

Linux の `loopback` でホームの下のソケットに届くことが分かり、ホームも空に見せて要るものだけ見せ直す案を検討しました。その結果、塞がないことにしました。

- `loopback` は、初めから「この機械の中の口は信用する」という約束です。macOS では、この機械のアドレスで待っている TCP のサービス（開発用の DB、手元の LLM のサーバー、管理画面）にすでに届きます。Linux で Unix ソケットだけを完全に塞いでも、守りの強さは揃いません。
- ホームを空に見せると、見せ直していない設定やキャッシュ（`.gitconfig` など）が見えなくなり、検査を sandbox の中で動かすという目的を壊す危険があります。

`loopback` が約束するのは、次のことだけです。初めの 2 つは、どの OS でもテスト（`TestSandboxLoopback`、`TestLoopbackInTheBackground`）で確かめます。3 つ目は、テストがネットワークを使わないので、規則（macOS の sandbox の規則、Linux のネットワークの名前空間）で守ります。

- KairoCode 自身の承認の口（デーモンのソケットと、データの置き場）には届かない。
- ほかのコマンドやセッションの一時フォルダー（`/tmp`、`TMPDIR`、`$XDG_RUNTIME_DIR`）のソケットには届かず、動かすことも消すこともできない。
- 外の機械には届かない。

それ以外の、この機械の中の口（macOS では localhost の TCP の全部、Linux ではホームの下のソケット）には届きます。それが困る人は、設定の `sandbox.loopback` を `"ask"` か `"deny"` にします。既定は `"allow"` のままです。

ネットワークのない普段の sandbox は、どの OS でも Unix ソケットを作れません（Linux は seccomp。`TestSeccompFilter`）。こちらは何にも届きません。

Linux に、パスの上の Unix ソケットへの接続をパスで制限する仕組み（Landlock）が入り、広く使えるようになったら、それを重ねることを見直します（2026 年 4 月の時点ではレビュー中だった）。

## 結果

- KairoCode のエージェントは、検査を聞かれずに sandbox の中で動かせます。外に出すのは、入れ子の sandbox が要るテストを含めた全部を動かすときだけです。
- 許される範囲が広がるのは、`loopback` のコマンドから手元の TCP のサービス（手元のデータベース、開発用のサーバー、エディターの待ち受けなど）につなげることです。これは `sandbox.loopback` で「聞く」か「使わない」にできます。
- Claude Code や Codex の sandbox（エージェントの側）は変わりません。この ADR が効くのは、KairoCode の上で動くエージェントだけです。
- テスト
  - `internal/sandbox`（`TestSandboxLoopback`、`TestSandboxLoopbackNeedsItsDirectory`）:
    - `loopback` で、`127.0.0.1` の待ち受けと接続、専用の一時フォルダーの中の Unix ソケットができる。
    - 専用の一時フォルダーの外（本物の `$TMPDIR`）の Unix ソケットと、外へのネットワークは止められる。
    - `loopback` でないときは、今までどおり止められる。
    - 専用の一時フォルダーは、コマンドが終わると消える。
  - `internal/sandbox`（`TestSandboxNestedIsUnavailable`）: sandbox の中から `Available()` を呼ぶと、使えないと答える（子のプロセスを sandbox の中で起動して確かめる）。
  - `internal/runtime`（`TestLoopbackInTheBackground`）: 裏で動く仕事の一時フォルダーが、仕事が終わると消える。
  - `internal/runtime`（`TestLoopbackSetting`、`TestLoopbackCommand`）: `loopback` が、既定では `bash` の規則で聞かずに通る。`sandbox.loopback` の `ask` で聞き、`deny` で断る。プロジェクトの設定の `allow` は、利用者の `ask` をゆるめない。
  - `scripts/check.sh` の、飛ばした件数、「一部」の印、`KAIROCODE_CHECK_FULL=1` での終了コード 3 は、手で確かめます（自動のテストはなし）。

## 検討した代替案

- **check.sh だけは聞かずに外へ出す（自動の escalate）:** 外で動くのはエージェントが書いたコードなので、何でも外で動かせるのと同じです。
- **判定役のモデルに、外へ出してよいかを決めさせる:** 読んだ issue や Web のページに仕込まれた指示を見落とせば、外への道が開きます。
- **書ける場所と localhost だけを許す、もう一段の sandbox を新しく作る:** 中身はこの `Loopback` と同じになります。新しい種類にするより、今の規則に印を 1 つ足す方が小さく済みます。
- **一時フォルダー全体の Unix ソケットを許す:** 規則は簡単ですが、本物のデーモンや tmux のソケットにつながり、sandbox の外に出られます。
- **入れ子のとき、内側のコマンドを外側の sandbox に任せて、そのまま動かす:** 外側の方がゆるい場合（例えば内側が読むだけのとき）に、意図より広く動いてしまいます。テストのためだけに本番の道をゆるめることになります。
- **Go の共有のビルドのキャッシュを、書ける場所に足す:** sandbox の外のビルドが使う中身を、sandbox の中から書き換えられてしまいます。
