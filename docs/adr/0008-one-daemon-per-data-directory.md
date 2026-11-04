# 0008 Put exactly one daemon that runs kairo per data directory. Everything else is a client

- Status: Accepted (2026-10-09). Decision 1 of the addendum (replacing the version) is superseded by 0019
- Date: 2026-10-09
- Related: requirements FR-11, FR-20, NFR-3, NFR-4; design section 8; ADR 0002 (ACP), 0004 (dspchr, `server.json`), 0007 (the Claude Code engine); invariant 7 in AGENTS.md; kairo's gaps #15, #19

## Context

In M1, we made it possible to give an approval from another process (`TestApprovalFromAnotherProcess`). But with SQLite, there is no way to be told about what another process did (a signal, the end of a run). kairo notices by rereading waiting runs in its sweep every lease period (30 seconds by default) (r-hashi01/kairo#8). Communication between live processes is in effect polling. The user decided: "I want to avoid polling as much as possible. I want it event-driven" (2026-10-09).

The sweep has two roles.

- **Picking up what a dead process left behind:** a dead process cannot tell anyone anything, so the only way is to pick it up by time. This stays as the mechanism for recovering from failures.
- **Communication between live processes:** this should really be conveyed by events. Now it relies on the sweep, because several processes run the same DB.

On top of that, with ACP (ADR 0002), an editor starts `kairocode acp` every time it opens. As things are, the number of processes running the same DB would grow, and the problem above would happen all the time.

## Decision

- **Put exactly one process (a daemon) that runs kairo per data directory (`~/.local/share/kairocode`).**
  - This daemon holds the kairo runtime, the MCP server (ADR 0007) and the HTTP API.
  - A client starts it automatically the first time it is needed.
  - The daemon writes `server.json` (permissions 0600; the address and token) and a lock file in the data directory. The lock keeps a second daemon from starting.
- **Everything else is a client.**
  - `kairocode acp` (started by an editor), `kairocode run`, `approvals` and `approve` connect to the daemon. They do not open the DB themselves.
  - Between a client and the daemon is HTTP over a local Unix socket (the API of design section 8.1). The stream of events is SSE.
- **With this, all communication between live processes becomes events inside the daemon.**
  - An approval is a `Signal` inside the daemon. A waiting `WaitFor` wakes up on the spot.
  - The end of a turn reaches the waiting client through an observation inside the daemon (`Options.Observe`).
  - There is no polling.
- **When the daemon is not running:**
  - A client first starts the daemon. Only when it cannot be started (read-only operations and so on) does it open the DB directly.
  - No process is waiting for a signal written directly to the DB. The next time the daemon starts, it continues from there.
- **The sweep's only role is picking up what dead processes left behind.**
  - When the daemon crashes and restarts, it picks up what was left. It opens with the same name (Owner), so it can take over without waiting for the lease period.
- **The daemon's lifetime:** it ends a fixed time (10 minutes by default) after there are no running sessions and no connected clients. A session that is only waiting for an approval holds no process (NFR-4).

## Consequences

- Approvals, the end of turns and streaming all arrive as events.
- `kairocode acp` becomes a thin relay, even when started per editor. The same session can be continued from another editor.
- Stage 2 of ADR 0004 (dspchr connects to KairoCode's server) becomes a connection to this daemon.
- Tests:
  - A second daemon does not start.
  - A client starts the daemon automatically.
  - An approval given from another client lets the waiting turn continue. We confirm it continues in a time well shorter than kairo's sweep clock (the lease period). The lease period is left at the default (30 seconds) to confirm that it did not continue because of the sweep.
  - When the daemon is SIGKILLed and restarted, the turn continues.
- It is implemented at the start of M2 (together with ACP's entry point). M1's `TestApprovalFromAnotherProcess` is rewritten then into the client-and-daemon shape.

### Implementation (2026-10-09, the start of M2)

- It is in `internal/daemon`. The tests are the four written in "Consequences" plus stopping when idle (`TestSecondDaemonDoesNotStart`, `TestClientStartsTheDaemon`, `TestApprovalFromAnotherClient`, `TestCrashDaemonRestart`, `TestIdleDaemonStops`). M1's `TestApprovalFromAnotherProcess` was replaced with `TestApprovalFromAnotherClient`.
- From an approval to the turn continuing takes a few milliseconds (when two processes ran the same DB, it waited for the sweep). We confirmed that if the daemon's Owner changes every time, `TestCrashDaemonRestart` waits for the lease period (30 seconds) and fails.
- "Idle" is decided from kairo's observations (the start and end of steps) and the number of HTTP connections. There is no polling. It does not stop while a client is waiting (`TestDaemonStaysWhileAClientWaits`).
  - A limit: time spent waiting on kairo's timers (the interval between `Retryable` retries, a later `ctx.Sleep`) is not counted. With no clients, the daemon may stop partway through that. That turn is not lost; it continues the next time the daemon starts.
- If the socket path is too long (about 104 bytes on macOS), it is put in a directory only the user can enter, under a name made from the hash of the data directory. That is `$XDG_RUNTIME_DIR` if it exists, otherwise `kairocode-<uid>` in the temporary directory (the owner and 0700 are checked, because on Linux `/tmp` is shared). Every request needs the token.
- A client takes `start.lock` before starting the daemon. Other clients that come at the same time wait on the lock and connect to the daemon that was started. That the daemon is ready is received as one line on stdout. When the client's ctx is cancelled (Ctrl-C), it stops waiting for both the lock and the start.
- A started daemon, if the previous daemon holds the lock while on its way out (on the way to stopping when idle, right after a SIGKILL), waits up to 10 seconds for the lock to be released (it waits with the kernel's flock; `TestDaemonWaitsForTheOneOnItsWayOut`).
- The daemon is KairoCode's own process, so it is started outside ADR 0005's sandbox. `internal/daemon/spawn.go` was added to the check (`internal/invariants`) as a file allowed to start it.
- The daemon's Owner is a fixed name. kairo expires all leases with the same name at `Open`, so if two processes open the same DB with this name, running steps run twice. For now the lock limits it to one. When "open the DB directly" is added, or when PostgreSQL is used across several hosts, the names will be separated.
- "Only when it cannot be started does it open the DB directly" in the Decision is not in yet. For now every command connects to the daemon.
- The daemon's configuration (engine, model) is taken from the environment variables at the time it started. After changing the configuration, wait for the daemon to stop, or stop it and then use it. → In the addendum below, the engine and the model became per session.

### Addendum (decided on 2026-10-09, revised on 2026-10-15 after review): what was added from comparing with opencode and Codex

We showed the user the results of comparing with Codex's daemon (the daemon of `codex app-server`), the user decided "let's put them in" (2026-10-09), and the following four went into M2.

1. **Compare the versions of the client and the daemon.** The daemon's version (the hash of the executable) is written in `server.json`. A client with a different version asks the daemon to "stop if idle" (`POST /daemon/stop?idle`). The daemon stops if no step is running at that moment and no other client is connected, and the client starts it again with its own version. Otherwise it returns 409 and keeps being used, and the client tells the user about `kairocode daemon stop`. This is checked only at that moment. Steps that start after that are waited for until they finish when it stops (the addendum below) (`TestOtherBuildReplacesTheDaemon`, `TestOtherBuildKeepsABusyDaemon`, `TestOtherBuildKeepsADaemonThatRuns`).
2. **The engine and the model are passed by the client per session.** `engine` and `model` of `SessionInput` are passed to `agent.load` and recorded there (invariant 2). If empty, the daemon's default is used. The daemon holds both engines (`TestEngineAndModelPerSession`). `KAIROCODE_CLAUDE` and the fake provider are still taken from the daemon's environment.
   - The versions of the session and turn workflows were raised to 3, and version 2 is kept with `kairo.Draining()`. Version 2 runs finish with the same function, and empty fields do not appear in the JSON, so the call IDs are the same as under version 2 (`TestVersion2RunsFinishHere`, `TestEmptyEngineKeepsVersion2Inputs`). This is because an old daemon that knows only version 2, reading a version 3 session in the shape without `engine` and `model`, would start the turn over with a different ID (a real tool could run twice). With version replacement (1), it becomes normal for the old and new daemons to run the same DB one after the other.
3. **Handle pending approvals across several clients.** The first answer goes through, other clients are told "an answer was given", and a client that joins later is sent the pending approvals again. This goes in together with the SSE and ACP work (M2's acceptance criteria). KairoCode's approvals are in the DB as kairo waits, so unlike Codex, they do not disappear even if the daemon crashes.
4. **`kairocode daemon stop`.** The daemon releases `server.json`, the listener, the runtime, the socket and the lock in this order before responding, and ends the process after it has finished writing the response. Cleanup is done only in this one place; after releasing the lock, it never deletes the next daemon's files. By the time the response arrives, a new daemon can be started (`TestStopTheDaemon`).
   - **Stopping is the same as crashing.** Steps that were running at that time are cut off, and real steps become blocked (needs confirmation) in the next daemon (invariant 4 is kept). Stopping when idle happens only when no step is running. Version replacement also happens only when the daemon checks at that moment and no step is running.
   - At first, this was implemented by closing the entrance to action handlers and waiting for running handlers to finish. Review found the following two problems, and it was removed.
     - A step refused at the entrance uses up its attempts within the same process even if it returns "can be retried". A real action has a single attempt, so it is settled as a failure at once.
     - kairo checks "was it closed" after a handler returns, and if closed, does not record the result. So even the result of a handler it waited for can be lost (reproduced 2 times in 40).
   - Stopping between steps needs an API on kairo's side that "starts no new steps, records the results of the running ones, and then closes" (r-hashi01/kairo#1). → In the addendum below, this now uses kairo 0.4.1's `Drain`.

### Addendum (2026-10-15): stop between steps with kairo 0.4.1's `Drain`

We filed this as r-hashi01/kairo#1, and `k.Drain(ctx)` went into kairo 0.4.0. We moved to 0.4.1, and when the daemon stops (`daemon stop`, version replacement, stopping when idle, signals), it now follows this order.

1. Start draining. No new step starts in this daemon, and workflows stop at their next call (the next daemon continues from there).
2. Delete `server.json` and close the listener. By the time `server.json` is gone, draining has started.
3. Wait for running steps for up to `DefaultDrain` (30 seconds). The results of finished steps are recorded, and the next daemon uses them (`TestStopLetsTheRunningStepFinish`; the model is called only once).
4. Release the runtime, the socket and the lock, in that order. The response to `daemon stop` is returned after this (even if draining times out, the process does not end until it has finished writing the response; `TestStopCutsAStepThatOutlastsTheDrain`).

- Steps that take longer than 30 seconds are still cut off, as before. Real steps become blocked (needs confirmation) in the next daemon (invariant 4). A turn on the Claude Code engine is one step: the run of `claude`. If `claude` calls a tool during draining, that call does not run, and the `claude` run is cut off after waiting 30 seconds (confirmed from the code in review; there is no test). After it is cut off, the next daemon chooses the same call again, so it does not run twice.
- Sending Ctrl-C (SIGINT) or SIGTERM twice to a `kairocode daemon` running in the foreground makes it stop waiting and stop at once on the second one. Steps that were running are cut off and treated the same as a crash (`TestSecondSignalStopsTheDaemonNow`).
- The time a started daemon waits for the lock (`WaitLock`) was set to `DefaultDrain` plus 10 seconds. It can start right after a daemon that is draining.
- Version replacement still happens only when no step is running and no other client is present, as before. Otherwise, a client with a different version would end up waiting for the drain if a step were running.
- In the same version, the IDs of `kairo.Parallel`'s second and later branches changed (fixed in kairo 0.4.0). A turn that started on 0.3.0 and moved past its first step will, when resumed on 0.4.x, run its tool calls again with new IDs. There was no turn in progress at the time of the upgrade, so by the user's decision the workflow version was not raised.

## Alternatives considered

- **Notify between processes even with SQLite (r-hashi01/kairo#8):** even with several processes running the same DB, things would be conveyed by events. But it needs a change on kairo SDK's side (adding a notifying hook to `Notifier`). For KairoCode the daemon shape is enough, but for other users of the SDK, it is proposed as a kairo ADR.
- **Use PostgreSQL locally too (LISTEN/NOTIFY):** things would be conveyed by events. But it departs from requirement NFR-6 (the only required local infrastructure is SQLite).
- **Shorten the sweep interval:** that only shortens the polling interval.
