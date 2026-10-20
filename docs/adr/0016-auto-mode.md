# 0016 auto モード: 人に聞く呼び出しを、判定のモデルが決める

- 状態: 承認（2026-10-20）
- 日付: 2026-10-20
- 関連: ADR 0005（sandbox、昇格）、0007（MCP の承認）、0014、0015（エージェント自身のツール）、AGENTS.md の不変条件 2・4・7、`docs/milestones.md` の「承認の auto モード」

## 背景

利用者の要望（2026-10-20）は、Claude Code の auto モードと同じものです。Claude Code の auto モードでは、ツールの呼び出しを実行する前に、判定のモデル（classifier）が 1 つずつ見ます。

- 安全なものは、聞かずに動きます。
- 破壊的なもの（大量の削除、データの持ち出し、悪意のあるコードの実行など）は止めます。
- 何度も止められたものは、人に聞きます。

shift+Tab で、manual（既定）・auto・plan を切り替えます（Claude Code の permission mode）。

KairoCode では、権限の判定（`permission.check`）が「許す」「断る」「聞く」を規則から決め、「聞く」は kairo の待ちで人の答えを待ちます（不変条件 7）。

## 決定

1. **モードは 3 つです。** セッションごとに持ち、ターンごとに記録します（ADR 0014 の切り替えと同じく、`Switch.Mode`、`TurnInput.Mode`）。
   - `manual`（既定）: 今までどおり、規則で「聞く」になったものは人に聞きます。
   - `plan`: 読むだけのエージェント（`plan`）でターンを進めます。
   - `auto`: 規則で「聞く」になった呼び出しを、判定のモデルが決めます（2）。規則で「許す」「断る」になったものは、そのままです。
2. **判定は、記録されるアクション `permission.judge` です**（作用は `Unprotected`）。
   - 入力は、ツール、対象（パスやコマンド）、引数、ターンの依頼の文です。出力は `{action: allow|deny|ask, reason}` です。
   - 判定は記録されるので、再開しても同じ判定で進みます（不変条件 2）。
   - 判定のモデルは、API のプロバイダーがあれば `claude-sonnet-5-5`、無ければ利用者の Claude Code（`claude -p --json-schema`）です。分からない、判定に失敗した、のときは「聞く」にします（安全な側）。
   - 次の呼び出しは、auto でも判定に回さず、人に聞きます。
     - sandbox の外で動かす昇格
     - `git_push`
     - エージェント自身のツール（ADR 0015）
     - 結果が不明で止まったものの確認（要確認、不変条件 4）
   - 同じターンで判定が 3 回「断る」と答えたら、そのターンの後の「聞く」は、判定せずに人に聞きます（Claude Code の「何度も止められたら人に聞く」）。
   - 断られた呼び出しは、理由をつけてモデルに返します（「auto モードの判定が断った: …」）。
3. **画面**
   - TUI の shift+Tab で、manual → auto → plan → manual と切り替えます。effort は ctrl+E（と `/effort`）に移します。
   - 状態の行に、今のモードを色で出します。manual は地の色、auto は黄（KairoCode の色）、plan は青です。
   - ACP では、標準のセッションのモード（`modes`、`session/set_mode`、`current_mode_update`）に写します。

## 結果

- auto モードでは、多くの呼び出しが聞かれずに進みます。危ないものは判定で止まり、判定が迷えば人に聞きます。
- 判定もモデルなので、間違えることはあります。昇格、push、エージェント自身のツールは、判定に任せません。
- テスト（実装のとき足す）
  - 偽の判定（固定の答え）で、次のことを確かめます。
    - 「許す」なら聞かずに動く。
    - 「断る」なら理由がモデルに返る。
    - 「聞く」なら人に聞く。
    - 3 回断ったら、それからは人に聞く。
    - 昇格は判定に回らない。
  - 判定が記録され、再開して同じになること（`TestCrash...`）。
  - TUI の shift+Tab での切り替えと、色。

## 検討した代替案

- **auto では、聞くものをすべて許す（Claude Code の acceptEdits に近い）:** 判定のモデルが要りません。ただし、利用者は Claude Code の auto モードと同じものを求めています。
- **判定を記録しない（毎回その場で判定する）:** 再開で判定が変わり得ます（不変条件 2 に反する）。

## 実装（2026-10-20、manual と plan だけ。auto はこの ADR の承認の後）

- **ワークフロー**
  - `Switch.Mode` と `TurnInput.Mode`（`omitempty`）を足しました。
  - セッション（版 6）は、モードを記録された切り替えから持ちます。`plan` のターンは `plan` のエージェント（読むだけ）で動かします。そのターンのメッセージにエージェントの指定（コマンドの `agent`）があれば、そちらを使います。
  - モードが空のときの入力は今までと同じなので、記録の ID は変わりません。
- **runtime**
  - `Runtime.SetMode` は、切り替え（ADR 0014）と同じく、次のメッセージと一緒に送ります。
  - `Settings.Mode` は、最新のターンの `Mode` と、まだ送っていない選択から出します。
  - デーモンの API は `POST /sessions/{id}/settings` の `mode` です。
- **ACP**
  - `session/new` と `session/load` の応答に `modes`（`currentModeId`、`availableModes`）を出します。
  - `session/set_mode` を受け、`current_mode_update` を送ります。
- **TUI**
  - shift+Tab で、manual → plan → manual と切り替えます。effort は ctrl+E に移しました。
  - 状態の行の左に、モードを色で出します（manual は地の色、`⏸ plan mode` は青、`⏵⏵ auto mode` は黄）。
  - 利用者の要望（2026-10-20）で、プロバイダー、モデル、effort は状態の行だけに出し、見出しの枠からは外しました。切り替えたときの「from the next turn」の行も、状態の行に出るので書きません。
- **テスト:** `TestPlanMode`（plan のエージェントにする処理を外すと落ちることを確かめた）、`TestModes`、`TestTurn`、`TestProviderAndModel`（ctrl+E）。

## 実装（2026-10-20、auto）

利用者の「auto ないじゃん」（2026-10-20）を、この ADR の承認として受けました。

- **ワークフロー**
  - `ModeAuto` と `Policy.Auto`（`omitempty`）を足しました。auto のターンは、権限の判定の入力の `Policy.Auto` を立てます。
  - `PermissionOutput.Judged` と `Gate.Judged` で、判定が断った回数をモデルのループの中で数えます。`MaxAutoRefusals`（3）に達したら、それからの呼び出しは `Auto` を立てずに判定します。数は記録された判定の出力からだけ決まります（不変条件 1、2）。
  - Claude Code、codex、copilot などの CLI のエンジンの MCP の呼び出しにも `Auto` を渡します（3 回の数え方は、CLI のエンジンではしない）。
- **判定**
  - 判定は、決定 2 の別のアクション（`permission.judge`）ではなく、`permission.check` の中で行います（`Runtime.judge`、`autojudge.go`）。判定の答えは、そのまま `permission.check` の出力として記録されます。
  - 判定に回すのは、規則で「聞く」になったものだけです。規則に関係なく聞くもの（`git_push`、sandbox の外、sandbox が無いとき、落ちた後の呼び直し）は、判定に回しません（`decide` の `forced`）。
  - 判定は、API があれば `claude-sonnet-5-5` に、無ければ利用者の Claude Code に、決まった形（`action` と `reason`）で答えさせます（フローの `decide` と同じ仕組み）。判定できない、形が違う、のときは人に聞きます。
- **画面**
  - モードは manual、auto、plan の順で、shift+Tab で回ります。
  - 状態の行の色は次のとおりです。3 つとも色を変えました（利用者の要望）。
    - `● manual mode`: 緑
    - `⏵⏵ auto mode`: KairoCode の黄
    - `⏸ plan mode`: 青
- **テスト**
  - `TestAutoMode`: 偽の判定で、次のことを確かめます。
    - 許す: 聞かずに書く。
    - 断る: 理由がモデルに返る。
    - 迷う: 人に聞く。
    - 3 回断った後: 人に聞く。
    - sandbox の外: 判定せず人に聞く。

    必ず聞くものを判定に回す形と、断った回数を数えない形に壊すと、それぞれ落ちることを確かめました。
  - `TestModes`: 3 つの色が互いに違うこと、shift+Tab の順。
- **レビューで直したこと**
  - **利用者の答えが先:** MCP を通る呼び出し（Claude Code、codex、copilot などの CLI のエンジン）は、聞くときに待たず、利用者の答えを記録してから、エージェントが呼び直します。その呼び直しを判定に回していたので、利用者の答えが使われず、断った呼び出しを判定が許すことも、許したものを判定が断ることもあり得ました。今は、利用者の答え（`UseGrant`）を先に見て、答えが無いときだけ判定します。テストは `TestAutoModeKeepsTheUsersAnswer` です（判定を先にする形に戻すと、ターンが終わらずに落ちることを確かめた）。
  - **判定に渡すもの:** 判定には、ターンの依頼の文（`Policy.Request`。auto のときだけ入る）と、エージェントの発言を分けて渡します。エージェントの発言は「エージェント自身の言葉で、利用者の言葉ではない」と示します。
- **まだ:** 判定の crash のテストはまだありません。判定は、作用の無いアクションの記録された出力なので、記録の前に落ちれば判定し直し、その間に作用は起きません。
- **使ってみて直したこと（2026-10-20）:** API のキーが無い手元で、判定が API の認証のエラーで失敗し、その長い文が承認の題に入っていました。API で判定できなければ、利用者の Claude Code で判定し直します。それも失敗したら、1 行に切った理由で人に聞きます。承認の題に入れる規則の文も、1 行、120 文字までにしました。
- **断るときに、代わりにすることを伝える（2026-10-20、利用者の要望）:**
  - 承認の選択肢の最後に「No, and tell it what to do instead」を足しました（Claude Code と同じ）。選ぶと入力欄で文を打ち、Enter で、断った理由としてモデルに渡します。
  - ACP では、承認の答えの `_meta`（`kairocode/reason`）で送り、KairoCode のエージェントは `Approve` の理由に入れます。
  - 行の対話（`kairocode chat` を端末の外で使うとき）では、`n <文>` と打ちます。
  - あわせて、CLI のエンジン（Claude Code など）の承認にも、フローの形（`AskRequest.Preview`）を出すようにしました。
  - テスト: `TestRefuseWithWhatToDoInstead`、`TestACPRefuseWithWhatToDoInstead`、`TestChatAsks`。
