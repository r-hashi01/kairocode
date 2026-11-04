# 0012 Start from GitHub, and hold no process while waiting (serverless)

- Status: Proposed
- Date: 2026-10-19
- Related: requirements "CI and automation", NFR-4, NFR-6, design sections 1, 8 and 10 (M4), ADR 0003 (`wait`), 0005 (sandbox), 0006 (saving the conversation), 0008 (daemon), kairo's ADR 0052 and 0053 (HTTP, suspend mode)

## Context

M4's acceptance criteria are "start from an Issue comment, and no process remains while waiting for approval and the CI result". Until now, KairoCode's local daemon (ADR 0008) opens SQLite and keeps running in wait mode. For serverless, we need to decide the following.

1. Where and how events (Issue comments, approvals, CI results) arrive.
2. With no process while waiting, how to carry the working directory (the files the agent is editing) over to the next start.
3. To which named wait approvals and CI results are delivered.
4. The store (PostgreSQL), and who wakes up clock waits (the deadline of `wait`).

kairo 0.4.1 has suspend mode (`kairo.Suspend`), `/tick` in `k.Handler()`, `Options.Wake`, `TickHandler`, and a PostgreSQL store (`NewSQLStore`, `pgnotify`).

## Decision

1. **The entry point is `kairocode github` (an HTTP handler)** (`internal/github`, `cmd/kairocode`).
   - Receive GitHub webhooks (`issue_comment`, `check_suite`/`check_run`, `workflow_run`) at `POST /github`. Verify the signature (`X-Hub-Signature-256`) with a secret from an environment variable. If it does not match, do nothing.
   - Open kairo in suspend mode and mount `k.Handler()` on the same server (`/tick`; `TickSecret` from an environment variable).
   - Within one request, turn the event into a signal, advance as far as `k.Tick` can, and return. The process may end at the host's convenience (Cloud Run, Fly, Lambda and so on).
   - For clock waits (the `timeout` of `wait`, approval deadlines), an outside scheduler wakes `/tick` at the time `Options.Wake` reports. Just calling `/tick` from a cron every minute also works (it returns right away if there is nothing to do).
   - As a one-shot form with the same content, also provide `kairocode github -event <file>` (from GitHub Actions: pass the event JSON, advance once, and exit).
2. **An Issue comment `/kairo <request>` starts a session.**
   - The session ID is `s/gh/<owner>/<repo>/<Issue number>/<comment ID>` (invariant 8; even if the same webhook arrives twice, it is the same session).
   - Who can start it is decided by write permission on the repository (`author_association` is `OWNER`, `MEMBER` or `COLLABORATOR`). Other comments are ignored.
3. **The working directory is carried over on the session's branch `kairocode/<Issue number>-<comment ID>`.**
   - At the start of a request, if the working directory (`<data directory>/work/<session>`) does not exist, clone that branch (or the default branch if it does not exist).
   - At the end of a request (when `Tick` returns and no step is running), if the working directory has changes, commit and push them to that branch. This is not a model tool but infrastructure processing that stands in for a disk, so it does not ask for approval.
   - A step finishes within one request (lease). So if we carry over at request boundaries, files and the step record do not disagree.
   - **Limit:** If the process is killed in the middle of a request, changes after the last push are lost and disagree with the record (edits recorded as written). Writing tools compare against the "hash before writing" before they write, so the next edit comes back to the model as a conflict (it does not write twice or silently overwrite).
   - CI runs on pushes to this branch.
4. **Approvals and events are delivered by comments and webhooks.**
   - When it starts waiting for an approval, it comments on the Issue: "Answer with `/kairo approve <key>` or `/kairo deny <key>`". Blocked (needs confirmation) (`blocked`) works the same way, answered with `/kairo ran <key>` or `/kairo notran <key>`.
   - A comment puts the key in a marker in its body (`<!-- kairocode:<key> -->`), and is not written if a comment with the same marker exists (it does not write twice).
   - Answer comments are accepted only from people with the same permission as for starting. They are passed to the current `Runtime.Approve` and `Resolve`.
   - A flow's `wait(event, timeout=)` (ADR 0003) waits on the signal `event/<name>` with `kairo.WaitFor` (`timeout` is `kairo.WaitTimeout`). The returned value is `{"ok": bool, "value": <event content>, "timed_out": bool}`.
   - Completion of `check_suite` and `workflow_run` is sent as the event `ci.result` (content: `conclusion`, `url`, `head_sha`) to the run in that branch's session that is waiting on `event/ci.result`. If no run is waiting, it is dropped.
   - Locally too, `kairocode event <session> <name> <JSON>` can send an event (for tests and checking by hand).
5. **If `KAIROCODE_DB` starts with `postgres://`, the store is PostgreSQL.**
   - kairo's store is `kairo.NewSQLStore(db, kairo.Postgres)`. Tables such as `kc_message` are created with the same columns in PostgreSQL types (the shape stays as in ADR 0006).
   - The driver is `github.com/jackc/pgx/v5` (`stdlib`) (an allowed dependency).
   - The local daemon stays SQLite and wait mode, as before.
6. **Secrets are read from environment variables inside actions and infrastructure processing.**
   - This covers the GitHub token (an App installation token or `GITHUB_TOKEN`), the webhook secret, `TickSecret`, and the model API key.
   - They are not put in records, comments or logs (invariant 6).
7. **In serverless, the default for tool approvals that ask a person uses the same rules as locally.**
   - The sandbox is the host OS's (bubblewrap on Linux), and if there is none it fails as in ADR 0005.

## Consequences

- It starts from an Issue comment, and no process is needed while waiting for approval and CI. Waits live in the DB, and it runs only when an event (a comment, a webhook, `/tick`) arrives.
- The working directory stays on a branch, so people can also see the state midway.
- If it is killed in the middle of a request, changes after the last push are lost. It falls to the safe side (a conflict), but the work has to be redone.
- Tests (added at implementation)
  - Run the acceptance criteria through to the end with a fake GitHub (an `httptest` server), a fake model, and SQLite in suspend mode.
    - Flow: `/kairo <request>` → approval comment → `/kairo approve` → push → `ci.result` → done.
    - Check that between requests no process (the one that opened kairo) remains (open and close per request).
  - Webhooks with a mismatched signature and comments from people without permission are ignored.
  - Even if the same webhook arrives twice, there is one session and one comment.
  - Kill the process in the middle of a request, and the next request does not redo finished steps (`TestCrash...`).
  - PostgreSQL tests run only when `KAIROCODE_TEST_PG` is set (the network-free tests go through the same flow with SQLite).

## Alternatives considered

- **Run with GitHub Actions only (no webhook server):** The only infrastructure is PostgreSQL. But an Actions job starts for every event (slow, billed by the minute), and deadline `/tick`s also need cron Actions. The one-shot `-event` form makes this usage possible too.
- **Keep the working directory in the DB as a snapshot (the tree of `snapshot` in design section 7):** No push needed. But CI needs a branch, so we push anyway. A branch is also what lets people see the state midway.
- **Push on every step:** Nothing is lost when killed. But there are many pushes, it gets slow, and the push itself becomes a real effect.
- **Take approvals through GitHub PR reviews:** Approvals are needed even at a stage with no PR (only an Issue), so we use comments. PR reviews can be added later as an event of `wait("github.pr_review")`.
