# AGENTS.md

Working instructions for coding agents (and people) working in this repository.

- Requirements: `../kairo/docs/kairocode/requirements.md`
- Design: `../kairo/docs/kairocode/design.md` (the language and how kairo is used are replaced by ADR 0001)
- kairo's Go SDK: `../kairo/sdk/go/README.md`, `go doc github.com/r-hashi01/kairo/sdk/go`
- How we proceed and each milestone's acceptance criteria: `docs/milestones.md`
- What kairo is missing: `docs/kairo-gaps.md`
- Design decisions on KairoCode's side: `docs/adr/`

The requirements and the design live in the kairo repository. We assume `kairo` is cloned next to this one (`../kairo`).

## What this is

KairoCode is a coding agent built on kairo (a durable workflow runtime). It is the same kind of tool as opencode or Claude Code, but it runs sessions as kairo workflows. The goal is that it "does not break when it crashes, waits, or runs side by side."

- Go 1.27.2. kairo is used through the embedded runtime (`kairo.Open`) of the Go SDK `github.com/r-hashi01/kairo/sdk/go` (v0.4.1 or later, package name `kairo`) (ADR 0001).
- Storage is SQLite (`modernc.org/sqlite`) locally, and PostgreSQL for serverless and teams. The local server uses wait mode; serverless uses suspend mode.
- It is split into a server (HTTP + SSE) and clients that connect to it (TUI and CLI). It ships as a single binary (`cmd/kairocode`).

## Packages and the boundaries of their responsibilities

| Package | Responsibility | I/O and goroutines |
|---|---|---|
| `internal/workflows` | kairo workflows (`session`, `turn`, `flow` (runs a Starlark flow inside a workflow. ADR 0003)). Controls the agent loop | **Entirely forbidden.** Sees the outside only through the SDK's `*kairo.Context` (`kairo.Call`, `Parallel`, `WaitFor`, `Child`, `ctx.Now`, `ctx.Random`, `ctx.Sleep`) |
| `internal/runtime` | `kairo.Open`, declaring workflows and actions. The action implementations (`agent.load`, `model`, `history.extend`, `tool.<name>`; later `permission.check`, `snapshot`, `restore`). Mapping tool effect types to kairo's `Effect`. A small API for the server and CLI (`CreateSession`, `Send`, `WaitTurn`) | Does I/O here |
| `internal/tools` | Built-in tools. One tool per file, with its effect type written in `tools.Def` | Does I/O here |
| `internal/provider` | Model provider adapters (Anthropic, and a fake for tests) | Does I/O here |
| `internal/plugins` | Plugins and marketplaces (ADR 0025, 0027): install, update, remove, list, read plugin definitions (`kairocode-plugin.json`, Claude Code plugins). Connecting to the servers and using the credentials is runtime's job | Does I/O here (runs git when installing) |
| `internal/secrets` | Storing plugin credentials (ADR 0025): the macOS Keychain, Linux's Secret Service, or else a file only the user can read. Keeps values out of the record (invariant 6) | Does I/O here |
| `internal/store` | Message storage (`kc_message`, looked up by content hash. Written only once) | Does I/O here |
| `internal/daemon` | One daemon per data directory (ADR 0008). The lock, `server.json`, HTTP over a Unix socket, exiting when idle. The client other processes use, and starting the daemon automatically | Does I/O here |
| `internal/api` | The types between the daemon and clients (sessions, events, approvals, providers, plugins, flow runs) and named errors. runtime uses them through aliases; tui and acp use these instead of runtime (`TestClientsDoNotImportTheRuntime`) | None (types and small functions only) |
| `internal/server` | The HTTP + SSE API (design section 8.1). Live output flows from `k.Subscribe(<session ID>)` (by ID prefix match, descendants' output arrives too) | Does I/O here |
| `internal/opencode` | Reads opencode's agent, command and skill definitions (`.opencode/agents`, `commands`, `SKILL.md`) with the same meaning as opencode, and expands commands. Reads the frontmatter YAML with a small parser of its own | Only reads files (a command's `` !`…` `` is run in the sandbox by the caller) |
| `internal/acp` | The ACP agent (ADR 0002) and the ACP client the terminal UI uses. stdio JSON-RPC; maps the daemon's sessions, events and approvals to ACP. Holds no wait state (approval waits live in kairo). Does not import runtime (types come from `internal/api`) | Does I/O here |
| `internal/jsonrpc` | JSON-RPC 2.0 over newline-delimited JSON (ACP's stdio). Used by `internal/acp` (KairoCode as an ACP agent) and by `internal/runtime`'s ACP engine (KairoCode as an ACP client of codex and copilot. ADR 0014) | Only the reader and writer it is given |
| `internal/cli` | `kairocode chat` (also `kairo` with no arguments; the terminal UI that talks as its own ACP client), `run / approvals / approve / daemon / acp / usage`, later `replay`. Does not open the DB itself; connects to the daemon | Does I/O here |
| `internal/flowfile` | Where the project's flow scripts live (`.kairocode/workflows/<name>.star` and drafts), listing them, reading and writing them. Reads and writes only inside the working directory (`os.Root`) | Flow files only |
| `internal/flowchart` | The shape of a flow (ADR 0003). From a Starlark script's syntax tree, reads the primitive calls as sequence, branch, loop and parallel. Also a tree-shaped text chart | None (only reads) |
| `internal/tui` | `kairo`'s terminal UI (ADR 0010, Bubble Tea v2). Holds the ACP client's display and input. For the flow chart and editing, reads and writes flow scripts through `internal/flowfile`. Does not import runtime (types come from `internal/api`) | Terminal input and output, and flow files through `internal/flowfile` only |
| `internal/clipboard` | The OS commands that read clipboard images (ADR 0026), and saving pasted images. Runs only when the user presses a key | Does I/O here |
| `internal/tui/screen` | Draws the bottom area of the terminal and writes the conversation above it (ADR 0033. The same mechanism as Codex). Raw mode and terminal settings | Terminal input and output only |
| `internal/lsp` | Language server diagnostic errors for written files (ADR 0009). Servers start inside the sandbox, are kept inside the daemon, and are stopped when unused | Does I/O here |
| `internal/config` | Where settings live (the user's `$XDG_CONFIG_HOME/kairocode` and the project's `kairocode.json`) and reading them (layered: the user's, then the project's). What each section means is up to the reader | Only reads settings files |
| `internal/locale` | The reader's language (`KAIROCODE_LANG`, the user's `kairocode.json`, the locale) and replacing the wording of flow reviews (field names and scripts are not replaced) | Only reads environment variables and the user's settings |
| `internal/trust` | The list of trusted folders (ADR 0010. `trust.json` in the data directory). Only clients read and write it | Does I/O here |
| `internal/invariants` | Checks for those invariants below that can be read from the source (go/ast, go/types) | None |
| `cmd/kairocode` | Entry point | — |

Workflows may import only the packages on the allow list in `internal/invariants/invariants.go` (kairo's SDK, Starlark (`go.starlark.net`. ADR 0003), `internal/msg` (message types; a pure package), and the deterministic parts of the standard library). When a workflow needs a type from another internal package, split it into a pure package first, then add it to the list.

## Invariants you must not break

Breaking them breaks the core of the requirements (does not break when it crashes, nothing happens twice). Each is guarded by an automated test or a hook.

1. **Workflow code is deterministic.** Non-test code in `internal/workflows` does not use the clock (`time.Now` and the like), random numbers, environment variables, files, network or processes, goroutines, `select`, or map iteration (including iterators such as `maps.Keys`). Time and random numbers come from `ctx.Now()` and `ctx.Random()`, parallelism from `kairo.Parallel`, and everything else goes through actions. Iterate maps with `slices.Sorted(maps.Keys(m))`. → `TestWorkflowCodeIsDeterministic` (the import allow list, map detection by type), hook (`scripts/workflow-forbidden.txt`)
2. **Workflows branch only on recorded values.** Settings (permission rules, agent definitions, model choice) are not read directly inside a workflow; they are recorded as the result of an action (`agent.load`, `permission.check`) and used from there. Package variables and caches are not read either. Even if settings change and the session resumes, a turn in progress continues with the values from its start (design sections 4 and 5).
3. **Every tool declares its effect type. Without a declaration, it is treated as "real".** The types are `tools.Read`, `tools.Idempotent` and `tools.Real`, and the zero value is `Real` (the same rule as kairo's `ir.Effect`). Every `tools.Def` literal states `Effect` explicitly, even when it is `Real`. The mapping to kairo is `Read` → `kairo.Unprotected`, `Idempotent` → `kairo.Real` + `kairo.IdempotentRetry()`, `Real` → `kairo.Real` (`internal/runtime`). MCP and custom tools follow the same rule. → `TestToolsDeclareEffect`
4. **A real tool with an unknown outcome is never treated as a success.** An `Idempotent` tool checks automatically, then retries (edit and write compare "the hashes before and after the write" and replace. Design section 3.1). A `Real` tool stops in kairo's blocked (needs confirmation) state, asks a person "did it run?", and returns the answer with `k.Resolve` or `k.ResolveFailed`. When a handler returns a failure where "we do not know whether it took effect", wrap it in `kairo.Unknown(err)`; wrap it in `kairo.Retryable(err)` only when you can say it did not take effect. Never run it again without checking. → crash tests
5. **The input to a model call is a list of hashes.** Do not put the history's text in the input of a `model` call. Each message is written once to `kc_message` keyed by its content hash, and never rewritten (design section 2.2, NFR-5).
6. **Secrets stay out of the record.** API keys are read inside actions from environment variables or settings. Do not put them in workflow inputs, action inputs or outputs, `kc_message`, logs, error text, or test records (testdata). → `TestNoSecretsInSources`, hook
7. **Release the process while waiting.** Wait for approvals and external events with `kairo.WaitFor` (kairo signals). Do not implement waiting with polling, `time.Ticker`, or wait state held inside the process (NFR-4).
8. **Run IDs follow fixed rules.** Only the root (the session) is named, with `kairo.WithID("s/<ID>")`. Turns, subagents and plan leaves start with `kairo.Child`, and their IDs are left to the SDK's rule (`<parent ID>/<first 24 characters of sha256(kind + input)>.<n>`). To tell apart children with the same input, put the turn number or the position in the plan in the child's input. Do not put unrecorded values (random numbers, time, settings) in a child's input. The runs for MCP calls of the Claude Code engine (ADR 0007) are `<turn ID>/mcp/<hash of the tool name and normalized arguments>.<attempt number>` (the tool_use ID is not used, because it changes when the call is made again after a crash).
9. **Tests do not call real models.** Providers are the fake or ones that return recorded responses. All tests must pass without a network connection.

## Recording design decisions (ADR)

- Decisions on KairoCode's side are written in `docs/adr/` (the `/adr` skill). As in kairo's ADR 0017, **a change that needs an ADR is implemented only after the user approves it.** Never approve an ADR you filed yourself.
- Changes that need an ADR: adding, changing or relaxing the invariants above; adding a tool effect type; changing the run ID rules; changing a storage format such as `kc_message`; adding a dependency (other than the "allowed dependencies" below); departing from the requirements or the design; deciding an open question in design section 11.
- **Propose what kairo is missing as a kairo ADR (the `/kairo-gap` skill).** Do not edit `../kairo` from this repository (a hook refuses it). If you put a workaround on KairoCode's side, write it in `docs/kairo-gaps.md` and add a `// kairo-gap #N:` comment in the code.

## How to work

```sh
scripts/check.sh           # the condition for done: gofmt, vet, build, all tests (-race, including crash tests), invariants, ADR checks
scripts/check.sh --quick   # the light version the hooks use (go test -short; crash tests skip themselves under -short)
make check | quick | build
```

- **The condition for done is that `scripts/check.sh` passes.** If you finish while it does not pass, state clearly what fails and why you cannot fix it.
- **Run `scripts/check.sh` outside the agent's sandbox.** The tests listen on `127.0.0.1` and Unix sockets (fake servers, the daemon) and start commands in KairoCode's sandbox. That is not possible inside the agent's sandbox, so the check stops at `== env` before the tests and says what it cannot do (it does not count as a pass).
  - KairoCode: call `bash` with `sandbox: "loopback"` (inside the sandbox, allowing only local traffic. ADR 0031). No approval is needed (it follows the `bash` rules). Tests that start the sandbox skip themselves because it cannot be nested, and the check is a "partial" pass (`partial: N skipped`). Use `sandbox: "escalate"` (outside the sandbox with the user's approval) only when you run everything. With `KAIROCODE_CHECK_FULL=1`, a "partial" result does not count as a pass and it stops with exit code 3.
  - Codex: run `scripts/check.sh` by asking for permission to run outside the sandbox (escalated). Allowing `network_access` alone is not enough (macOS cannot start another sandbox inside a sandbox).
  - The exit code when it stops at `== env` is 3. In that case the Stop hook says "run it outside" rather than "fix it". If it passes outside, `check.sh` writes that it passed (`.claude/.cache/last-green`), so the Stop hook does not repeat the check until the source changes.
  - If you cannot get permission to run it outside, attach the `== env` output and ask the user to run it outside.
- **CI** (`.github/workflows/check.yml`) runs `scripts/check.sh` on Linux (with `-race`) and macOS (without it), but only when something other than documentation changed (`scripts/ci-changes.sh` decides; a documentation-only change runs just the ADR check). Tags, the weekly run and manual runs always run everything. The `main` ruleset requires one check, `ci`, which sums up the others: changing the jobs does not mean changing the ruleset. Work reaches `main` only through a pull request.
- Work on a milestone (M0 to M4) with the `/milestone` skill. Write the acceptance criteria as tests first, and finish the milestone when they pass.
- For changes involving real tools, workflows or resuming, write **a test that force-kills the process and resumes** (a crash test). Name it `TestCrash...` and start it with `if testing.Short() { t.Skip(...) }`. Start the test binary again as a child process (`os.Args[0]` and `-test.run`, passing the role in an environment variable), `SIGKILL` it partway, and resume on the same DB. Check "nothing happens twice" by the number of times the tool's handler was called, or by the number of externally visible effects (files, git records).
- Do not loosen test thresholds or expected values to make tests pass. Wait for a condition, not a fixed `time.Sleep`.
- Time budgets (NFR-2, NFR-3) do not decide pass or fail in tests. Measure them and compare before and after (the same as kairo's ADR 0056).

## Procedures by kind of change

- **Adding or changing a tool.** The `/tool-change` skill (effect type, how to check an unknown outcome, the shape of the permission rule, the crash test).
- **Changing workflows (`internal/workflows`).** The `/workflow-change` skill (determinism, recorded values, ID rules, handling versions).
- **kairo's API is missing something.** The `/kairo-gap` skill.
- **Adding a dependency.** The allowed dependencies are `github.com/r-hashi01/kairo` (and `sdk/go/pgnotify`), `modernc.org/sqlite`, `github.com/jackc/pgx/v5`, `github.com/anthropics/anthropic-sdk-go`, `github.com/modelcontextprotocol/go-sdk`, `go.starlark.net` (ADR 0003, approved), `github.com/elastic/go-seccomp-bpf` and `golang.org/x/net` (ADR 0005. Only the Linux sandbox uses `x/net/bpf`), `charm.land/bubbletea/v2`, `charm.land/bubbles/v2` and `charm.land/lipgloss/v2` (ADR 0010. Only `internal/tui` uses them), and `golang.org/x/oauth2` (ADR 0025. The `auth` types of the MCP SDK. Only plugin OAuth uses it). Anything else needs the user's consent and an ADR (a hook asks for confirmation).

## Coding conventions

- Everything in the repository is in English: code, comments, identifiers, docs, commit messages, PR titles and bodies, and release notes. The one exception is product text localized for users (`internal/locale`, and the Japanese terminal notifications). Match the density and style of the surrounding code.
- Follow gofmt (a hook formats after each edit).
- Do not swallow errors; return them or record them. A tool's "settled failure" (a conflict, outside the working directory, and so on) goes back to the model as a result, not as an error.
- File operations must not leave the working directory (the session's repository) (normalize paths and resolve symbolic links to check).
- When you start a goroutine, always give it a way to stop (`context` cancellation).

## Pitfalls

- `kairo.Call` is one kairo run. On resume, the workflow function runs again from the top, and calls with the same ID return their results from the record. If you change the order or the input of calls, they run anew as different calls. When you change what the turn workflow does, raise the version with `kairo.WorkflowVersion` and keep the old version with `kairo.Draining()` (kairo's ADR 0060). Delete the old version only after checking with `k.List` that no runs of that version remain.
- The record of a finished tree is deleted once `KeepFinished` (default 24 hours) passes after its root finishes. A waiting tree is not deleted (kairo's ADR 0054).
- A real call runs only after its intent is committed to the DB. If the process crashes, another process or the next `Tick` picks it up once the lease (`Options.Lease`, default 30 seconds) expires. The real call becomes `blocked`, and the workflow function runs again from the start (kairo's ADR 0059).
- Write clock-related tests (long approval waits, deadlines, sleeps) with `kairotest.New(t)` and `env.Advance(d)`. Do not wait in real time.
- kairo's real steps run only after their intent is durable. To check in a test that something "ran only once", count inside the handler and keep the count in a file or the DB (the process is killed, so a count in memory does not survive).
- kairo's own invariants (a pure core, real commands only from durable intents, and so on) are in `../kairo/AGENTS.md`. Check the SDK's promises in `../kairo/sdk/go/README.md` and kairo's ADRs 0058 to 0060.

## The agent harness in this repository

- hooks in `.claude/settings.json`
  - `SessionStart`: shows the branch, the number of uncommitted changes, the latest check result, and the current milestone (`docs/milestones.md`).
  - `PreToolUse` (Edit/Write): refuses forbidden patterns in `internal/workflows`, strings that look like API keys, edits to runtime data (`*.db`), and edits to `../kairo`. Asks for confirmation when adding a module to `go.mod` that is not allowed.
  - `PostToolUse` (Edit/Write): runs gofmt, and go vet and `go test -short` for the package. If you change `internal/workflows` or `internal/tools`, it runs the invariant checks too.
  - `Stop`: does not let you finish unless `scripts/check.sh --quick` passes. Skipped if the source has not changed since the last pass.
- Skills: `/verify`, `/milestone`, `/tool-change`, `/workflow-change`, `/kairo-gap`, `/adr`
- Subagent: `durability-reviewer` (reviews a diff against the invariants and the design. Read-only). Use it before finishing when you change `internal/workflows`, `internal/runtime` or `internal/tools`.
- `scripts/check.sh` also runs `scripts/adr-lint.sh`.

Do not get around hooks or tests by disabling them. If you think one is a false positive, propose a change that fixes the hook or check itself.
