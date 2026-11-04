# 0002 Model features and the shape of configuration on opencode, and make ACP the entry point on the user's side

- Status: Accepted (2026-10-09)
- Date: 2026-10-09 (the same day's proposal "connect opencode's client to KairoCode's server" was replaced with ACP by the user's decision)
- Related: requirements FR-5, FR-10, FR-11, FR-18, FR-20, FR-21, FR-23, FR-24; design sections 7, 8 and 11 (2, 3); ADR 0001, 0003, 0004, 0007

## Context

KairoCode is a tool used the same way as opencode (requirements section 2). On 2026-10-09 the user chose the following two things.

1. Match opencode in usability and features.
2. Make ACP (Agent Client Protocol) the entry point on the user's side. The first proposal was "connect opencode's client to KairoCode's server", but it was replaced with ACP. The user's view is that "it can present things better than streaming text in a terminal".

On 2026-10-09 we looked into opencode and ACP (reading the sources and the public documents directly).

- **opencode** (`github.com/anomalyco/opencode`, v1.18.35, 2026-10-06)
  - Permission rules: `allow`, `ask` and `deny`, written per tool or per `bash` pattern. The last match wins.
  - Agents: defined in Markdown (`.opencode/agents/`).
  - Commands: `.opencode/commands/*.md`.
  - Skills: `SKILL.md`, read from `.opencode/skills/`, `.claude/skills/` and `.agents/skills/`.
  - LSP: after an edit, it adds diagnostics to the tool's output.
  - The server's HTTP API: it has no versioning. There are about 30 releases a month.
  - opencode itself also runs as an ACP agent (`opencode acp`). opencode's TUI is not an ACP client.
- **ACP** (`github.com/agentclientprotocol/agent-client-protocol`, Apache-2.0)
  - Version: the stable version is `protocolVersion: 1`. The schema is v1.10.2 (2026-10-01). v2 was published as a Draft on 2026-07-20.
  - Transport: JSON-RPC 2.0 over stdio.
  - Main methods: `session/new`, `session/prompt`, `session/update` (messages, `tool_call`, `tool_call_update`, `plan`), `session/request_permission`, `session/cancel`, `session/load`, `session/resume`.
  - How a permission request is answered: the options are `allow_once|allow_always|reject_once|reject_always`. The spec has no time limit.
  - Clients that support it: Zed, JetBrains, Neovim (CodeCompanion, avante.nvim and others), Emacs (agent-shell.el), VS Code-family extensions and more.
  - The official SDKs are only Rust and TypeScript; Go ones are made by the community (`coder/acp-go-sdk` and others).
  - What changes in v2:
    - `fs/*` and `terminal/*` (the mechanism that leaves file and terminal operations to the client) go away.
    - `session/load` is merged into `session/resume` and `replayFrom`.
    - A turn's state is conveyed by `state_update`.

## Decision

### 1. Match opencode in features and the shape of configuration

KairoCode's configuration lives in KairoCode's own places (`kairocode.json`, `.kairocode/`). On top of that, we match the shapes and the places read, so that opencode users can move over as they are.

- **Permission rules:** the same shape and meaning as opencode (`allow`, `ask`, `deny`; per tool or per pattern; the last match wins; expansion of `*`, `?` and `~`). "The decision is recorded too" in design section 4 (`permission.check`) is decided by these rules.
- **Agents:** read `.kairocode/agents/*.md` and opencode's places (`.opencode/agents/`, `~/.config/opencode/agents/`). The fields are the same as opencode's.
  - Add `effect_limit` as a KairoCode field. It is the upper limit of the effect types that agent can use (`read` and so on).
  - Add `engine` (`api` or `claude-code`; ADR 0007) as a KairoCode field.
  - Of opencode's built-in agents, provide `build`, `plan`, `general` and `explore`.
- **Commands:** read `.kairocode/commands/*.md` and `.opencode/commands/*.md`. Workflows (ADR 0003) are also listed as commands `/<name>`. `$ARGUMENTS`, `$1` and `@file` are expanded the same way as opencode. `` !`shell` `` goes through the same permission decision as the bash tool.
- **Skills:** in addition to `.kairocode/skills/`, read the same places as opencode (`.opencode/skills/`, `.claude/skills/`, `.agents/skills/`, and the home-directory side of each). The `name` rule (1 to 64 lowercase alphanumeric characters and hyphens, matching the directory name) is also the same.
- **LSP diagnostics:** after writing, the edit and write tools get LSP diagnostics and add any errors to the tool's output. Getting diagnostics is a separate read-only action, apart from the tool's effect.
- **Models (decides item 3 of design section 11):** only Claude is used, with two engines: the Claude API (`api`) and Claude Code (`claude-code`, ADR 0007) (the user's decision, 2026-10-09). The provider adapter boundary (`internal/provider`) stays, but OpenAI-compatible and local models are not added. Requirement FR-18's "a shape where adapters can be added" is met by having the boundary.
- **opencode plugins are not supported.** They are JS that runs inside the server process, so they do not run on a Go server. Outside tools connect through ACP (2 below) and ADR 0004's status reports. Custom tools are added through MCP (ADR 0001).

### 2. Make ACP the entry point on the user's side (decides item 2 of design section 11)

- **KairoCode is an ACP agent.** An editor starts `kairocode acp` over stdio. It implements ACP v1, in a shape that makes it easy to follow v2's changes (`session/resume` and `replayFrom`, `state_update`).
  - The ACP process opens with the runtime (kairo and the same DB) embedded. Even when the editor closes the process, the session stays in the DB. The next process started continues it with `session/load` or `session/resume`.
  - File and command operations are done by KairoCode itself (kairo's records, effect types, the sandbox; ADR 0005). It does not rely on the client's `fs/*` and `terminal/*` (they go away in v2).
- **KairoCode's concepts are mapped onto ACP.**

  | KairoCode | ACP |
  |---|---|
  | Session (`s/<ID>`) | `session/new` (`cwd`). ACP's `sessionId` is KairoCode's session ID |
  | Turn | `session/prompt`. The response is returned at the end of the turn (`state_update` in v2) |
  | Streaming | `agent_message_chunk` of `session/update` (built from `k.Subscribe`) |
  | Tool call | `tool_call` and `tool_call_update` (kind, target files, diff, status) |
  | The tree of a workflow (ADR 0003) | `plan` (each node as an entry, with its state mapped to `pending`, `in_progress`, `completed`) |
  | Waiting for an approval (`kairo.WaitFor`) | `session/request_permission`. The options mean the same as opencode's (this time only, allow from now on, reject). `allow_always` is recorded as a rule |
  | Blocked (needs confirmation) (a real tool that crashed) | `session/request_permission`. The options are "it ran" (`allow_once` → `k.Resolve`) and "it did not run" (`reject_once` → `k.ResolveFailed`) |
  | Cancellation | `session/cancel` → the turn's `k.Cancel` |
  | Resume | `session/load` (resends `session/update` from the record), `session/resume` |
  | `/undo`, `/redo`, workflow commands | ACP slash commands (`available_commands_update`) |

- **Where the meaning is made stronger:** an ACP permission request can be held only while the process and the connection are alive. KairoCode records waits for approvals and for blocked (needs confirmation) calls in kairo, so they are not lost even when the editor or the process crashes. A client that resumes is sent the pending `session/request_permission` again.
- **The screen used in a terminal:** in M2 we check whether an ACP terminal client is enough. If not, we build a minimal line-based chat (`kairocode chat`) as our own ACP client. `kairocode run` (non-interactive) runs without going through ACP.
- **Go implementation of ACP:** whether to use a community SDK or build it ourselves from the spec's JSON Schema is decided in M2's first prototype. If we use an SDK, we get the user's consent as an added dependency.

## Consequences

- Instead of building the chat screen ourselves, we can use the display that ACP clients (Zed, JetBrains, Neovim, Emacs and so on) have. Plans, tool calls, diffs and permission requests appear in a structured form. KairoCode-specific state (the workflow tree, waiting for approval, blocked (needs confirmation)) can also be carried on `plan` and `session/request_permission`.
- There is no cost of chasing opencode's server API (which has no versioning). Instead, we follow ACP's move from v1 to v2.
- The HTTP API of design section 8.1 (`internal/server`) stays as the entry point for internal use and automation. dspchr (ADR 0004) and CI use it.
- Tests (M2's acceptance criteria):
  - Contract tests for requests and responses against ACP's JSON Schema (a pinned version).
  - Integration tests using an ACP client that sends messages in a fixed order (`session/new` → `prompt` → `tool_call` updates → `request_permission` → answer → end of turn).
  - Stop the ACP process while it waits for an approval. When the next process does `session/load`, the pending `request_permission` arrives again, and answering it lets the turn continue (`TestACPApprovalSurvivesRestart`).
  - Check by hand in at least one editor (Zed).
- A new rule to keep: the ACP layer only maps KairoCode's concepts onto ACP's shapes. It does not weaken KairoCode's meaning (the invariants).

## Alternatives considered

- **Connect opencode's client to KairoCode's server (this ADR's first proposal):** opencode's TUI and desktop app could be used. But we would keep chasing an API with no versioning (about 70 operations the TUI calls). opencode's TUI is not an ACP client, so it would not fit with ACP either.
- **Build our own TUI:** building the screen takes time. It is unlikely to be better than the display of ACP clients.
- **Fork opencode:** it is TypeScript, so it goes the opposite way from ADR 0001.
- **Leave file and terminal operations to the ACP client's `fs/*` and `terminal/*`:** they go away in v2. The effects would happen outside kairo's records and the sandbox.

## Implementation (2026-10-19, M2)

- **We built ACP in Go ourselves.** It is only JSON-RPC and stdio, so no dependency (community SDK) was added. It is in `internal/acp`.
- **Pinned version:** ACP's schema v1 (`schema-v1.26.0`) is in `internal/acp/testdata/acp-schema-v1.26.0/` (Apache-2.0; the LICENSE is there too). The test client checks every message it receives from the agent against this schema. The checker is a small one inside the tests that has only the keywords ACP's schema uses (`TestSchemaChecker` checks what it passes and what it rejects).
- **The ACP process is a client of the daemon** (ADR 0008; this supersedes "the runtime is embedded" above). Even when the editor closes `kairocode acp`, sessions and approval waits stay in the daemon (kairo).
- The methods implemented are `initialize`, `session/new`, `session/load`, `session/prompt`, `session/update` (`agent_message_chunk`, `tool_call`, `tool_call_update`, `user_message_chunk`), `session/request_permission` and `$/cancel_request`.
  - When another client answers an approval first, the `request_permission` that was sent is withdrawn with `$/cancel_request` (`TestACPPermissionAnsweredElsewhere`).
  - `session/load` resends the conversation up to the last finished turn, and asks again about pending approvals and blocked (needs confirmation) calls (`TestACPApprovalSurvivesRestart`).
- **Both options of a blocked (needs confirmation) call are of the `reject_once` kind.** The kind of an ACP option is a hint for display, but some clients allow tools automatically. If "it took effect" were `allow_once`, a blocked call could be settled without a person answering (invariant 4). The option IDs are `did_not_take_effect` and `took_effect`. An answer with an ID that was not offered is not accepted, and the wait stays in kairo (`TestCrashACPReview`).
- Blocked (needs confirmation) calls and the Claude Code engine's approvals do not yet have a `tool_call` told to the client. So a `tool_call` with the same ID is sent before `request_permission`.
- The response to `session/prompt` is returned after all of that turn's updates are sent. Watching starts before `Send`.
- **We built the terminal chat (`kairo`, `kairocode chat`) as our own ACP client** (2026-10-19; "the screen used in a terminal" above).
  - It matches the user's request (as with claude, `kairo` with no arguments starts a chat). `kairo` is a link to the same binary (`make build`).
  - It runs the ACP agent inside the same process and connects them with a pipe. Because it goes through the same path as an editor, approvals, blocked (needs confirmation) calls, resuming with `session/load` and slash commands work the same as in an editor.
  - It is line-based and uses no TUI library (no dependency added). Replies are written as they arrive.
  - A permission request is answered with the option's number. When there is exactly one option that allows and one that refuses, y and n also work. A blocked (needs confirmation) call has both options of kind `reject_once`, so it is answered only by number.
  - Even after leaving, the session stays in the daemon. `kairo -session <ID>` redisplays the conversation and then continues.
  - ACP notifications are handled in the order received, before the responses that follow (`TestClientKeepsTheOrderOfUpdates`).
  - The tests are `TestChat`, `TestChatAsks`, `TestNoCommandIsTheChat` and `TestClientPermission`.
- Not in yet:
  - `allow_always` (record it as a rule)
  - `plan`
  - showing diffs (`diff` of `tool_call`)

### opencode agents, commands and skills (2026-10-19, M2)

We read the source of opencode v1.19.3 and made them read with the same meaning (`internal/opencode`).

- **Locations:** read the user's `~/.config/opencode` and each `.opencode` from the repository root down to the working directory. Later ones take precedence.
  - In opencode's code, a `.opencode` higher up wins over a closer one. Here, as with the configuration file (`opencode.json`), the closer one wins.
- **Agents** (`agent/` and `agents/`; a nested name is `team/review`)
  - The body replaces the default system prompt.
  - `model` accepts only `anthropic/...`. A model set for the session comes first.
  - `steps` (`maxSteps`) is accepted.
  - `tools` and `permission` are layered after the project's rules (the rule that matches later wins). opencode's `edit` maps to both `edit` and `write` here.
  - A session cannot be run with a `mode: subagent` agent (subagents are M3).
  - The definition is recorded with `agent.load` (invariant 2).
- **Commands** (`command/` and `commands/`)
  - The order of expansion is the same as opencode's: `$1`… (the highest number takes the rest) → `$ARGUMENTS` → appending the arguments → `` !`…` `` → trimming leading and trailing whitespace → `@file`.
  - `` !`…` `` runs inside the sandbox, read-only and with no network.
  - `@file` attaches only files inside the working directory, after the body.
  - Expansion is done in the daemon, and the expanded text is recorded as a message. On resume it is not expanded again.
  - A command's `agent` and `model` apply only to that turn (`Agent` and `Model` of `SessionEvent`; session version 4).
  - `subtask` runs as a normal turn until M3.
  - In ACP they are shown as slash commands.
- **Skills** (`SKILL.md` under `.claude/skills`, `.agents/skills`, and `skill/` and `skills/` of the configuration locations, at any depth)
  - Named by `name` in the frontmatter.
  - The list goes into the system prompt in the same form as opencode.
  - The content is given through the `skill` tool (effect type Read): the body, the location, and up to 10 other files.
  - If there are no skills, the `skill` tool is not shown.
- **Records and permissions:** the output of a command's `` !`…` `` and the content of `@file` are recorded as the user's message and sent to the model. They are treated the same as the results of the bash and read tools. So a command denied by the project's permission rules is not run (the command becomes an error), and a denied file is not attached. `` !`…` `` is cut off after 30 seconds. Secrets in environment variables are removed by the sandbox (ADR 0005).
- **Definitions that cannot be read:** a definition whose frontmatter is opened but not closed is an error (opencode skips it, but we do not want permission rules to disappear silently). Outside a repository, `.opencode` above the working directory is not read.
- **YAML:** the frontmatter is read by a small parser of our own, covering only what opencode definitions actually use. That covers nested maps (keeping order), quotes, block scalars, comments, and descriptions containing colons. Lists, flow style and anchors are refused rather than misread. No dependency was added.


## Addendum: skill locations, the name rule, /<skill>, codex (2026-10-21, the user's request "let's support skills")

The user chose the following three. `allowed-tools` was not chosen, so it is not in.

- Make skills callable with `/<skill>`
- Locations and the name rule
- Turn off codex's own skills

- **Locations** (`opencode.SkillDirs`): added the `.kairocode/skills/` named in the Skills item of the Decision. That is the user's (`~/.config/kairocode/skills`) and `.kairocode/skills` from the repository root down to the working directory. They come last in order, so for the same name KairoCode's wins.
- **The name rule** (`opencode.CheckedSkills`)
  - As in opencode, a name is words of lowercase letters and digits joined by `-`, up to 64 characters. It must also match the name of the directory that holds SKILL.md.
  - Ones that do not fit the rule, whose frontmatter cannot be read, or that have no name are not taken, and the reason is kept.
  - For one replaced by a later one with the same name, which one replaced it is kept too.
- **`/<skill>`**
  - A skill is offered as a `/name` command if there is no command with the same name (`commands/*.md`) (`opencode.Command.Skill`; in ACP the description starts with `(skill)`).
  - Typing it starts a turn with the skill's body (the same as what the skill tool returns) and "What the user asks of it: <arguments>" as the user's message (`Runtime.runSkill`).
  - The text sent is recorded as a message, so a resume does not reread the skill (invariant 2).
- **`/skills`:** without starting a turn, shows the list of skills (`/name — description` and location), plus the SKILL.md files not taken and why. The daemon's API is `GET /sessions/{id}/skills`.
- **codex's own skills**
  - codex lists `.agents/skills`, `~/.agents/skills`, `/etc/codex/skills`, its bundled skills and plugin skills to its own model.
  - KairoCode already passes its own skill list, so they would be doubled, and skills from outside KairoCode would be mixed in.
  - So when starting codex-acp, we pass `skills.include_instructions = false` in `CODEX_CONFIG`, so codex does not show its skill list to the model. If the user has their own `CODEX_CONFIG`, we add to it.
  - How we checked: with the local codex 0.163, running `codex debug prompt-input -c skills.include_instructions=false` made the "Available skills" section disappear.
    - However, that checked it as a setting of codex itself (`-c`). That codex-acp adds `CODEX_CONFIG` to the session config and passes it to codex was checked only from the README ("JSON object merged into the Codex session config") and the code. Checking it by running a turn on real codex has not been done yet.
    - If the user's `CODEX_CONFIG` is not JSON, it is passed through untouched. codex runs, but its own skills are not hidden (this is logged).
  - Claude Code already has its own skills turned off (`--setting-sources ""`).
- **Where the name rule comes from:** in the Agent Skills spec, name must match the name of SKILL.md's parent directory. We followed that spec. Skills that were read before from a directory with a different name are no longer read. For those, `/skills` shows the reason.
- **Skills with the same name as a built-in command** (`skills`, `model`, `undo` and so on): the built-in comes first. They cannot be called with `/`, but the model can use them through the skill tool.
- **Tests:** `TestSkillRules`, `TestSkills`, `TestSkillCommands`, `TestCodexOwnSkillsOff` (a configuration that is not JSON is left as is), `TestACPSkills`.

## Addendum: read definitions in .claude and .agents too (2026-10-21, the user's request "let's read the .claude and .agent directories too")

- **Where agents and commands are read** (`opencode.DefinitionDirs`)
  - In order where later ones win: first the user's `~/.claude`, `~/.agents`, and opencode's user configuration.
  - Then `.claude`, `.agents` and `.opencode` at each level from the repository root down to the working directory.
  - What is read is the `.md` files in each one's `agents/` and `commands/` (and the singular forms).
  - For the same name, `.opencode` wins. The places that read only opencode's (`Dirs`) were left as they are.
- **Claude Code agents** (`.claude/agents/*.md`)
  - The name comes from `name` in the frontmatter, and without `mode` it is a subagent (every Claude Code agent is a subagent).
  - `tools: Read, Grep` (or the list form) narrows the tools given to that agent (`Agent.Only`).
    - Names are converted to KairoCode's tools (Read→read, Edit/MultiEdit→edit, LS→list, `Bash(git:*)`→bash and so on). Unknown names (MCP tools and so on) are used as they are. Names that contain a glob such as `*` are dropped.
    - Tools not in the list are not shown to the agent, and a rule that refuses them is added too (recorded with `agent.load`).
    - Tools in the list are not allowed by this. Whether they are allowed or asked about stays with the user's and the project's rules. Claude Code's `tools` also only narrows the usable tools.
    - `tools` of an opencode definition still accepts only a map, as before.
  - For `model`, `sonnet`, `opus` and `haiku` are converted to the latest Claude models. `inherit` sets no model. `color` and the like are not read.
- **Claude Code commands** (`.claude/commands/*.md`): expanded the same way as opencode commands (`$ARGUMENTS`, `!` commands and so on). `argument-hint` becomes the input hint of the ACP command.
- **Definitions that cannot be read are left out.** Files in `.claude` and `.agents` belong to other tools, so ones that cannot be read are left out without stopping the rest. This covers a different shape, no body, and values such as `mode` that cannot be accepted. Ones in `.opencode` still stop as errors, as before.
  - Which files belong to another tool is decided by the directory the definition is read from (`.claude`, `.agents`). Even if `/.claude/` appears in the middle of the path (a Claude Code worktree, `…/.claude/worktrees/x`), that project's `.opencode` belongs to opencode.
  - Another tool's files cannot change KairoCode's built-in agents (`build`, `plan`).
- **`!` in Claude Code commands:** if `allowed-tools` does not allow Bash, it is not run and is replaced with "(not run: …)". This is the same handling as Claude Code.
- **Model names:** converting `sonnet` and the like to a Claude model happens only for the API and Claude Code engines. For codex and others, it becomes that engine's default model.
- **Reading frontmatter**
  - A list of scalars (lines of `- Read`, or a one-line `[a, b]`) is now read as one scalar. The line form joins the items with ", ", and the one-line form is read as is, brackets included.
  - Claude Code's `tools` and `argument-hint: [env]` can now be read. Flow maps (`{}`), anchors and tags are still not accepted, as before.
- **Instruction files:** in addition to `AGENTS.md` and `CLAUDE.md`, read `.claude/CLAUDE.md`, `CLAUDE.local.md` and the user's `~/.claude/CLAUDE.md`. The user's comes first.
- **The list of subagents:** when the task tool is offered, the agents that can be given (not primary, not hidden or disabled, and with a description) are listed in the system instructions as `<available_agents>`. Claude Code agents also become visible to the model.
- **The text of the trust prompt:** added the places read (.kairocode, .opencode, .claude, .agents, AGENTS.md, CLAUDE.md).
- **What was fixed after durability-reviewer's findings**
  - In the first form, the tool list was a layer that "allows what is in the list". That overrode the user's "ask" and "refuse" rules, and writing `*` allowed everything. Now it only narrows and does not allow.
  - Also fixed (the parts written above):
    - misjudging inside a worktree
    - overriding the built-in agents
    - handling values that cannot be accepted
    - handling `!`
    - model names
    - comments inside lists and commas inside brackets
- **Tests:** `TestClaudeDefinitions`, `TestToolList`, `TestDefinitionsInAClaudeWorktree`, `TestParseLists`, `TestClaudeCodeDefinitions`, `TestClaudeAgentToolsList` (the list does not override the user's "ask"; it fails if put back into the allowing-layer form), `TestAgentsRefuseBadDefinitions`.

## Addendum: do not give CLI agents the instructions twice (2026-10-21, results of checking)

KairoCode puts instructions such as `AGENTS.md` and `CLAUDE.md` and the skill list into its own system instructions and gives them to the agent. If a CLI agent also reads the same things itself, they go into the model twice. We checked each one without calling a model.

| Agent | How KairoCode passes them | What it reads itself | How it is turned off |
|---|---|---|---|
| Claude Code 2.1.305 | `--setting-sources ""` | nothing | already off |
| codex 0.163 | `CODEX_CONFIG` | the project's `AGENTS.md` | added `project_doc_max_bytes = 0` |
| copilot 1.0.98 | startup arguments | the project's `AGENTS.md` and `CLAUDE.md` (`copilot instruction list`) | added `--no-custom-instructions` |

- **Claude Code**
  - How we checked: we started a server on local 127.0.0.1 that only records requests and returns 400, and ran it with `ANTHROPIC_BASE_URL` pointed there. Nothing was sent to Anthropic.
  - With the same arguments as KairoCode (`--setting-sources ""`), none of `~/.claude/CLAUDE.md`, `CLAUDE.md`, `.claude/CLAUDE.md` or `CLAUDE.local.md` went into the request. The only instructions to the model were KairoCode's system instructions.
  - For comparison, removing `--setting-sources` made all four go in once each.
  - The test that enforces it: `TestClaudeEngineReadsNoSettingsOfItsOwn` (that `--setting-sources ""` is passed, and that the user's CLAUDE.md goes into the system instructions exactly once).
- **codex**
  - How we checked: with `codex debug prompt-input`, we saw that the project's `AGENTS.md` goes into codex's own instructions.
  - We also confirmed that `-c project_doc_max_bytes=0` keeps it out, so we added it to `CODEX_CONFIG`. codex's own `~/.codex/AGENTS.md` is not read by KairoCode, so it is left to codex.
- **copilot**
  - How we checked: with `copilot instruction list`, we saw that it reads the project's `AGENTS.md` and `CLAUDE.md`.
  - We added `--no-custom-instructions` ("do not read custom instructions from AGENTS.md and so on") to the ACP startup. We confirmed that it accepts this flag when started over ACP.
  - However, whether a session with the flag really does not read them cannot be checked without calling a model. So this relies on the wording of copilot's description.
- **Tests:** `TestCodexOwnSkillsOff` (codex's `project_doc_max_bytes`, copilot's flag).
