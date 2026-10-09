# 0005 KairoCode が動かすプロセスは、Codex にならった OS の sandbox の中で動かす

- 状態: 承認（2026-10-09）
- 日付: 2026-10-09
- 関連: 要件 FR-6、FR-7、FR-10、FR-12、NFR-7、NFR-8、設計 3.2 節・4 節、ADR 0002（権限の規則）、0003（ワークフロー）、AGENTS.md の不変条件 4・6

## 背景

bash のツールは、作用の型が「実」で、結果が不明なら人に確認します（設計 3.2 節）。権限の規則（ADR 0002）だけで守ると、安全にするには「確認」を増やすしかなく、opencode と同等の手間でこなす、という目標から離れます。

Codex CLI は、実行するコマンドを OS の sandbox に入れ、作業ディレクトリの外への書き込みとネットワークを止めています。利用者は、この方向を良しとしました（2026-10-09）。

2026-10-09 に、`github.com/openai/codex` の main（commit `2351d9e1`）のソースを調べました。以下のパスは `https://github.com/openai/codex/blob/main/` からの相対です。

- **macOS:**
  - `/usr/bin/sandbox-exec -p <SBPL> -D<KEY>=<path>… -- <cmd>` で動かします（`codex-rs/sandboxing/src/seatbelt.rs`）。
  - ポリシーは `(deny default)` から始まり、読み取りは全体を許し、書き込みは writable root ごとに許します。
  - writable root の直下の `.git`、`.agents`、`.codex`、`.aws` は書き込みから除き、それらの祖先の改名と削除も止めています。
  - パスは文字列に埋め込まず、`-D` の引数で渡します。
- **Linux:**
  - bubblewrap を使います。`--ro-bind / /` の上に writable root を `--bind` し、`.git` を読み取り専用に戻します。名前空間は `--unshare-user`、`--unshare-pid`、ネットワークを止めるときは `--unshare-net` です。
  - さらに、自分自身を補助のプロセスとして起動し直し（`codex-linux-sandbox`）、`PR_SET_NO_NEW_PRIVS` と seccomp をかけます。
  - Landlock は古い道として扱われています。
- **モード:** `read-only`、`workspace-write`、`danger-full-access` の 3 つです。workspace-write の writable root は、作業ディレクトリ、`/tmp`、`$TMPDIR`、設定で足したものです。ネットワークは既定で止めます。
- **昇格:** モデルは、ツールの引数（`sandbox_permissions`、`justification`）で sandbox の外での実行を求められます。sandbox に拒まれた失敗は、終了コード（Linux の SIGSYS）と出力の語で推定して見分けます。
- **環境変数:** 子のプロセスの環境変数は、いったん空にしてから、方針で作り直します。
- **ライセンス:** Apache-2.0 です。基本のポリシーは Chromium（BSD）に由来します。
- **既知の問題:**
  - GHSA-w5fx-fh39-j5rw: モデルが渡した `cwd` を writable root として扱い、作業ディレクトリの外に書けた。
  - Ubuntu 24.04 の AppArmor による userns の制限で、bubblewrap が動かない。
  - `sandbox-exec` は Apple が非推奨にしている。

## 決定

### 対象

- KairoCode が起動するコマンドは、すべて sandbox の中で動かします。対象は、bash のツール、ワークフローの `tool("bash", …)`（ADR 0003）、コマンドの `` !`shell` ``（ADR 0002）です。
- 対象外のものが 2 つあります。どちらも利用者が設定したものです。
  - LSP のサーバー（長く動き続けるもの）
  - MCP のサーバー

### モード

| モード | 書き込み | ネットワーク | 使うところ |
|---|---|---|---|
| `read-only` | なし | なし | plan のモード、`effect_limit: read` のエージェント |
| `workspace-write` | 作業ディレクトリ、`/tmp`、`$TMPDIR`、設定で足した root | なし（設定で開ける） | build のモード（既定） |
| `full-access` | 制限なし | あり | 昇格を承認したコマンドだけ |

- **writable root は、セッションを始めたときの作業ディレクトリで固定します。** モデルが渡す `cwd` は、その中に正規化できるときだけ使います（GHSA-w5fx-fh39-j5rw の教訓）。パスの途中にシンボリックリンクがある root は拒否します。
- **書き込みから守るもの:** writable root の直下の `.git`、`.kairocode`、`.opencode`、`.claude`、`.agents` は、書き込みを止めます。これらの祖先の改名と削除も止めます。
  - ここには KairoCode の hook と権限の設定（ADR 0002、0003）が置かれます。sandbox の中のコマンドが、自分の権限や自動で走る hook を書き換えられないようにするためです。
  - `.git` の中身を変える操作（commit など）は、git のツール（設計 3 節の冪等な git の操作）で行います。

### 作用の型との関係

- `read-only` の sandbox の中のコマンドは、書き込みもネットワークもできません。そのため、作用の型を `tools.Read` として扱えます。結果が不明でもやり直してよく、人に訊く必要がありません。
- `workspace-write` の中のコマンドは、これまでどおり `tools.Real` です。結果が不明なら要確認で止めます（不変条件 4）。sandbox は、作用の及ぶ範囲を作業ディレクトリに限るものであり、二重に起きないことを保証するものではありません。

### 権限の規則との関係

- 権限の規則（ADR 0002）の判定は、これまでどおり先に行います。sandbox は、その後の被害の範囲を限るものです。
- 既定の規則を、sandbox を前提にして緩めます。
  - build のエージェントの bash は、sandbox の中なら `allow` です。
  - 昇格（sandbox の外での実行）は、規則によらず必ず `ask` です。
- **昇格の流れ:**
  - モデルは bash のツールの引数で `sandbox: "escalate"` と理由（`justification`）を渡します。
  - KairoCode は理由を添えて利用者に承認を求めます（`kairo.WaitFor`。ACP では `session/request_permission`）。
  - 承認されたら `full-access` で 1 回だけ動かします。承認を「以後このコマンドの形は許可」として記録する選択肢も用意します。
- **sandbox に拒まれた失敗:**
  - 終了コードと出力から推定して、昇格を提案する文をモデルへの結果に足します。
  - 自動で sandbox の外で動かし直すことはしません。

### OS ごとの仕組み

- **macOS:**
  - `/usr/bin/sandbox-exec` を絶対パスで使います。
  - SBPL は Codex のポリシー（`seatbelt_base_policy.sbpl`、`seatbelt_network_policy.sbpl` ほか）を元にします。帰属表示と変更点の明記をつけて、`third_party/codex-sandbox/` に置きます（Apache-2.0 と Chromium の BSD）。
  - パスは `-D` の引数で渡します。
- **Linux:**
  - bubblewrap で名前空間とファイルシステムを作ります。
  - 自分自身を補助のプロセス（`kairocode __sandbox`。引数で役を切り替える）として起動し直し、`PR_SET_NO_NEW_PRIVS` と seccomp をかけてから、目的のコマンドを exec します。
  - seccomp のフィルタは、cgo の要らない純粋な Go のもの（`github.com/elastic/go-seccomp-bpf`）で作ります。単一のバイナリで配るためです（ADR 0001）。
- **sandbox が使えないとき（fail-closed）:** bubblewrap が無い、userns が使えない（AppArmor、コンテナ）、`sandbox-exec` が無い、などの場合です。
  - **黙って sandbox の外に落としません。** 起動時に知らせ、bash は毎回 `ask` にします。
  - コンテナのように外側で隔離している環境のために、設定 `sandbox: external` を用意します。明示的に選んだときだけ、sandbox なしで動かします。

### 環境変数

- 子のプロセスの環境変数は、いったん空にしてから作り直します。
- 名前に `KEY`、`SECRET`、`TOKEN`、`PASSWORD` を含むものは、**既定で除きます**（Codex の既定より安全側。不変条件 6）。設定 `env.include` で、名前を挙げて渡せます。
- `KAIROCODE_SANDBOX=<モード>` を設定します。ネットワークを止めたときは `KAIROCODE_SANDBOX_NETWORK_DISABLED=1` も設定します。

### この ADR で決めないこと

- ネットワークの許可リスト（プロキシ）。Codex の `network-proxy` のような仕組みは、必要になったら別の ADR で決めます。
- ワークフローのスクリプトの隔離。ワークフローは Starlark で書くので（ADR 0003）、言語の側で隔離されます。OS の sandbox は要りません。ワークフローから呼ぶ `tool("bash", …)` は、この ADR の sandbox を通ります。

## 結果

- 依存に `github.com/elastic/go-seccomp-bpf` を足します。
- `third_party/codex-sandbox/` に、Codex の SBPL とライセンス、NOTICE を置きます。
- 不変条件を 1 つ足します（AGENTS.md）: **「KairoCode が起動するコマンドは、sandbox の中で動かす。sandbox の外で動かすのは、利用者が承認した昇格と、明示した `sandbox: external` のときだけ」。** sandbox を使わずに `os/exec` でコマンドを起動するコードは、`internal/sandbox` の外に置かせません。検査を `internal/invariants` に足します。
- テスト（M1 の受け入れ条件に足す。macOS と Linux の両方で流す）:
  - `TestSandboxDeniesWriteOutsideWorkspace`、`TestSandboxProtectsGitAndConfig`（`.git`、`.kairocode` などへの書き込みと、その改名が拒否される）。
  - `TestSandboxDeniesNetwork`、`TestSandboxStripsSecrets`。
  - `TestSandboxIgnoresModelCwdOutsideRoot`（GHSA-w5fx-fh39-j5rw と同じ形）。
  - `TestSandboxFailsClosed`（sandbox が使えないとき、bash が毎回 `ask` になる）。
  - `TestEscalationAlwaysAsks`。
- CI の Linux のランナーでは、userns の制限（Ubuntu 24.04 の AppArmor）を設定で外す必要があります。外せない環境のためのテストは、fail-closed の側を確かめます。
- `sandbox-exec` は Apple が非推奨にしています。無くなったときは、macOS での隔離をやり直す必要があります。Codex も同じ立場です。

## 検討した代替案

- **権限の規則だけで守る:** 安全にするには「確認」を増やすしかなく、操作の手間が増えます。規則のすり抜け（同じ作用の別の書き方）も防げません。
- **Codex の `codex sandbox` を外のコマンドとして呼ぶ:** 実装は要りませんが、Codex を入れる必要があり、この CLI には安定の保証がありません。
- **Linux で Landlock を主に使う:** Codex でも古い道として扱われています。ネットワークと Unix ソケットの隔離が足りません。
- **コンテナ（Docker など）の中で動かす:** 隔離は強いですが、Docker が要り、起動も遅くなります。手元で使う道具には重すぎます。コンテナの中で KairoCode を動かす使い方は、`sandbox: external` で支えます。
