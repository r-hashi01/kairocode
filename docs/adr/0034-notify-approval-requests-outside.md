# 0034 Telling of approval requests outside the machine: an `approval.requested` hook, run by the daemon as a flow

- Status: Accepted (2026-11-06)
- Date: 2026-11-06
- Related: ADR 0003 (hooks and flows), 0013 (`http` and `http.auth`), 0025 (plugins and their credentials), 0032 (terminal notifications), 0008 (the daemon), invariants 2, 4, 6, 7 and 8, M7

## Context

A turn that needs a person stops until the person comes back to the screen. ADR 0032 tells a user at this machine (a terminal notification), but not one who is away from it. M7 asks for approval requests to reach Slack and the like: "add an approval request event to hooks and send it with a flow". Answering from outside is decided later.

What there is already:

- A person is needed in three ways: an approval (`Kind: "approval"`), a review of a real call whose outcome is not known ("did it take effect?", `"review"`, kairo's blocked), and a question (a flow's `ask`, whether to run a hook; `"question"`). `Runtime.Pending` lists all three, and the daemon's event hub emits `approval.requested`, `review.requested` and `question.requested` when one appears.
- Hooks (ADR 0003): `hooks` in `kairocode.json` binds flows to `turn.end`, `tool.after` and `session.start`. They are read and recorded by `agent.load` (invariant 2). A hook asks before it first runs, at the start of a turn that has it (`hook-trust`, kept with the script's hash), and only the allowed script runs.
- Sending: `http` (POST, `Real`) adds a secret header from an environment variable inside the action (`http.auth`, ADR 0013), so the token is not in the record. A plugin's MCP tools use credentials kept in the OS credential store (ADR 0025).

A review comes from kairo's blocked state: no workflow code runs when a step is blocked. So a hook run by the turn workflow, before its wait, would miss reviews.

## Decision

1. **The event.** `approval.requested` is a hook event: `{"hooks": {"approval.requested": ["notify"]}}`. It covers all three ways a person is needed (approvals, reviews, questions), told apart by `kind`. Like the other hooks, it is read by `agent.load` (user and project settings) and recorded with the turn.
2. **The daemon runs it, once per request.**
   - When a request appears (the event hub's `approval.requested`, `review.requested`, `question.requested`), the runtime starts a run of the `notify` workflow for it, named `kairo.WithID("n/<session ID>/<first 24 hex of sha256(request key)>")`. Starting it again (the event seen twice, another process) finds the same run, so a request is told once.
   - When the daemon opens, it starts the runs of the requests waiting in its sessions. A request that appeared just before a crash is told after it.
   - This adds a second kind of named root to the run ID rules (invariant 8): `n/…`, beside the session's `s/…`. Its name is made only from recorded values (the session ID and the request's key).
   - The `notify` workflow runs each bound flow as a child (`kairo.Child`, as a hook's flow runs now), in order, with the request as its input. It waits for nothing; the turn waiting for the person goes on waiting (invariant 7), whatever the notification does.
3. **Which hooks, from the record.** The hooks are those recorded by `agent.load` of the turn the request belongs to (its top turn, for a flow's agents). They go into the input of the `notify` run, so it branches only on recorded values (invariant 2). A settings change applies from the next turn on, as for the other hooks.
4. **Only allowed scripts run.** At the start of a turn, `approval.requested` hooks ask before they first run, like the others (the same `hook-trust` record and script hash). The `notify` run runs only a hook already allowed for that script; one not yet allowed (or refused) is skipped and the reason logged. It never asks: nobody is at the screen.
5. **The flow's input:** `{"kind", "key", "session", "title", "dir", "tool", "subject", "rule", "turn"}`. `subject` is what the approval shows (the command, the URL and body preview of `http`, the question). What leaves the machine is up to the flow: it is the user's (or the project's, allowed by the user) script.
6. **Sending, with no secret in the record (invariant 6).** A flow sends with the tools it has:
   - `http` to an API with a token in `http.auth`. For Slack: `chat.postMessage` with a bot token, `{"http": {"auth": {"slack.com": {"header": "Authorization", "env": "SLACK_BOT_TOKEN", "prefix": "Bearer "}}}}`.
   - Or a plugin's MCP tool.
   - An incoming webhook whose URL is itself the secret cannot be used without putting it in the record: not supported here. `docs/notify.md` says so, with the example above.
7. **No notifications of notifications.** A request that belongs to a `notify` run (one of its flows' calls waiting for approval or review) is not told. It shows in `kairocode approvals` like any other. For the calls to go out unasked, the user allows them (`"http": {"https://slack.com/api/*": "allow"}`); the example says so.
8. **A failed notification does not fail anything else.** A flow that fails, or a `notify` run that fails, is logged and kept in the flow run history (ADR 0029); the session is not told. The `http` call is `Real`: a crash while it is sent leaves it blocked for review (invariant 4), and that review is not told (7).

Where this shows in the implementation: `internal/workflows` (the `notify` workflow, `ApprovalRequested` in the hook events), `internal/runtime` (starting `notify` runs from the event hub and on opening, the input from the turn's `agent.load`, skipping not-yet-allowed hooks, leaving out requests under `n/`), `internal/api` (the input type), `docs/notify.md` (setting it up for Slack).

## Consequences

- A person away from the machine hears of each request once, from a flow they wrote or allowed. The turn does not wait on it.
- The run ID rules gain `n/<session>/<hash>`. Listing a session's runs (`kairocode log`, flow runs) does not see them under the session; the flow run history does.
- Tests:
  - `TestApprovalRequestedHookRuns`: an approval, a question and a review each start one `notify` run, and the flow gets the request.
  - `TestApprovalRequestedOncePerRequest`: the event seen twice and the daemon reopened start no second run.
  - `TestCrashApprovalRequested`: killed after the request appears and before the run starts, the reopened daemon tells it once.
  - `TestApprovalRequestedNotAllowedHookSkipped`: a hook not yet allowed does not run.
  - `TestNoNotifyOfNotify`: a `notify` flow's own call waiting for approval is not told.
  - The secret of `http.auth` is not in the `notify` run's record.
- Answering from outside (approving from Slack) is not covered. It is decided separately later.

## Alternatives considered

- **Run the hook in the turn workflow before it waits.** It is in the turn's record, with no new root. But reviews (kairo's blocked) run no workflow code, so they would not be told, and the wait would start only after the notification is sent.
- **Run it as a turn of the session.** The session runs one turn at a time, and the turn waiting for the person is that one: the notification would wait behind it.
- **Let the daemon send directly (a webhook URL in settings), with no flow.** Simple, but it is not "sent with a flow" (M7). What is sent and where could not be changed without changing KairoCode, and a webhook URL is the secret itself.
- **Use Claude Code's `Notification` hook (ADR 0018).** It is a command hook in Claude Code's format, run in the sandbox (no network). It would need escalation for every notification, and it is not a flow.
