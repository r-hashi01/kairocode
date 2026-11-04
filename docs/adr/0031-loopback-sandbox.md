# 0031 A sandbox that allows only local traffic: running checks without leaving the sandbox

- Status: Accepted (2026-10-27)
- Date: 2026-10-27
- Related: ADR 0005 (sandbox), ADR 0028 (background commands), AGENTS.md "How to work"

## Context

`scripts/check.sh` is meant to run outside the agent's sandbox. In KairoCode, `bash` is called with `sandbox: "escalate"` (outside the sandbox, with the user's approval).

Running it outside means that, through `go test`, the test code the agent just wrote runs without limits. A permission saying "let only check.sh go outside without asking" amounts to "anything can run outside". So going outside cannot be made automatic, and when one agent is trusted with a long loop, every check waits for a human approval.

We ran the checks inside KairoCode's sandbox (macOS, `WorkspaceWrite`) and confirmed what is blocked (2026-10-27).

1. **Listening on `127.0.0.1`** (fake servers): `bind: operation not permitted`.
2. **Unix sockets** (the daemon): `bind: operation not permitted`, even inside a temporary folder.
3. **Nested sandboxes** (tests that start commands in KairoCode's sandbox): `sandbox_apply: Operation not permitted`. On macOS, another sandbox cannot be started inside a sandbox.
4. **Go's build cache** (`~/Library/Caches/go-build`): it cannot be written, so `go test` stops at the build stage.

In a prototype, we added rules that allow only 1 and 2, and put the build cache inside the repository. Then `internal/flowchart` and `internal/server` passed inside. `internal/daemon` also passed, except 1 test that hits 3 (`TestCrashDaemonEventsAfterRestart`, which runs bash in the sandbox). Outbound network (`curl https://example.com`) and Unix sockets outside the allowed place stayed blocked.

## Decision

1. **Add "local traffic" to the sandbox rules.** Add `Loopback bool` to `sandbox.Policy`. With `Loopback`, only these 2 are allowed.
   - **localhost TCP and UDP:** listening, accepting, connecting (in the macOS rules, `(local ip "localhost:*")` and `(remote ip "localhost:*")`).
   - **Unix sockets:** creating and connecting are allowed only inside a temporary folder just for that command (`/private/tmp/kc-XXXX`, mode 0700).
     - The command's `TMPDIR` is set to that folder. Tests' `t.TempDir()` and the daemon's socket location (ADR 0008) also go inside it.
     - Sockets in the whole temporary folder (`$TMPDIR`, `/tmp`) are not allowed. Those hold the real daemon's socket (`$TMPDIR/kairocode-<uid>/`) and the sockets of tmux and herdr. If they could be connected to, a command could approve itself or type into a terminal, and get outside the sandbox.
     - With `loopback`, the only writable temporary folder is this dedicated one (`/tmp` and `$TMPDIR` are read only). If they were writable, someone else's socket or its folder could be `mv`ed into the dedicated folder and connected to. Sockets outside the dedicated folder can be neither moved nor deleted.
     - The folder is deleted when the command ends. For background commands (ADR 0028), it is deleted when that job ends.
   - Everything else is the same as `WorkspaceWrite`. Outbound network, writes outside the writable places, and protected names (`.git`, the config directory) are not allowed.
   - On Linux (bubblewrap), cutting the network with `--unshare-net` gives the command its own localhost. So TCP is handled as it is now. The temporary folder for Unix sockets is made the same as on macOS.
   - Implementation: `internal/sandbox` (`Policy.Loopback`, `seatbelt.go`, `platform_linux.go`, creating and cleaning up the temporary folder).
2. **Add `"loopback"` to `bash`'s `sandbox`.**
   - By default, approval follows the same rules as ordinary `bash`. If the settings let `bash` pass without asking, `loopback` also passes without asking. This is the difference from `escalate` (ask every time).
   - **It can be changed in settings.** Write one of these in `sandbox.loopback` in `kairocode.json`.
     - `"allow"` (default): follow the same rules as `bash`.
     - `"ask"`: ask every time for `loopback` calls.
     - `"deny"`: `loopback` cannot be used (only `escalate` remains).
   - The user's settings can be any of these. Project settings (inside the repository) only take effect in the stricter direction (`ask`, `deny`) than the user's. This is so that a repository you obtained cannot loosen it on its own.
   - The setting is read inside `permission.check` and recorded as part of that decision (invariant 2).
   - When a failure looks like the sandbox blocked it, the guidance to the model becomes "if you need local traffic, use `loopback`; if you need it for anything else, `escalate`".
   - Implementation: `internal/tools/bash.go`, `internal/tools/jobs.go`, `internal/runtime/permission.go`.
3. **Treat nested sandboxes as unavailable.**
   - On macOS, `sandbox.Available()` checks not only whether `sandbox-exec` exists, but actually starts it once (the result is remembered; Linux already does this).
   - When nested, it returns `ErrUnavailable`. Production `bash` still fails rather than running without a sandbox, as before (it does not fall toward loosening).
   - Tests that start a sandbox already skip themselves with "no sandbox". The same skip is added to those that do not skip yet (`TestCrashDaemonEventsAfterRestart` and so on).
4. **Make `scripts/check.sh` pass inside the sandbox too.**
   - When `KAIROCODE_SANDBOX` is set, `GOCACHE` is set to `.cache/go-build` (not tracked by git).
     - The shared cache is not made writable from the sandbox. If it were, a command inside the sandbox could put in content that a build outside the sandbox later uses.
   - envcheck drops "nested sandboxes are unavailable" from its reasons to stop. Instead it says "tests that start a sandbox were skipped". When localhost or Unix sockets cannot be used, it stops as before (exit code 3).
   - If any tests were skipped because of nesting, at the end it prints "skipped N tests; run them all outside (CI or a local terminal)".
   - By default, the exit code in that case is 0. But `last-green` is marked "partial". The Stop hook lets "partial" pass, and committing is possible. Passing everything is confirmed by running outside (CI, locally before push).
   - **It can be changed in settings.** With the environment variable `KAIROCODE_CHECK_FULL=1`, if any tests were skipped, it ends with exit code 3 (run it outside). People and machines (CI and so on) that do not accept a "partial" pass set this. It can be written in `env` in `.claude/settings.json` or in KairoCode's agent settings.
5. **AGENTS.md:** rewritten to say that in KairoCode, `scripts/check.sh` is run with `sandbox: "loopback"`. `escalate` is used only when running everything.

Decided during implementation (the intent of the decision is unchanged):
- **On macOS, "localhost" is all of this machine's own addresses.** `localhost` in the sandbox rules also includes connections to this machine's other addresses, such as its LAN IP (confirmed by trying). Addresses of other machines (`8.8.8.8` and so on) are blocked. Only services inside this machine are reachable, which is the same as the scope of the decision.
- **The build cache inside the sandbox is `.cache/go-build` (at the repository root; not tracked by git).** `.claude` is a name the sandbox protects, so it cannot be written from inside. For the same reason, `check.sh` inside the sandbox cannot write `last-green` or the "partial" mark either (a pass is a pass even if it cannot be written). The mark is left only when it is run outside.
- **Unix sockets on Linux (2026-10-27, found in CI and fixed).** In the bubblewrap sandbox, Unix sockets on a path could be connected to even when made read-only files and even with a separate network namespace. This was so not only for loopback but also for the ordinary sandbox without network. The daemon's socket (normally in the data directory, `~/.local/share/kairocode/kairocode.sock`) could also be connected to, which means a command inside the sandbox could approve its own calls. It was found when `TestSandboxLoopback` failed on Linux in CI. The fix is as follows.
  - The ordinary sandbox without network: `socket(AF_UNIX)` is denied with seccomp (as on macOS, Unix sockets cannot be created). `socketpair`, which connects to nothing, is kept. The seccomp library compares arguments as 64 bits and lets other architectures (i386 calls on amd64) through, so our own instructions are put before it (all calls from other architectures are denied, and the socket type is checked on the low 32 bits; `TestSeccompFilter`).
  - `loopback`: `/tmp`, `/var/tmp`, `TMPDIR`, `$XDG_RUNTIME_DIR` and the daemon's data directory (`sandbox.HideFromLoopback`; the daemon and the runner of background commands each declare it) are covered with an empty tmpfs, and sockets directly under `/run` (docker's) are covered with empty files. The writable places are bound again after that, and covered places that are under writable places (the data directory, for a session started in the home folder) are covered again after re-binding. If a covered place is itself the working folder (a session started in `/tmp`), `loopback` cannot be used. Programs under `/tmp` cannot be seen from inside `loopback`.
  - Other sockets under the home folder (gpg-agent and so on) can be connected to from inside Linux's `loopback`. By the user's decision, this is not closed off (the addendum below).
- **What remains:** sockets inside the working folder (those of that project's processes) can be connected to by moving the folder containing the socket into the dedicated folder, because the working folder is writable. Sockets outside the project (the daemon, tmux, herdr) cannot be reached.
- **Leftovers:** the dedicated folder is deleted after the command. When the daemon or the runner of a background job is killed, `/private/tmp/kc-*` remains (left to the OS's cleanup of temporary folders).
- How skipped tests are counted: tests that start a sandbox skip themselves with `sandboxtest.Require(t)` (`internal/sandbox/sandboxtest`). When doing so, they write the test name into the file named by the environment variable `KAIROCODE_SKIPPED`. `check.sh` counts these.
- Tests made to skip themselves when nested: background commands, Claude Code hooks, file snapshots (undo, fork), `escalate` approvals, `kairocode jobs run` and others (45 with `--quick`).

## Addendum (2026-10-27, the user's decision): what `loopback` promises

When it turned out that Linux's `loopback` reaches sockets under the home folder, we considered also making the home folder look empty and showing only what is needed again. As a result, it was decided not to close this off.

- `loopback` was from the start a promise that "ports inside this machine are trusted". On macOS, it already reaches TCP services listening on this machine's addresses (development DBs, local LLM servers, admin pages). Even if only Unix sockets were fully closed off on Linux, the strength of protection would not be even.
- Making the home folder look empty would hide settings and caches that were not shown again (`.gitconfig` and so on), and risks breaking the purpose of running checks inside the sandbox.

`loopback` promises only the following. The first 2 are confirmed by tests on every OS (`TestSandboxLoopback`, `TestLoopbackInTheBackground`). The 3rd is guarded by the rules (macOS's sandbox rules, Linux's network namespace), since tests do not use the network.

- It does not reach KairoCode's own approval channel (the daemon's socket and the data directory).
- It does not reach sockets in the temporary folders of other commands or sessions (`/tmp`, `TMPDIR`, `$XDG_RUNTIME_DIR`), and cannot move or delete them.
- It does not reach other machines.

Other ports inside this machine (on macOS, all localhost TCP; on Linux, sockets under the home folder) are reachable. People who are troubled by that set `sandbox.loopback` in settings to `"ask"` or `"deny"`. The default stays `"allow"`.

The ordinary sandbox without network cannot create Unix sockets on any OS (Linux uses seccomp; `TestSeccompFilter`). It reaches nothing.

When Linux gets a mechanism to restrict connections to Unix sockets on a path by path (Landlock) and it becomes widely available, adding it on top will be reconsidered (as of April 2026 it was under review).

## Addendum (2026-11-02): `sandbox.loopback` is decided by the value at the start of the turn

The `sandbox.loopback` setting is recorded at the start of the turn, like the permission rules (invariant 2). Until now, `permission.check` read the settings file each time it was called, so if the setting was changed in the middle of a turn, later calls in the same turn were decided by the new value.

- `agent.load` returns the setting's value as `Agent.Loopback`. The workflow passes it to `permission.check` as `Policy.Loopback` (`internal/workflows/types.go`).
- `permission.check` decides by `Policy.Loopback` if it is set. It is empty for turns that started before it was recorded. Only then does it read the settings as before (the recorded input is not changed).
- Test: `internal/runtime` (`TestLoopbackKeptAsTheTurnStarted`). Even if it is changed to `deny` in the middle of a turn, that turn's `loopback` calls pass by the value at the start.

## Consequences

- KairoCode's agent can run checks inside the sandbox without being asked. It goes outside only when running everything, including tests that need nested sandboxes.
- What becomes newly allowed is connecting from `loopback` commands to local TCP services (local databases, development servers, editors' listeners and so on). This can be set to "ask" or "do not use" with `sandbox.loopback`.
- The sandboxes of Claude Code and Codex (on the agent's side) do not change. This ADR applies only to agents running on top of KairoCode.
- Tests
  - `internal/sandbox` (`TestSandboxLoopback`, `TestSandboxLoopbackNeedsItsDirectory`):
    - With `loopback`, listening on and connecting to `127.0.0.1`, and Unix sockets inside the dedicated temporary folder, work.
    - Unix sockets outside the dedicated temporary folder (the real `$TMPDIR`) and outbound network are blocked.
    - Without `loopback`, they are blocked as before.
    - The dedicated temporary folder disappears when the command ends.
  - `internal/sandbox` (`TestSandboxNestedIsUnavailable`): calling `Available()` from inside a sandbox answers that it is unavailable (checked by starting a child process inside the sandbox).
  - `internal/runtime` (`TestLoopbackInTheBackground`): a background job's temporary folder disappears when the job ends.
  - `internal/runtime` (`TestLoopbackSetting`, `TestLoopbackCommand`): by default, `loopback` passes without asking under `bash`'s rules. With `ask` in `sandbox.loopback` it asks, and with `deny` it refuses. `allow` in project settings does not loosen the user's `ask`.
  - In `scripts/check.sh`, the skipped count, the "partial" mark, and exit code 3 with `KAIROCODE_CHECK_FULL=1` are checked by hand (no automatic test).

## Alternatives considered

- **Let only check.sh go outside without asking (automatic escalate):** what runs outside is code the agent wrote, so it is the same as being able to run anything outside.
- **Let a judge model decide whether going outside is OK:** if it misses instructions planted in an issue or web page it read, a way outside opens.
- **Build a new, additional sandbox level that allows only the writable places and localhost:** its content would be the same as this `Loopback`. Adding one flag to the current rules is smaller than a new kind.
- **Allow Unix sockets in the whole temporary folder:** the rules are simple, but it connects to the real daemon's and tmux's sockets, and gets outside the sandbox.
- **When nested, leave the inner command to the outer sandbox and run it as is:** if the outer one is looser (for example, when the inner one is read only), it runs wider than intended. It would loosen the production path just for tests.
- **Add Go's shared build cache to the writable places:** content that builds outside the sandbox use could be rewritten from inside the sandbox.
