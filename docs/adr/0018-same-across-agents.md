# 0018 エージェントが変わっても同じことができる: KairoCode の約束、足りないものの補い方、フックは KairoCode の sandbox で

- 状態: 承認（2026-10-21）
- 日付: 2026-10-21
- 関連: ADR 0002（opencode と Claude Code の定義、スキル）、0003（KairoCode のフック）、0005（sandbox、昇格）、0007（Claude Code のエンジン）、0014・0015（codex、copilot、ACP のエージェント）

## 背景

利用者の判断（2026-10-21）は、次の 3 つです。

- 「何を渡すかを決めるのが KairoCode。読み込みはエージェント側でよい。足りないものは補う」
- 「KairoCode として一定の品質で開発できるようにする。エージェントが変わっても同じことができなければならない」
- 「フックは動かす。ただし KairoCode の sandbox 上で。sandbox を出るときは必ず承認を受ける」

エージェントが自分で読むものを、モデルを呼ばずに確かめました（2026-10-21）。

| | Claude Code 2.1.305 | codex 0.163 | copilot 1.0.98 |
|---|---|---|---|
| 確かめ方 | 手元のサーバーに要求を受けさせて中身を見た | `codex debug prompt-input` | `copilot instruction list`・`skill list` |
| AGENTS.md | 読まない | 読む | 読む |
| CLAUDE.md（project） | 読む* | 読まない | 読む |
| .claude/CLAUDE.md、CLAUDE.local.md、~/.claude/CLAUDE.md | 読む* | 読まない | 読まない |
| .claude/skills、~/.claude/skills | 読む*（project・利用者） | 読まない | project だけ読む |
| .agents/skills、~/.agents/skills | 読まない | 読む | 読む |
| .opencode・.kairocode の skills | 読まない | 読まない | 読まない |
| 自分のフック | 動かす* | 動かす（features.hooks） | 未確認 |
| 自分のものとして足すもの | プラグイン | 同梱のスキル、プラグイン、~/.codex/AGENTS.md | 同梱のスキル、.github/skills、~/.copilot/skills |

\* Claude Code は、自分の設定（`--setting-sources user,project,local`）を読むときだけ、CLAUDE.md とスキルを読みます。そのとき、設定のフックも、KairoCode の sandbox の外で動きます。

確かめたことは次のとおりです。

- `--settings` で渡したフックは、`--setting-sources ""` のときも動きました。
- `disableAllHooks` を付けると、`--settings` で渡したフックも止まりました。
- そのため、Claude Code に CLAUDE.md とスキルを自分で読ませると、利用者のフックを KairoCode の sandbox に入れる手立てがありません。

## 決定

1. **KairoCode の約束（どのエンジンでも同じ）**: モデルは、どのエンジンでも、次のものをちょうど 1 度ずつ受け取ります。
   - 指示: `~/.claude/CLAUDE.md`、`AGENTS.md`、`CLAUDE.md`、`.claude/CLAUDE.md`、`CLAUDE.local.md`
   - スキル: ADR 0002 の置き場所のすべて（`.claude`、`.agents`、`.opencode`、`.kairocode` と利用者のもの）
   - KairoCode のツール、権限、sandbox。コマンド（`/name`）、スキル（`/skill`）、フロー
   - フック（3）
2. **読み方: 任せられるものは任せ、足りないものは KairoCode が補う**（`internal/runtime`、エンジンごとの表 `nativeReads`）
   - API: KairoCode が全部を読みます。
   - Claude Code: 自分で読ませると、フックが KairoCode の sandbox の外で動きます（背景）。そのため、これまでどおり `--setting-sources ""` で自分では読ませず、KairoCode が全部を補います。スキルは、KairoCode の skill ツール（MCP）で使います。
   - codex: `AGENTS.md` と `.agents/skills`・`~/.agents/skills` は codex に任せます（ADR 0002 の追記の `project_doc_max_bytes = 0` と `skills.include_instructions = false` は外す）。KairoCode は、残り（CLAUDE.md の類、それ以外の置き場所のスキル）だけを補います。
   - copilot: `AGENTS.md`・`CLAUDE.md`（project）と、`.claude/skills`・`.agents/skills`（project）・`~/.agents/skills` は copilot に任せます（`--no-custom-instructions` を外し、copilot 自身のスキルのツールを `--available-tools` に足す）。KairoCode は残りを補います。
   - エージェントが自分のものとして足すもの（同梱のスキル、プラグイン、~/.codex/AGENTS.md など）は、そのエージェントの性質として残します。約束のものが欠けたり、二重になったりはしません。
3. **フックは、どのエンジンでも KairoCode が動かす**（ADR 0003 の KairoCode のフックに合わせる）
   - **読むもの:** Claude Code の書式のフック（`~/.claude/settings.json`、`.claude/settings.json`、`.claude/settings.local.json` の `hooks`）を、KairoCode が読みます。
   - **事象の対応:**
     - `SessionStart` → `session.start`
     - `UserPromptSubmit` → `turn.start`（新）
     - `PreToolUse` → `tool.before`（新）。KairoCode のツールを呼ぶ前に動き、終了コード 2 なら呼び出しを止め、stderr をモデルに返します。
     - `PostToolUse` → `tool.after`
     - `Stop` → `turn.end`
     - ほかの事象（Notification など）は、まだ動かしません。`/hooks` で、動かないことを示します。
   - **入力:** Claude Code と同じ JSON（`session_id`、`cwd`、`hook_event_name`、`tool_name`、`tool_input` など）を stdin に渡します。
   - **sandbox:** フックは、bash と同じ KairoCode の sandbox（作業ディレクトリに書ける、ネットワークなし）で動かします。
   - **sandbox の外:** フックごとに（コマンドの内容のハッシュで）、初めて動く前に利用者に聞きます。答えは「sandbox で動かす」「sandbox の外で動かす」「動かさない」の 3 つで、記録します。sandbox の外で動くのは、利用者がそう答えたものだけです。フックの中身が変われば、また聞きます。
   - **実行の記録:** フックの実行は KairoCode のアクションとして記録します（作用は実。落ちたら要確認、不変条件 4）。
   - **エージェント自身のフック:** 止めます。Claude Code は `--setting-sources ""`、codex は `features.hooks = false` です。copilot は、フックの仕組みがあるかを実装のときに確かめ、あれば止めます。
4. **品質の確かめ方**
   - エンジンごとの「自分で読むもの」の表をコードに持ち、「約束 − 自分で読むもの = KairoCode が補うもの」をテストで守ります。
   - `scripts/agent-probe.sh` を足します。手元の Claude Code、codex、copilot が実際に何を読むかを、モデルを呼ばずに確かめ（背景の表の確かめ方）、表と食い違えば知らせます。CLI を上げたときに流します。CLI が要るので `check.sh` には入れません。
5. **知られている差:** codex は `AGENTS.md` を根から作業ディレクトリまで重ねて読みます。KairoCode は作業ディレクトリのものだけを読みます。差は表に書いておきます。

## 結果

- どのエンジンでも、指示、スキル、フック、権限、sandbox が同じに効きます。エージェントの得意な読み方（codex と copilot のスキル）は、そのまま使えます。
- フックは、sandbox の外へ出るものを必ず利用者が承認します。Claude Code のフックも、Claude Code の外（API、codex、copilot）で動きます。
- テスト（実装のとき足す）
  - 補うものの表（エンジンごと）。
  - フックの事象の対応と、stdin の JSON。
  - sandbox の外の承認（聞く、答えを記録、中身が変わればまた聞く）。
  - `tool.before` の終了コード 2 で止まること。
  - フックの実行の crash のテスト。
  - 各エンジンでエージェント自身のフックが止まっていること。

## 検討した代替案

- **エージェントに何も読ませず、KairoCode が全部を渡す（今の形）:** 確実で揃いますが、利用者の「読み込みはエージェント側でよい」に合いません。また、codex と copilot のスキルの使い方を活かせません。Claude Code だけは、フックの理由でこの形を残します。
- **Claude Code にも自分で読ませ、フックはそのまま:** フックが KairoCode の sandbox の外で、承認なしに動きます。利用者の条件に合いません。
- **Claude Code のフックを `SHELL` で KairoCode の sandbox に通す:** Claude Code は、フックに `SHELL` も `CLAUDE_CODE_SHELL` も使わないことを確かめました。

## 実装（2026-10-21）

### 1. 足りないものを補う（`internal/runtime/agent.go` の `nativeReadsOf`）

- `agent.load` が、エンジンのエージェントが自分で読む指示とスキルの置き場所を除いて、システムの指示を作ります。記録はこれまでどおりです。
- codex: `AGENTS.md`、`~/.agents/skills` と各階層の `.agents/skills` は、codex に任せます。`CODEX_CONFIG` からは `project_doc_max_bytes = 0` と `skills.include_instructions = false` を外し、`features.hooks = false` を入れました。
- copilot: `AGENTS.md`・`CLAUDE.md` は、copilot に任せます（`--no-custom-instructions` を外した）。
- **決定 2 から変えたこと（copilot のスキル）:** copilot のスキルのツールの名前は、モデルを呼ばずには確かめられませんでした。そのため、`--available-tools` に足していません。copilot には、スキルを KairoCode が全部渡します（KairoCode の skill ツール）。copilot 自身が自分のスキルをモデルに並べるかどうかは、確かめられていません。並べるなら一覧が二重になります。`scripts/agent-probe.sh` と、本物での確かめの残りです。
- テスト: `TestEnginesGetWhatTheyDoNotRead`（エンジンごとの補うもの、codex の 1 ターンで渡るもの）、`TestCLIAgentsConfig`。
