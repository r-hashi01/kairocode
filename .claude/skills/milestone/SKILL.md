---
name: milestone
description: Work through a KairoCode milestone (M0-M4 in docs/milestones.md) acceptance-test first - pick the current milestone, write its acceptance criteria as failing tests, implement until they pass, review, and mark it done. Use when starting or continuing implementation work, or when the user says "next milestone", "M0", "M1", etc.
---

# milestone

1. `docs/milestones.md` の「今の段階」と、その段階の中身・受け入れ条件を読みます。設計（`../kairo/docs/kairocode/design.md`）の関係する節と、要件の該当する FR / NFR も読みます。
2. 段階の中に、ADR が要る判断（設計 11 節の未決事項、依存の追加、AGENTS.md の「設計判断の記録」に当たるもの）があるかを洗い出します。あれば先に `/adr` で起票し、利用者の承認を待ちます。待っている間は、ADR が要らない部分だけを進めます。
3. kairo の側に足りないもの（`docs/kairo-gaps.md`）で、この段階に必須のものがあるかを確かめます。あれば `/kairo-gap` で扱いを決めます。
4. 受け入れ条件を、まずテストとして書きます。落ちることを確かめます（落ちないなら、テストが条件を確かめていない）。
   - 「二重に起きない」「再開できる」は、`TestCrash...` として書きます。先頭で `if testing.Short() { t.Skip(...) }` とし、テストのバイナリを子のプロセスとして起動し直し（`os.Args[0]`、`-test.run`、環境変数で役を渡す）、途中で `SIGKILL` して、同じ DB（`t.TempDir()`）で再開します。強制終了の時点は、fake のプロバイダーやツールの handler の中から合図（ファイルや標準出力の行）を出して決めます（固定の sleep で決めない）。
   - 回数は、handler の中で数えて、DB かファイルに残し、親のプロセスで確かめます。
5. 小さな単位で実装し、受け入れ条件のテストを 1 つずつ通します。ツールは `/tool-change`、ワークフローは `/workflow-change` の手順に従います。
6. `/verify` を実行し、`durability-reviewer` に差分をレビューさせ、指摘を直します。
7. `docs/milestones.md` の受け入れ条件にチェックを付けます。すべて通ったら、その段階の状態を「完了（日付）」にし、「今の段階」を次に進めます。
8. 利用者には、通した受け入れ条件、起票した ADR と kairo の不足、残った課題を短く報告します。
