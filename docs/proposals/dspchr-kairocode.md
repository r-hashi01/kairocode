# Draft ADR for dspchr: agents without a process also line up in the same sidebar

This draft writes stage 2 of KairoCode's ADR 0004 as a decision on dspchr's side. Create it in the dspchr repository with `scripts/new-adr`, adjust it to dspchr's style, and then use it. KairoCode's side does not edit dspchr.

---

# ADR-NNN: Agents without a process also line up in the same sidebar

dspchr has been built on "agents should feel like processes". What lines up is what is running inside a PTY, and its state is decided by looking at that process ([ADR-005](0005-Agent-状態は-CPU-time-delta-と画面ハッシュで推定する.md), [ADR-039](0039-Agent-が自分の状態を言えるようにする-ただし言わなくても動く.md)).

KairoCode (`../kairocode`) is outside this premise. It runs a session as a durable workflow (kairo), so **while it waits for approval, there is no process**. It continues even if approved hours later, from a different terminal. If it crashes, it carries on from where it was. As long as we look at processes, a waiting KairoCode "does not exist".

**Line them up.** There is no process, but we know what it is waiting for. Things waiting where you cannot see them are the ones that most need to be visible.

### The daemon becomes a client of KairoCode's server

In [ADR-042](0042-リモートのセッションは-同じサイドバーに並ぶ.md), the daemon became a client of another daemon and lined up the sessions over there in the same list. We take the same shape.

- The local KairoCode server writes its address and token to `~/.local/share/kairocode/server.json` (0600). If this exists, the daemon connects. If not, it does nothing.
- The daemon subscribes to the list of sessions and their events (SSE), and turns them into sidebar rows (in the shape of `ReadRows` / `RowList`).
- KairoCode states a row's state. We do not guess (the same as "if there is a report, do not guess" in [ADR-039](0039-Agent-が自分の状態を言えるようにする-ただし言わなくても動く.md)).

### Answer what is waiting, here

Waiting approvals and blocked (needs confirmation) calls (it is unknown whether the command that was running at the crash was run) are asked in **the central dialog** (in the shape of [ADR-092](0092-yesno-は真ん中の箱で訊く-下のバーは-打っているものと知らせのため.md)). The answer is sent to KairoCode's `POST /approvals/{key}` and `POST /reviews/{key}`. The default is `no`.

### Opening a pane starts the continuation

When you select a row with no process and open a pane, it starts `kairocode --session <ID>` (a kind of resume from [ADR-068](0068-opencode-は-id-ではなく-続き-で-resume-する.md)).

### Show the tree

KairoCode's subagents and workflows line up as a tree under their parent. The requirement §24 "do not spawn an Agent where the user does not know" is met by **the user being able to see** what KairoCode spawns. It is not dspchr that spawns them.

### Usage

KairoCode records the tokens. Do not read the transcript; show the value in the list's row (in the unit of [ADR-066](0066-コストはトークンで数える-ドルでは数えない.md), as is).

---

**Not yet confirmed (check before filing):**

- Whether `ReadRows` / `RowList` can represent a row with no process (a row with no PTY).
- The fields of KairoCode's server API. dspchr talks to KairoCode's own API (section 8.1 of KairoCode's design), not the opencode-compatible layer (KairoCode's ADR 0002). The list's fields (state, usage, the keys for approvals and blocked calls, the tree of children) are decided in KairoCode's M2.
