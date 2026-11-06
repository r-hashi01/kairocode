# 0022 The user's MCP servers, custom tools, todo

- Status: Accepted (2026-10-21)
- Date: 2026-10-21
- Related: requirements FR-5 (todo), FR-8 (MCP) and FR-9 (custom tools), design section 3 (the table of effects), invariants 3, 4 and 6, ADR 0002 (opencode's settings), 0005 (sandbox), 0009 (language server lifetime), 0018 (project-defined things need approval)

## Context

KairoCode passes its own tools to CLI engines over MCP (ADR 0007). On the other hand, KairoCode's agents cannot use the tools of the user's own MCP servers (GitHub, Linear and so on) (FR-8). There is also no way for the user to add tools (FR-9), and no todo (FR-5).

Every tool declares its effect type, and without a declaration it is treated as "real" (invariant 3). MCP servers do not declare the effects of their tools (MCP has annotations such as `readOnlyHint`, but they are the server's own claim).

MCP servers and custom tools run commands on the user's machine. Those defined by a project's settings are commands run by a cloned repository, so, like ADR 0018's hooks, they must not run outside the sandbox without approval.

## Decision

1. **MCP server settings are read in opencode's form.** It is `mcp` in `kairocode.json` (the same form as `mcp` in opencode's `opencode.json`. ADR 0002).
   - `{"<name>": {"type": "local", "command": ["..."], "environment": {...}}}` and `{"type": "remote", "url": "...", "headers": {...}}`. Entries with `enabled: false` are not used.
   - `mcpServers` in Claude Code's `.mcp.json` and `~/.claude.json` is also read (in line with ADR 0018's "read each agent's settings").
2. **Effect types are declared in KairoCode's settings.** Write them per tool in `tools` in `kairocode.json`.
   - Example: `{"tools": {"github_search_issues": {"effect": "read"}, "github_*": {"effect": "real"}}}`. Names are `<server>_<tool>`, and `*` can express a prefix match.
   - Without a declaration it is `real` (invariant 3). MCP's `readOnlyHint` is not used in place of a declaration (it is the server's own claim). It is used in the text that suggests a declaration to the user.
   - For tools declared `idempotent`, KairoCode does not know how to check an unknown outcome. Only `read` is retried automatically; `idempotent` is treated the same as `real` for MCP tools (a person is asked).
3. **Permissions:** rules can be written by MCP tool name, like any other tool (`{"permission": {"github_*": "ask"}}`). The default is `ask`. The target of the decision (`subject`) is the arguments' JSON.
4. **Where they run:**
   - `local` servers in the user's settings (`~/.config/kairocode/kairocode.json`, `~/.claude.json`) are the user's commands, so they run outside the sandbox (treated the same as the user's own `bash` settings).
   - `local` servers in project settings (`kairocode.json`, `.mcp.json`) are asked about the first time they are used, with the same question as ADR 0018's hooks (in KairoCode's sandbox / outside the sandbox / do not run). The answer is recorded per directory, name and command, and asked again when the command changes. The sandbox cuts off the network, so for most servers the choice will be "outside".
   - `remote` servers are connected from KairoCode's process. For a project's `remote`, the destination is asked about (so the user's authentication headers are not sent to a destination the project picked).
5. **Lifetime:** MCP server processes are held in the daemon, one per (working directory, name), and stopped after a while unused (the same form as ADR 0009's language servers). The tool list is recorded at the start of a turn by `agent.load` (even if a server's tools change during a turn, that turn proceeds with the list from its start. Invariant 2).
6. **Secrets:** values of `environment` and `headers` are read inside actions and not kept in the record (invariant 6). Environment variables can be referred to in the `{env:NAME}` form (the same as opencode).
7. **Custom tools:** written as commands in `tools` in `kairocode.json`.
   - Example: `{"tools": {"deploy_preview": {"description": "...", "schema": {...}, "command": ["./scripts/preview.sh"], "effect": "real"}}}`. The input JSON is passed on standard input, and standard output is the result.
   - `effect` is required (FR-9). Ones without it are not loaded, and the reason is shown to the user (`/tools` or a loading warning).
   - They run the same way as MCP `local` servers. Ones from the user's settings run outside the sandbox; ones from project settings are asked about the first time they are used.
8. **todo:** added as a built-in tool (`internal/tools/todo.go`, effect `read`).
   - It receives the whole list (contents, state: not started, in progress, done) and returns it as the result as is. It stores nothing. The list stays in the turn's record (the tool result) (design section 3's "include it in the turn's output").
   - Clients show the result of the session's last todo. In ACP it is sent as a `plan` update (`sessionUpdate: "plan"`).
   - In the next turn, the last list is in the conversation, so the model can continue from it.

Where this shows in the implementation: `internal/runtime` (managing MCP clients, recording tools in `agent.load`, `tool.<name>` actions, running custom tools, reading effect declarations, recording questions), `internal/tools/todo.go`, `internal/opencode` (reading `mcp`), `internal/acp` (`plan` updates), `internal/tui` (showing todo), `internal/invariants` (allowing files that may launch commands).

## Consequences

- The user's MCP server tools can be used under effect types and permission rules. Without a declaration they are treated as real, and if the outcome is unknown a person is asked (invariants 3 and 4).
- Servers and tools defined by a project do not run outside the sandbox without approval.
- Tests (added during implementation)
  - A fake MCP server's (the test binary) tool can be called as a declared `read`. An undeclared tool is treated as real, and crashing while it is being called makes it blocked (needs confirmation) (`TestCrashMCPServerTool`).
  - A declaration like `github_*` takes effect as a prefix match.
  - A project's MCP servers and custom tools are not started before asking. Answering "do not run" means they are not started.
  - The values of `environment` are not kept in the record.
  - A custom tool without `effect` is not loaded.
  - todo results arrive as ACP `plan` updates.

## Alternatives considered

- **Use MCP's `readOnlyHint` as the effect declaration:** no declaring effort, but the server's own claim could cause a real effect to be treated as read-only by mistake (invariant 3).
- **Write custom tools as TypeScript files, like opencode:** convenient for opencode users, but KairoCode is a single Go binary, so it would need a way to run TypeScript. In command form, they can be written in any language.
- **Store todo in a KairoCode table:** holding state adds to the record. Kept as a tool result, no new storage form is needed.

## Implementation

### 1. todo (2026-10-21)

- Added `todo`, with effect `read`, in `internal/tools/todo.go`. It receives the whole list and returns it as marked text (`[x]` done, `[>]` in progress, `[ ]` not started). Items whose state is none of the three are returned as a failure.
- The default permission rule is `allow` (it only shows a list).
- When a `todo` call starts, the ACP agent sends the list as the session's `plan` update (Zed and others show it as a plan). The terminal UI shows the tool result (the marked list).
- Tests: `TestTodo`, `TestACPTodoPlan`.

### 2 and 3. MCP servers and custom tools (2026-10-21)

- **Settings read (`externals` in `internal/runtime/external.go`):** the user's `kairocode.json`, `~/.claude.json` (`mcpServers`), the project's `kairocode.json`, `.mcp.json`. Later ones are closer to the project.
  - `mcp` is read in opencode's form (`command` and `environment` for `local`, `url` and `headers` for `remote`; `enabled: false` entries are not used). `mcpServers` is read in Claude Code's form (`command`, `args`, `env`, `url`, `headers`).
  - Entries in `tools` with `command` are custom tools; those without are effect declarations for MCP tools.
- **Effect types (`effectOf`):** an exact name match takes precedence over a prefix match ending in `*`. Among prefix matches, the longer one wins. Only `read` is treated as a read-only tool; everything else (no declaration, `idempotent`, `real`) is real. A project's declarations apply only to tools of the project's servers (so a project cannot say the user's servers' tools are "read-only").
- **The turn's flow**
  - For the API engine, `agent.load` records the servers and custom tools in the settings as `Agent.External`.
  - The turn decides how to run the project's ones in `loadExternal` (`internal/workflows/hooks.go`). It uses the same question and record as ADR 0018's hooks (`hook.check`, `hook.decide`), with names `mcp:<name>` and `tool:<name>`. For remote servers it asks "connect / do not connect". The user's own are not asked about and run as the user's settings say (outside the sandbox).
  - Next, `ext.load` starts the servers, lists the tools, and records each tool's definition (server name, tool name, effect type, how to run it. Without the values of environment variables and headers) in `kc_message`. It also adds to the tool list passed to the model.
  - Calls run with the recorded definition as `tool.ext.read` (unprotected) or `tool.ext.real` (real). The permission decision also judges plan mode by the effect in the recorded definition.
  - Turns without `Agent.External` (earlier records, no settings) call nothing, so the turn's version was not bumped.
- **Custom tools:** the input JSON is passed on standard input, and standard output is the result. A nonzero exit code is returned as a failure. It is cut off after 2 minutes, and a real tool that was cut off becomes "unknown outcome". Ones answered to run in the sandbox run in KairoCode's sandbox; others run outside.
- **MCP server processes:** held in the daemon, one per (working directory, settings location, name, how it runs), and stopped after 10 minutes unused. `environment` and `headers` are read when starting and connecting, with `{env:NAME}` replaced by the environment variable's value. They are not kept in the record. Servers run outside the sandbox also get the user's environment variables as they are (the same as when the user runs them).
- **invariants:** `internal/runtime/external.go` was added to the files allowed to run things outside the sandbox (`fullAccessUsers`).
- **What was fixed after durability-reviewer's findings (all about approval and secrets)**
  - **Approval could be slipped past with a server of the same name:** how to run was looked up by name only, so a project server with the same name as a user server could start under the user's "outside". Now the positions the turn recorded are matched, and the answer is used only for one whose settings location, name and fingerprint are the same.
  - **A command changed after approval could start:** settings were reread at start time, so while waiting for the answer, or after the model rewrote the file, a different command could start under the old answer. Now the fingerprint of the whole server setting (command, URL, values of environment variables and headers) is recorded (`ExtSource.Spec`). The fingerprint is also in the question's key, so a changed one is asked about again. Only one with the same fingerprint is started; if it differs, it is returned as a failure without calling. The values themselves are not recorded.
  - **Project settings could leak the user's secrets:** `{env:NAME}` was also expanded in a project server's environment variables, headers and URL. Now it is expanded only in the user's settings, and a project's are passed as written. The question shows the names of the environment variables and headers to be passed.
  - **Secrets could end up in records and logs:** a remote server's URL is recorded and shown without its credentials and query parts. The URL is also removed from connection failure messages (`safeErr`).
  - **Connection races:** the timer that stops unused servers could close a connection just handed out or a new one. On a call failure, it could also close the connection of other calls to the same server. Now it only stops the same connection, and only when it has been unused for a while.
  - The permission decision looks up the recorded definitions of outside tools before built-in tools.
  - When Claude Code's `~/.claude.json` or `.mcp.json` has a form that does not match, that file is skipped (before, all API engine turns failed).
  - Tests: `TestProjectMCPServerOfTheUsersName`, `TestProjectMCPServerChanged`, `TestProjectMCPServerGetsNoUsersValues`.
- **Not yet**
  - Using MCP's `readOnlyHint` in the text suggesting a declaration is not done yet.
- **Tests:** `TestMCPServerTool`, `TestCustomTool` (ones without an effect type are not passed), `TestProjectMCPServerAsks` (not started before asking. Not started if "do not run"), `TestMCPEnvironmentNotRecorded` (no values left in the DB), `TestCrashMCPServerTool` (crashing in the middle of a call of an undeclared tool asks a person instead of calling again. It was called once).

### 4. CLI engines (2026-10-22)

- `agent.load` records outside MCP servers and custom tools on every engine. As with the API engine, the turn decides how to run them at its start (`loadExternal`) and lists them.
- To CLI engines (Claude Code, codex, copilot), the outside tools are passed on KairoCode's MCP server together with the built-in tools (`ClaudeTurnInput.Ext`, `mcpServer.open`). The route is the same as before: `--mcp-config` for Claude Code, ACP's `mcpServers` for codex and copilot. The user's servers are not passed to the agent directly (if passed directly, calls would not go through KairoCode, and effect types, blocked (needs confirmation), permissions, the sandbox and project approval would not apply).
- Calls carry the recorded definition in `MCPCallInput.Ext` and run as `tool.ext.read` or `tool.ext.real`, as with the API engine. Asking when Claude Code makes the same real call again after a crash (ADR 0007) applies to outside tools too.
- The new fields (`ClaudeTurnInput.Ext`, `MCPCallInput.Ext`) are `omitempty`, so IDs of records without outside tools do not change.
- Tests: `TestMCPServerToolOnClaudeCode` (passed to Claude Code as `mcp__kairocode__fake_lookup`, and the call runs), `TestCrashMCPServerToolOnClaudeCode` (crashing while Claude Code is calling an undeclared tool: even if the resumed Claude Code makes the same call, it is not called again but a person is asked. It was called once).

### 5. codex's own MCP servers and `/mcp` (2026-10-22)

- **What we checked (codex-acp 2.2.2. No model calls)**
  - In a throwaway `CODEX_HOME`, we wrote an MCP server that only creates a marker file when started. ACP's `authenticate` was passed with a fake API key. Then only `session/new` was sent to codex-acp.
  - **codex, even over ACP, starts the MCP servers in its own settings when creating a session.** Both those in the user's `config.toml` and those in the working directory's `.codex/config.toml` were started. They were started even in a folder codex had never trusted.
  - With `mcp_servers.<name>.enabled = false` in the `CODEX_CONFIG` KairoCode passes, that server does not start. `mcp_servers = {}` does not stop them.
- **Decision (the user's decision, 2026-10-22):** KairoCode stops the MCP servers in codex's own settings.
  - Those in the project's `.codex/config.toml` are always stopped. A cloned repository's commands would run outside the sandbox without approval (the same kind as copilot's repository hooks, ADR 0018 section 6).
  - Those in the user's `~/.codex/config.toml` (`CODEX_HOME`) are also stopped by default. Their calls do not go through KairoCode (effect types, blocked (needs confirmation) and permissions do not apply). A server the user wants to use can be written in `mcp` in `kairocode.json`, and then it is used through KairoCode on any engine.
  - `{"agents": {"ownMCP": true}}` in the user's `kairocode.json` leaves only the user's own unstopped. The project's stay stopped.
- **Implementation:** `codexMCPOff` (`internal/runtime/agenthooks.go`) reads server names from the project's `.codex/config.toml` (from the root to the working directory) and the user's `config.toml`. `acpArgv` adds `enabled = false` for each to `CODEX_CONFIG`.
  - No TOML-parsing dependency was added; names are read from the forms `[mcp_servers.<name>]`, `[mcp_servers."<name>"]`, `mcp_servers.<name>.<key> =`. If `mcp_servers` is written in another form (`mcp_servers = { ... }` and so on), they cannot be stopped, so codex is not launched.
  - Even if codex's hooks are allowed with `agents.ownHooks`, MCP servers are stopped (a separate setting).
- **`/mcp`:** an ACP built-in command (the server's `GET /sessions/{id}/mcp`, `Runtime.MCP`). It shows:
  - For each MCP server, whether it is the user's or the project's, how it runs (in the sandbox, outside, do not run, not yet asked), whether it connected, and its tools with their effect types.
  - Custom tools (ones without an effect type, with the reason they are not loaded).
  - codex's own servers (that they are stopped).
  - Project servers not yet asked about are not started even for the list.
- **Tests:** `TestCodexOwnMCPServersOff` (the project's are always stopped, the user's are stopped unless `ownMCP`. With an unreadable form codex is not launched), `TestMCPList`.
- **What was fixed after durability-reviewer's findings**
  - **Misreading names stopped a different name:** TOML has many ways to write a table name (`'…'`, surrounding spaces, escapes, quoting `mcp_servers` itself as in `"mcp_servers".x`, and so on). The first reader read different names from those, stopped names that did not exist, and the real server could start. A cloned repository's settings can use any of those forms.
    - Now only the plain forms are read (`[mcp_servers.<name>…]`, `[mcp_servers."<name>"…]`, `mcp_servers.<name>.<key> =`. Names are letters, digits, `_` and `-`; inside quotes, spaces and `.` too).
    - Any other line containing `mcp_servers`, and table names and keys containing escapes, are all errors, and codex is not launched.
  - **Error messages could contain secrets:** the message gives only the file and line number, not the line's contents (because it is recorded as the turn's reply).
  - Servers the user wrote in `CODEX_CONFIG` themselves are also stopped as the user's (unless `ownMCP`).
  - The location of codex's settings is decided by the environment passed to the agent (`CODEX_HOME`, or else `.codex` in its `HOME`). `/mcp` looks at the same place.
  - It crashed when `CODEX_CONFIG` was `null`.
  - Tests: `TestCodexMCPNamesStrict`, `TestCodexConfigServersOff`.
- **Not checked:** codex's system settings (`/etc/codex` and so on) are not read.

### 6. Flows and subagents (2026-10-26)

- A flow's `tool(...)` also lists outside tools when it reads the session agent's definition (the first `tool()`) (`loadExternal`), and runs calls of outside tools with `tool.ext.read` and `tool.ext.real`.
- Turns of a flow's `agent(...)` and of subagents (`task`) also have outside tools (before, `Sub` turns did not read them).
- In a try run (ADR 0017), outside tools are not loaded (no questions, no servers started).
- In every case, calls are added only when the recorded `agent.load` has outside settings, so the version was not bumped.
- Tests: `TestFlowUsesMCPServerTools`.

## Addendum: codex's plugins are off with its MCP servers (2026-11-06, the user's decision)

- **What we found (codex 0.164):** codex's plugins bring MCP servers of their own (`codex mcp list` showed `codex_app` and `cua_repl` from plugins), which codex starts by itself, outside KairoCode. The settings of a plugin's server (`[plugins."<id>".mcp_servers.<name>.tools.js]`) made `codexMCPNames` refuse to start codex, while a plugin with no such line went through: the servers ran either way.
- **Decision:** unless `agents.ownMCP`, KairoCode starts codex with `features.plugins = false`, like its own MCP servers. Checked: `codex -c features.plugins=false mcp list` lists no plugin's servers. With plugins off, the tables under `[plugins.…]` name no server that runs, and are passed over; with `agents.ownMCP` (plugins on), such a table in a project's `.codex/config.toml` still keeps codex from starting.
- This changes ADR 0018's "what an agent adds as its own (plugins …) stays" for codex's plugins: their servers' calls would not go through KairoCode.
- **Tests:** `TestCodexOwnMCPServersOff` (plugins off without `ownMCP`, on with it; a plugin's table in the user's settings read, in a project's with plugins on not), `TestCodexMCPNamesStrict`, `TestAgentsOwnHooks`.
