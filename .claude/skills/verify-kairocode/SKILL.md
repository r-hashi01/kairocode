---
name: verify-kairocode
description: Drive the real KairoCode binary the way a user does and capture proof - the kairocode CLI and its daemon (run -json, approvals, -stream-json, lines and work items) and the kairo terminal UI through tmux - in an isolated root with a scripted fake model. Use to confirm a change works in the built app (not only in tests), to reproduce a bug as a user would, or when asked to drive, try, demo or prove a KairoCode feature. Not the test suite: /verify runs scripts/check.sh.
---

# verify-kairocode

Drives `bin/kairocode` end to end and leaves proof behind. Every run lives in its own **root**: its own HOME, settings, data directory (so its own daemon), workspace, tmux server and fake model. The helper `scripts/kcv` builds that environment for every command, so nothing reaches the user's daemon (`~/.local/share/kairocode`), settings (`~/.config/kairocode`), keychain, tmux or API keys.

The feature map in [features/README.md](features/README.md) says what to drive and what proves it. Read it before you drive anything.

## Rules

- Go through `kcv` for everything: `kcv kc ARGS` runs `bin/kairocode ARGS` in the root. Never run `bin/kairocode`, `kairo` or `kairocode` bare. Without the root's environment they talk to the user's own daemon and data.
- Stop only what you started: `kcv stop` and `kcv clean`. Never `pkill`, `killall` or kill by process name. The user's own daemon may be running.
- The model is the fake (`KAIROCODE_PROVIDER=fake`) unless the root was made with `kcv new --real`. Never read or print `.env`.
- Shell state does not carry over between tool calls. Start every Bash call with the preamble below and pass the root explicitly.
- The user's shell is zsh. Quote every request (`"KCV-TEXT what is this?"`): an unquoted `?` or `*` is a glob in zsh and fails with `no matches found`.

## Launch

Run from the repository root. Building uses your normal environment (the Go toolchain and its module cache live under the real HOME):

```sh
make build                                   # bin/kairocode and bin/kairo; Go 1.27.4 is fetched by the toolchain line
export PATH="$PWD/.claude/skills/verify-kairocode/scripts:$PATH"
root=$(kcv new)                              # prints the root, e.g. /private/var/folders/.../T/kcv.Ab12Cd
KCV_ROOT=$root kcv up                        # starts the root's daemon through a client call
```

- `kcv new` makes `$root/home` (HOME, with a git identity), `$root/home/.config` (XDG_CONFIG_HOME), `$root/data` (XDG_DATA_HOME, so the data directory is `$root/data/kairocode`), `$root/cache`, `$root/fake.json` (a copy of [fake.json](fake.json)), and `$root/ws`. `$root/ws` is the workspace: a git repository with one commit (`README.md`) on `main`.
- It is ready when `kcv up` prints `ready: daemon pid <N>, data <root>/data/kairocode` and `kcv doctor` exits 0. Any client command also starts the daemon on demand. `kcv up` only makes the start explicit, so a failure shows up here and not halfway through a journey.
- Each later Bash call starts with this preamble:

  ```sh
  cd "$(git rev-parse --show-toplevel)"
  export PATH="$PWD/.claude/skills/verify-kairocode/scripts:$PATH" KCV_ROOT=<root> ev=<evidence dir>
  ```

  If your harness refuses the `git rev-parse` line (a worktree-isolated agent may), `cd` to the checkout's literal path instead.

- Several roots can run side by side, one per feature or per agent. Each has its own daemon, socket and tmux server.
- If a command fails with `operation not permitted`, a socket error, or `sandbox-exec` refusing to start, your own tool sandbox is in the way. The daemon listens on a Unix socket, and KairoCode runs `bash` inside macOS `sandbox-exec`, which cannot nest. Rerun that command outside your sandbox.

## Doctor

```sh
KCV_ROOT=$root kcv doctor
```

Read-only. Run it after launch and whenever anything looks off. Each line is `ok` or `FAIL`, and it exits 1 on any `FAIL`. It checks that:

- the root is a kcv root and its data directory is inside it;
- the workspace is a git repository;
- `bin/kairocode` exists and is newer than every non-test file under `cmd/` and `internal/` and `go.mod`/`go.sum`, embedded sandbox policies included (`FAIL ... run kcv build` means you are about to verify old code);
- the fake script parses (in a real root: the key's variable is set, value not shown);
- the daemon in `$root/data/kairocode/server.json` is alive and its command line is `daemon -data <this root's data dir>`, so it is ours;
- the daemon runs this build (`server.json`'s `version` equals the first 24 hex digits of `shasum -a 256 bin/kairocode`);
- its socket exists and it answers `sessions -all -json`;
- whether the root's tmux server runs.

On a stale build: `make build`, then `kcv stop`. The next command starts the new build.

## Drive

| Surface | How | Feature files |
|---|---|---|
| CLI (primary; machine-readable since ADR 0041) | `kcv kc <command> ...`, or `kcv rec $ev <step> <command> ...` to keep the proof | run, approvals, stream-json, lines-items |
| Terminal UI (`kairo`, `kairocode chat`) | `kcv chat`, `kcv keys`, `kcv wait-screen`, `kcv screen` (a tmux server of the root's own) | tui-chat |
| Workspace git | `kcv git ...` (in `$root/ws`, as the root's user) | lines-items |

- **Working directory.** `kcv kc` runs in `$root/ws`, so `-dir` defaults to the workspace. Set `KCV_DIR=<path>` to run elsewhere.
- **Project settings.** `$root/ws/kairocode.json` holds the project's settings. `run`, `line` and `lines` read it directly. The chat asks first whether you trust the folder.
- **The fake model.** It is chosen at daemon start: `KAIROCODE_FAKE=$root/fake.json` is read once by the daemon. After you edit `$root/fake.json`, run `kcv stop`; the next command starts a daemon that reads it again. The fake picks a scripted reply from the text of the conversation's first user message. For a `steps` key, the Nth model call of a conversation gets the Nth entry, and the last entry past the end. The shipped keys:

  | Put in the request | The fake does | Then replies |
  |---|---|---|
  | `KCV-TEXT` | nothing | `KCV-TEXT reply: no tools were called.` |
  | `KCV-WRITE` | calls `write` with `hello.txt` = `hello from kcv\n` | `KCV-WRITE finished.` |
  | `KCV-BASH` | calls `bash` with `echo ran > bash.txt` (in KairoCode's sandbox) | `KCV-BASH finished.` |
  | (a line's station prompt, `You are at the station "build"`) | calls `write` with `notes.txt` = `notes from the line\n` | `The build station wrote notes.txt.` |
  | (a notice to the conversation, `[KairoCode] ...`) | nothing | `Noted.` |
  | anything else | nothing | `kcv fake: the request named no KCV- key, so this is the fallback reply.` |

  The fallback answers only the first 4 model calls of a conversation. After that it fails with `fake provider: no reply 4 in a script of 4`. To script a new journey, add a `steps` key to [fake.json](fake.json) (the format is `internal/provider/fake.go`'s `Fake`). Keys are tried in sorted order, and the first contained in the message wins.
- **A real model (optional).** Use one only when the fake cannot reach the behavior, for example a provider adapter or a real model's tool use. It costs money. Make a separate root, because the daemon keeps the environment it started with. In every command that may start the daemon, load the key in that same command; `kcv` passes only `OPENAI_API_KEY` (or the names in `KCV_PASS`) through:

  ```sh
  root=$(kcv new --real)
  set -a && . ./.env && set +a && KCV_ROOT=$root kcv kc run -json -provider oai:openai -model gpt-5.4-mini "Reply with the word ok."
  ```

  In a linked worktree `.env` is not there. Source the main checkout's copy: `"$(git rev-parse --path-format=absolute --git-common-dir)/../.env"`. In a fake root, never pass `-provider oai:...`: it would bypass the fake, and only the missing key stops it.

## Evidence

```sh
ev=$(kcv evidence <feature-id>)   # .claude/.cache/verify-kairocode/<UTC time>-<feature-id>/ in this checkout
```

- Evidence goes under `.claude/.cache/verify-kairocode/`. It is ignored by git (`.claude/.cache/`), outside every root, and survives `kcv clean`.
- **CLI step.** `kcv rec $ev 01-name <command> ...` writes `01-name.cmd` (the command, replayable), `.out`, `.err` and `.code`. It returns the command's own exit code, so a nonzero exit is not a helper failure. Number the steps in the order you drive them.
- **TUI step.** `kcv screen $ev/01-name.txt` after each action, once `kcv wait-screen` has seen the expected text.
- **Side effects.** Save what a user would check: `cp $root/ws/hello.txt $ev/`, `ls -la $root/ws > $ev/ws-after.txt`, `kcv git log --oneline --all > $ev/git.txt`. Save `kcv kc log <session>` as the record of what ran.
- **Never copy `server.json`**: it holds the daemon's bearer token. `daemon.log` holds no secrets and may be copied (`cp $root/data/kairocode/daemon.log $ev/`).
- End with `$ev/summary.txt`: the feature and sub-feature IDs driven, each with its entry point, PASS or FAIL, and the evidence files that show it. Also list sub-features not driven, with why.

Proof standards:

- Drive the user's path: the CLI commands and keys a user would type. Do not call the daemon's HTTP API, runtime functions or test helpers.
- Capture the action and the state it leads to, not only the final screen. For a mutation, add a second, read-only view: the file on disk, `approvals -json`, `items -json`, `log`, git.
- The fake model is a mock at the model's boundary, which `KAIROCODE_PROVIDER` already isolates. Everything after the model (permissions, tools, the sandbox, the record, the daemon) is the real code. Say "with the fake model" in the summary.
- A failure is a finding. Report it with its evidence. Never edit the journey, the fake's replies or the expected values to make it pass.

## Cleanup

```sh
KCV_ROOT=$root kcv clean
ls "$ev"                          # the proof is still there
```

- `kcv clean` kills the root's tmux server (`tmux -L kcv-<root name> kill-server`). It stops the root's daemon with `kairocode daemon stop` in the root's environment, waits up to 45s (steps under way get 30s), and `kill -9`s that one pid only if it is still alive and still this root's daemon. Then it removes the root.
- Items' worktrees live in `$root/data/kairocode/worktrees`, so they go with the root.
- Run `kcv clean` after every failed attempt too. A forgotten daemon exits by itself after `KCV_IDLE` (default `10m`) with nothing to do.
- Lost the root's path? The roots are `${TMPDIR}kcv.*`. Run `KCV_ROOT=<each> kcv clean` only on roots you made.
- `kcv stop` stops without removing: use it to restart the daemon (a new build, an edited `fake.json`) and keep the root.

## Helpers

`scripts/kcv` (bash; `kcv help` prints this list):

| Command | What it does |
|---|---|
| `kcv build` | `make build` |
| `kcv new [--real]` | make a root and print its path |
| `KCV_ROOT=… kcv up` | start the root's daemon and wait until it answers |
| `KCV_ROOT=… kcv doctor` | read-only health check (above) |
| `KCV_ROOT=… kcv kc ARGS…` | `bin/kairocode ARGS` in the root: cleared environment, `cwd` `$root/ws` |
| `KCV_ROOT=… kcv rec DIR NAME ARGS…` | the same, saving `NAME.cmd/.out/.err/.code` in DIR; returns the exit code |
| `KCV_ROOT=… kcv git ARGS…` | git in `$root/ws` with the root's identity |
| `KCV_ROOT=… kcv chat [NAME] [CHAT FLAGS…]` | `kairocode chat` in tmux session NAME (default `chat`) of the root's tmux server, 120x40 (`KCV_COLS`, `KCV_ROWS`) |
| `KCV_ROOT=… kcv keys KEY…` | `tmux send-keys` to it (`-l "text"` for literal text, `Enter`, `C-c`, `BTab`); `KCV_TMUX=NAME` picks the session |
| `KCV_ROOT=… kcv wait-screen TEXT [SECONDS]` | wait until the screen shows TEXT (default 30s); on timeout, print the screen and exit 1 |
| `KCV_ROOT=… kcv screen [FILE]` | the screen as text |
| `KCV_ROOT=… kcv stop` | stop the root's tmux server and daemon |
| `KCV_ROOT=… kcv clean` | stop, then remove the root |
| `kcv evidence FEATURE` | make and print an evidence folder |

The environment `kcv kc` gives the binary, and every daemon the binary starts: `PATH` (system paths and Homebrew), `HOME`, the `XDG_*` folders above, `TMPDIR`, `USER`, `LANG=en_US.UTF-8`, `TERM`, and `KAIROCODE_LANG=en`. It also sets `KAIROCODE_NO_CLIS=1` (the user's claude, codex and copilot are not found), `KAIROCODE_ACP_REGISTRY=off` and `KAIROCODE_LSP=off` (no network, no language servers), `KAIROCODE_SECRETS=file` (no keychain), `KAIROCODE_DAEMON_IDLE`, and the fake's two variables. Nothing else of yours passes through, so `KAIROCODE_ENGINE` and `KAIROCODE_MODEL` from your shell cannot redirect a run.

## Surfaces not mapped yet

- `kairocode acp`: the ACP agent over stdio JSON-RPC (ADR 0002). The chat is a client of the same agent, in process.
- The daemon's HTTP API over its Unix socket. Its bearer token is in `server.json`; never print it.
- Flows: `run "/flow <name>"`, `kairocode flow graph|show|runs`, and `answer` (`scripts/demo-durability.sh` drives a flow with the fake model and a `kill -9`).
- `sessions`, `fork`, `-worktree`, `plugin`, `gc -dry-run`.

Add a feature file when you drive one of these. [features/README.md](features/README.md) says how.
