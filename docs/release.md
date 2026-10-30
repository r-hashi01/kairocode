# 公開とリリースの手順

KairoCode を個人向けの OSS として公開し（M6）、版を出す手順です。

## 1. 公開の前に確かめること

- M6 の受け入れ条件（`docs/milestones.md`）が、リリースの項目を除いて済んでいる。特に「公開の前の関門」（Zed と TUI での手での確認）。
- README が、手での確認の結果に合っている。
- `main` で CI（`check`、macOS と Linux）が通っている。
- kairo の SDK を、`replace` なしで取れる（proxy.golang.org に `github.com/r-hashi01/kairo` の使う版がある）。
- 作者のアドレスが、すべてのコミットで GitHub の noreply になっている（2026-10-27 に書き換え済み）。

```sh
git log --format='%ae%n%ce' | sort | uniq -c
# 146686591+r-hashi01@users.noreply.github.com と、GitHub の上でマージしたコミットの
# コミッターの noreply@github.com だけ
```

## 2. リポジトリを公開する

今のリポジトリ（非公開）は、PR #12 の force push の記録から、書き換える前のコミット（古いアドレス）を辿れます。そのまま公開にはしません。

モジュールのパスが `github.com/r-hashi01/kairocode` なので、公開するリポジトリもこの名前にします（`go install` がこの名前で取りに来る）。

1. 今のリポジトリの名前を変えて、非公開のまま残す（例: `kairocode-dev`）。GitHub の Settings → Repository name。手元の `origin` も付け替える。

   ```sh
   git remote set-url origin https://github.com/r-hashi01/kairocode-dev.git
   ```

2. 同じ名前 `r-hashi01/kairocode` で、空の公開リポジトリを作る（README や LICENSE は足さない）。
3. `main` とタグだけを push する（PR、Issue、古いブランチは持ち込まない）。

   ```sh
   git remote add public https://github.com/r-hashi01/kairocode.git
   git push public main
   git push public --tags
   ```

4. 公開したリポジトリで、`main` の CI が通ることを確かめる。Settings で、`main` を守る（force push を禁じる）。

## 3. 版を出す

版は `vMAJOR.MINOR.PATCH` のタグです（最初は `v0.1.0`）。

1. `main` を最新にして、タグを打って push する。

   ```sh
   git switch main && git pull
   git tag -a v0.1.0 -m "v0.1.0"
   git push public v0.1.0
   ```

2. `.github/workflows/release.yml` が動く。
   - まず `check.yml`（macOS と Linux、全部の検査）。通らなければ、リリースは作られない。
   - 通れば、`scripts/release-build.sh` が、macOS と Linux の amd64・arm64 の 4 つの tar.gz、`checksums.txt`、Homebrew の formula（`kairocode.rb`）を作り、**下書き**のリリースに載せる。
3. GitHub の Releases で下書きを開き、載ったファイルと、自動で作られた変更の一覧を確かめて、公開する。

同じものは手元でも作れます（中身を確かめたいとき）。

```sh
scripts/release-build.sh v0.1.0 dist
```

## 4. Homebrew の tap

1. 初回だけ: 公開リポジトリ `r-hashi01/homebrew-tap` を作る。
2. リリースに載った `kairocode.rb` を、tap の `Formula/kairocode.rb` に置いて push する（版ごとに置き換える）。
3. 確かめる。

   ```sh
   brew install r-hashi01/tap/kairocode
   kairocode version   # v0.1.0
   brew test kairocode
   ```

Linux の Homebrew では、`bubblewrap` を一緒に入れます（formula の `depends_on`）。

## 5. 入れ方（公開の後、README に書く）

| 入れ方 | コマンド |
|---|---|
| Homebrew（macOS、Linux） | `brew install r-hashi01/tap/kairocode` |
| Go | `go install github.com/r-hashi01/kairocode/cmd/kairocode@latest`（`kairo` のリンクは作られない） |
| バイナリ | Releases から tar.gz を落として、`kairocode` と `kairo` を `PATH` に置く |
| ソース | `make build` |

## 6. 気をつけること

- **macOS の署名と公証はしていません。** Go が arm64 のバイナリに付ける ad-hoc の署名だけです。Homebrew と `curl` で入れたものは、そのまま動きます。ブラウザで落としたものは、Gatekeeper に止められることがあります（`xattr -d com.apple.quarantine kairocode`）。公証には Apple の開発者の登録が要るので、必要になったら別に決めます。
- **Windows には出しません**（sandbox が無い）。
- リリースのワークフローが使う権限は、リリースを作る `contents: write` だけです（`GITHUB_TOKEN`）。tap への push は手で行うので、別のトークンは要りません。自動にするなら、tap に書ける細かい権限のトークンを secret に置くことになります（そのとき決める）。
