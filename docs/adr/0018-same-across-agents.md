# 0018 The same things work whichever agent is used: KairoCode's promise, how missing pieces are filled in, hooks in KairoCode's sandbox

- Status: Accepted (2026-10-21)
- Date: 2026-10-21
- Related: ADR 0002 (opencode and Claude Code definitions, skills), 0003 (KairoCode's hooks), 0005 (sandbox, escalation), 0007 (the Claude Code engine), 0014 and 0015 (codex, copilot, ACP agents)

## Context

The user's decisions (2026-10-21) are these three.

- "KairoCode decides what to pass. Loading can be done on the agent's side. Fill in whatever is missing."
- "Make it possible to develop at a steady quality as KairoCode. The same things must work even when the agent changes."
- "Run hooks. But on KairoCode's sandbox. Leaving the sandbox always needs approval."

We checked, without calling a model, what each agent reads by itself (2026-10-21).

| | Claude Code 2.1.305 | codex 0.163 | copilot 1.0.98 |
|---|---|---|---|
| How checked | Had a local server receive its request and looked at the contents | `codex debug prompt-input` | `copilot instruction list`, `skill list` |
| AGENTS.md | Does not read | Reads | Reads |
| CLAUDE.md (project) | Reads* | Does not read | Reads |
| .claude/CLAUDE.md, CLAUDE.local.md, ~/.claude/CLAUDE.md | Reads* | Does not read | Does not read |
| .claude/skills, ~/.claude/skills | Reads* (project, user) | Does not read | Reads project only |
| .agents/skills, ~/.agents/skills | Does not read | Reads | Reads |
| skills in .opencode, .kairocode | Does not read | Does not read | Does not read |
| Its own hooks | Runs* | Runs (features.hooks) | Not checked |
| What it adds as its own | Plugins | Bundled skills, plugins, ~/.codex/AGENTS.md | Bundled skills, .github/skills, ~/.copilot/skills |

\* Claude Code reads CLAUDE.md and skills only when it reads its own settings (`--setting-sources user,project,local`). In that case the hooks in those settings also run, outside KairoCode's sandbox.

What we checked:

- Hooks passed with `--settings` ran even with `--setting-sources ""`.
- With `disableAllHooks`, hooks passed with `--settings` stopped too.
- So if Claude Code is left to read CLAUDE.md and skills itself, there is no way to put the user's hooks into KairoCode's sandbox.

## Decision

1. **KairoCode's promise (the same on every engine)**: on every engine, the model receives each of the following exactly once.
   - Instructions: `~/.claude/CLAUDE.md`, `AGENTS.md`, `CLAUDE.md`, `.claude/CLAUDE.md`, `CLAUDE.local.md`
   - Skills: all the locations in ADR 0002 (`.claude`, `.agents`, `.opencode`, `.kairocode` and the user's)
   - KairoCode's tools, permissions, sandbox. Commands (`/name`), skills (`/skill`), flows
   - Hooks (3)
2. **How things are read: leave to the agent what can be left, and KairoCode fills in what is missing** (`internal/runtime`, the per-engine table `nativeReads`)
   - API: KairoCode reads everything.
   - Claude Code: if it reads things itself, hooks run outside KairoCode's sandbox (Context). So, as before, it does not read them itself (`--setting-sources ""`), and KairoCode fills in everything. Skills are used through KairoCode's skill tool (MCP).
   - codex: `AGENTS.md` and `.agents/skills`, `~/.agents/skills` are left to codex (drop `project_doc_max_bytes = 0` and `skills.include_instructions = false` from ADR 0002's addendum). KairoCode fills in only the rest (the CLAUDE.md family, skills in the other locations).
   - copilot: `AGENTS.md`, `CLAUDE.md` (project), and `.claude/skills`, `.agents/skills` (project), `~/.agents/skills` are left to copilot (drop `--no-custom-instructions`, and add copilot's own skill tool to `--available-tools`). KairoCode fills in the rest.
   - What an agent adds as its own (bundled skills, plugins, ~/.codex/AGENTS.md and so on) stays as part of that agent's nature. Nothing in the promise goes missing or appears twice.
3. **KairoCode runs hooks on every engine** (in line with KairoCode's hooks in ADR 0003)
   - **What is read:** KairoCode reads hooks in Claude Code's format (`hooks` in `~/.claude/settings.json`, `.claude/settings.json`, `.claude/settings.local.json`).
   - **Event mapping:**
     - `SessionStart` → `session.start`
     - `UserPromptSubmit` → `turn.start` (new)
     - `PreToolUse` → `tool.before` (new). Runs before a KairoCode tool is called; exit code 2 stops the call and returns stderr to the model.
     - `PostToolUse` → `tool.after`
     - `Stop` → `turn.end`
     - Other events (Notification and so on) are not run yet. `/hooks` shows that they do not run.
   - **Input:** the same JSON as Claude Code (`session_id`, `cwd`, `hook_event_name`, `tool_name`, `tool_input` and so on) on stdin.
   - **Sandbox:** hooks run in the same KairoCode sandbox as bash (can write the working directory, no network).
   - **Outside the sandbox:** for each hook (by the hash of the command's contents), the user is asked before it first runs. There are three answers, "run in the sandbox", "run outside the sandbox", "do not run", and the answer is recorded. Only hooks the user answered that way run outside the sandbox. If a hook's contents change, the user is asked again.
   - **Recording runs:** a hook run is recorded as a KairoCode action (the effect is real. A crash leads to blocked (needs confirmation), invariant 4).
   - **The agent's own hooks:** stopped. Claude Code with `--setting-sources ""`, codex with `features.hooks = false`. For copilot, whether it has a hook mechanism is checked during implementation, and if so it is stopped.
4. **How quality is checked**
   - The code holds a per-engine table of "what it reads itself", and a test keeps "the promise − what it reads itself = what KairoCode fills in".
   - Add `scripts/agent-probe.sh`. It checks, without calling a model, what the local Claude Code, codex and copilot actually read (the method in the Context table), and reports if that disagrees with the table. Run it when a CLI is upgraded. It needs the CLIs, so it is not in `check.sh`.
5. **A known difference:** codex reads `AGENTS.md` layered from the root down to the working directory. KairoCode reads only the one in the working directory. The difference is written in the table.

## Consequences

- On every engine, instructions, skills, hooks, permissions and the sandbox work the same. The agents' own good ways of reading (codex's and copilot's skills) are used as they are.
- Any hook that leaves the sandbox is always approved by the user. Claude Code's hooks also run outside Claude Code (API, codex, copilot).
- Tests (added during implementation)
  - The table of what is filled in (per engine).
  - The hook event mapping and the stdin JSON.
  - Approval for outside the sandbox (ask, record the answer, ask again when the contents change).
  - Stopping on exit code 2 from `tool.before`.
  - A crash test of hook runs.
  - The agent's own hooks are stopped on each engine.

## Alternatives considered

- **Let agents read nothing and have KairoCode pass everything (the current form):** reliable and consistent, but it does not fit the user's "loading can be done on the agent's side". It also cannot make use of how codex and copilot use skills. Only Claude Code keeps this form, because of hooks.
- **Let Claude Code read things itself too, and leave hooks as they are:** hooks run outside KairoCode's sandbox without approval. This does not meet the user's condition.
- **Route Claude Code's hooks into KairoCode's sandbox through `SHELL`:** we checked that Claude Code uses neither `SHELL` nor `CLAUDE_CODE_SHELL` for hooks.

## Implementation (2026-10-21)

### 1. Filling in what is missing (`nativeReadsOf` in `internal/runtime/agent.go`)

- `agent.load` builds the system instructions leaving out the instruction and skill locations the engine's agent reads itself. Recording is as before.
- codex: `AGENTS.md`, `~/.agents/skills` and `.agents/skills` at each level are left to codex. `project_doc_max_bytes = 0` and `skills.include_instructions = false` were removed from `CODEX_CONFIG`, and `features.hooks = false` was added.
- copilot: `AGENTS.md` and `CLAUDE.md` are left to copilot (`--no-custom-instructions` removed).
- **Changed from decision 2 (copilot's skills):** the name of copilot's skill tool could not be confirmed without calling a model. So it was not added to `--available-tools`. KairoCode passes all skills to copilot (KairoCode's skill tool). Whether copilot itself also lists its own skills to the model is not confirmed. If it does, the list appears twice. This remains for `scripts/agent-probe.sh` and checks with the real thing.
- Tests: `TestEnginesGetWhatTheyDoNotRead` (what is filled in per engine, what is passed in one codex turn), `TestCLIAgentsConfig`.

### 2. KairoCode runs hooks on every engine (`internal/workflows/hooks.go`, `internal/runtime/commandhooks.go`)

- **Reading:** `claudeHooks` reads `type: "command"` hooks from `~/.claude/settings.json`, `.claude/settings.json`, `.claude/settings.local.json`. They are added to `agent.load`'s hook list and recorded. Files that cannot be read are left out.
- **When to ask:** asking happens at the start of the turn, before any hook runs (`decideHooks`).
  - `hook.check` looks at whether an answer exists for that folder. For those without one, the user is asked with the same mechanism as a flow's question (`Kind: question`. The answers are "Run it in KairoCode's sandbox", "Run it outside the sandbox", "Do not run it"). The answer is kept with `hook.decide`.
  - The answer is written to the store once, keyed by the folder, the Claude Code event and the command. If the command changes, the user is asked again.
  - Because asking happens at the start, there is no wait in the middle of an MCP call of a CLI engine such as Claude Code.
  - So "ask before it first runs" is, precisely, "ask at the start of a turn that has that hook" (decision 3).
- **Running:** `hook.command` (the effect is real) runs it with `sh -c`.
  - The sandbox is the same as bash: can write the working directory, no network. Only hooks answered "outside" run outside the sandbox.
  - stdin gets the same JSON as Claude Code (`session_id`, `cwd`, `hook_event_name`, `tool_name`, `tool_input`, `tool_response`, `prompt`). The environment variable `CLAUDE_PROJECT_DIR` is also passed.
  - It waits up to `timeout` (default 60 seconds).
  - Outside the sandbox it runs with `sandbox.FullAccess`, the same as bash's escalation. `internal/runtime/commandhooks.go` was added to the files allowed to use `FullAccess` in the invariant check (`fullAccessUsers`) (by decision 3 of this ADR).
  - If the process crashes while the hook is running, it is not run again. The user is asked "did it take effect" (blocked (needs confirmation), invariant 4), and after the answer, the turn goes on as if the hook did not stop it.
- **Events and exit codes** (the same as Claude Code)

  | Event | Exit 0 | Exit 2 |
  |---|---|---|
  | SessionStart (session.start) | Adds stdout to the first request | — |
  | UserPromptSubmit (turn.start) | Adds stdout to the request | Ends the turn with "[A hook stopped this request: …]" without asking the model |
  | PreToolUse (tool.before) | — | Stops the call and returns stderr to the model |
  | PostToolUse (tool.after) | — | Attaches stderr to the result (for MCP calls, through `hook.note`) |
  | Stop (turn.end) | — | Sends the model back with stderr (up to 3 times) |

  - Other events (Notification and so on) are not run yet.
  - Since workflows cannot use regexp, matchers are limited to the forms Claude Code uses: a name, names joined with `|`, `*`, and a prefix match ending in `.*`.
- **Engines**
  - API: all events run inside the turn.
  - CLI engines: session.start and turn.start at the start of the turn, tool.before and tool.after in MCP calls (`RunMCPCall`, `ClaudeTurnInput.Hooks` → `MCPCallInput.Hooks`), and turn.end after the last round.
  - Flow agents and subagents (`Sub` turns) do not run them, like the existing flow hooks. A session's turn runs session.start and turn.start even when the agent runs as a flow (`workflow`).
- **The agent's own hooks:**
  - Claude Code: they do not run with `--setting-sources ""` (checked with `scripts/agent-probe.sh`).
  - codex: stopped with `features.hooks = false` in `CODEX_CONFIG`.
  - copilot: **not stopped (a known hole).**
    - copilot has its own hooks (`copilot help config`): the user's settings (`hooks` in `~/.copilot/settings.json`) and `.github/hooks/*.json` in folders copilot trusts.
    - The setting that stops them is `disableAllHooks`, but there is no flag or environment variable that applies to a single launch. It can only be written into copilot's user settings file.
    - KairoCode does not rewrite the user's copilot settings on its own, so on the copilot engine, copilot's own hooks can run outside KairoCode's sandbox without approval.
    - We asked the user whether to write `disableAllHooks` (2026-10-21). The answer and what followed are in section 4.
- **What was fixed after durability-reviewer's findings**
  - **A hook stopped by cancellation is not treated as finished:** a hook stopped by cancellation, such as when the process is being stopped, is returned as an "unknown outcome" (`kairo.Unknown`), not an exit code, and becomes blocked (needs confirmation). Before, being killed was returned as exit code -1, so a call could go ahead even though a PreToolUse hook had stopped.
  - **Retrying is allowed only if it did not start:** a failure after it started is an "unknown outcome".
  - **codex's settings:** when the user's `CODEX_CONFIG` is not JSON (codex-acp cannot read it either), only KairoCode's settings are passed, and codex's own hooks are stopped. Before, it was passed through as is, so hooks were not stopped.
  - **The blocked (needs confirmation) message for MCP calls:** what was cut off by the crash can be a hook, so it says "this call (or a hook around it)".
  - **Tests not yet written:**
    - A crash test of a hook cut off inside an MCP call
    - A test that it is confined in the sandbox
    - A hook left behind as a child of the crashed process (sleep and so on) keeps running to the end (the same as bash).
- **Why the version is not bumped:** the new calls happen only when the hooks recorded by `agent.load` include command hooks. Records from before this change have none, so on resume the sequence of calls is the same.
- **Tests**
  - `TestClaudeHooksRunHere` (four events on the API, asks once, Notification does not run)
  - `TestClaudeHookNotRun` (not run; asks again when the command changes)
  - `TestClaudeHooksOnClaudeCode` (PreToolUse in an MCP call)
  - `TestCrashHookRun` (after a crash, it asks instead of running again. We checked that it fails if the effect is changed to `Unprotected`)

### 3. The checking script (`scripts/agent-probe.sh`)

- It checks, without calling a model, what the local Claude Code, codex and copilot read themselves and which hooks they run. If that disagrees with what KairoCode relies on (`nativeReadsOf`), it ends with a failure.
- The result on 2026-10-21, with Claude Code 2.1.305, codex 0.163.0 and copilot 1.0.101, was that all 22 items matched.
- It needs the CLIs, so it is not in `check.sh`. Run it when a CLI is upgraded.

### 4. Stopping or allowing the agent's own hooks (Addendum (2026-10-21))

- The user's answer: for copilot, stopping with `disableAllHooks` is fine. But make it possible to undo through KairoCode's settings.
- **Default (stop)**
  - Each time copilot is launched (`acpArgv`), `"disableAllHooks": true` is written into copilot's settings (`settings.json` in `$COPILOT_HOME`, or `~/.copilot` if unset). Other entries are kept, and the file's permissions are kept. It is written to a temporary file and swapped in, so no half-written state is left.
  - That it was written is marked with `kairocode-disabled-hooks` in the same directory. The mark is written first, so even with a crash in between, undoing never removes too much.
  - If the user has written `disableAllHooks` themselves, nothing is done and no mark is left.
  - If the settings cannot be read as JSON, they cannot be stopped, so copilot is not launched, and the user is told to fix it or use the setting below.
  - In tests (`KAIROCODE_NO_CLIS`), nothing is written unless `Config.CopilotHome` is passed.
- **Undoing (`agents.ownHooks`)**
  - Writing `{"agents": {"ownHooks": true}}` in the user's `kairocode.json` (`~/.config/kairocode/`) allows the agents' own hooks.
    - codex: `features.hooks = false` is not added (the user's `CODEX_CONFIG` as is).
    - copilot: if the mark exists, the `disableAllHooks` that KairoCode wrote is removed, and the mark too. If there is no mark (the user wrote it), it is not touched.
    - Claude Code: no change. Hooks in Claude Code's format are run by KairoCode in the sandbox on every engine.
  - Allowed hooks run outside KairoCode's sandbox without approval. It is a setting the user chooses.
  - **It does not take effect in a project's `kairocode.json`.** This keeps a cloned repository from turning on hooks that run outside the sandbox.
- **What was fixed after durability-reviewer's findings**
  - If `settings.json` is a symbolic link (dotfiles management), the link is kept and the target is written. fsync after writing.
  - If the settings cannot be read when undoing, copilot is not stopped, and only the mark is removed.
  - If the home directory is unknown, it does not write to a relative path, and logs that it could not write.
  - If copilot from the ACP registry has its own `COPILOT_HOME`, that is where it writes.
- **Remaining weaknesses (accepted)**
  - copilot's settings have no lock, so changes written by another process (copilot, the user) between KairoCode's read and write can be lost.
  - The mark can only say "KairoCode wrote it". In cases such as crashing after writing the mark and before writing `disableAllHooks`, and then the user writing it themselves, undoing also removes the user's value.
- **copilot's repository hooks (checked. Section 6):** `disableAllHooks` did not stop them. KairoCode does not run copilot in that repository.
- **Tests:** `TestAgentsOwnHooks` (other entries and permissions are kept, no effect in project settings, undoing removes only what KairoCode wrote, the user's own is kept, does not launch with unreadable settings, launches when undoing even if unreadable, keeps symbolic links, codex's `features.hooks`)

### 5. What we checked with the real model (Addendum (2026-10-21))

- With the user's permission, we checked with the real model (codex-acp with codex 0.163, copilot 1.0.101. KairoCode's `run`, in a separate data directory).
- **codex reads `.agents/skills` itself even through codex-acp.** A skill placed in the project's `.agents/skills` showed up in codex's own list, and its contents could be used.
- **On codex, skills with the same name were listed twice (fixed).**
  - The cause was that skills with the same names as codex's own skills (`~/.codex/skills`, the bundled `~/.codex/skills/.system`) were also in `~/.claude/skills`, and KairoCode passed those too.
  - The fix is that KairoCode does not pass skills with the same name as one codex has itself (`nativeReads.ownSkillDirs`). codex's own takes their place.
    - The targets are `$CODEX_HOME/skills` (default `~/.codex/skills`, walked down, including `.system` below it) and `/etc/codex/skills`. For `CODEX_HOME`, the environment the ACP registry passes to codex is checked first.
    - Names are compared by the `name` in `SKILL.md`, not the directory. A codex skill that cannot be read is not counted as codex's (at worst it is listed twice; it does not disappear).
    - Project skills are passed even with the same name. The project's come first.
  - We checked again after the fix, and the overlap was gone.
- **On copilot, the skill list was only KairoCode's one.** copilot's own skill tool is off (`Disabled tools: … skill`), and copilot's own list is not shown to the model. Nothing is duplicated.
- **Tests:** `TestEnginesGetWhatTheyDoNotRead` gained skills with the same names as codex's own skills (`~/.codex/skills` and `.system`), and a project skill with the same name. `/etc/codex/skills` is put elsewhere in tests.

### 6. copilot's repository hooks, and the remaining tests (Addendum (2026-10-21))

- **What we checked (copilot 1.0.101, ACP. One small model call each)**
  - With a throwaway `COPILOT_HOME`, we placed a `SessionStart` hook that creates a marker file in each of: the user's settings, the repository's `.github/hooks/*.json`, and `.github/copilot/settings.json`. Hooks run on the first prompt (not on `session/new`).
  - Writing `"disableAllHooks": true` in the user's settings stops the user's hooks. **Hooks in `.github/hooks/*.json` do not stop.** Whether the repository's settings had `false`, and `settings.local.json`, made no difference.
  - Even in a folder copilot does not trust, hooks in `.github/hooks/*.json` ran over ACP (`copilot help` says it reads them only in trusted folders).
  - `hooks` in the repository's `settings.json` did not run.
  - We found no flag or environment variable that stops repository hooks for a single launch.
- **Decision:** if the working directory's repository (from the root to the working directory) has copilot hook definitions, KairoCode does not launch copilot (`copilotRepoHooks`). The turn gets back the reason, and the choice of using another provider or allowing it with `agents.ownHooks`.
  - Counted as definitions: `.github/hooks/*.json`, and `hooks` in `.github/copilot/settings.json` and `settings.local.json`. Settings that cannot be read are also counted as having definitions, since copilot might be able to read them.
  - If the user has allowed `agents.ownHooks`, it launches.
  - With the real copilot, we checked that a repository with hooks is refused (no model call).
- **Tests added**
  - `TestCopilotRepoHooks` (four forms of hook definitions, a repository without definitions, `agents.ownHooks`, no effect on codex)
  - `TestCrashHookOnMCPCall` (crash in the middle of a PreToolUse hook on a Claude Code MCP call. On resume it does not run again but asks a person. The hook runs once. We checked that it fails if the hook's effect is `Unprotected`)
  - `TestClaudeHookSandboxed` (a hook run in the sandbox cannot write outside the project, and one answered to run outside can. We checked that it fails if the sandbox is `FullAccess`)
- **What was fixed after durability-reviewer's findings**
  - **Do not let the sandbox write copilot's hooks:** if a command in the sandbox can write `.github/hooks`, then for a user who allowed `agents.ownHooks`, the next copilot turn runs it outside the sandbox. `.github/hooks` and `.github/copilot` were added to `sandbox.Protected` (ADR 0005's protected names. Same reason as `.claude`).
    - Other places under `.github` (`workflows` and so on) can be written as before.
    - write and edit refuse them too (`protected` in `internal/tools` now also matches nested paths). They are not in snapshots either.
    - On Linux, as with the other protected names, ones that do not exist yet can still be created (ADR 0005's known weakness). Even then, unless `agents.ownHooks` is allowed, copilot is not launched on the next turn.
  - **An unreadable `.github/hooks` counts as having definitions:** before, failing to list it meant "no definitions".
  - When copilot is launched for authentication (no prompt sent), the working directory is passed as a temporary directory and goes through the same check.
  - Temporary folders left behind when tests are killed were added to `.gitignore`.
  - Tests: `TestSandboxProtectsGitAndConfig` and `TestWriteRefusesProtectedNames` gained `.github/hooks`, `.github/copilot`, and `.github/workflows`, which stays writable.
- **Not checked:** whether copilot reads `.github` outside the repository or above the root. KairoCode looks from the root (where `.git` is) to the working directory.
- Of the "tests not yet written" in section 2, the first two are done. That a hook left as a child of the crashed process runs to the end stays accepted.

### 7. The second Stop hook run, and replies that wait for the user's answer (Addendum (2026-10-28))

The user's point (2026-10-28): when a reply ends with a question to the user, the Stop hook (this repository's `scripts/check.sh --quick`) stops it, the model is sent back and goes off to fix the check, and the question is washed away. Even if the user answers, it is buried under the check's report.

- **Pass `stop_hook_active: true` from the second time on (done, 2026-10-28).** Claude Code passes this when it continues after being stopped by a Stop hook. KairoCode did not pass it, so even a hook that does nothing the second time kept stopping three times.
  - The round number is read from the `.<n>` of the hook run's ID (which time with the same input, counted by the SDK. Invariant 8). A turn.end hook's input is the same each round, so this is the round number. It is a recorded ID, so it is the same on resume.
  - No field is added to the input, so the IDs of the turn's calls do not change, and the turn's version is not bumped.
  - Tests: `TestStopHookActive` (false the first time, true the second, ends on the second), `TestHookRepeat`.
- **A reply that waits for the user's answer is not sent back (Accepted (2026-10-30, the user's "go ahead"). The implementation is under "Implementation" below).**
  - Add a tool for the model to ask the user, `ask_user` (a question, and options if any). It is the same as Claude Code's `AskUserQuestion`.
    - Inside the turn, it waits as the same question as a flow's `ask` (`Kind: question`) (`kairo.WaitFor`, invariant 7). The answer goes back to the model as the tool result, and the turn continues.
    - It does nothing to the outside, so its effect is `tools.Read` (no approval needed).
    - turn.end hooks run only when the work is really done. The check does not cut in before the question.
  - When the model asks in prose and ends without using `ask_user`, turn.end hooks run as before. The send-back text in that case changes from "fix it before ending" to: "The check at the end did not pass. If your reply was a question to the user, do no other work; restate the question in one line and end. Otherwise fix it before ending."
    - This text is a value inside the turn workflow, so changing it bumps the turn's version (the previous version is `Draining`).
  - In the terminal and clients, the reply after a send-back is shown following the previous reply (the previous reply is not erased). Whether that is already so is checked during implementation.
  - **Alternatives considered**
    - **Detect whether the reply text is a question (ends with "?", and so on):** cannot be detected deterministically, and when it misses, the check is skipped.
    - **When a turn.end hook stops it, do not send back, only show the user:** the meaning differs from Claude Code's Stop hook (exit 2 stops), and the same settings would behave differently (against the purpose of this ADR).
    - **Do not run turn.end hooks for questions (the model marks them):** forgetting the mark leads to the same problem, and the mark could be used to skip the check. With `ask_user`, the check is not skipped, only postponed.
  - Tests (at implementation): answering a turn that waited with `ask_user` continues the same turn, and the turn.end hook runs only once after that. After a crash and resume, the answer comes from the record (`TestCrash...`). In a turn that asked in prose and ended, when a hook stops it, the send-back text is the new one.

- **Implementation (2026-10-30, carried out on the user's decision):** the proposal above was implemented as follows. Where it differs from the proposal is noted too.
  - **`ask_user`** (`internal/tools/askuser.go`, effect `Read`, allowed by default). The arguments are the question and 2 to 6 answers to choose from. KairoCode adds "Something else: I will say it in my next message" to the answers (because ACP clients can only answer with option buttons). If that is chosen, the model gets "The user will answer in their own words in the next message. End the turn now."
  - **The API engine:** where the turn workflow checks calls one by one (the same place it waits for approval, outside `Parallel`), it asks as the same question as a flow's `ask` (`flow.ask` and `kairo.WaitFor`), and the answer becomes the call's result (`askUser`).
  - **CLI engines (Claude Code, codex, copilot):** `claude` is not kept waiting (the same as approval in ADR 0007). The MCP `ask_user` returns at once with "The question was passed to the user. End the turn.", and puts the question in the turn's `Asks` (`AskRequest.Options`). When that round ends, the turn asks and waits, and passes the answer to `claude` as "You asked the user (ask_user): …" to continue.
  - **The send-back text:** the new text is used only when the turn's tools include `ask_user` (the `agent.load` record): "If your reply was a question to the user, do nothing else; ask with `ask_user`, and continue once you have the answer. Otherwise fix it before ending." (`stopNote`).
  - **The turn's version was not bumped.** The proposal said the version would be bumped because the send-back text changes. In the implementation, both the new text and the `ask_user` handling take effect only in turns whose recorded tool list includes `ask_user`. Turns started before `ask_user` do not have it in their recorded list, so they make the same calls with the same inputs as before (invariant 2; this fits the version thinking of kairo's ADR 0060).
  - **A custom tool with the same name:** from before `ask_user`, the user could create a custom tool named `ask_user` (the name was free). In turns that have such a tool, `ask_user` works as the custom tool as before, and the send-back text stays the old one (neither the API path nor `stopNote` applies the new handling when it is a custom tool. The MCP path routes custom tools first, so it already works this way). This keeps "calls of turns started earlier do not change" true in this case too.
  - "In the terminal and clients, the reply after a send-back is shown following the previous reply" is not checked.
  - Tests: `TestAskUser`, `TestAskUserArguments`, `TestAskUserBeforeStopHook` (while waiting for the answer the Stop hook does not run, and it runs once after the answer), `TestCrashAskUser` (crash while waiting on the question and resume; answering the same question continues the same turn. The earlier model call is not redone), `TestClaudeEngineAskUser`, `TestStopNote`.

### 8. The sandbox for Claude Code's hooks: local network access, and where they keep their own records (Addendum (2026-10-28), Accepted (2026-10-28))

This continues section 7. The main reason the Stop hook stops things was the sandbox the hook runs in.

- This repository's Stop hook (`scripts/check.sh --quick`) has tests that listen on `127.0.0.1` and Unix sockets. KairoCode's sandbox has no network, so the tests do not start, and it stops with "this does not run in this environment" (exit 3).
- The record of passing (`.claude/.cache/last-green`) cannot be written either, since `.claude` is a protected name. A turn that changes source is always stopped, even if the code is correct.
- We reject moving the record outside `.claude` (to a place the sandbox can write). The model's bash could write the same place, so it could write "passed" without running the check and skip the check at the end.

Decision (proposal):

- **Run hooks in a sandbox that allows only local network access (`Loopback`, ADR 0031).** The same as bash's `sandbox: "loopback"`: only localhost and Unix sockets in the hook's own TMPDIR. No outside network, as before. The daemon's socket is not visible (it cannot approve its own calls).
- **Allow only hooks to write `.claude/.cache`** (`Policy.Writable`). It is the name Claude Code's hooks use as the place to keep their own state. Other places in `.claude` (settings, hook scripts) stay unwritable, as before. The model's bash is not allowed.
  - With this, a hook can run the check, pass it, and record that it passed. With the same source, the check is not repeated after that.
  - The model cannot write the record to skip the check. The hook scripts are in `.claude/hooks`, and changing them leads to asking again (section 2).
- Hooks answered "run outside the sandbox" are as before.
- Tests: a hook can listen on localhost. A hook can write `.claude/.cache` and cannot write `.claude/settings.json`. bash in the same sandbox cannot write `.claude/.cache`. On both macOS and Linux.
- Alternatives considered:
  - **Put the record outside `.claude`:** as above, the model could skip the check.
  - **Have the user answer to run the hook outside the sandbox:** possible today, but then the hook script can do anything. The check needs only local network access and a place for its record.
  - **Do not stop when a hook stops with "this does not run in this environment":** then it could end with no check.
- **Implementation (2026-10-28):** `hookCommandAction` runs hooks in the sandbox with `Loopback`, and for Claude Code's hooks adds `.claude/.cache` to `Policy.Writable` (created if missing; not added if `.claude` or `.claude/.cache` is a link). On Linux, when a writable place is inside a protected name of another writable place, it is made writable again after the protected names are made read-only (macOS has rules per writable place, so it is writable as is). Tests: `TestSandboxWritableInsideProtected`, `TestClaudeHookLoopbackAndCache`, `TestHookCache` (passed on macOS. Linux in CI).

### 9. A setting to not read Claude Code's hooks (Addendum (2026-10-30), the user's decision)

The user's point (2026-10-30): it is scary when a hook whose contents you cannot see suddenly runs. KairoCode does not run hooks without asking, but if Claude Code's settings have hooks, it asks every time before they first run in that folder. There was no way to stop being asked at all.

- **`"agents": {"claudeHooks": "off"}` in `kairocode.json` stops reading Claude Code's hooks (`~/.claude/settings.json`, `.claude/settings.json`, `.claude/settings.local.json` from section 2).** The default `"ask"` stays as before (ask before the first run).
  - It is decided by the user's settings; project settings can only set `"off"` (make it stricter) (the same as `sandbox.loopback` in ADR 0031). Unknown values count as `"off"`.
  - It is read inside `agent.load`, so the value is recorded, and a turn in progress goes on with the value from its start (invariant 2). It does not affect KairoCode's own hooks (`hooks` in `kairocode.json`).
- **How the question is shown (same day):** a hook's question includes the first 60 lines of the file the command points to (from `~` or the folder), and says that this setting can stop it (read when the ACP agent shows it; not kept in the record). As with flow questions, every option is of a kind that is not selected until the user picks it (reject).
- Tests: `TestClaudeHooksOff` (combinations of values, and with `"off"` the turn proceeds without asking about hooks), `TestACPHookQuestion`.
