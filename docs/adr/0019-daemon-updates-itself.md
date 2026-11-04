# 0019 Move the daemon to a new version automatically

- Status: Accepted (2026-10-21)
- Date: 2026-10-21
- Related: ADR 0008 (one daemon per data directory. Replaces "swapping versions" in its decision 1), 0010 (the terminal UI), requirement NFR-4, invariants 4 and 7

## Context

In ADR 0008's decision 1, a client of a different version can replace the daemon only when the daemon is doing nothing and no other client is connected. Otherwise, the daemon keeps running at the old version, and the client says "run `kairocode daemon stop` by hand". The terminal UI (`kairo`) tends to stay open, and turns are long, so even after a new version is installed, the old daemon stays around for a long time.

The user wants to use it without thinking about the daemon, that is, starting, stopping and swapping versions should all be automatic (2026-10-21).

Starting and stopping are already automatic (ADR 0008).
- Start: a client starts it automatically when it is needed.
- Stop: it stops after 10 minutes with no running steps and no clients. A session that is only waiting for approval holds no process (NFR-4).
- It also stops even if something is waiting on kairo's clock (the interval of a `Retryable` retry, `ctx.Sleep`, the deadline of a flow's `wait`). The clock is not lost; the next time the daemon starts, the ones whose time has passed are picked up (they are only late). Unattended runs that need to run on time are handled by M4's suspend mode (an outside scheduler wakes kairo).

Only swapping versions is not automatic. There are two difficulties.
- Version hashes have no order. If both a client of the old version (an old executable in memory) and a client of the new version connect, each could try to replace the other's daemon, going back and forth.
- When the daemon stops, the connected clients' connections and event streams (SSE) are cut.

During the discussion we also considered removing the daemon (each client runs kairo itself). The daemon stays, so that turns continue after a client is closed, and so that notices between processes arrive as events (the user's decision, 2026-10-21).

## Decision

1. **The right version is "the version currently on disk".**
   - The daemon holds the path of the executable it was started from (written in `server.json`) and its hash (the current version).
   - The daemon re-measures the hash of the file at that path at these times. It does not watch on a clock (no polling).
     - When a client of a different version connects.
     - When the number of running steps becomes 0.
   - If the version on disk differs from its own, the daemon enters a "retiring" state.
   - Clients do not try to replace the daemon. If the versions differ, they only ask the daemon to "check" (`POST /daemon/update`. Replaces `stop?idle`).
     - Since the disk decides, old and new clients never go back and forth.
     - If the client itself is older than the version on disk, it tells the user "This client is an old version. Please reopen it."
2. **A retiring daemon stops when the number of running steps becomes 0.**
   - It does not wait for connected clients. With kairo's `Drain`, it waits for running steps to finish and then stops (the same as ADR 0008's addendum).
   - Stopping in the middle of a turn is fine, as long as it is between steps. The workflow resumes from the record, and no real step is running, so nothing becomes blocked (needs confirmation).
   - Waiting for approval holds no step, so it does not prevent stopping.
3. **If clients are connected, a retiring daemon starts the new version before stopping.**
   - This is so there is somewhere to reconnect. If no client is connected, it just stops, and a client starts it the next time it is needed (as now).
   - Starting is limited to one with `start.lock`, as now, and the new daemon waits for the previous daemon to release the lock (ADR 0008's addendum).
4. **Clients reconnect.**
   - When `daemon.Client` cannot reach the daemon (no socket, `server.json` changed), it rereads `server.json`, and if there is none, starts a new daemon through the startup steps (`Connect`). What it starts is the executable at the path on disk, so even an old client starts the new version.
   - It reconnects only when the connection could not be made (when the request was not sent). A request cut off after being sent (sending a message, approving) is not sent again. It is unknown whether it arrived, so it is returned as an error (the same idea as invariant 4).
   - When the event stream is cut, the ACP agent watches again as it does now. Watching again re-delivers whatever is waiting (approvals).
5. **Paths:** package managers (Homebrew and others) install each version in its own directory and repoint a symbolic link. The daemon holds the path it was started from (still as the link), not `os.Executable`, and follows the link when measuring.
6. **Starting and stopping do not change.** As in ADR 0008, it starts when needed and stops after 10 minutes of doing nothing. It stops even if something is waiting on the clock, and that is picked up at the next start (NFR-4).

Where this shows in the implementation: `internal/daemon` (the path in `Info`, `idleTimer`'s notice of zero steps, `/daemon/update`, the retiring state and starting the next daemon, `Client`'s reconnecting), `internal/cli` (the notice for old clients).

## Consequences

- The user does not need to start or stop the daemon. `kairocode daemon stop` stays only for when you want to stop it by hand.
- After a new version is installed, the daemon switches to the new version the next time steps run out.
- The terminal UI continues through reconnecting. A message sent during reconnection may come back as an error (what may or may not have arrived is not sent again).
- Tests (added during implementation)
  - When the executable on disk is replaced, the daemon stops once steps reach 0, and if clients are connected the new version starts.
  - It does not stop while a step is running.
  - When an old-version client connects, the new-version daemon does not stop (no going back and forth).
  - Reconnecting re-delivers pending approvals. A request cut off after being sent is not sent again.
  - ADR 0008's `TestOtherBuildKeepsABusyDaemon` and others are rewritten for the new form.

## Alternatives considered

- **Keep things as they are (swap only when idle, otherwise stop by hand):** reliable, but does not meet the request.
- **Remove the daemon and have each client run kairo:** managing version swaps would no longer be needed. But closing a client stops the turn (it continues the next time it is opened), and notices between processes need a change to kairo's SDK (kairo gap #19) and a way to stream live output to other clients. By the user's decision, the daemon stays.
- **Do not stop the daemon while something waits on the clock:** things run on time, but it goes against NFR-4 (do not hold a process while waiting). The clock for unattended runs is handled by M4's suspend mode.
- **When a client of a different version comes, swap even if it cuts off running work:** nothing breaks, but running real steps become blocked (needs confirmation), and the user gets asked more.
- **The newer client wins (first come):** if an old client stays open, it goes back and forth. Treating the disk as the truth avoids that.
- **The daemon watches its own executable on a clock:** that is polling. File change notification needs a dependency (fsnotify).

## Implementation (2026-10-21)

- **Daemon (`internal/daemon/daemon.go`)**
  - Added `Options.Installed` (the version currently on disk) and `Options.Successor` (how to start the new version). `InstalledAt(path)` re-measures the hash only when the file's size, time or inode changes, so checking it every time steps reach 0 costs one `stat`.
  - `handover`: takes `start.lock`, drains and lets go, starts the new version and waits for it to be ready if clients are connected, then releases `start.lock`. Clients that come in the meantime wait on `start.lock` and connect to the new version.
  - When running steps reach 0 (`onNoStep` of `idleTimer`), it checks the version and hands over if it is old.
  - `POST /daemon/update`: checks the version; if it is old and no step is running, hands over on the spot (`replaced`); if steps are running, answers later (`later`).
  - `POST /daemon/stop?idle` was kept for clients from before 0019.
- **Client (`internal/daemon/client.go`)**
  - If the versions differ, it asks `/daemon/update`. To a daemon from before 0019 (no `/daemon/update`), it asks with ADR 0008's swap.
  - If the daemon's version is the version on disk and its own differs, it says "This is an old version. Please reopen it." (`Stale`).
  - Only requests that did not reach the daemon (could not connect, 401 with an old token) are sent again after reconnecting (`unreached`). A request cut off after being sent is not sent again.
- **CLI (`internal/cli/cli.go`)**
  - The daemon is started with the path that was used to invoke it (still as the link, an absolute path) (`invokedPath`). The client's own version is, as before, the hash of the running executable (`daemon.FileVersion`).
  - The daemon checks the version at the path of its own `os.Args[0]`, and starts the new version with the same arguments and environment.
- **Tests:** `TestNewBuildReplacesTheDaemon`, `TestNewBuildReplacesADaemonInUse` (the connected client reconnects and is told it is an old version), `TestNewBuildWaitsForTheStep` (hands over between steps; the turn finishes on the new version), `TestOlderClientKeepsTheDaemon` (no going back and forth), `TestOnlyUnreachedIsSentAgain`. We checked that removing reconnection makes `TestNewBuildReplacesADaemonInUse` fail, and removing the check at zero steps makes `TestNewBuildWaitsForTheStep` fail. ADR 0008's `TestOtherBuildKeepsABusyDaemon` and `TestOtherBuildKeepsADaemonThatRuns` were replaced with this form.
- **Not yet:** that pending approvals are re-delivered after reconnecting is left to the ACP agent's re-watching (the current design), and no test was added for this ADR.
- **What was fixed after durability-reviewer's findings**
  - A daemon that could not measure its version name (`""`) is not compared with the version on disk. Before, it could keep swapping every time work paused.
  - Before handing over, keep-alive connections are turned off (`SetKeepAlivesEnabled(false)`). Before, requests on existing connections could reach the closed runtime. Now they become a could-not-connect failure (safe to send again).
  - The time to wait for the handover before the process exits was extended by the time to wait for the new version to be ready. Before, `start.lock` could be released while it was getting ready, and a waiting client could start another one.
  - The text "This is an old version" was changed to "This is not the installed version" (it may be newer, such as a development build).
- **Not yet:** "This is not the installed version" after reconnecting goes into `Client.Stale`, but ACP and the terminal UI do not show it (the CLI shows it only on the first connection).
