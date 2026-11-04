# 0028 Background commands: waiting for hours inside a long loop

- Status: Accepted (2026-10-26)
- Date: 2026-10-26
- Related: design section 3 (tool effect types), ADR 0005 (sandbox), ADR 0007 (MCP calls of the Claude Code engine), ADR 0008 (daemon), invariants 3, 4, 6 and 7

## Context

One `bash` call lasts at most 10 minutes, and commands that run longer (the full check, a large build, long tests, a data migration) are stopped midway. While dogfooding (building KairoCode with KairoCode), `scripts/check.sh` ran over 10 minutes inside KairoCode and was stopped, and the agent could only ask the user to run it outside.

Also, agents are moving toward having one agent do everything in a long loop rather than splitting the work into subagents, because it is more efficient (Anthropic's announcement). KairoCode's durable workflows exist to run such a long loop for hours, safely even through crashes. Inside a long loop, it is essential to start a command in the background, keep working on other things, and wait for its end where it is needed. Claude Code's `run_in_background` plays the same role.

## Decision

1. **Add `background: true` to `bash`.** It starts the command in the background and immediately returns an ID (`bg_<12 characters>`) and how to read the output.
   - The effect type is "real", the same as `bash`. When it is unknown whether it started, the user is asked as before (invariant 4). Permission rules and the sandbox (including `escalate`) are also the same as `bash`.
   - The ID is derived from the session and the tool call's ID (the same ID comes out of the record).
   - Limits: up to 8 at the same time per session. The default duration is 2 hours, up to 24 hours with `timeout_ms`. Past that, it is stopped.
2. **Commands run detached from the daemon.** A background command is run by a watcher in the KairoCode binary (`kairocode jobs run <folder>`), in its own process group.
   - The watcher writes the output of the command inside the sandbox to a file, writes the exit code when it ends, and stops it at the duration limit.
   - Even if the daemon stops (exiting when idle, being replaced by another build, crashing), the command keeps running. When it ends, the watcher tells the daemon (waking it if there is none).
   - The record lives in `jobs/<ID>/` in the data directory (the command, the output, the exit code, the waiting runs, whether it was told).
3. **`bash_wait <ID>`: wait until it ends. While waiting, the process is released.** The workflow treats this call as a wait (invariant 7).
   - The action that registers the wait (`job.watch`) returns the result right away if it has ended, and if not, records "this run is waiting". Then the workflow waits with kairo's `WaitFor`. When it ends, the daemon sends a signal to that run.
   - Even if the signal arrives before the wait starts, kairo delivers it when it reaches the wait. If the daemon crashes, the wait stays in kairo and the registration stays in the file.
   - By default, it waits until the end. No timer is set (at first there was a default 30-minute deadline, but it was dropped on 2026-10-27 after the user pointed it out, because each deadline needs a timer and keeping the daemon awake for it). The end reaches the wait in one of these ways.
     - The watcher writes how it ended and tells the daemon (waking it if there is none; it retries for 3 minutes until delivered).
     - The daemon that started the watcher sees the watcher process end.
     - A daemon that did not start the watcher (after a replacement and so on) waits, when it wakes, for the lock of a live watcher, and tells when it gets it (however the watcher ended, even by SIGKILL, the OS releases it).
     - When the watcher itself is gone (a PC restart and so on), the next daemon to wake reports it as "unknown ending" (lost). The next thing to wake the daemon is a user connection or another watcher's notice. Until then the wait continues.
   - Only when `timeout_ms` (up to 24 hours) is given does it become a kairo wait with a deadline. When the deadline comes, it returns "still running" and the end of the output (so the agent can do other work while waiting, and wait again).
   - When called over MCP (the Claude Code engine, ADR 0007), the other CLI is waiting for the response, so one wait is up to 10 minutes.
   - The effect type is "read" (it only waits and changes nothing).
4. **`bash_output <ID>` (read) and `bash_kill <ID>` (real).** Reads the rest of the output (from `since`) and the state. Stopping is done for the whole process group.
5. **If it ends with no one waiting, the session is told.** If, when it ends, neither `bash_wait` nor `bash_output` has received that result yet, the daemon sends the session a message: "background command bg_… ended (exit code N)". The session handles it as an ordinary message, in a new turn after the current one (the workflow is not changed).
6. **Default permissions:** `bash_output` and `bash_wait` only read, so they pass without asking. `bash_kill` stays at the default "ask".

Where this shows up in the implementation: `internal/tools/bash.go` (`background`), `internal/tools/jobs.go` (the definitions of `bash_output`, `bash_wait` and `bash_kill`, and `Env.Jobs`), `internal/runtime/jobs.go` (managing jobs, the watcher, notices), `runGated` in `internal/workflows/turn.go` (making `bash_wait` a wait), `internal/cli` (`kairocode jobs run`), `internal/daemon` (notices from the watcher).

## Consequences

- One agent in a long loop can run commands over 10 minutes in the background, keep working on other things, and wait for the end. The wait lasts for hours, and survives the daemon being replaced.
- Tests
  - Start in the background, read the output so far with `bash_output`, and receive the end with `bash_wait`. While waiting, the workflow run is woken by a signal.
  - Even if the Runtime is closed and reopened while waiting, the wait wakes when it ends (resuming from the record and the files).
  - It stops at the duration limit. It stops with `bash_kill`. Going over the concurrency limit is refused.
  - If it ends with no one waiting, a new message reaches the session. If the result was already received, it does not.
  - No secrets get into the output or the record (as with `bash`, the command output is passed as is).
  - Implementation: `TestBackgroundCommandWaited`, `TestBackgroundWaitSurvivesRestart`, `TestBackgroundEndToldToTheSession`, `TestBackgroundTimeoutKillAndLimit`, `TestCrashBackgroundWait` (SIGKILL the process while waiting; the command continues, and the next process takes over the wait; it runs once and starts once), `TestJobsRun` (the CLI watcher), `TestBackgroundWaitRanOutThenEnded`, `TestBackgroundTwoWaitsInARun`, `TestJobRunnerAlive`, `TestRunnerKilledAfterRestart` (a daemon that did not start it notices the end of a watcher that was SIGKILLed).

Decided during implementation (the intent of the decision is unchanged):
- To stop it, a "asked to stop" marker (`stop`) is written first, and then SIGTERM is sent to the watcher. Even if the watcher disappears before saying so (right after starting and so on), it counts as "stopped".
- The watcher tells the daemon every time it ends (it does not replace a daemon of a different version when connecting). It tells even when stopped by `bash_kill`, and against a daemon that is closing (503) it retries for up to 3 minutes until it reaches the next daemon. The Runtime that started it also waits for the watcher's end and tells, so it can arrive twice, but it is reported once.
- "Told" (`notified`) is written after the signal to the wait and the message to the session have been sent. If the process stops before that, the next process tells again (a second signal is harmless; for the session message, `told` is written once it is sent, so it is not sent twice). A Runtime that is closing does not write "told".
- Each call's wait is a separate signal (`job/<ID>/<call>`). Even if one run waits for the same command in 2 places, they do not take each other's. A wait whose deadline passed is unregistered, so if the command ends after that, the session is told.
- While alive, the watcher holds a lock (`lock`, flock) and writes its pid. Whether it is alive is judged by the lock, not the pid (after a restart, the pid may belong to another process). Even if the daemon stops right after starting the watcher, the watcher makes itself visible.
- **While there is a wait with a deadline, the daemon does not stop.** The daemon stops after 10 minutes with no running steps and no clients. But while there is a kairo timer (today, a `bash_wait` with `timeout_ms` and workflow sleeps), it does not stop (`Runtime.Awaiting`; reconsidered each time the stop time comes). This is because kairo deadlines fire only inside a process. If there are only waits without deadlines and background commands, it may stop (the watcher wakes the daemon when one ends). When the daemon crashes, the passed deadlines all come together the next time it wakes. Tests: `TestIdleDaemonStaysWhileAwaiting`, `TestAwaitingWhileWaiting` (a wait without a deadline sets no timer, and a wait with a deadline keeps the daemon from stopping).
- Asking the user when it is unknown whether it started is the same as for "real" so far. In that case, if `jobs/<ID>` exists, it has started (the ID is derived from the call). Showing this in the question is future work.

## Alternatives considered

- **Raise `bash`'s limit:** simple. But during that time the turn can do nothing, and keeps holding the process. Inside a long loop, it cannot do other work while waiting.
- **Always start a new turn when it ends (no waiting tool):** the workflow need not change. But the result cannot be used inside the one turn of a long loop, and the context breaks each time a turn is cut.
- **Hold commands inside the daemon (not detached):** a small build. But replacing the daemon or exiting when idle would stop a long command, or lose track of how it ended.
