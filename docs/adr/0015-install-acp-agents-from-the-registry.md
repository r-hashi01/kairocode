# 0015 Install ACP agents from the registry in the chat box and use them (/provider)

- Status: Accepted (2026-10-19)
- Date: 2026-10-19
- Related: ADR 0014 (switching providers, the ACP engine), 0002 (ACP), 0005 (sandbox), 0007 (Claude Code engine), AGENTS.md invariants 3, 4, 6 and 7

## Context

The user's request (2026-10-19) is to be able to install ACP agents from the chat box. `/provider` takes on that role.

In ADR 0014, codex and copilot are started as hard-coded commands the user installed themselves (`codex-acp`, `copilot --acp`).

ACP has a registry of agents (`https://cdn.agentclientprotocol.com/registry/v1/latest/registry.json`). Fetched on 2026-10-19, version 1.0.0 had 42 agents. For example `claude-acp`, `codex-acp`, `github-copilot-cli`, `gemini`, `opencode`, `cursor` and `goose`.

Each agent's `distribution` is one of the following.

- `npx`: an npm package. The version is pinned (`@agentclientprotocol/codex-acp@2.2.2` and so on), and it has `args` and `env`.
- `uvx`: a PyPI package.
- `binary`: has an archive URL per platform (`darwin-aarch64` and so on), `cmd`, `args` and `sha256`.

## Decision

1. **The `/provider` list is built from the registry.**
   - The order is as follows.
     - Built-in ones: `anthropic` (API), `claude-code` (ADR 0007's `claude -p`).
     - Agents from the registry: name, version, description, whether installed.
   - The daemon fetches the registry and keeps it in the data directory for 1 hour (`<data>/agents/registry.json`). When it cannot be fetched, the kept copy is used.
   - Ones that cannot run on this machine (no entry for the current platform in `binary`, no node for `npx`, no uv for `uvx`) are shown as "unusable" with the reason.
2. **Install only when the user chooses to.**
   - When an agent that is not installed is chosen in `/provider`, the TUI shows a confirmation. It shows what will be installed, from where, and at which version (the package name or URL). The daemon installs it only when "install" is chosen.
   - Installing is the user's operation, not the model's, so it is not a kairo action (it is configuration work). It uses the network and runs outside the sandbox.
   - **How to install**
     - `npx`: install with `npm install --prefix <data>/agents/<id>/<version> <package@version>`. What starts is that package's `bin`.
     - `uvx`: install into `<data>/agents/<id>/<version>` with `uv tool install`.
     - `binary`: fetch the archive, check its `sha256`, and extract it into `<data>/agents/<id>/<version>`. If it does not match, do not install.
   - What was installed is recorded in `<data>/agents/installed.json` (ID, version, command and arguments to start, environment variables).
   - Upgrading the version also happens only when the user chooses to (the `/provider` list shows "a new version is available").
3. **Installed agents run on ADR 0014's ACP engine.**
   - The engine name is `acp:<registry ID>` (e.g. `acp:codex-acp`).
   - ADR 0014's `codex` and `copilot` stay as aliases of `acp:codex-acp` and `acp:github-copilot-cli`.
   - What starts is the installed command. If not installed, a command of the same name on PATH is used. `KAIROCODE_CODEX` and `KAIROCODE_COPILOT` still work as before.
4. **The agent's own tools go through the user's approval.**
   - Many agents cannot remove their built-in tools. ADR 0014 handles them as follows.
     - KairoCode tools (`kairocode-<name>` or `mcp__kairocode__<name>`, or just the name for copilot with its built-in tools removed) are allowed.
     - Everything else is refused.
   - As is, many agents cannot work. So the other `session/request_permission` requests are routed to KairoCode's approval (asking a person).
     - On the spot it answers `reject_once`, records that it is asking, and ends the turn's round.
     - The workflow waits for approval (the same as ADR 0007's MCP "ask"; `kairo.WaitFor`, invariant 7).
     - If allowed, in the next round it says "try the allowed call again". To a request for the same call (same hash of title and input), it answers `allow_once` for that round only.
     - The allowance is kept in the record, like ADR 0007's `mcp.grant` (invariant 2).
   - **Limit:** The effects of the agent's own tools, once allowed, happen inside the agent. KairoCode's effect types, sandbox and "ask a person if the outcome is unknown" (invariant 4) do not apply. The approval screen says "runs outside KairoCode".
   - The effects of agents that ask KairoCode for file reads and writes and terminals (`fs/*`, `terminal/*`) go through KairoCode's tools, as in ADR 0014.
5. **Secrets**
   - Neither when installing nor when running are KairoCode's secrets (keys in environment variables and so on) passed (`cliEnv`).
   - Each agent's authentication is its own (ACP's `authenticate` is not used; it is left to the agent's sign-in). If it replies that authentication is needed, the user is told so, along with how to sign in to the agent.

## Consequences

- The user can install any ACP agent from the registry with `/provider` in the chat box, and switch to it in the middle of a conversation.
- Installing happens only after the user's confirmation. Binaries are checked by hash.
- The agent's own tools ask a person every time (the same call only for that round). That they run outside KairoCode is shown on screen.
- Tests (added at implementation)
  - With a fake registry (`httptest`) and fake distributions, check the following.
    - How the list is built.
    - The install confirmation.
    - Matching the binary's `sha256` (not installed if it does not match).
    - The install record.
    - Starting with `acp:<id>`.
  - With a fake agent that asks for approval of its own tools, check the following.
    - It waits for approval and releases the process.
    - If allowed, only the same call is allowed in the next round.
    - If refused, it does not run.
    - After a crash and resume, the allowance does not double.
  - TUI: in `/provider`, choosing one that is not installed shows a confirmation, and "install" installs it.

## Alternatives considered

- **Leave installing to the user and use only what is on PATH (the current ADR 0014):** Does not fit the user's request (install from the chat box).
- **Start with `npx -y <package@version>` every time (no install step):** Fewer steps. But the first download happens inside a turn, and the time limit or a network failure becomes a turn failure. What was installed when is not recorded either.
- **Allow every request for the agent's own tools:** Effects happen outside KairoCode without confirmation (the form rejected in ADR 0014's review).
- **Refuse every request for the agent's own tools:** Safe, but many agents that cannot remove their built-in tools cannot work.

## Implementation (2026-10-19)

- **`internal/agents`**
  - Fetching the registry (`Manager.Agents`; kept in `<data>/agents/registry.json` for 1 hour).
  - The list (`List`): marks whether it can run on this machine and what it fetches.
  - Installing (`Install`) and the install record (`installed.json`, `Command`).
- **How to install**
  - `binary`: fetch the archive, check its `sha256`, and extract it with `os.OpenRoot` so it cannot get outside `<data>/agents/<id>/<version>`. It can open `.tar.gz`, `.tgz`, `.tar.bz2` and `.zip`.
  - `npx`: install with `npm install --prefix` and start the package's `bin`.
  - `uvx`: install with `uv tool install`, with `UV_TOOL_DIR` and `UV_TOOL_BIN_DIR` set to that location.
  - Things that look like secrets are removed from the environment variables when installing.
  - Only `internal/agents/` can start the install commands; it was added to `commandStarters` in `internal/invariants`.
  - On failure, that location is deleted.
- **Engine**
  - `acp:<id>` uses ADR 0014's ACP engine via `acp.turn` (`acp:<id>` in `ClaudeTurnInput.Engine`; a new action, so it does not affect the IDs of calls in progress).
  - ADR 0014's `codex` and `copilot` correspond to the registry's `codex-acp` and `github-copilot-cli`. If one was installed from the registry, it is started; otherwise `codex-acp` and `copilot --acp` on PATH are started as before.
  - `github-copilot-cli`, however it is started, is limited to KairoCode's tools with `--available-tools`, and approval requests with only a name are treated as KairoCode tools.
- **The provider list and the daemon API**
  - `Runtime.Providers` adds each registry agent to `anthropic` and `claude-code`. Each agent is marked as usable, installable (and what it would fetch), or with the reason it is unusable.
  - The daemon API is `POST /providers/{id}/install`. Switching to one that is not installed is `ErrNotInstalled` (409).
  - The daemon can change the registry with `KAIROCODE_ACP_REGISTRY` (`off` means do not read it). Tests set it to `off` (invariant 9). The runtime does not read the registry unless given a URL.
- **ACP and TUI**
  - KairoCode's ACP agent adds `kairocode/install` (what it fetches and the version) to `_meta` of the value of a provider that is not installed. Installing is done with the extension method `_kairocode/install_provider` or by typing `/provider install <id>`.
  - In the TUI, choosing one that is not installed in the `/provider` list shows what it fetches and that it "runs outside the sandbox, as the user", and asks for confirmation. Enter installs and then switches; esc goes back to the list.
- **The agent's own tools**
  - A permission request that is not for a KairoCode tool is refused for that round and recorded as an `AskRequest` (the tool is the request's title, the subject is the input, the rule is "runs inside the agent, outside KairoCode's sandbox and record").
  - The workflow releases the process and waits for approval, as in ADR 0007. The answer is recorded with `mcp.grant`.
  - In the next round, the answer is used only once for a request for the same call (same hash of title and input) (`Store.UseGrant`). The answer's scope is within the turn.
  - codex cannot remove its built-in tools, so even a bare-name `write` is asked about as codex's own tool.
- **Tests**
  - `internal/agents`: `TestInstallBinary`, `TestInstallNpm`, `TestInstallAgainKeepsTheOldOne`, `TestCommandOutsideTheInstall`. We confirmed it fails when the `sha256` check is removed.
  - `internal/runtime`: `TestInstallProviderFromRegistry`, `TestAgentOwnToolAsksTheUser` (we confirmed it fails when broken to allow without asking), `TestSwitchProviderAndModel`, `TestCrashACPWrite`.
  - `internal/tui`: `TestProviderAndModel` (install confirmation, esc goes back, Enter installs and then switches).
- **Fixed after points raised by durability-reviewer**
  - **TUI:** The install confirmation screen accepts only Enter (install what was confirmed) and esc (back to the list). Arrows and digits can no longer choose something else (`TestProviderAndModel`).
  - **Record of allowances:** The name of the record of a used allowance is now made from the per-attempt MCP endpoint token for that round and the content hash, not from the call ID the agent decides. After a crash, even if an agent that reloaded the session sends the same request with the same ID, a used allowance is not reused, and it asks again (`TestCrashAgentToolAnswerUsedOnce`; we confirmed that going back to recording by the agent's ID makes it fail without asking again).
  - **Bare-name requests from codex and copilot:** Restored the tests that check them. codex asks (`TestCodexBareNameIsItsOwn`), copilot allows (`TestSwitchProviderAndModel`).
  - **Reinstalling:** Each install goes into a new location (`<id>/<version>-<random>`). What was installed before (which may be running) is not deleted.
  - **Writing records:** `installed.json` and `registry.json` are written to a temp file and then renamed.
  - **Version drift:** The checked version is passed along (`version`), and if the registry's version differs, it does not install (`ErrChanged`).
  - **The command to start:** The registry's `cmd` and npm's `bin` can no longer point outside what was installed (`filepath.IsLocal`).
- **Fixed after using it (2026-10-19, the user's report: "not installed: codex (-32602). Can't choose it, can't install it either")**
  - **Typed names:** `/provider codex` did not map to the registry ID (`codex-acp`), no install confirmation appeared, and it ended with a raw error. The TUI matches the typed name against ID, name, alias (`codex`, `copilot`, `claude`), and partial match, in that order. If not installed, it shows the confirmation; if several match, it opens the list narrowed down. `/provider install codex` also installs by alias.
  - **List:** It was hard to search among 44 entries, so typing now filters it, and it is ordered usable, installable, unusable. Choosing by digit was dropped (it does not work with more than 9 entries).
  - **Error text:** The error for switching to one that is not installed now says how to install it (`/provider install <id>`) and what it fetches.
  - **`-provider codex-acp`:** Provider names are now turned into engines when creating a session and when opening the daemon. Sessions with an unknown agent (not installed, not on PATH or in the registry) are not created.
  - **Check with the real thing:** Installed `codex-acp` 2.2.2 from the real registry with `npm` and ran 1 turn with `kairocode run -provider codex-acp`. codex created `hello.txt` with the KairoCode tools in the MCP we passed (`list`, `write`, `read`).
  - Fixed in review:
    - Model names are not chosen by partial match; they are used exactly as typed.
    - The daemon no longer opens with an unknown engine as its default (a typo in `KAIROCODE_ENGINE`).
    - When creating a session, it checks that the agent exists. For that, it reads the kept copy of the registry. Only when there is no copy does it fetch, and it waits up to 5 seconds.
    - `-provider claude` is treated as `claude-acp`.
  - Tests: `TestProviderByNameAndFilter` (TUI), `TestEngineAndModelPerSession`.
- **Not yet:** Upgrading versions (showing that a new version exists). Cleaning up old versions' locations. (Authentication was added in the addendum below.)

## Addendum: Sign-in state and `/login` (2026-10-20)

Added in response to the user's point: "ACP has authentication, right? It doesn't even show whether I'm signed in or not, so I can't tell."

- **State** (`internal/runtime/auth.go`)
  - Each provider has `signed-in`, `needed` or `unknown`. It is kept only in the daemon's memory and not put in the record. It is not used for workflow branching, so invariant 2 does not apply.
  - How the state is decided, per provider:
    - API: decided by credentials in the environment (`ANTHROPIC_API_KEY` and so on; the value is not read, only whether it exists) and by whether model calls succeed.
    - Claude Code: decided by `claude auth status --json`.
    - codex: decided by `codex login status`. The CLI's answer is remembered for 1 minute.
    - ACP agents: decided by the result of actually running them. If `session/new` returns `auth_required` (-32000), `needed`; if a session could be created, `signed-in`.
  - On `auth_required`, the turn ends with "<agent> needs you to sign in first: /login shows how". It does not retry.
- **Ways to sign in:** Built from `authMethods` in ACP's `initialize`.
  - Ones with `_meta.terminal-auth` (copilot's `copilot login` and so on) are commands to run in the terminal.
  - Ones with `type: "terminal"` (running the agent itself in the terminal) are not shown.
  - For the others (codex's ChatGPT, API key), the daemon calls ACP's `authenticate`. The agent may open a browser, so it waits up to 10 minutes.
  - An agent that has not run yet is started once in a temp directory to ask for its methods.
- **API**
  - Daemon: `GET` and `POST /providers/{id}/login`.
  - ACP: the state goes into `_meta` (`kairocode/auth`) of the provider's value. The extension methods are `_kairocode/login_methods` and `_kairocode/login`.
  - Typed text `/login [provider] [method]` is also accepted (for clients such as Zed that do not have their own `/login`). For terminal commands, it tells the user to type them.
- **TUI**
  - The `/provider` and `/model` lists show `signed in` (green) and `sign-in needed  /login <id>` (yellow).
  - The status line adds `(sign in: /login)` if the current provider is `needed`.
  - `/login [provider]` shows the methods. If there is one, it goes ahead; if there are several, it lets you choose.
  - Terminal commands run with the screen handed over via `tea.ExecProcess`, and when they finish, the daemon is told (the state goes back to `unknown` and is decided the next time it runs).
  - The only commands the TUI starts are sign-in commands the user chose. `internal/tui/login.go` was added to the invariant check's "files allowed to start commands" (`commandStarters`).
- **Tests**
  - `TestSignInToAnAgent` (runtime): a fake ACP agent returns `auth_required`; it checks reading the methods, `signed-in` after `authenticate`, and that the next turn runs.
  - `TestSignIn` (TUI), `TestACPLogin` (ACP).
- **Fixed after points raised by durability-reviewer**
  - **Show terminal commands before running them:** The command is passed by the agent in `initialize` and runs outside the sandbox. Even if there is only one method, `runs: <command>` is shown in the list, and it runs only after the user chooses it with Enter (`TestSignInCommandIsShownFirst`). The `env` of `terminal-auth` is not passed yet.
  - **Do not treat every -32000 as sign-in:** Agents also use -32000 for other failures. Only when the message talks about signing in is sign-in considered needed. Otherwise it retries as before (`TestAuthRequired`).
  - **How `withAgent` stops:** Even if something the agent opened (a browser and so on) keeps the output open, it stops reading after 5 seconds (same as ACP turns).
  - **Slowness when creating a session:** The sign-in check (`claude auth status` and so on) is done only when showing the list (`Providers`). It is not done when creating a session or when switching.

## Addendum: codex's own shell is off, and its sessions are read-only (2026-11-06, the user's decision)

- **What we found (codex 0.164, codex-acp 2.2.2, with the real model, M7's demo):** decision 4 counted on an agent asking (`session/request_permission`) before its own tools act. codex-acp's default mode (`agent`, "auto review") asks only for what it takes to be unsafe: codex ran `echo … >> ledger.txt` with its own shell, unasked. Given the same request twice, codex used KairoCode's `bash` once and its own shell once. What its own shell did was not recorded, and a command cut off by a crash was not asked about: the guarantees did not hold on codex.
- **Decision**
  - **codex's own shell is off:** `CODEX_CONFIG` gets `features.shell_tool = false` and `features.unified_exec = false`. The model's only shell is KairoCode's `bash`, recorded and never run twice. Checked without calling a model: `codex debug prompt-input` lists no `exec_command` with them off.
  - **Its sessions are read-only:** codex-acp is started with `INITIAL_AGENT_MODE=read-only`, and each session (new or loaded) is set to the `read-only` mode (`session/set_config_option`, `mode`). What is left of its own tools (`apply_patch`, …) asks before it writes a file, runs what changes anything, or reaches the network; the asking comes to KairoCode as decision 4 says. If the mode is not taken, the turn does not go on. Reading stays unasked: it has no effect.
  - **Codex asks for KairoCode's MCP calls too, by id:** in the read-only mode codex asks before each MCP call (`_meta.is_mcp_tool_approval`), with no title, only the id of the tool call it announced before (`session/update`, `tool_call`, titled `mcp.kairocode.<tool>`). KairoCode keeps each call announced in this run (not those replayed by `session/load`) by its id, and a request marked so takes its title from there; a call of its own MCP server is allowed (the tool checks it as it runs, as before). A request with an id not announced is asked about. `mcp.kairocode.<tool>` is read as KairoCode's, like `kairocode-<tool>` and `mcp__kairocode__<tool>`.
  - **If codex does not take the mode** (an agent that has no such mode), the turn ends before codex is prompted, saying why. If codex stopped instead, the step is done again, as at its other steps.
- **What we checked with the real codex:** `scripts/demo-engines.sh codex` (five `bash` steps, the daemon killed in the middle of one) passed three times out of three: every step through KairoCode's `bash`, the step cut off asked about, none run twice. Asked to use its own `apply_patch`, codex asked first ("Edit files"), and nothing was written.
- **Remaining limits:** what codex does with its own tools once a person allows it is still outside KairoCode's record (decision 4's limit). With `agents.ownMCP`, a server of the user's own codex settings named `kairocode` would be taken for KairoCode's. The settings named here are codex's own: `scripts/agent-probe.sh` checks that its shell is off, without calling a model, and is run when codex is upgraded.
- **Tests:** `TestCLIAgentsConfig` (the features and `INITIAL_AGENT_MODE`), `TestSwitchProviderAndModel` (codex's sessions, new and loaded, set read-only; copilot's not; the fake agent forgets the mode on load, as codex-acp does), `TestCodexAsksByID` (a call asked for by id is KairoCode's when announced so, and asked about when it is codex's own or its id was not announced), `TestCodexModeRefused` (not prompted when the mode is refused). We checked that the tests fail without the mode or the lookup by id.
