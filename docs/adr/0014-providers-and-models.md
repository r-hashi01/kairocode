# 0014 Switch providers and models in the middle of a conversation (/provider, /model)

- Status: Accepted (2026-10-19)
- Date: 2026-10-19
- Related: item 2 of design section 11 (the first provider), ADR 0002 (opencode as the model), 0007 (Claude Code engine), 0008 (engine and model are per session), 0010 (TUI), AGENTS.md invariants 2, 4, 6 and 8

## Context

The user's request (2026-10-19) is to switch between Claude, Codex and Copilot, and to switch models too, as in opencode. In the chat box, `/provider` and `/model` switch them.

The user chose these 2 things.

- How they run: "both". First make it possible to use the user's CLIs (`codex`, `copilot`) as engines (the same shape as ADR 0007), and later add a form where KairoCode calls the API directly.
- Switching takes effect from the next turn, even in the middle of a conversation.

Now, the engine (`api`, `claude-code`) and the model are decided by `SessionInput` when the session is created, and do not change midway (ADR 0008's addendum).

CLIs checked on this machine (2026-10-19):

- `codex` 0.163.0
  - Has `codex exec [--json] [-m <model>] [-s read-only] [-C <dir>] [-c mcp_servers.<name>.url=…] <prompt>` and `codex exec resume <id>`.
  - Codex decides the session ID and returns it in a `--json` event.
  - The built-in shell cannot be removed. The sandbox (`-s read-only`) can stop writes.
- `copilot` 1.0.54
  - Has `copilot -p <prompt> --output-format json [--model <model>] --session-id <uuid> --additional-mcp-config <json> --available-tools <tool>…`.
  - `--available-tools` can narrow it to only KairoCode's MCP tools.
  - With `--session-id`, KairoCode can decide the UUID of a new session (same as claude).

## Decision

### 1. The list of providers

| ID | How it runs | Authentication | Phase |
|---|---|---|---|
| `anthropic` | API (the current `api` engine) | API key (environment variable) | done |
| `claude-code` | CLI engine (ADR 0007) | The user's Claude Code | done |
| `codex` | CLI engine (`codex exec`) | The user's Codex (ChatGPT sign-in or API key) | 1 |
| `copilot` | CLI engine (`copilot -p`) | The user's Copilot CLI (GitHub sign-in) | 1 |
| `openai` | API (OpenAI's Responses API) | API key (`OPENAI_API_KEY`) | 2 |
| `github-copilot` | API (Copilot's chat completions) | GitHub sign-in (how to authenticate is decided in an addendum to this ADR before phase 2) | 2 |

- The current `engine` values (`api`, `claude-code`) are accepted as is, as aliases of provider IDs. `api` is the same as `anthropic`. Existing records and configuration can be read as they are.
- A model is a name within a provider. `/model` chooses a model of the current provider. In the configuration it can also be written as `<provider>/<model>` (same as opencode).

### 2. Switching is recorded as an event to the session (invariant 2)

- `/provider <ID>` and `/model <name>` are a new event to the session, `EventSettings{Provider, Model}`. The session workflow receives it as a recorded value and changes `Engine` and `Model` of `TurnInput` from the next turn on.
- A turn in progress finishes with the values it started with.
- The session workflow version goes up (6), and the current version 5 is kept with `kairo.Draining()`.
- The turn child's ID includes `Engine` and `Model` in its input, so it differs before and after a switch. This follows invariant 8 (only recorded values go in).

### 3. Carry the conversation across engines

- API engines (`anthropic`, and later `openai` and `github-copilot`) hold the conversation as a chain of `kc_message` (ADR 0006).
- CLI engines hold the conversation in each CLI's own record (transcript). KairoCode's chain keeps each turn's user message and final response (ADR 0007).
- **When switching to a CLI engine,** start a new session of that CLI. Before the first prompt, attach the conversation so far (only the text of the user messages and responses in the chain) as "the conversation so far". The contents of tool calls are not attached.
  - The CLI session's ID is decided from KairoCode's session ID, the provider, and which switch it is (invariant 8). Returning to the same CLI also gets a new session.
- **When switching to an API engine,** keep using the chain as before. Turns from while it was a CLI engine have only the user messages and responses in the chain, so it just continues.
- When only the model changes (same provider), the conversation stays as is. For a CLI engine, `--model` changes and the same CLI session continues.

### 4. The codex and copilot engines (phase 1): KairoCode calls them as an ACP client

(Rewritten on 2026-10-19 by the user's decision. The first draft started each CLI non-interactively (`codex exec`, `copilot -p`) and read its JSON output. See the addendum below.)

KairoCode becomes an ACP (ADR 0002) client, starts each agent as a child process, and talks to it over ACP.

| Provider | What it starts | Notes |
|---|---|---|
| `codex` | `codex-acp` (Zed's ACP adapter; can be changed with `KAIROCODE_CODEX`) | The user installs it (npm) |
| `copilot` | `copilot --acp` (`KAIROCODE_COPILOT`) | The Copilot CLI speaks it directly |

`claude-code` stays in the shape of ADR 0007 (`claude -p` and MCP) as before. A form that uses Claude's ACP adapter (`claude-code-acp`) will be added if it becomes necessary.

- **Turn**
  - A turn is one action (`codex.turn`, `copilot.turn`; effect `Unprotected`).
  - Its contents are, in order: start the agent and send `initialize`. Create or load the session (`session/new` or `session/load`), choose the model (`session/set_config_option`), and send `session/prompt`. Collect the answer (`agent_message_chunk`) while streaming it with `TaskContext.Emit`, and record it when done.
- **Session**
  - On the first turn, send `session/new`, and record the returned session ID in `kc_engine_session` under a name KairoCode decides (session, provider, which switch it is; invariant 8).
  - From the next turn on, continue with `session/load`. If the agent does not have `loadSession`, use a new session every turn and pass the conversation so far with it (3).
- **Effects go through KairoCode's tools**
  - KairoCode offers `fs.readTextFile`, `fs.writeTextFile` and `terminal` as client capabilities.
  - File writes (`fs/write_text_file`) and terminals (`terminal/create`) from the agent are run as KairoCode's `write` and `bash` tools. It is the same mechanism as ADR 0007's MCP calls: the run ID follows invariant 8, and the effect type, permission decision, approval, blocked (needs confirmation) and sandbox all apply as they are.
  - Reads (`fs/read_text_file`) are read on the spot, only inside the working directory.
  - KairoCode's MCP server is also passed in `mcpServers` of `session/new` (for the other tools).
- **Approval**
  - To the agent's `session/request_permission`, if it is a call to a KairoCode tool, answer `allow_once` on the spot. The actual effect is decided where it goes through KairoCode's tool, as above. Other calls are refused (the addendum in the implementation section).
  - A call that came to "ask" there is, as in ADR 0007, returned to the agent as waiting for approval. The turn's round ends, the workflow waits for approval, and the answer is passed in the next round's prompt.
- **Resuming after a crash**
  - A turn marker is added to the end of the prompt.
  - On resume, if the history returned by `session/load` (`user_message_chunk`) has the marker, it does not resend the prompt but sends "continue".
- **Limits**
  - If an agent writes files or runs commands itself without asking the client, those effects do not remain in KairoCode's record.
  - Whether each agent works by asking will be checked with the real agents at implementation and added to this ADR.
- **Files that start commands**
  - Only the ACP engine file in `internal/runtime` starts them (added to `commandStarters` in `internal/invariants`).
  - For JSON-RPC, the same code as KairoCode's ACP agent (`internal/acp`) is split into `internal/jsonrpc` and used.

### 5. `/provider` and `/model` in the chat box (ADR 0010's TUI, ACP)

- `/provider` shows the list (ID, how it runs, whether it is usable) and you choose with the arrows. `/provider <ID>` chooses directly.
  - "Usable" means, for a CLI engine, whether the command exists, and for an API, whether there is a key.
- `/model` shows the list of the current provider's models to choose from. `/model <name>` chooses directly.
  - The list comes from the daemon's `GET /providers` and `GET /providers/{id}/models`.
  - For an API provider, it is that API's list of models.
  - For a CLI engine, it is a known list held by KairoCode, plus free input.
- The status line shows the current provider and model.
- In ACP, these map to session settings (`session/set_model` and modes).
- `-provider` and `-model` of `kairocode run` and `chat` decide the values at the start of the session. The environment variables `KAIROCODE_ENGINE` and `KAIROCODE_MODEL` also still work.

### 6. Phase 2: API providers

- `openai`: Call OpenAI's Responses API directly with `net/http` (no added dependency). The key is read from an environment variable inside the action (invariant 6).
- `github-copilot`: Call Copilot's API. How to authenticate with GitHub will be decided in an addendum to this ADR, after checking the terms of use and which OAuth app to authorize with.
- Both are adapters in `internal/provider`. They map tool calls to the current `msg` shape.

## Consequences

- In the middle of a conversation, you can switch between Claude, Codex and Copilot and their models. Switches are recorded, so after a crash and resume it continues with the same provider and model.
- With CLI engines, each subscription can be used as is. But model calls inside the loop do not remain in KairoCode's record (tool calls do; same as ADR 0007).
- Tests (added at implementation)
  - With a fake `codex` and a fake `copilot` (the test binary started as a child, emitting fixed JSON events and calling MCP tools), a turn goes through.
  - Writing tools run only once through MCP (even after a crash and resume; `TestCrash...`).
  - Turns after `/provider` and `/model` run with the new engine and model. A turn in progress finishes with its starting values.
  - Switches are recorded and are the same after resume. Version 5 sessions run to the end.
  - When crossing engines, the text of the earlier conversation goes into the first prompt of the new CLI session.
  - Choosing with `/provider` and `/model` in the TUI, and the status line display.

## Alternatives considered

- **Switch only from a new session:** Smaller to build. But the user chose switching in the middle of a conversation.
- **For CLI engines, pass the whole conversation every turn (do not continue the CLI session):** Cannot use the CLI's context compaction or records, and long conversations get slow and expensive.
- **Use codex as an MCP server (`codex mcp-server`):** The interaction shape is different, and configuring it to point tools at KairoCode's MCP is more complex than with `exec`.
- **Call Copilot and OpenAI through the API from the start:** Authentication (especially Copilot's OAuth) would have to be decided first. We followed the order the user chose (CLI first).

## Addendum (2026-10-19): From starting the CLIs to an ACP client

- The original decision 4 started each CLI non-interactively and read its JSON output. On 2026-10-19, with `codex` 0.163.0 and `copilot` 1.0.54 on this machine, we confirmed the following.
  - `codex exec --json` emits `thread.started`, `item.completed` (`agent_message`) and `turn.completed` (`usage`).
  - `copilot -p --output-format json` emits `assistant.message_delta`, `assistant.message` and `result` (`sessionId`).
  - This version of copilot does not accept `--session-id`, and continues by session name (`--name`, `--resume=<name or ID>`).
- The user instructed "the setup where KairoCode calls claude, codex and copilot over ACP", and decision 4 was rewritten into the ACP form.
  - With ACP, the agent's file writes and commands can be run by KairoCode's tools, acting as the client. Approvals can be handled there too.
  - There is also no longer a need to parse each CLI's output differently.


## Implementation (2026-10-19, phase 1)

- **Switching**
  - Added `SessionEvent.Switch` (`workflows.Switch{Engine, Model}`) and raised the session workflow to version 6 (`RunSession`). Version 5 is kept as `RunSessionV5` (`Draining`).
  - Each time the engine changes, `TurnInput.Epoch` goes up by 1. The conversation (`ClaudeTurnInput.History`) is passed to a CLI engine's turn only when `Epoch > 0`. Claude Code turns in the first epoch have unchanged input, so the call IDs of a turn in progress do not change either.
  - The choice made with `/provider` and `/model` is kept in the daemon's memory (`Runtime.Switch`) and sent attached to the next event to that session (`signal`). That is when it is recorded (invariant 2). If the daemon stops before sending, the choice is lost.
  - `Runtime.Settings` gives the next turn's provider and model from the latest turn's input and the choice not yet sent.
  - The daemon's API is `GET /providers`, `GET /providers/{id}/models` and `GET|POST /sessions/{id}/settings`.
- **ACP engine** (`internal/runtime/acpengine.go`)
  - `codex.turn` and `copilot.turn` start the agent and talk with `internal/jsonrpc` (the JSON-RPC split out of `internal/acp`).
  - `fs/write_text_file` and `terminal/create` from the agent are run as KairoCode's `write` and `bash` through `mcpServer.call` (the same as ADR 0007's MCP calls: a run of `<turn>/mcp/<content hash>.<n>`). `fs/read_text_file` is read on the spot, only inside the working directory.
  - To `session/request_permission`, it answers `allow_once` only for calls to KairoCode tools (see "Fixed" below).
  - The agent's session ID is recorded by name in `kc_engine_session`. The next turn continues with `session/load`.
  - When the agent does not accept the specified model, the turn does not stop; it answers with the agent's own model and notes that at the head of the answer. Checking with `copilot --acp` (1.0.54) on 2026-10-19, the session's model list changed with the account's state (after `gpt-4.1` went through, only `auto` was left).
- **Check with a real agent** (2026-10-19, `copilot --acp`)
  - Creating a file went through KairoCode's `write`, and `ls` through KairoCode's `bash` (copilot used the KairoCode tools in the MCP we passed).
  - `codex-acp` is not installed on this machine, so it has not been checked yet.
- **Screen**
  - The ACP agent (`internal/acp`) offers the `provider` and `model` settings in the responses to `session/new` and `session/load` and in `config_option_update`, and accepts `session/set_config_option`. Typed `/provider` and `/model` are handled too.
  - The TUI shows a list for `/provider` and `/model` to choose from with the arrows, or chooses directly by name. The status line shows the current values.
  - Added `-provider` and `-model` to `run` and `chat`.
- **Tests**
  - `TestSwitchProviderAndModel`: with a fake ACP agent, switch API → Codex → Copilot → API. It checks the following (we confirmed it fails for each of 3 ways of breaking it: not sending the switch, not passing the conversation, and writing directly).
    - Carrying over the conversation
    - Continuing with `session/load`
    - Writes are recorded as runs of KairoCode tools
  - `TestProviderAndModel` (TUI).
- **Fixed after points raised by durability-reviewer**
  - **Approval:** `session/request_permission` allows only calls to KairoCode tools and refuses others. Whether it is a KairoCode tool is judged by the title.
    - If the title is `kairocode-<name>` or `mcp__kairocode__<name>`, it is a KairoCode tool.
    - A title with only a name is treated as a KairoCode tool only for copilot, where the built-in tools are removed. For codex, whose built-in tools cannot be removed, codex's own tool with the same name may exist, so it is refused (a point from the re-review). copilot is started with `--available-tools=kairocode-<tool>,…`, which disables copilot's own built-in tools (bash, apply_patch, view and so on). With copilot 1.0.54 we confirmed that the built-in tools are removed and the work goes through KairoCode's tools. copilot's permission requests come with only the tool name as the title (`write`, `bash`) even for MCP tools, so removing the built-in tools is required. The form with `codex-acp` has not been checked yet.
  - **Switching to Claude Code:** A turn with `Epoch > 0` starts in a new Claude Code session made from `(session, Epoch)`. The conversation is attached too. Turns in the first epoch keep their IDs and inputs as before (`TestSwitchToClaudeCode`).
  - **Carrying over the conversation:** codex and copilot turns get the conversation from the first epoch on. Even an agent that cannot `session/load` can be given the conversation attached to a new session. It is a new action, so it does not affect the IDs of calls in progress.
  - **Process handling:**
    - The agent's stderr is now safe to read while it is being written.
    - It waits for the output to be fully read before `Wait`.
    - Stopping stops the whole process group, and `WaitDelay` is 5 seconds.
    - The answer text is read under a lock.
    - Waiting for the output to be fully read is capped at 5 seconds. It does not stay stuck even if a descendant that left the group keeps the output open.
  - **Crash test:** `TestCrashACPWrite`. The agent's write is done, and the process is killed before the result is recorded. On resume, it loads the session, the agent responds to "continue" and asks for the same write again, but the write runs only once (we confirmed that breaking it so the run is not reused makes it 2 and the test fails).
  - **Files that start commands:** `internal/runtime/acpengine.go` (agents) and `internal/runtime/cliengines.go`. The latter starts `codex debug models` and `copilot help config` outside the sandbox to read the model lists. Both are read-only commands.
  - **Left as is:** Deciding which leftover agents to stop is done by the command name of the recorded PID (`codex-acp`, `copilot`). If the PID was reused, it could stop another copilot of the user (ADR 0007's claude tells them apart by session ID).
- **Not yet:** Phase 2 (the `openai` and `github-copilot` APIs). Checking with `codex-acp`.

## Addendum (2026-10-19): The model and effort on switching

The user's requests (2026-10-19) are these 2.

- The model should be the provider's default, or the one used in a previous session.
- Where effort exists, effort should be changeable too.

- **Remember the model and effort**
  - For each provider, the last chosen model and effort are kept in a new table, `kc_provider_pref`. They are kept when chosen explicitly with `/model`, `/effort` or `/provider <id>`.
  - When switching to a provider without naming a model or effort, the kept ones are used. If there are none, that provider's default. This holds even if they were chosen in a different session.
  - The same applies when creating a session without naming a model or effort: the kept ones go into `SessionInput`.
  - Values are used only after being recorded as a switch event or as session input (invariant 2).
- **effort**
  - Added `Switch.Effort`, `SessionInput.Effort`, `TurnInput.Effort`, `AgentLoadInput.Effort`, `Agent.Effort`, `ModelInput.Effort` and `ClaudeTurnInput.Effort`. All are left out of the JSON when empty, so call IDs in existing records do not change.
  - Changing the provider resets effort to empty (default). If not named, the one remembered for that provider is used.
  - How it is passed, per provider:
    - API: the request's `output_config.effort` (`provider.Request.Effort`; if absent, the provider's default `high`).
    - Claude Code: `claude --effort`.
    - copilot: `--reasoning-effort` at startup (not in the ACP session settings).
    - Other ACP agents: passed with `session/set_config_option` to the session setting whose category is `thought_level` (or whose ID contains `effort`). For codex-acp it is `reasoning_effort`.
  - The values that can be chosen:
    - API and Claude Code: `low`, `medium`, `high`, `xhigh`, `max`.
    - ACP agents: the list of models and effort they announced in `session/new` is kept in `kc_provider_pref` and used (until run once, codex and copilot use a fixed list).
  - Providers without effort are not shown effort choices.
- **Screen:** Added `effort` (category `thought_level`) to the ACP settings, selectable with `/effort`. The TUI's status line is "provider · model · effort".
- **Tests:**
  - `TestModelAndEffortKept`: checks the following. We confirmed it fails if the code that uses what was remembered for the target provider is removed.
    - What was chosen for API and codex is used when returning and in a new session.
    - effort reaches the API call and the codex session.
    - The list codex announced is used.
  - `TestProviderAndModel` (`/effort`).
- **Fixed in review:**
  - Choosing the same provider as now again does not send a switch event. This prevents the effort on screen and the next turn's effort from disagreeing in a session on the default engine.
  - Values not in that provider's effort list are not accepted or remembered, so a typo does not keep living on in other sessions.
  - claude gets it in the form `--effort=<value>`.
  - effort is also passed to `task` subagents. It is not passed to flow agents (flow input has no effort).
- **Left as is:** For ACP sessions that do not announce settings, we try sending `reasoning_effort` and do not report it if it fails.
- **TUI (2026-10-20, the user's request):** shift+Tab switches effort to the next value (after the last comes the first; same as Claude Code). The status line always shows effort (`effort high`; `effort default` for the default, `effort none` for a provider without effort). Test: `TestProviderAndModel`.
- **`/model` starts from the provider (2026-10-20, the user's request)**
  - Listing every provider's models at once is hard to read, so `/model` now has 2 levels. First choose the provider, then choose that provider's model (starting at that provider's default).
  - Providers are ordered with the usable ones (installed) first and the not-installed ones after. Not-installed ones have their names in gray, with "not installed" and what to get.
  - Choosing a not-installed one shows an install confirmation, and after installing you choose a model.
  - Choosing a model of a provider different from the current one switches provider and model at once.
  - Other providers' models are read with the ACP extension method `_kairocode/models`.
  - Typing `/model <name>` chooses a model of the current provider directly.
  - Test: `TestModelProviderFirst`.

## Addendum (2026-11-02, with the user's consent): Keep choices not yet sent in the DB

In the implementation (phase 1), choices from `/provider`, `/model` and `/mode` were kept in the daemon's memory until sent with the next event. If the daemon stopped before sending (idle exit, update, crash), the choice was silently lost, and the settings shown on screen disagreed with the next turn.

- Choices not yet sent are kept in `kc_switch` (one row per session: the choice's JSON and the change count `seq`) (`internal/store`). `Runtime.SetProvider` and `SetMode` write it, and `Settings` and `signal` read it.
- `signal` sends the choice attached to an event, and once sent, deletes only the row with the `seq` it read. If the choice changed while sending, the changed one waits for the next event.
- If the daemon stops between sending and deleting, the same choice is sent again. The session already has that value, so nothing changes (if the provider is the same, `Epoch` does not advance either). The record's shape (`SessionEvent.Switch`) does not change.
- Closing a session also deletes the waiting choice (there is no next event).
- Tests: `internal/runtime` (`TestSwitchSurvivesAReopen`, `TestSwitchSentAgainChangesNothing`), `internal/store` (`TestSwitchClearedAsSent`).

## Addendum (2026-11-02, with the user's consent): decide for engines that cannot answer with a schema

A flow's `decide`, in codex and copilot sessions, had the API judge, and if there was no API, had Claude Code judge. In sessions with agents from the ACP registry, it asked the API using that agent's model name as is (it did not work without an API). The auto mode judge switched to Claude Code when there was no API or the API did not answer (no key and so on).

- Both now follow the same order (`Runtime.decideBy`): if there is a provider, the API (the default model for flows, `judgeModel` for the judge); if not, or if the API does not answer, Claude Code. If neither answers, return both reasons. If it is a kind of failure where the API will not answer even if retried (no key and so on), it does not retry.
- Sessions with agents from the ACP registry are treated the same as codex. Before, it asked the API using that agent's model name as is.
- Which engines answer with a schema, which can be tried, and which action drives their turns are gathered in one place, `factsOf` in `internal/runtime/engines.go`.
- Tests: `internal/runtime` (`TestFlowDecideFallsBackToClaudeCode`, `TestEngineFacts`).
