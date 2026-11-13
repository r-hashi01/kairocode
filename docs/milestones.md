# How we proceed (M0 to M9)

The milestones of design section 10, turned into acceptance-criteria tests. Work on a milestone with the `/milestone` skill. Write the acceptance criteria as tests before implementing (they fail at first). When they all pass, set that milestone's "Status" to "done (date)".

Current stage: **M13 (checks decide when work is done) is done (2026-11-13)**: the first step toward KairoCode as a software factory you talk to (`docs/factory.md`, ADR 0042), checked by hand with a real model (three faults fixed). **M14 (lines and work items) is in progress (2026-11-13)**: step 2 of `docs/factory.md`, ADR 0043. **M12 (check by hand what was left) is done (2026-11-11)**: worktrees in the terminal UI, `/usage` with Claude Code, OpenAI's own API, pasting images on Linux (X11 and Wayland, in a container) and a real 429 (the kind that never passes; the kind that passes after a wait, with a fake server) are checked; six faults were found and fixed. M11 (rate limits per provider) is done (2026-11-09): the calls to each provider keep to its requests and tokens a minute, and wait as long as a 429 asks (ADR 0040). M10 (usage and limits) is done (2026-11-09): `/usage` shows what has been used in the session, today and this week against the limits, and limits stop a turn (ADR 0039); checking by hand with a real model remains. M9 (deepen what sets it apart) is done (2026-11-09): a flow made from a session (ADR 0036), and sessions side by side in git worktrees with `/sessions` (ADR 0037); checking the worktrees by hand in the terminal UI remains. M7 (show the strengths) is done (2026-11-06): the durability demo, with a flow and with Claude Code and codex as the engine, and approval requests told outside the machine (ADR 0034). M8 (close the gap at the entry points) is done (2026-11-09): `/init`, and OpenAI-compatible APIs as providers (ADR 0035). What is left of it is checking by hand: the adapter with OpenAI's own API, and pasting images on Linux (a real Ollama was checked). M6 is done (2026-11-04, v0.1.0 published). M5 is done (2026-10-22). M3 is done too. What remains of M2 is dspchr (waiting for ADR 0004's approval) (checking by hand with the real `claude` and in Zed was done at M6's gate). What remains of M4 (PostgreSQL, suspend mode, starting from GitHub) is on hold together with ADR 0012.

## M0 Skeleton

Status: done (2026-10-09)

Contents: add kairo's SDK (`github.com/r-hashi01/kairo` v0.3.0 or later) and `modernc.org/sqlite` to `go.mod`. `internal/runtime` (`kairo.Open`, mapping tool effects). The server, the CLI, the session and turn workflows, the model (Anthropic and fake), the reading tools (read, glob, grep, list), message storage (`kc_message`).

Prerequisite: approval of ADR 0001.

To decide: the shape of sessions and turns (design section 2.1). Either the session (the root) repeats `kairo.WaitFor("message")` and `kairo.Child("turn", …)`, or each turn starts as a root run with `WithID`. The former can use deletion by whole tree and the way cancellation propagates, but on resume the session function re-reads the record once per turn.

Acceptance criteria:


- [x] `TestCrashTurnResume`: force-kill the process in the middle of a turn (during the model's second call); on resume it proceeds to the same final response. A finished model call is not called again (checked with the fake's call count).
- [x] `TestHistoryStoredOnce`: in a 10-turn conversation, the number of `kc_message` rows equals the number of messages. The input of a model call contains no message text (NFR-5).
- [x] `TestWorkflowIDs`: the session ID is `s/<ID>`, and if the same message is sent again after a failed turn, the turn is a different run (the child's input contains the turn number).
- [x] `kairocode run "<request>"` runs to the end with the fake provider.
- [x] The `internal/invariants` checks pass against the real workflows and tools.

## M1 Writing tools and approvals

Status: done (2026-10-09)

Carried over from M0's review (`durability-reviewer`, 2026-10-09):

- In a real tool's handler, wrap errors that happen after the effect (a failure to save the tool_result, and so on) in `kairo.Unknown`. Let `tools.Def.Run` return "we do not know whether it took effect" (invariant 4).
- Writing tools walk paths with `os.OpenRoot` or `O_NOFOLLOW`, to guard against a swap after `resolve` (TOCTOU).
- `kairocode serve` prints the token it created automatically to stderr. Replace it with ADR 0004's `server.json` (0600). → Solved at the start of M2 with ADR 0008's daemon (`kairocode serve` is gone, and the token lives in `server.json`).

Contents: edit, write, bash, git (commit, push), effect types, permission rules and decisions (`permission.check`), approval waits, blocked (needs confirmation), the command sandbox (ADR 0005).

Order of work (2026-10-09):

1. The tool contract: allow returning "we do not know whether it took effect". Wrap errors after the effect in `kairo.Unknown`.
2. Move file operations to `os.Root` (closes TOCTOU).
3. edit and write: two stages, "plan" (read only; records the hashes before and after the write) and "apply" (idempotent; compare and replace) (design section 3.1). `TestCrashWriteOnce`.
4. Permission rules, `permission.check`, approval waits, plan mode (needs approval of ADR 0002's part 1).
5. The sandbox (ADR 0005). macOS and Linux.
6. bash and blocked (needs confirmation). `TestCrashBashNeedsReview`.
7. git commit and push. `TestCrashGitCommitOnce`.
8. With the Claude Code engine, route real tools through MCP. Record results in our own table (the remedy for r-hashi01/kairo#4). If the same real call as one that finished in the previous process comes in, ask a person.
9. `TestResumeAfterLongApproval`. Move the server's token to `server.json`.

Acceptance criteria:

- [x] `TestCrashWriteOnce`: force-kill during write and edit and resume, and the file ends up written once. If a person changed it in between, it goes back to the model as a conflict.
- [x] `TestCrashGitCommitOnce`: force-kill during git commit and resume, and exactly one commit is made.
- [x] `TestCrashBashNeedsReview`: force-kill during bash (real), and on resume it becomes blocked (needs confirmation); the answer (it ran / it did not) comes back with `k.Resolve` / `k.ResolveFailed`, and it continues. The command is not run again without checking.
- [x] `TestApprovalFromAnotherProcess`: give an approval from another process to a session waiting for approval, and the session continues.
- [x] `TestPermissionRecordedAtTurnStart`: even if the permission settings change while waiting for approval, the resumed turn proceeds with the decision from its start.
- [x] In plan mode, only `tools.Read` tools are called.
- [x] The sandbox (ADR 0005): `TestSandboxDeniesWriteOutsideWorkspace`, `TestSandboxProtectsGitAndConfig`, `TestSandboxDeniesNetwork`, `TestSandboxStripsSecrets`, `TestSandboxIgnoresModelCwdOutsideRoot`, `TestSandboxFailsClosed` pass on both macOS and Linux (Docker).
- [x] `TestEscalationAlwaysAsks` (bash escalation. In step 6).
- [x] With the Claude Code engine, route real tools through MCP (ADR 0007): `TestClaudeEngineAsksForApproval`, `TestClaudeEngineRepeatAfterInterruptionAsks`, `TestCrashClaudeEngine`.
- [x] A turn that waited more than 24 hours for approval, then crashed and resumed, does not redo finished calls (`TestResumeAfterLongApproval`. Keeps checking, on KairoCode's turns, what kairo 0.3.0 solved).

## M2 Clients, streaming, cancellation, dspchr

Status: in progress

Contents: the ACP agent (ADR 0002. `kairocode acp`, v1), the terminal UI (`kairocode chat`, as an ACP client), the Claude Code engine (ADR 0007), token streaming (`TaskContext.Emit` and `k.Subscribe`), snapshots and `/undo` / `/redo`, LSP diagnostics, Markdown definitions of agents and commands, stage 1 of the dspchr integration (ADR 0004).

Prerequisite: approval of ADR 0002, 0004, 0007 and 0008.

Acceptance criteria:

- [x] One daemon per data directory (ADR 0008): a second one does not start (`TestSecondDaemonDoesNotStart`). Clients start it automatically, and `server.json` is 0600 (`TestClientStartsTheDaemon`). An approval from another client lets the waiting turn continue right away (`TestApprovalFromAnotherClient`. The lease period stays at the default 30 seconds, and it continues within milliseconds). Restarting a crashed daemon lets the turn continue without waiting for the lease period (`TestCrashDaemonRestart`). It stops when idle (`TestIdleDaemonStops`), and does not stop while a client waits (`TestDaemonStaysWhileAClientWaits`). A daemon started after one that is on its way out waits for the lock and takes over (`TestDaemonWaitsForTheOneOnItsWayOut`).
- [x] Checking the daemon's version (ADR 0008's addendum): a client of a different version replaces a daemon that runs no steps and that no one else uses (`TestOtherBuildReplacesTheDaemon`); otherwise it keeps using it and tells the user (`TestOtherBuildKeepsABusyDaemon`, `TestOtherBuildKeepsADaemonThatRuns`).
- [x] Choose the engine and model per session (ADR 0008's addendum): `TestEngineAndModelPerSession`.
- [x] `kairocode daemon stop`: replies after the daemon lets go of the data directory (`TestStopTheDaemon`). Running steps get up to 30 seconds to finish before it stops (kairo 0.4.1's `Drain`, `TestStopLetsTheRunningStepFinish`).
- [x] Handle a waiting approval from many clients (ADR 0008's addendum): every client watching the same session gets the approval request, the first answer wins, and the other clients get "answered". Clients that connect later (including ACP's `session/load`) get the waiting approval sent again. → The SSE side is done (`TestApprovalToManyClients`, `TestEventsReplayPendingOnJoin`, `TestCrashDaemonEventsAfterRestart`). The ACP side is `TestACPApprovalSurvivesRestart`, `TestACPPermissionAnsweredElsewhere`.

- [x] Over SSE, tokens, tool start and end, approval requests, blocked (needs confirmation), and the end of the turn arrive (`TestSSEDeliversTurnEvents`, `TestEventsOfATurn`, `TestEventsStreamTokens`, `TestEventsOfAClaudeTurn`, `TestCrashEventsAfterRestart`).
- [x] `TestACPContract`: the requests and responses of the implemented ACP methods match a pinned version of ACP's JSON Schema (v1, `schema-v1.26.0`).
- [x] With an ACP client that sends messages in a fixed order, `session/new` → `prompt` → `tool_call` updates → `request_permission` → answer → end of turn works (`TestACPTurn`, `TestACPCommand`). Blocked (needs confirmation) can also be answered with `request_permission` (`TestCrashACPReview`).
- [x] Build the terminal UI (`kairocode chat`, `kairo` with no arguments) as its own ACP client (ADR 0002): it writes replies as they arrive, shows tool calls, asks for permission in the terminal, and with `-session` redisplays the conversation and continues (`TestChat`, `TestChatAsks`, `TestNoCommandIsTheChat`, `TestClientKeepsTheOrderOfUpdates`, `TestClientPermission`).
- [x] `kairo` is a TUI in a terminal (ADR 0010): header, input box, status line, reply and tool lines, approvals chosen with arrows (blocked (needs confirmation) starts with nothing selected), withdrawal, command completion, Ctrl-C twice to quit (`TestTurn`, `TestAsk`, `TestKeys`, `TestQueuedDuringATurn`, `TestHelp`, `TestReplyWraps`, `TestNoSize`). In a folder seen for the first time it asks for trust, and on "no" it reads nothing (`TestTrustQuestion`, `TestChatInAFolderNotTrusted`, `TestTrust`). Esc stops the turn (ADR 0011).
- [x] `TestACPApprovalSurvivesRestart`: even if the ACP process is stopped while waiting for approval, `session/load` in the next process delivers the pending `request_permission` again, and answering it lets the turn continue.
- [ ] The Claude Code engine (ADR 0007): `TestClaudeEngineResumeDoesNotRepeatARealCall` (with a fake `claude`, a real tool called again under a new ID after a crash does not run twice). Check by hand with the real `claude`. → The tests pass (`TestClaudeEngineResumeDoesNotRepeatARealCall`, `TestCrashClaudeEngine`). Checking with the real `claude` remains.
- [x] Turn cancellation (ADR 0011, #11): stopping during a model call ends the turn as cancelled, and the next turn's history contains the user's message and a note. Stopping during an approval wait removes the wait, and the tool does not run. Crashing and resuming after a cancellation does not duplicate the note. A version 4 session drops the stopped turn. ACP's `session/cancel` and the TUI's Esc stop it. (`TestCancelTurn`, `TestCancelWhileAwaitingApproval`, `TestCancelStopsARunningCommand`, `TestCancelInAVersion4Session`, `TestCrashCancel`, `TestCancelOverHTTP`, `TestACPCancel`, `TestEscStopsTheTurn`. Session version 5)
- [x] After an edit, LSP diagnostic errors are added to the tool's output. (ADR 0009. `TestWriteGetsLSPErrors`, `TestCrashWriteOnceWithLSP`, the tests in `internal/lsp`)
- [x] Read `.opencode/agents/*.md`, `.opencode/commands/*.md` and `.opencode/skills/` with the same meaning as opencode. (`TestAgents`, `TestCommands`, `TestExpand`, `TestSkills`, `TestOpencodeAgent`, `TestOpencodeAgentRefused`, `TestOpencodeSkills`, `TestOpencodeCommand`, `TestACPProjectCommand`)
- [x] `/undo` returns the working files to the previous turn's state, and `/redo` brings them back. The record is not deleted. (`TestUndoRedo`, `TestUndoThenNewTurnClearsRedo`, `TestUndoAfterRestart`, `TestCrashUndo`, `TestACPUndoRedo`. Session and turn version 4)
- [ ] `TestDspchrStateReports`: when `DSPCHR_SESSION` is set, `dspchr state` is started as in ADR 0004's table at the start and end of a turn, an approval request, blocked (needs confirmation), a failure, and on close (`dspchr` is replaced by a fake command that records its arguments). Without `DSPCHR_SESSION`, nothing is started.
- [x] `kairocode usage --session <ID> --json` returns the recorded usage (input, output, cache). (`TestUsageOfASession`, `TestRunWithFakeProvider`)
- [ ] Everyday work (fixing bugs, adding features, refactoring) can be done from an ACP client (Zed) with the same effort as opencode (checked by hand).

## M3 Subagents, and workflows started by people and events

Status: done (2026-10-19)

Contents: task (subagents). Starlark flows (ADR 0003: `.kairocode/workflows/<name>.star`, the primitives `agent`, `parallel`, `decide`, `tool`, `ask`, `phase`, divergence detection, limits. `wait` for outside events is M4). Entry points are `/flow <name>`, hooks (`turn.end`, `tool.after`, `session.start`), and an agent's `workflow:`. Making one from words (`/flow new <description>`). The flow chart in the terminal (when making one, while it runs, `kairocode flow graph`, `show`, `stats`), and changes on the chart.

Prerequisite: approval of ADR 0003 (2026-10-19), ADR 0011 (stopping the parent stops the children).

Acceptance criteria:

- [x] `TestCrashWorkflowScriptResume`: force-kill a script that runs 10 `agent`s in `parallel` partway and resume, and finished leaves are not redone.
- [x] `TestWorkflowScriptDivergence`: resuming with a script that makes calls different from the record stops without running.
- [x] `TestWorkflowScriptLimits`: stops at the limits on steps, number of agents and tokens. (`TestFlowMistakesAreReplies`, `TestFlowTokenLimit`)
- [x] `TestTurnEndHookBlocks`: when a `turn.end` flow returns `ok` as false, the message goes back to the model and the loop continues, stopping at the limit on the number of times.
- [x] `TestWorkflowGraph`: the flow chart correctly shows sequence, `if`, `parallel`, `for`, expansion of `def`, and `phase`, and when the width is too small it collapses and switches to a tree chart. (`TestBuild`, `TestChartLooks`, `TestChartStates`, `TestChartTooNarrow`, `TestFlowGraph`)
- [x] `TestWorkflowLive`: the state of the run tree shows in the boxes' marks and colors and in loop iterations. `kairocode flow show <run> --follow` prints the same chart. (`TestFlowEvents`, `TestFlowAgentWaiting`, `TestFlowLive`, `TestACPFlowPlan`, `TestFlowShowFollow`)
- [x] `TestDraftFromWords`: with `/flow new`, the script the model wrote passes the checks and becomes a file, is visible in the chart, and runs with `/flow <name>`. A change asked for in the input box redraws the chart. (`TestNewFlow`, `TestFlowWrite`, `TestCheck`, `TestDiff`, `TestFlowsTheTurnMade`)
- [x] `TestEditOnTheChart`: rewriting, deleting or reordering a box's contents on the chart changes only that argument and the order of statements in the script. (`TestSetText`, `TestDeleteAndMove`, `TestEditOnTheChart`)
- [x] Even if the script is rewritten while it runs, the running run finishes with the recorded script. (`TestFlowKeepsItsScript`)
- [x] Before a repository's hook runs for the first time, ask the user. (`TestTurnEndHookBlocks`. Asks again when the script changes)
- [x] When an agent definition has `workflow:`, that agent's turns proceed with that flow. (`TestAgentWorkflow`)
- [x] Stopping the parent stops the children (ADR 0011). (`TestCancelStopsTheFlow`)

## M4 Non-interactive and serverless

Status: in progress (ADR 0012 is on hold by the user's decision (2026-10-21). The HTTP(S) tools are done (ADR 0013))

Contents: the flow's `wait(event, timeout=)` (ADR 0003), PostgreSQL, suspend mode (`kairo.Suspend`, `/tick` of `k.Handler()`), the HTTP(S) tools, starting from GitHub.

Acceptance criteria:

- [ ] Start from an issue comment, and no process stays around while waiting for approval and CI results.
- [x] The HTTP(S) tools (ADR 0013): `fetch` (GET, read only) and `http` (POST, real). If it crashes during a POST, it does not resend and asks a person. Authentication in the user's settings stays out of the record. (`TestFetch`, `TestHTTPPost`, `TestHTTPUnknown`, `TestNetAuth`, `TestNetPrivateRefused`, `TestNetRedirects`, `TestNetSettingsAreTheUsers`, `TestCrashHTTPPost`)
- [x] `kairocode run` pauses at an approval and continues with `kairocode resume`. (`kairocode run -detach` stops where it waits for a person (exit code 3), and `kairocode resume <session>` follows the latest turn to the end. When stopped with Ctrl-C it also shows how to continue. `TestRunDetachAndResume`)

## M5 Long-term use: compaction and forks, wider tools, records

Status: done (2026-10-22). ADR 0024 (limits and rate limiting) is on hold by the user's decision

Contents (chosen by the user, 2026-10-21. What remains of M4 is on hold together with ADR 0012):

- History compaction (FR-3) and forks (FR-4). ADR 0020, 0021.
- Tools from the user's MCP servers (FR-8), custom tools (FR-9), todo (FR-5). ADR 0022.
- Browsing the record (FR-24). ADR 0023. Replay (FR-25) will not be built, by the user's decision (2026-10-22, ADR 0023's implementation item 2).
- Limits (FR-26) and rate limiting (FR-27) are on hold by the user's decision (2026-10-22). If a need arises, such as KairoCode billing on its own or narrowing the usable models, they go in with ADR 0024 (still proposed).

Acceptance criteria:

- [x] A conversation nearing the context limit is summarized and continues. The record from before the summary is not deleted and can be browsed. Even after crashing and resuming, it is summarized only once (ADR 0020. `TestCompactNearTheContext`, `TestCompactAtTheTurnsStart`, `TestCompactWhenTooLong`, `TestCrashCompact`).
- [x] A session forked from a past turn continues with the conversation up to that turn. The original session does not change (ADR 0021. `TestForkFromATurn`, `TestForkWithFiles`, `TestForkOnClaudeCode`).
- [x] Tools from the user's MCP servers can be used with their declared effect types. Tools without a declaration are treated as real, and if the outcome is unknown a person is asked. MCP servers and custom tools defined by the project do not run outside the sandbox without approval (ADR 0022. With every engine. `TestMCPServerTool`, `TestCrashMCPServerTool`, `TestCrashMCPServerToolOnClaudeCode`, `TestProjectMCPServerAsks`, `TestProjectMCPServerChanged`, `TestCodexOwnMCPServersOff`).
- [x] The todo list continues across turns and is visible to clients (ADR 0022. `TestTodo`, `TestACPTodoPlan`).
- [x] You can see all of a session's turns, model inputs and outputs, tool calls and results, waits, and usage (`kairocode log`, `/log`. Amounts of money after ADR 0024. `TestSessionLog`).

## M6 Preparing to publish (release as open source for individuals)

Status: done (2026-11-04)

Contents (chosen by the user, 2026-10-27: release as open source for individuals): license, version display, CI, releases, README, checking by hand. Publish once these are done (limits were moved to after publishing on 2026-10-30).

Acceptance criteria:

- [x] Add a LICENSE (MIT) (2026-10-27). Keep the Apache-2.0 and NOTICE of `third_party/codex-sandbox` as they are. Making them reachable from the README is done in the README item.
- [x] Outside people can fetch kairo's SDK without `replace` (proxy.golang.org has v0.4.1. Checked on 2026-10-27).
- [x] `kairocode version` prints the version and commit embedded at build time (if none were embedded, from Go's build info. `TestVersionLine`, `TestVersionCommand`).
- [x] `scripts/check.sh` passes in CI (GitHub Actions, macOS and Linux) (2026-10-28, `341af18` in PR #12. Passed on both, including the tests that start the sandbox).
- [x] A tag produces release binaries (macOS and Linux, amd64 and arm64), and it installs with `go install` and a Homebrew tap (2026-11-04, v0.1.0).
  - 2026-11-04: published. The private repository was renamed to `r-hashi01/kairocode-dev` and kept, and `r-hashi01/kairocode` was newly made public with only `main` and the tags pushed (force pushes and deletion of `main` are forbidden). The first tag's binaries called themselves `+dirty` (`dist/` was not in `.gitignore`, so the builds from the second on saw untracked files), so we ignored `dist/` and re-tagged before publishing. Confirmed it installs with `brew install r-hashi01/tap/kairocode` (and `brew test`) and with `go install …@v0.1.0`. The tap is `r-hashi01/homebrew-tap`.
  - 2026-10-30: the machinery is ready. On a `v*` tag, after passing the checks, it attaches the four binaries, checksums and the Homebrew formula to a draft release (`.github/workflows/release.yml`, `scripts/release-build.sh`). The publishing procedure is `docs/release.md`. The actual tag and tap come at publishing time.
- [x] A README for users (how to install, first setup, how to use `kairo`, `/flow`, the sandbox, supported OSes macOS and Linux, third-party licenses) (2026-11-04, published with an install section added).
- [x] The gate before publishing: do everyday work (fixing bugs, adding features, refactoring) by hand in Zed and the TUI (what remained of M2). Fix the bugs that come up before publishing (2026-11-04, checked by the user).
  - 2026-10-28, fixed bugs found using the TUI: screen flicker (write replies above, one line at a time), asking about the second run of the same command as "after an interruption" (ADR 0007's addendum), the end-of-turn check not passing inside the sandbox and pushing the question away (ADR 0018's sections 7 and 8). Added: clipboard images (ADR 0026's addendum), terminal notifications (ADR 0032), continuing from a failed session (ADR 0021's addendum).
  - 2026-10-30, TUI: reproduced in tmux and fixed the bug where old lines of the input box remain after resizing the window (ADR 0033's addendum). Aligned the look on KairoCode's vertical line, and showed what continues in the folder under the header. Pasted labels show the start of the text and can be opened with Ctrl+O.
  - 2026-10-30, Zed: quitting Zed while waiting for approval and opening it again showed the same approval as the requested message; allowing it went on from where it was, and the answer came back (dify-oss, the Claude Code engine). Fixed along the way: a question (hook) not shown in an answerable form, the list of commands right after creating a session being dropped, a turn's wait looking like a failure on handover to a new version, the messages of a turn in progress not showing on reopening, display names (Claude Code, the default model and effort, the default rules). Questions from hooks come with the contents of the script they run.
  - 2026-11-02: on reopening, also replay calls that already ran in the turn in progress (PR #17). Checked in Zed.
  - 2026-11-02, cleaning up the code before publishing (PR #17 to #21): split large files by job, and added `internal/config`, `api`, `plugins`, `secrets`, `clipboard`, `flowfile`. tui and acp do not import runtime (`TestClientsDoNotImportTheRuntime`). Fixed along the way: decide `sandbox.loopback` by the value at the start of the turn (ADR 0031's addendum), keep `/provider` and `/mode` choices not yet sent even if the daemon stops (ADR 0014's addendum), do not read the user's settings by a relative path when there is no home, align the flow's `decide` to the order API, then Claude Code (ADR 0014's addendum).
  - 2026-11-04, fixed what came up in the hand check (PR #23 to #26): with just `/flow` in the TUI, a screen to choose a flow (run it, change it on the chart, view runs, make a new one); after a command's name, show what that command takes (`/plugin install`, `/flow new`, flow names, models, and so on). Zed: answering a question looked like a rejection (×) whatever was chosen, `flow_write`'s draft was not visible (now shown as a diff), the flow review was garbled (sections as Markdown, the script as a diff).
  - Remaining: none (the publishing procedure is the two items above).

## M7 Show the strengths

Status: done (2026-11-06)

- [x] A durability demo: a recording showing that it continues after `kill -9` and nothing happens twice, and steps to reproduce it (`docs/durability.md`). Put it at the top of the README. (2026-11-06: `scripts/demo-durability.sh` runs a five-step flow and kills its daemon in the middle of a step, in a folder of its own, with the fake provider; `make demo-gif` records it as `docs/durability.gif` with asciinema and agg.)
- [x] Notify approval requests outside the local machine (Slack and so on). Add an "approval request" event to hooks and send it with a flow (needs an ADR). Answering from outside is decided separately later. (2026-11-06, ADR 0034: the `approval.requested` hook; the daemon starts a `notify` run per request, `n/<session>/<hash of the key>`, which runs the hooks its turn recorded, those already allowed only. `docs/notify.md` sets it up for Slack with `http.auth`. Tests: `TestApprovalRequestedHookRuns`, `TestApprovalRequestedOncePerRequest`, `TestCrashApprovalRequested`, `TestApprovalRequestedNotAllowedHookSkipped`, `TestNoNotifyOfNotify`, `TestApprovalRequestedSecretNotRecorded`, which runs the flow of `docs/notify.md`.)
- [x] An explanation and demo that the same guarantees hold when another company's agent (Claude Code, codex) is the engine. Also finish checking with the real `claude` (what remains of M2) here. (2026-11-06: `docs/durability.md` explains it, and `scripts/demo-engines.sh claude-code|codex` asks the real agent for five `bash` steps and kills the daemon in the middle of one. It passed with Claude Code 2.1.317 and, three times out of three, with codex 0.164. The first runs with codex found that it used its own shell unasked half the time: its own shell is now off and its sessions read-only (ADR 0015's addendum), and its plugins are off with its MCP servers (ADR 0022's addendum).)

## M8 Close the gap at the entry points

Status: done (2026-11-09). Checked by hand (2026-11-11, M12): the adapter with OpenAI's own API (a fault fixed), and pasting images on Linux (X11 and Wayland, in a container)

- [x] An adapter for OpenAI-compatible APIs (including local models). Written with the standard library's HTTP (ADR 0014's addendum or a new ADR). (2026-11-09, ADR 0035: Chat Completions, streamed; `oai:openai`, `oai:ollama`, and providers named in the user's own settings; the key from an environment variable, https or loopback only, kept out of the record. Checked with a real Ollama on 2026-11-09; not yet with OpenAI's own API.)
- [x] Image input (ADR 0026). From files (`@<path>`, files dropped on the terminal), ACP and the API (2026-10-27).
- [x] Pasting clipboard images into the TUI (ADR 0026's addendum, 2026-10-28). On Linux, checking by hand remains.
- [x] `/init`: read the repository and make a draft AGENTS.md. (2026-11-09: KairoCode's own command, in the terminal UI, editors and `kairocode run /init`; the model reads the docs, build files, CI and other agents' instructions, and writes AGENTS.md, or improves the one there. A project's command or skill named `init` comes before it. `TestInitCommand`.)

## M9 Deepen what sets it apart

Status: done (2026-11-09). Checked by hand (2026-11-11, M12): worktrees in the terminal UI

- [x] Make a flow from a session that went well (the idea in "Future work" below. Needs an ADR). (2026-11-09, ADR 0036: `/flow new --from-session[=<id>] [words]` gives the model the session's record, cut to size, and it writes a draft as `/flow new` does. `TestFlowFromSession`, `TestFitTurns`.)
- [x] Sessions run side by side in worktrees, and a screen listing them (needs an ADR). (2026-11-09, ADR 0037: `kairo -worktree`, `kairocode run -worktree`, `/new --worktree`; `/sessions` and `kairocode sessions -repo`; the git tools write the repository's common git directory. `TestWorktreeSession`, `TestWorktreeMadeAgainAfterACrash`, `TestGitCommitInWorktree`, `TestRunInAWorktree`, `TestRepoSessions`, `TestLinked`.)

## M10 Usage and limits

Status: done (2026-11-09; ADR 0039; chosen by the user). Checked by hand (2026-11-11, M12): `/usage` and a limit with Claude Code; two faults fixed. It took the place of answering from outside the machine, which was dropped the same day (ADR 0038, rejected): what ADR 0034 already does (telling, with the turn waiting in kairo) is enough.

- [x] A person sees what they have used, in this session, today and this week, against their limits, as Claude Code's `/usage` and Codex's `/status` show it (`/usage`, `kairocode usage`). (2026-11-09: `TestPrices`, `TestUsageLedger`, `TestUsageWarning`.)
- [x] Limits on a turn (steps, time), a session, a day and a week (tokens, cost) stop a turn before its next model call, from recorded values only, and say which limit and until when (ADR 0039, replacing ADR 0024). (2026-11-09: `TestLimitSessionTokens`, `TestLimitDayCost`, `TestLimitTurnTimeNotWaiting`, `TestLimitsProjectCannotLoosen`, `TestLimitsRecordedAtTurnStart`.)

## M11 Rate limits per provider

Status: done (2026-11-09; ADR 0040; chosen by the user). Checked by hand (2026-11-11, M12): OpenAI's real 429 for a request too large; the 429 that passes after a wait, with a fake server only. Two faults fixed.

- [x] Calls to one provider, from every session, agent and flow of the daemon, stay within its requests and tokens a minute (`rateLimits`), and a 429 makes them wait as long as the provider asks, instead of failing after three quick tries (FR-27, ADR 0040). (2026-11-09: `TestRateLimitRequests`, `TestRateLimitTokens`, `TestRateLimit429`, `TestRateLimit429Turn`, `TestRateLimitCancelled`, `TestRateLimitProjectCannotLoosen`, `TestRateLimitErrors`.)

## M12 Check by hand what was left

Status: done (2026-11-11; chosen by the user). Six faults were found and fixed.

- [x] Worktrees in the terminal UI (M9): `/new --worktree feat` made the worktree and its branch and moved the session there; `/sessions` listed both worktrees with the untracked file; Enter took up the main session; `d` asked again for a worktree with changes, then removed it and its branch; its session showed as "folder removed".
- [x] `/usage` and a limit with a real model (M10), Claude Code as the engine: the status line and `/usage` showed the cost Claude Code reported; a session limit already passed did not stop the next turn (fixed: a cost limit counts what was spent even with no price), and the reason was not shown to ACP clients (fixed). `kairocode usage` now reads the folder's limits.
- [x] The OpenAI-compatible adapter with OpenAI's own API (M8), `gpt-5.4-mini` (2026-11-11): the first turn with tools was refused (400: a tool's schema must be `type: "object"`; the built-in tools give only their properties, which Anthropic's API and Ollama take). Fixed: the adapter adds `type` and `properties` where a schema leaves them out. Then a turn reading two files ran through, and with `rateLimits` `rpm: 2` its third call waited about a minute, said on the status line (the seconds shown do not count down), and `/usage` showed the limit and the model's cost as not known.
- [x] Pasting images on Linux (M8), in a Debian container (2026-11-11): on X11 (Xvfb, the PNG put on the clipboard with `xclip`) and on Wayland (headless sway, `wl-copy`, no `DISPLAY`), Ctrl+V put `[Image #1]` in the input, and `gpt-5.4-mini` answered "Red" for the red square sent with it. Found on the way: OpenAI's 401 echoes the key masked (`sk-proj****…F0A`) into the error, which is shown and kept; keys are now taken out of OpenAI-compatible servers' messages.
- [x] A provider's real 429s (M11), with OpenAI's own API (2026-11-11). Met for real: the 429 for one request larger than `gpt-4.1`'s tokens a minute ("Request too large", an `AGENTS.md` of about 610,000 tokens), refused unbilled. The provider was held back and the call asked again as designed, said on the status line and counted by `/usage` (25 429s in about 6 minutes, then the turn failed). Fixed: a 429 no wait makes pass ("Request too large", `insufficient_quota`) now fails at once. Not met for real: the 429 for a minute's limit used up ("Please try again in 2.3s"), which this account's high limits do not give; checked with a fake server answering as OpenAI does, and fixed on the way: the wait its message names is now read (it waited the default 10 seconds).

## M13 Checks decide when work is done

Status: done (2026-11-13; ADR 0042 accepted; chosen by the user). The first step toward KairoCode as a software factory you talk to (`docs/factory.md`): who decides that work is done moves from the model to gates, on a flow's agents and on a plain turn, with the verdict in the record. Lines, whose stations will carry the same gates, come next. Results other programs read (ADR 0041, proposed) come after. Updating to kairo's main (the fixes of kairo#2 to #7, with their workarounds removed) waits until the user's work on kairo settles.

- [x] A flow gates its agents: `agent(..., checks=)` passes work on only when the checks pass, sends the agent back up to `rounds`, and else returns `.error == "checks"` with the verdicts for the flow to decide; `check()` runs a check as a step; `flow stats` shows each gate's yield (ADR 0042). (2026-11-13: `TestFlowChecks`.)
- [x] A plain turn with `checks.turn` ends done only when they pass, else `Stopped: "checks"`. No checks named: as today. (2026-11-13: `TestChecksTurn`, `TestChecksRoundsSpent`, `TestChecksNoneNamed`, `TestChecksDeclined`, `TestChecksNotDeclared`, `TestChecksOnClaudeCode`.)
- [x] Killed while a check runs, the turn resumes by running the check again, not the model, and no one is asked whether it ran; the rounds spent are rebuilt from the record (`TestCrashDuringCheck`, 2026-11-13).
- [x] Paths a check protects are not changed by the agent's writing tools, and a change to them by any means fails the gate. (2026-11-13: `TestChecksProtect`, `TestProtects`.)
- [x] Checked by hand with a real model: a plain turn gated by this repository's tests, and a flow that gates an agent. (2026-11-13, `gpt-5.4-mini`: the turn sent back three times, then passing; the flow's first agent stopped by its gate, a rework passing, `check()` passing, and `flow stats` showing each gate. Three faults fixed: no `go test` check could pass on macOS (the shared build cache is not written from the sandbox; a check now has one of its own), an escalation was asked where `bash` was denied, and the chart did not show an agent's gate. ADR 0042, "Checked by hand".)

## M14 Lines and work items

Status: in progress (2026-11-13; ADR 0043 accepted; chosen by the user). Step 2 of `docs/factory.md`: work goes down lines behind the conversation, each item a session of its own in a worktree of its own.

- [ ] Lines are read from `.kairocode/lines/<name>.json`, and a bad one is named with why (`TestLinesRead`).
- [ ] A line runs as an item's first turn: stations in order, each output passed on, an agent's gate sending it back, a flow station's gate stopping the item at that station (`TestLineRun`); killed during a station, finished stations are not run again (`TestCrashDuringLine`).
- [ ] The conversation starts an item with `line`: a worktree and a session, at once, the conversation going on; never two for one call (`TestItemStart`, `TestCrashStartingItem`); a line put together on the spot is always asked, and refused with `lines.onTheSpot: "deny"` (`TestLineOnTheSpot`); a fifth item running is refused (`TestItemCap`).
- [ ] The conversation is told when an item ends or waits, once each, and an item's wait is answered from the conversation (`TestItemTold`, `TestItemWaitsInConversation`); `items`, `/items` and `kairocode items` list them.
- [ ] An item taken (its branch merged) or dropped is cleaned up (`TestItemCleanedUp`).
- [ ] `/line` and `kairocode line` start one from a named line; `/line keep` writes a line put together on the spot.
- [ ] Checked by hand with a real model: a standard line and one put together on the spot, two items side by side, told back into the conversation.

Left for after M6 to M12: a VS Code extension, Windows, GitHub and serverless (ADR 0012), sharing sessions, answering approvals from outside (rejected for now, ADR 0038), the problem of check reports cutting in after an answer and pushing the answer away (the Stop hook. Put it in M6 if it is urgent).

## Future work not assigned to a milestone

- **How long what KairoCode keeps is kept** (the user's question, 2026-11-13): sessions left open keep their records for ever; messages (`kc_message`) are never removed; snapshot stores are never pruned (`gc.auto 0`). Measured then: 36MB of records and messages, 112MB of snapshots. Needs an ADR of its own: closing sessions long unused, removing messages nothing refers to (invariant 5 says they are never rewritten, not that they are kept for ever), and thinning old snapshots.
- **Make a flow from a session's contents** (the user's idea, 2026-10-22): from the record of a session that went well (what was asked, the tools called, where approvals were given), write a flow (`.kairocode/workflows`) that can repeat the same procedure.
  - The material is the browsing record (`Runtime.Log`, `kairocode log -json`). We are thinking of having the model read it and write the flow with the same mechanism as `/flow new` (the review screen, try).
  - Decide with an ADR when building it.

- **(Done: ADR 0016) auto mode for approvals** (the user's request, 2026-10-09): add `auto` to the rules; a judging action (`permission.judge`) returns allow, deny, or ask a person, with a reason.
  - The judgment is received as a typed value (kairo's ADR 0055 typed decisions, or structured output). The judgment is recorded, so on resume it proceeds with the same judgment (invariant 2).
  - If the judgment is to ask a person, it falls back to the current approval wait.
  - Escalations that run outside the sandbox and `git_push` ask a person by default even in auto (can be changed in settings).
  - Whether to use a model for the judgment, and which model, and so on, are decided with an ADR when building it.
