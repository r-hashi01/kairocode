# KairoCode's design decisions (ADR)

These are the records of the design decisions on KairoCode's side. They are run the same way as kairo's ADR 0000. A status is one of "Proposed", "Accepted (date)", "Rejected" or "Superseded by NNNN", and only the user moves an ADR to "Accepted".

Decisions on kairo's side are in kairo's `docs/adr/`.

**How to read them:** only the ADRs whose status is "Accepted" decide what KairoCode is now. "Proposed" ones are undecided ideas and are not implemented (the ones marked "on hold" are waiting for the time to revisit them).

| Number | Title | Status |
|---|---|---|
| [0000](0000-record-decisions.md) | Record design decisions as ADRs | Accepted (2026-10-09) |
| [0001](0001-go-on-the-kairo-go-sdk.md) | Write KairoCode in Go and run it on kairo's Go SDK (the embedded runtime) | Accepted (2026-10-09) |
| [0002](0002-opencode-features-and-acp.md) | Model features and the shape of configuration on opencode, and make ACP the entry point on the user's side | Accepted (2026-10-09) |
| [0003](0003-workflows-started-by-people-and-events.md) | Workflows are started by people and events. Their body is Starlark, written from words, and shown in the terminal as a flow chart | Accepted (2026-10-19) |
| [0004](0004-dspchr-integration.md) | Become an agent that behaves well inside dspchr | Proposed (on hold) |
| [0005](0005-sandbox-for-processes.md) | Run the processes KairoCode starts inside an OS sandbox modeled on Codex | Accepted (2026-10-09) |
| [0006](0006-conversation-storage.md) | Keep the conversation in one table looked up by content hash, plus a chain of links per turn. The session runs turns as children, in order | Accepted (2026-10-09) |
| [0007](0007-claude-code-engine-for-local-use.md) | Locally, the user's own Claude Code (subscription) can be used as the model engine | Accepted (2026-10-09) |
| [0008](0008-one-daemon-per-data-directory.md) | Put exactly one daemon that runs kairo per data directory. Everything else is a client | Accepted (2026-10-09) |
| [0009](0009-lsp-diagnostics-after-edits.md) | Add language server diagnostic errors to the result of a tool that wrote a file | Accepted (2026-10-19) |
| [0010](0010-terminal-ui.md) | Make the terminal UI a TUI shaped like claude (Bubble Tea). Ask about trust in a folder seen for the first time | Accepted (2026-10-19) |
| [0011](0011-cancel-a-turn.md) | Cancelling a turn: stop it with kairo's cancellation, and keep the fact that it was stopped in the conversation | Accepted (2026-10-19) |
| [0012](0012-github-and-serverless.md) | Start from GitHub, and hold no process while waiting (serverless) | Proposed |
| [0013](0013-http-tools.md) | HTTP(S) tools: `fetch` for GET and `http` for POST | Accepted (2026-10-21) |
| [0014](0014-providers-and-models.md) | Switch providers and models in the middle of a conversation (/provider, /model) | Accepted (2026-10-19) |
| [0015](0015-install-acp-agents-from-the-registry.md) | Install ACP agents from the registry in the chat box and use them (/provider) | Accepted (2026-10-19) |
| [0016](0016-auto-mode.md) | auto mode: a judge model decides calls that would ask a person | Accepted (2026-10-20) |
| [0017](0017-try-a-flow-before-it-is-written.md) | Try a flow before it is written (a try run): stop effects on the outside and show "what it would have done" | Accepted (2026-10-20) |
| [0018](0018-same-across-agents.md) | The same things work whichever agent is used: KairoCode's promise, how missing pieces are filled in, hooks in KairoCode's sandbox | Accepted (2026-10-21) |
| [0019](0019-daemon-updates-itself.md) | Move the daemon to a new version automatically | Accepted (2026-10-21) |
| [0020](0020-compact-the-history.md) | Compacting the history: summarize and continue when the context limit is near, and keep the earlier record | Accepted (2026-10-21) |
| [0021](0021-fork-a-session.md) | Forking a session: continue from a past turn as a separate session | Accepted (2026-10-21) |
| [0022](0022-mcp-servers-custom-tools-todo.md) | The user's MCP servers, custom tools, todo | Accepted (2026-10-21) |
| [0023](0023-view-and-replay-records.md) | Viewing and replaying records | Accepted (2026-10-22) |
| [0024](0024-limits-and-rate-limits.md) | Limits (tokens, cost, time) and per-provider rate limits | Proposed (on hold) |
| [0025](0025-plugins.md) | Plugins: bundles of connections to outside services and skills, `/plugin` and `/plugin connect` | Accepted (2026-10-26) |
| [0026](0026-image-input.md) | Image input: an image part in messages, and how each engine receives it | Accepted (2026-10-27) |
| [0027](0027-plugin-marketplaces.md) | Plugin marketplaces: a bundled catalog, and catalogs you can add | Accepted (2026-10-26) |
| [0028](0028-background-commands.md) | Background commands: waiting for hours inside a long loop | Accepted (2026-10-26) |
| [0029](0029-flow-run-history.md) | Flow run history: keep finished runs, and look back at them later | Accepted (2026-10-27) |
| [0030](0030-flow-drafts-and-publishing.md) | Flow drafts and publishing: separating writing from making usable | Accepted (2026-10-27) |
| [0031](0031-loopback-sandbox.md) | A sandbox that allows only local traffic: running checks without leaving the sandbox | Accepted (2026-10-27) |
| [0032](0032-terminal-notifications.md) | Terminal notifications: telling a user who is not looking at the screen when a human is needed and when a turn ends | Accepted (2026-10-28) |
| [0033](0033-draw-the-inline-area-as-codex-does.md) | Drawing the bottom area of the terminal UI ourselves, the same way Codex does | Accepted (2026-10-28) |
| [0034](0034-notify-approval-requests-outside.md) | Telling of approval requests outside the machine: an `approval.requested` hook, run by the daemon as a flow | Accepted (2026-11-06) |
| [0035](0035-openai-compatible-providers.md) | OpenAI-compatible APIs as providers, local models among them | Accepted (2026-11-09) |
