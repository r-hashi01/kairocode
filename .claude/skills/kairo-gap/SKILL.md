---
name: kairo-gap
description: Handle something KairoCode needs that kairo's Go engine does not provide (the code-first flow API, child-run listing, replay mode, a non-resident embedded mode, keeping finished calls while the parent runs, ...). Records it in docs/kairo-gaps.md and drafts a kairo ADR proposal instead of editing ../kairo. Use when an implementation hits a missing kairo API.
---

# kairo-gap

1. `docs/kairo-gaps.md` を読み、同じ不足がすでにあるかを確かめます。`../kairo/engine/`（`go doc github.com/r-hashi01/kairo/engine` も使える）と `../kairo/docs/adr/README.md` を読み、本当に足りないのか（エンジンの公開 API で表せないか）を確かめます。
2. 足りないなら、`docs/kairo-gaps.md` の表に 1 行足すか、既存の行を更新します（使う要件、必要度、回避策）。
3. KairoCode の側で回避するかを決めます。回避するなら、回避のコードに `// kairo-gap #N:` のコメントを付け、表の「回避策」の欄に書きます。回避が不変条件を緩めるなら、回避せずに止めて利用者に相談します。
4. kairo の側の ADR の案を、kairo の `docs/adr/template.md` の形で書き、利用者に見せます。**`../kairo` のファイルは編集しません**（hook が拒否します）。起票は、利用者が kairo のリポジトリで行うか、kairo のリポジトリで開いた別のセッションで行います。
5. 利用者には、不足の要点、どの段階を止めるか、回避策の有無を 1〜2 文で伝えます。
