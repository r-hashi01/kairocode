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

### 2. フックは、どのエンジンでも KairoCode が動かす（`internal/workflows/hooks.go`、`internal/runtime/commandhooks.go`）

- **読む:** `claudeHooks` が、`~/.claude/settings.json`、`.claude/settings.json`、`.claude/settings.local.json` の `type: "command"` のフックを読みます。`agent.load` のフックの一覧に足し、記録します。読めないファイルは外します。
- **聞くとき:** 聞くのは、ターンの初め、どのフックも動く前です（`decideHooks`）。
  - `hook.check` が、そのフォルダーで答えが出ているかを見ます。出ていないものは、フローの質問と同じ仕組みで聞きます（`Kind: question`。答えは「Run it in KairoCode's sandbox」「Run it outside the sandbox」「Do not run it」）。答えを `hook.decide` で残します。
  - 答えは、フォルダー、Claude Code の事象、コマンドの組で、ストアに 1 度だけ書きます。コマンドが変われば、また聞きます。
  - 初めに聞くので、Claude Code など CLI のエンジンの MCP の呼び出しの途中で、待つことはありません。
  - このため、「初めて動く前に聞く」は、正確には「そのフックがあるターンの初めに聞く」です（決定 3）。
- **動かす:** `hook.command`（作用は実）が `sh -c` で動かします。
  - sandbox は bash と同じで、作業ディレクトリに書け、ネットワークは無しです。「outside」と答えたフックだけ、sandbox の外で動かします。
  - stdin には、Claude Code と同じ JSON（`session_id`、`cwd`、`hook_event_name`、`tool_name`、`tool_input`、`tool_response`、`prompt`）を渡します。環境変数 `CLAUDE_PROJECT_DIR` も渡します。
  - 待つのは `timeout`（既定 60 秒）までです。
  - sandbox の外は、bash の昇格と同じ `sandbox.FullAccess` で動かします。不変条件の検査で `FullAccess` を使ってよいファイル（`fullAccessUsers`）に、`internal/runtime/commandhooks.go` を足しました（この ADR の決定 3 による）。
  - 動いている途中で落ちたら、もう一度は動かしません。「効いたか」を利用者に聞き（要確認、不変条件 4）、答えたら、そのフックは止めなかったものとしてターンを進めます。
- **事象と終了コード**（Claude Code と同じ）

  | 事象 | 終了 0 | 終了 2 |
  |---|---|---|
  | SessionStart（session.start） | stdout を最初の依頼に足す | — |
  | UserPromptSubmit（turn.start） | stdout を依頼に足す | ターンを、モデルに聞かずに「[A hook stopped this request: …]」で終える |
  | PreToolUse（tool.before） | — | 呼び出しを止め、stderr をモデルに返す |
  | PostToolUse（tool.after） | — | stderr を結果に添える（MCP の呼び出しは `hook.note` で） |
  | Stop（turn.end） | — | stderr でモデルを送り返す（3 回まで） |

  - ほかの事象（Notification など）は、まだ動かしません。
  - matcher は、workflows で regexp が使えないので、Claude Code で使われる形に限ります。名前、`|` でつないだもの、`*`、`.*` で終わる前方一致です。
- **エンジン**
  - API: ターンの中で、すべての事象を動かします。
  - CLI のエンジン: session.start と turn.start をターンの初めに、tool.before と tool.after を MCP の呼び出し（`RunMCPCall`、`ClaudeTurnInput.Hooks` → `MCPCallInput.Hooks`）で、turn.end を最後の回の後に動かします。
  - フローのエージェントとサブエージェント（`Sub` のターン）では、これまでのフローのフックと同じく、動かしません。セッションのターンは、エージェントがフローで動くとき（`workflow`）も、session.start と turn.start を動かします。
- **エージェント自身のフック:**
  - Claude Code: `--setting-sources ""` で動きません（`scripts/agent-probe.sh` で確かめた）。
  - codex: `CODEX_CONFIG` の `features.hooks = false` で止めます。
  - copilot: **止められていない（既知の穴）。**
    - copilot には、自分のフックがあります（`copilot help config`）。利用者の設定（`~/.copilot/settings.json` の `hooks`）と、copilot が信頼したフォルダーの `.github/hooks/*.json` です。
    - 止める設定は `disableAllHooks` ですが、1 回の起動だけに効かせる旗も環境変数もありません。copilot の利用者の設定ファイルに書くしかありません。
    - KairoCode は利用者の copilot の設定を勝手に書き換えないので、copilot のエンジンでは、copilot 自身のフックが KairoCode の sandbox の外で、承認なしに動き得ます。
    - 利用者に、`disableAllHooks` を書くかを聞きました（2026-10-21）。答えと、その後の形は 4 節です。
- **durability-reviewer の指摘で直したこと**
  - **取り消しで止まったフックを、終わったことにしない:** プロセスを止めるときなどの取り消しで止まったフックは、終了コードではなく「結果が不明」（`kairo.Unknown`）として返し、要確認にします。前は、殺されたことを終了コード -1 として返していたので、PreToolUse のフックが止まっても呼び出しが進み得ました。
  - **やり直してよいのは、始まらなかったときだけ:** 始まった後の失敗は「結果が不明」とします。
  - **codex の設定:** 利用者の `CODEX_CONFIG` が JSON でないとき（codex-acp も読めない）、KairoCode の設定だけを渡し、codex 自身のフックを止めます。前は、そのまま渡していたので、フックが止まりませんでした。
  - **MCP の呼び出しの要確認の文:** 落ちて途切れたものがフックのこともあるので、「この呼び出し（か、その周りのフック）」としました。
  - **まだのテスト:**
    - MCP の呼び出しの中で途切れたフックの crash のテスト
    - sandbox の中に閉じ込められることのテスト
    - 落ちたプロセスの子として残ったフック（sleep など）は、最後まで動き続けます（bash と同じ）。
- **版を上げない理由:** 新しい呼び出しは、`agent.load` が記録したフックにコマンドのフックがあるときだけ起きます。この変更の前の記録には無いので、再開しても同じ呼び出しの列です。
- **テスト**
  - `TestClaudeHooksRunHere`（API で 4 つの事象、聞くのは 1 度、Notification は動かない）
  - `TestClaudeHookNotRun`（動かさない、コマンドが変われば聞き直す）
  - `TestClaudeHooksOnClaudeCode`（MCP の呼び出しの PreToolUse）
  - `TestCrashHookRun`（落ちても動かし直さず聞く。作用を `Unprotected` に変えると落ちることを確かめた）

### 3. 確かめるスクリプト（`scripts/agent-probe.sh`）

- 手元の Claude Code、codex、copilot が自分で読むものと、動かすフックを、モデルを呼ばずに確かめます。KairoCode が当てにしていること（`nativeReadsOf`）と食い違えば、失敗で終わります。
- 2026-10-21 の結果は、Claude Code 2.1.305、codex 0.163.0、copilot 1.0.101 で、22 項目すべて一致でした。
- CLI が要るので、`check.sh` には入れません。CLI を上げたときに流します。

### 4. エージェント自身のフックを止める・許す（2026-10-21 追記）

- 利用者の答え: copilot は `disableAllHooks` で止めてよい。ただし、KairoCode の設定で解除できるようにする。
- **既定（止める）**
  - copilot を起動するたびに（`acpArgv`）、copilot の設定（`$COPILOT_HOME`、なければ `~/.copilot` の `settings.json`）に `"disableAllHooks": true` を書きます。ほかの項目は残し、ファイルの権限も保ちます。一時ファイルに書いて置き換えるので、途中の形は残りません。
  - 書いたことを、同じディレクトリーの `kairocode-disabled-hooks` に印として残します。印を先に書くので、間で落ちても、解除のときに取りすぎることはありません。
  - 利用者が自分で `disableAllHooks` を書いていたら、何もせず、印も残しません。
  - 設定が JSON として読めないときは、止められないので copilot を起動せず、直すか下の設定を使うよう伝えます。
  - テスト（`KAIROCODE_NO_CLIS`）では、`Config.CopilotHome` を渡さなければ何も書きません。
- **解除（`agents.ownHooks`）**
  - 利用者の `kairocode.json`（`~/.config/kairocode/`）に `{"agents": {"ownHooks": true}}` と書くと、エージェント自身のフックを許します。
    - codex: `features.hooks = false` を足しません（利用者の `CODEX_CONFIG` のまま）。
    - copilot: 印があれば、KairoCode が書いた `disableAllHooks` を消し、印も消します。印がなければ（利用者が書いたもの）触りません。
    - Claude Code: 変わりません。Claude Code の書式のフックは、どのエンジンでも KairoCode が sandbox で動かしているからです。
  - 許したフックは、KairoCode の sandbox の外で、承認なしに動きます。利用者が自分で選ぶ設定です。
  - **プロジェクトの `kairocode.json` では効きません。** clone したリポジトリが、sandbox の外で動くフックを有効にできないようにするためです。
- **durability-reviewer の指摘で直したこと**
  - `settings.json` がシンボリックリンク（dotfiles の管理）なら、リンクを残して、その先に書きます。書いた後に fsync します。
  - 解除のときに設定が読めなければ、copilot を止めず、印だけを消します。
  - ホームディレクトリーがわからなければ、相対パスに書かず、書けないことをログに残します。
  - ACP のレジストリーの copilot が自分の `COPILOT_HOME` を持つなら、そこに書きます。
- **残る弱さ（受け入れたもの）**
  - copilot の設定にはロックがないので、KairoCode が読んで書く間に、ほかのプロセス（copilot、利用者）が書いた変更は失われ得ます。
  - 印は「KairoCode が書いた」としか言えません。印を書いた後、`disableAllHooks` を書く前に落ち、その後で利用者が自分で書いた場合などは、解除のときに利用者の値も消します。
- **copilot のリポジトリのフック（確かめた。6 節）:** `disableAllHooks` では止まりませんでした。KairoCode は、そのリポジトリで copilot を動かしません。
- **テスト:** `TestAgentsOwnHooks`（ほかの項目と権限が残る、プロジェクトの設定では効かない、解除で KairoCode の書いたものだけを消す、利用者の書いたものは残す、読めない設定では起動しない、解除のときは読めなくても起動する、シンボリックリンクを残す、codex の `features.hooks`）

### 5. 本物のモデルで確かめたこと（2026-10-21 追記）

- 利用者の許可を得て、本物のモデルで確かめました（codex-acp と codex 0.163、copilot 1.0.101。KairoCode の `run` を、別のデータの置き場で動かした）。
- **codex は、codex-acp を通しても `.agents/skills` を自分で読みます。** プロジェクトの `.agents/skills` に置いたスキルが、codex 自身の一覧に出て、中身も使えました。
- **codex で、同じ名前のスキルが二重に並んでいた（直した）。**
  - 原因は、codex 自身のスキル（`~/.codex/skills`、同梱の `~/.codex/skills/.system`）と同じ名前のスキルが `~/.claude/skills` にもあり、KairoCode がそれも渡していたことです。
  - 直し方は、codex が自分で持つスキルと同じ名前のスキルを、KairoCode から渡さないことです（`nativeReads.ownSkillDirs`）。codex のものが代わりになります。
    - 対象は `$CODEX_HOME/skills`（既定は `~/.codex/skills`。下の `.system` も含めて、下へたどる）と `/etc/codex/skills` です。`CODEX_HOME` は、ACP のレジストリーが codex に渡す環境を先に見ます。
    - 名前は、ディレクトリーではなく `SKILL.md` の `name` で比べます。読めない codex のスキルは codex のものと数えません（悪くても二重に並ぶだけで、消えはしない）。
    - プロジェクトのスキルは、同じ名前でも渡します。プロジェクトのものが先です。
  - 直した後に確かめ直し、重なりがなくなりました。
- **copilot では、スキルの一覧は KairoCode の 1 つだけでした。** copilot 自身のスキルのツールは止まっていて（`Disabled tools: … skill`）、copilot 自身の一覧はモデルに並びません。二重にはなっていません。
- **テスト:** `TestEnginesGetWhatTheyDoNotRead` に、codex 自身のスキル（`~/.codex/skills` と `.system`）と同じ名前のスキルと、同じ名前のプロジェクトのスキルを足しました。`/etc/codex/skills` はテストでは別の場所にします。

### 6. copilot のリポジトリのフックと、残りのテスト（2026-10-21 追記）

- **確かめたこと（copilot 1.0.101、ACP。モデルの小さな呼び出しを 1 回ずつ）**
  - 使い捨ての `COPILOT_HOME` で、利用者の設定・リポジトリの `.github/hooks/*.json`・`.github/copilot/settings.json` に、それぞれ印のファイルを作る `SessionStart` のフックを置きました。フックは最初の prompt で動きます（`session/new` では動かない）。
  - 利用者の設定に `"disableAllHooks": true` を書くと、利用者のフックは止まります。**`.github/hooks/*.json` のフックは止まりません。** リポジトリの設定の `false` の有無、`settings.local.json` も関係しませんでした。
  - copilot が信頼していないフォルダーでも、ACP では `.github/hooks/*.json` のフックが動きました（`copilot help` は、信頼したフォルダーでだけ読むと書いている）。
  - リポジトリの `settings.json` の `hooks` は、動きませんでした。
  - 1 回の起動だけでリポジトリのフックを止める旗や環境変数は、見つかりませんでした。
- **決定:** 作業ディレクトリーのリポジトリ（根から作業ディレクトリーまで）に copilot のフックの定義があれば、KairoCode は copilot を起動しません（`copilotRepoHooks`）。ターンには、理由と、別のプロバイダーを使うか `agents.ownHooks` で許すかを返します。
  - 定義とみなすのは、`.github/hooks/*.json`、`.github/copilot/settings.json` と `settings.local.json` の `hooks` です。読めない設定も、copilot は読めるかもしれないので、定義があるとみなします。
  - 利用者が `agents.ownHooks` を許しているときは、起動します。
  - 本物の copilot で、フックのあるリポジトリが拒まれる（モデルを呼ばない）ことを確かめました。
- **足したテスト**
  - `TestCopilotRepoHooks`（フックの定義の 4 つの形、定義のないリポジトリ、`agents.ownHooks`、codex には効かない）
  - `TestCrashHookOnMCPCall`（Claude Code の MCP の呼び出しの PreToolUse のフックの途中で落ちる。再開しても動かし直さず、人に聞く。フックは 1 回。フックの作用を `Unprotected` にすると落ちることを確かめた）
  - `TestClaudeHookSandboxed`（sandbox で動かしたフックはプロジェクトの外に書けず、外で動かすと答えたものは書ける。sandbox を `FullAccess` にすると落ちることを確かめた）
- **durability-reviewer の指摘で直したこと**
  - **sandbox から copilot のフックを書かせない:** sandbox の中のコマンドが `.github/hooks` を書けると、`agents.ownHooks` を許した利用者では、次の copilot のターンがそれを sandbox の外で動かします。`sandbox.Protected` に `.github/hooks` と `.github/copilot` を足しました（ADR 0005 の守る名前。`.claude` と同じ理由）。
    - `.github` のほかの場所（`workflows` など）は、これまでどおり書けます。
    - write と edit も同じく拒みます（`internal/tools` の `protected` を、入れ子のパスにも合うようにした）。スナップショットにも入りません。
    - Linux では、ほかの守る名前と同じく、まだ無いものは作れてしまいます（ADR 0005 の既知の弱さ）。その場合も、`agents.ownHooks` を許していなければ、次のターンで copilot を起動しません。
  - **読めない `.github/hooks` は、定義があるとみなす:** 前は、一覧を取れないと「定義なし」になっていました。
  - 認証のために copilot を起動するとき（prompt を送らない）も、作業ディレクトリーを一時ディレクトリーとして渡し、同じ検査を通します。
  - テストが殺されて残る一時フォルダーを `.gitignore` に足しました。
  - テスト: `TestSandboxProtectsGitAndConfig`、`TestWriteRefusesProtectedNames` に、`.github/hooks`・`.github/copilot` と、書けるままの `.github/workflows` を足しました。
- **確かめていないこと:** copilot が、リポジトリの外や根より上の `.github` を読むか。KairoCode は、根（`.git` のあるところ）から作業ディレクトリーまでを見ます。
- 2 節の「まだのテスト」のうち、上の 2 つは済みました。落ちたプロセスの子として残ったフックが最後まで動くことは、受け入れたままです。
