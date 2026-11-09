# 0038 Answering requests from outside the machine: the notify flow reads the answer and gives it

- Status: Proposed
- Date: 2026-11-09
- Related: ADR 0034 (telling of requests outside), 0003 (flows), 0013 (`fetch`, `http.auth`), 0016 (auto mode), 0008 (the daemon), invariants 2, 4, 6 and 7, M10

## Context

ADR 0034 tells a person away from the machine that a turn needs them, through a flow bound to `approval.requested` (posting to Slack, say). It left answering from there for later: the person still has to come back and run `kairocode approve`.

The daemon runs on the user's machine, on a Unix socket. Nothing outside can reach it: Slack's buttons and slash commands need a public HTTPS endpoint, and a service cannot push into a laptop. The answer has to be fetched by the daemon. The ways to do that:

- **Ask the service from time to time** (Slack's `conversations.replies`, a GitHub issue's comments, a mailbox): any service with an API, from a flow, as ADR 0034 sends.
- **Hold a connection open to one service** (Slack's Socket Mode, a WebSocket): answers come at once, but only from that service. The daemon would hold a WebSocket (none in the standard library), Slack's app-level token, and code that is Slack's alone.

Invariant 7 says to wait with `kairo.WaitFor`, not by polling. The turn's wait for the person stays such a wait. What is new is learning the answer from a service that cannot tell us: that can only be asked, and between askings a durable sleep (`ctx.Sleep`, a kairo timer) lets go of the process. The daemon already stays up while a kairo timer is set (`Runtime.Awaiting`), so it does not exit while a flow waits to ask again.

Answering is a strong power: an approval lets a command run. Whoever can make the flow answer can approve anything that waits. A project's `kairocode.json` can bind hooks (ADR 0034), and even a hook bound in the user's own settings runs the project's `.kairocode/workflows/<name>.star`: a cloned repository's `notify.star` that answers "allow" at once would turn every approval off.

## Decision

1. **Two new primitives of flows**, for the flow a `notify` run (ADR 0034) runs:
   - `answer(value, reason = "")` answers the request this run was started for, and no other: no key is passed. `value` is `"allow"` or `"deny"` for an approval, `"yes"` (it took effect) or `"no"` for a review, an option for a question. It returns `.ok`; answered already (at the screen, by another flow) is `.ok == False` with `"answered already"`. The answer goes through the same path as `kairocode approve` / `answer` (`Approve`, `Resolve`, `Answer`), and is recorded with who gave it: `notify flow <name>`.
   - `sleep(seconds)` waits, durably (`ctx.Sleep`): the process is let go, a restart goes on from it, and the daemon stays up while it waits. At least 5 seconds.
   - Elsewhere (a flow run by `/flow`, another hook), both are errors: `answer` has no request there, and `sleep` is not needed there yet.
2. **Only the user's own flows may answer.** A hook's flow is now looked for first in the user's own folder, `$XDG_CONFIG_HOME/kairocode/workflows/<name>.star`, then in the project's `.kairocode/workflows` (where it is today), for every hook event; so one `notify.star` serves every project. `answer()` works only in a flow from the user's folder, bound by a hook in the user's own `kairocode.json`. A project's flow, or one bound by a project's settings, runs (it can tell) but its `answer()` is refused with "answering is for your own flows (~/.config/kairocode/workflows)". Binding alone is not enough: today a hook in the user's settings runs the project's script, which a cloned repository chooses. Where each hook and its script came from is recorded by `agent.load` with the hook (invariant 2). The script still has to be allowed first (ADR 0034, 4), and the question says when it can answer.
3. **The run ends when the request does.** When a request is answered or goes away (at the screen, with `kairocode approve`, the turn cancelled, the session closed), the runtime cancels its `notify` run, so a flow asking in a loop stops. A flow also bounds its loop itself (Starlark loops are bounded), and `LIMITS = {"wait": "24h"}` caps how long a `notify` run may go on (24 hours by default), after which it is cancelled. The request still waits; it can be answered at the machine.
4. **Who may answer is the flow's to check.** In Slack, anyone in the channel can reply. The example flow takes an answer only from the Slack user IDs it lists, and only as a reply in the thread of the message it posted. `docs/notify.md` says plainly that whoever the flow takes an answer from can approve what waits.
5. **Reading the answer, with no secret in the record (invariant 6).** `fetch` (GET, `Read`) with `http.auth` reads Slack's `conversations.replies` (the token added inside the action, ADR 0013). It is a read, so it needs no approval; the permission rules still apply.
6. **auto mode and plan mode are unchanged.** A request is told (and so can be answered from outside) only when it would ask a person anyway.

Where this shows in the implementation: `internal/workflows` (the `answer` and `sleep` builtins, only in a `notify` run; the `notify` workflow's `LIMITS`), `internal/flowfile` (the user's folder of flows, for hooks), `internal/runtime` (an `answer` action that answers the run's own request, refused for a flow that is not the user's; cancelling a `notify` run when its request is answered or gone; the hook's source in `agent.load`'s record), `docs/notify.md` (answering from Slack, with the allowed users), `internal/flowchart` (the new primitives in the chart and the checks).

## Consequences

- A person away from the machine can approve, deny or answer from Slack (or any service a flow can read), and the turn goes on. The answer arrives within the flow's asking interval (30 seconds in the example), not at once.
- The daemon stays up while a `notify` run sleeps between askings: at most `LIMITS.wait` after the request.
- Each asking is a `fetch` in the `notify` run's record. For a 30-second interval and a request answered in an hour, about 120 small reads.
- Workflow version: the `notify` workflow gains steps (`sleep`, `answer`), and the flow interpreter two builtins. The `notify` workflow's version is raised and the old one kept draining (kairo's ADR 0060).
- Tests (to add):
  - `TestNotifyAnswers`: a `notify` flow that answers "allow" lets the turn's call run; "deny" refuses it; a review answered "yes" resolves it; a question answered with an option goes on with it.
  - `TestNotifyAnswerOnlyItsOwn` and `TestNotifyAnswerNotFromProject`: no key can be passed; a project's flow cannot answer, whether a project's settings or the user's bind it; the user's flow of the same name is taken before the project's.
  - `TestNotifyCancelledWhenAnswered`: answered at the screen, the sleeping `notify` run is cancelled, and nothing is answered twice.
  - `TestCrashNotifySleep`: killed while the flow sleeps, the reopened daemon goes on from the sleep, and the answer is given once.
  - `answer` and `sleep` are refused outside a `notify` run.
  - The token of `http.auth` is not in the `notify` run's record when it reads replies.

## Alternatives considered

- **Slack's Socket Mode in the daemon.** Answers at once and with buttons, and no polling. But it is Slack's alone, needs a WebSocket client and an app-level token, puts a service's protocol in the daemon rather than in a flow, and still has to choose who may answer. It can come later as a plugin, using the same `answer` path.
- **A public endpoint (the daemon listening on a port behind a tunnel, for Slack's interactivity).** Buttons and no polling, but it opens the daemon to the internet: requests signed by Slack would have to be checked, the tunnel set up and kept, and the attack surface is the daemon's.
- **Answer with any key (`answer(key, value)`).** One flow could answer for many requests, but a flow could then approve requests it was not told of. Each run answers its own.
- **Let a project's flows answer once the user allowed their script, or once the user's settings bind them.** The user is asked about a hook at the start of a turn, with the script; but they may allow a script they did not read, and one line would approve every command. Answering stays with scripts in the user's own folder, which no repository writes.
- **`wait` for an outside event, pushed into kairo (M4).** The right shape for services that can push. With a local daemon nothing can push, so it does not answer this.
