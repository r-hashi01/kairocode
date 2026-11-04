# 0009 Add language server diagnostic errors to the result of a tool that wrote a file

- Status: Accepted (2026-10-19)
- Date: 2026-10-19
- Related: M2's acceptance criteria (LSP diagnostics), ADR 0002 (opencode as the model), 0005 (sandbox), 0008 (daemon), AGENTS.md invariants 3, 4 and 7

## Context

M2's acceptance criteria include "after an edit, LSP diagnostic errors are added to the tool's output". The model learns whether the code it wrote passes type checking before it takes its next step.

We read the source of opencode v1.19.3 (`packages/opencode/src/lsp/`, `tool/edit.ts`).

- **Report format:** After edit, write and patch, it has the language server open the file (`didOpen`, then `didChange` from the second time on) and waits for diagnostics to arrive. It adds only that file's severity 1 (ERROR) diagnostics, at most 20, to the tool's output in this form.
  `LSP errors detected in this file, please fix:\n<diagnostics file="...">\nERROR [line:col] message\n</diagnostics>`
- **How it waits:** It waits for `textDocument/publishDiagnostics`. It settles 150ms after the last notification (debounce) and gives up after 5 seconds.
- **Servers:** 38 kinds are built in. It picks one by extension and by marker files that decide the root (`go.mod` and so on). It keeps the servers inside the process.
- **Automatic download:** It fetches a language server that is not found on its own (`go install gopls@latest`, npm and so on).
- **Configuration:** Done in `lsp` in `opencode.json`. `false` stops all of them. Per server you can write `disabled`, `command`, `extensions`, `env` and `initialization`.

In KairoCode, commands start only inside the sandbox (ADR 0005, AGENTS.md). A language server is a process that lives across turns. So we need to decide how it starts, how long it lives, and where it can write.

## Decision

1. **Add diagnostic errors to the result of a tool that wrote, in the same form as opencode.**
   - The targets are tools that write files (now `write` and `edit`; the ones in `internal/tools` that have a `Plan`). After the write finishes, inside the same action (`file.apply`), get the diagnostics for that file.
   - Add only ERRORs, at most 20, to the result in the form above.
   - The result is recorded, so a resumed turn uses the recorded diagnostics. When a write with an unknown outcome is checked and redone (design section 3.1), the diagnostics at that point are taken again.
   - MCP calls from the Claude Code engine go through the same action, so they get them added too.
2. **We wait only for a notification or a deadline (no polling).**
   - When `publishDiagnostics` arrives, wait for 150ms of quiet and settle. Give up after 5 seconds.
   - When it gives up, or the server is missing or has crashed, return the result with nothing added. Log it.
   - This is a bounded wait inside one step. It is not something that waits for a person or an outside event (invariant 7).
3. **The daemon holds the language servers** (`internal/lsp`, ADR 0008).
   - Start one per (server kind, root) the first time it is needed.
   - Stop it if it is unused for 10 minutes. Also stop it when the daemon stops.
   - The state (open documents, latest diagnostics) is a cache inside the process. Losing it only means starting again the next time it is needed.
4. **Start them inside the sandbox** (`internal/sandbox`, `WorkspaceWrite`).
   - The root is the session's working directory.
   - It can write only to the working directory, the temp directory, and that server's cache location (under the user's cache directory; e.g. gopls's `~/Library/Caches/gopls`).
   - The network is cut off. It cannot fetch modules and so on, so if dependencies are not available locally, diagnostics about them may appear.
   - The list of files allowed to start commands (`internal/invariants`) does not change, because they go through `internal/sandbox`.
5. **Language servers are not downloaded automatically.** Only ones on PATH are used. Downloading does not fit a sandbox with the network cut off, and it would install executables on the user's machine without their knowledge.
6. **Start with 5 built-in servers.** Same IDs, extensions and root markers as opencode.
   - `gopls` (`.go`; root is `go.work`, else `go.mod`)
   - `typescript` (`typescript-language-server --stdio`)
   - `pyright` (`pyright-langserver --stdio`)
   - `rust` (`rust-analyzer`)
   - `clangd`

   Others can be added in the configuration.
7. **Configuration goes in `lsp` in `kairocode.json`, in the same form as opencode.**
   - `false` stops all of them.
   - `{"<id>": {"disabled": true}}` stops a server.
   - `{"<id>": {"command": [...], "extensions": [...], "env": {...}, "initialization": {...}}}` adds or changes a server. An ID that is not built in needs `extensions`.
   - Which server to use is decided from the configuration inside `file.apply`, and shows up as is in the result (in line with invariant 2).

## Consequences

- The model learns of type and syntax errors right after writing. It is the same form as opencode, so we can expect the same behavior.
- Each write can take up to 5 seconds. If the server is warm, it is usually under 1 second.
- Tests that enforce this (added at implementation)
  - With a fake language server (the test binary started as a child, returning fixed diagnostics): errors are added to the edit result, and non-ERROR diagnostics and the 21st onward are not added.
  - It gives up at 5 seconds, and the result does not change when there is no server.
  - A server stopped in the configuration is not started.
  - Language servers are started inside the sandbox (the existing check in `internal/invariants`).
  - Crashing right after writing and resuming does not write twice (add to the existing crash test that it passes with diagnostics attached).

## Alternatives considered

- **Download automatically, like opencode:** Less effort. But it does not fit a sandbox with the network cut off. It would also install executables on the user's machine without their knowledge.
- **Run a compiler or check command (`go vet`, `tsc --noEmit`) every time instead of a language server:** Needs separate work per language, is slow, and the output formats differ.
- **Start and stop a language server for each tool call:** No state to hold. But gopls and tsserver take seconds to tens of seconds to start and index, which does not fit within the wait limit.
- **Diagnose everything at the end of the turn:** Only one wait. But the model does not learn before its next step. It also differs from opencode.

## Addendum (2026-10-19): What project configuration can decide

After a point raised by durability-reviewer, the user chose and decided this. Decision 7 is narrowed as follows.

- **Context:** If a project's `kairocode.json` can make any command a language server, code chosen by the repository runs on every file write without confirmation. Even inside the sandbox without a network, this is out of balance with hooks (ADR 0003; asked before they first run).
- **Only in the user's own configuration (`~/.config/kairocode/kairocode.json`):** adding a server, and changing a server's `command`, `extensions` and `env`.
- **Also in the project configuration:** `"lsp": false`, stopping a server (`disabled`), and changing a server's `initialization`.
- Disallowed fields written in the project configuration are not used, and what was not used is logged. The rest of the configuration is used as is.
- Tests: `TestServers`, `TestWriteGetsLSPErrors` (a project trying to change the fake server's `env` has no effect). We confirmed that removing this narrowing makes both fail.

## Implementation (2026-10-19)

- Placed in `internal/lsp`.
  - `Servers` decides which servers to use from the built-in servers and `lsp` in `kairocode.json` (the user's configuration, then the project's; what the project can decide is as in the addendum).
  - `For` picks a server by file extension and decides the root to run it in by the root markers. The root does not go above the working directory.
  - `Manager.Errors` gets diagnostics and turns them into a string in opencode's form.
  - `Format` lists only ERRORs, at most 20. The 21st onward are folded into `... and N more`.
- We wrote the JSON-RPC base protocol for talking to language servers ourselves. No dependency was added.
  - Requests from the server (`workspace/configuration` and so on) get an empty answer.
- Inside `file.apply` (`Runtime.applyWrite`), diagnostics are added after the write goes through.
  - Not added when it could not write because of a conflict and so on.
  - The result with them added is recorded as is.
  - A write that crashed after writing and was resumed is judged "already written". Even then, the diagnostics at that point are taken again and added.
- `Runtime` holds the `Manager` and stops it in `Close`.
  - Inside the daemon, `Runtime` has the same lifetime as the daemon, so the daemon ends up holding the servers.
  - The wait and lifetime constants are `Settle` (150ms), `Wait` (5 seconds) and `Idle` (10 minutes).
- Servers start with `WorkspaceWrite` of `sandbox.Command`, with the network cut off.
  - The only added writable locations are per-server locations under the user's cache directory (for gopls, `gopls` and `go-build`).
  - We confirmed by hand that diagnostics arrive from inside the sandbox with real gopls (0.3 seconds the first time).
- With the environment variable `KAIROCODE_LSP=off`, no server is started.
  - Tests of packages that write files (runtime, server, cli, acp, daemon) set this in `TestMain`, so they do not start gopls and the like on the development machine.
- Tests
  - `internal/lsp`: `TestErrorsOfAFile`, `TestNothingInTime`, `TestIdleServerStops`, `TestServers`, `TestRoot`. We confirmed they fail when the severity filter is removed, and when it settles on the first notification without waiting for quiet.
  - The fake language server (`lsp.FakeServer`) runs by starting the test binary as a child.
  - `internal/runtime`: `TestWriteGetsLSPErrors`, and `TestCrashWriteOnceWithLSP`, which checks it does not write twice with diagnostics attached. We confirmed that removing the code that adds diagnostics makes both fail.
- Fixed after points raised by durability-reviewer
  - Writes to the server also happen within the 5-second deadline. A server that stopped reading input is stopped and started again the next time it is needed (`TestStuckServer`; fails without the deadline).
  - There is one server per combination of configuration content, working directory and root. Two projects with different configurations do not share a process (`TestServerPerSettings`; fails if split only by kind and root).
  - Versioned diagnostics are not used if older than the document's current version (`TestStaleDiagnostics`; fails without it).
  - The write crash test keeps a form without a language server (it runs even without a sandbox), and the form with diagnostics was split into `TestCrashWriteOnceWithLSP`.
