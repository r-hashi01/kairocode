# 0025 Plugins: bundles of connections to outside services and skills, `/plugin` and `/plugin connect`

- Status: Accepted (2026-10-26)
- Date: 2026-10-26
- Related: ADR 0002 and 0018 (plugins were on hold), 0003 (flows), 0022 (MCP servers, custom tools, declaring effect types), invariants 3, 4 and 6

## Context

The plugins the user has in mind are "parts that connect to outside services", like those in Dify, n8n and Zapier. A plugin adds a connection to the outside (something like MCP), a capability (something like a skill), or both (2026-10-26).

The parts in all three products are made of the same 4 things.
- Authentication: credentials are held apart from the part, and reused.
- Operations: actions, searches.
- Starting points: triggers.
- Distribution: a marketplace.

MCP is also becoming the common interface. Zapier exposes the operations of the apps you connect as an MCP server.

ADR 0022 added MCP servers and custom tools to KairoCode. But the user writes raw settings (commands, URLs, environment variables) in `kairocode.json`, and these are missing:
- A place to hold authentication (OAuth, keys), and a way to see its state (connected or expired).
- A way to install and remove things as a unit.
- A way to ship them together with instructions (skills).

## Decision

1. **A plugin is a bundle of "an MCP server + a KairoCode manifest".** The MCP server does the actual work, and the manifest (`kairocode-plugin.json`) supplies what MCP lacks.
   - **Connection:** how to start the MCP server (`command`), or a remote URL (`url`).
   - **Effect types of the operations:** `read`, `idempotent` or `real` per tool (a `*` prefix match can be written). Anything undeclared is real (invariant 3). This is handled the same way as the `tools` declarations of ADR 0022.
   - **Credentials:** states what is needed.
     - `none`.
     - `key`: a named value. Passed to the MCP server in an environment variable or a header.
     - `oauth`: OAuth for a remote MCP server (MCP's authorization spec). Uses the authorization code flow and dynamic client registration of the MCP Go SDK (`auth`).
   - **Skills:** skills placed in `skills/` become readable by the agent (the same rules as the skills of ADR 0002).
   - A plugin may be a connection only, skills only, or both.
   - The Claude Code plugin format (`.claude-plugin/plugin.json`, `skills/`, `.mcp.json`) is also read. It has no effect type declarations, so its tools are treated as real.
2. **Where plugins live and how they are installed**
   - The user's plugins are in `plugins/<name>/` in the data directory.
     - `/plugin install <git URL or local path>` installs one, and `/plugin remove <name>` removes it.
     - What is installed is pinned to a git commit. It is updated only when asked explicitly with `/plugin update <name>`.
   - Project plugins are in `.kairocode/plugins/<name>/`. Like the project MCP servers of ADR 0022, the user is asked whether to run them the first time they are used.
3. **`/plugin connect <name>` (credentials)**
   - For `key` plugins, the TUI asks for the value (the input is hidden).
   - For `oauth` plugins, it opens the browser and receives the redirect on a local `127.0.0.1` port. It holds the refresh token and refreshes automatically before expiry.
   - Values go in the OS credential store (the macOS keychain via `security`, Linux Secret Service via `secret-tool`; both are called as commands, adding no dependency). Where there is no store, they go in a file with mode 0600 in the data directory, and the user is told so.
   - Values are read when the MCP server is started (environment variables) or connected to (headers), and are never left in records, logs or error messages (invariant 6, same as ADR 0022).
   - `/plugin disconnect <name>` deletes them.
4. **`/plugin` (list and state):** for each installed plugin, it shows:
   - Name, version (commit), and whether it is the user's or the project's.
   - **Connection state:** connected / not connected (`/plugin connect` needed) / authentication expired (refresh failed, or the server returned 401 or 403) / does not start (error) / not yet asked whether to run (project plugins).
   - The number of tools and the effect type of each. The names of the skills.
   - The state comes from the latest results the daemon holds (the result of the last start, connect or refresh, and the last authentication failure of a call). It is not put in kairo's record (it is not used for branching in workflows).
   - When a tool of a plugin whose authentication expired is called, the model gets "`/plugin connect <name>` is needed".
5. **Use:** tools are named `<plugin>_<tool>` and work in every engine through the same mechanism as the outside tools of ADR 0022 (definitions recorded at the start of the turn, run by effect type, blocked (needs confirmation)). They are also usable from a flow's `tool(...)` and `agent(...)`. Today those two in flows are not connected to the outside tools of ADR 0022, so that is fixed first (no ADR needed; it is a fix of a gap).
   - Addendum (2026-10-26): when trying a flow before writing it (ADR 0017), MCP servers and plugins are not started (ADR 0022). So their tools, even read-only ones, are not called; as "things not called" in ADR 0017, their names and arguments are kept in the try's result, and the flow gets "not called because this is a try" (`flow.would`). Until now it returned "no such tool", and the try stopped there. The `/flow new` model is told that MCP and plugin tools can also be called with `tool()`. Example: `docs/examples/plugin-in-flow.md`. Tests: `TestFlowUsesPluginTools`, `TestTryFlowWithPluginTools`.
6. **Triggers are not decided in this ADR.** A local daemon has no URL reachable from the outside, so receiving webhooks needs a relay (this overlaps with M4). It becomes a separate ADR when needed.

Where this shows up in the implementation: `internal/runtime` (loading plugins and wiring them into the outside tools of ADR 0022, storing credentials, OAuth, state), `internal/acp` and `internal/tui` (input for `/plugin` and `/plugin connect`, and opening the browser), `internal/cli` (`kairocode plugin ...`), `internal/invariants` (files allowed to start `security` and `secret-tool`).

## Consequences

- The user can install and connect to outside services, and check their state, without writing raw settings. An expired authentication is visible both in the list and in calls.
- The connection is MCP, so an existing MCP server (including Zapier's MCP) can become a plugin as is.
- Effect types and credentials stay under KairoCode's promises (nothing runs twice, no secrets are kept).
- Tests (added at implementation)
  - Install a plugin with a fake MCP server, and its tools are usable with the declared effect types.
  - A `key` credential reaches the server and is not left in the record (DB).
  - With a fake OAuth server, `/plugin connect` gets a token through the authorization code flow and refreshes it when it expires. If the refresh fails, the list shows "authentication expired".
  - When the server returns 401, the list shows "authentication expired" and the model is told `/plugin connect` is needed.
  - A project plugin does not start before the user is asked.

## Alternatives considered

- **Keep the `mcp` section of `kairocode.json` (ADR 0022):** the work of writing raw settings remains, and the authentication state stays invisible. `mcp` stays, as the layer under plugins.
- **Run plugin code inside KairoCode, as Dify and n8n do:** KairoCode is a single Go binary, so this needs a runtime for other languages. And running inside a workflow breaks determinism. Running it outside as an MCP server lets it be written in any language and fits the record and resume mechanism.
- **Write credentials in `kairocode.json`:** they would stay in plain text and could slip into project settings.

## Implementation (2026-10-26)

- Loading: `opencode.PluginDirs` (the user's, then the project's), `internal/runtime/plugins.go`. A plugin's server joins `externals()` as an `extServer` of ADR 0022 (with the plugin's name in `Plugin`), and tool names, records, effect types and the question for project plugins follow the same path as ADR 0022. The `tools` declarations in a manifest apply only to that plugin's tools (prefixed with `<plugin>_`). For skills, `SkillDirs` adds `<plugin>/skills`.
- Credentials: `internal/runtime/secrets.go` (`SecretStore`). The key is `<plugin>/<name>` for the user's plugins and `project/<hash of the folder>/<plugin>/<name>` for project plugins (`PluginSecretKey`). A project plugin with the same name as a user's plugin cannot read the user's values. That plugin's values are masked in error messages and logs (`serverErr`). Values are put into `{secret:NAME}` when the server is started or connected to (`value`); when there is none, the server is not started and is "not connected". `{env:}` is expanded only for the user's plugins, as in ADR 0022.
- CLI: `kairocode plugin [list|install|update|remove|connect|disconnect]` (`internal/cli/plugin.go`). `connect` turns off terminal echo, reads the value, puts it in the store, and if the daemon is running tells it with `POST /plugins/{name}/connected`. The daemon stops that plugin's server (it restarts with the new value on the next call) and clears "authentication expired".
- API: `GET /sessions/{id}/plugins`, `GET /plugins?dir=`, `POST /plugins/{name}/connected`. ACP's `/plugin` (list).
- TUI: `/plugin connect <name>` and `/plugin disconnect <name>` hand the terminal to `kairocode plugin connect` with `tea.ExecProcess` (the same as the `/login` command).
- OAuth (3, 2026-10-26): `internal/runtime/oauth.go`.
  - **Connecting:** `kairocode plugin connect` runs the authorization code flow (PKCE) with the MCP Go SDK's `auth.AuthorizationCodeHandler`. The client is registered dynamically. If the manifest has `auth.client_id`, that pre-registered client is used.
    - The browser is opened with `open` or `xdg-open`, and the URL is also shown. The redirect is received on a free `127.0.0.1` port.
    - What it gets (client ID, token endpoint, scopes, access token and refresh token) is stored as one value at `<plugin>/oauth` in the store.
  - **Daemon:** puts the access token in `Authorization` on each request. It refreshes before expiry (10 seconds before, the `oauth2` default) and writes the new token back to the store.
    - When the refresh is rejected (`invalid_grant` and so on) or there is no refresh token, it returns `errRefused` without sending the request, and the list shows "authentication expired".
    - When the token endpoint cannot be reached, or the error is something other than `invalid_grant` (503 and so on), the request is likewise not sent, but the state is not set to "authentication expired" (the model is told "not sent").
    - Inside the daemon, one holder owns the token for a given credential. Before refreshing, it re-reads the store, and if another holder (another daemon) has refreshed, it uses that (a refresh token can sometimes be used only once). It writes back only if the value in the store is still the one it refreshed from (if the user reconnected or disconnected in between, it does not write). Refreshing has a 30-second deadline.
    - Requests carrying credentials do not follow redirects to other hosts.
  - **Redirect receiver:** only accepts a redirect whose `state` matches the one sent (calls from other processes cannot stop a sign-in). The authorization page is opened only for `https` (`http` for local addresses). A project plugin's server cannot make it open any other kind of URL.
  - **Dependency:** the SDK's `auth` API is written with the types of `golang.org/x/oauth2` (`oauth2.Config`, `oauth2.Token`), so it is imported directly. It was already in as a dependency of the SDK (it only changes from indirect to direct in `go.mod`). With the user's consent (2026-10-26), it was added to the allowed dependencies in AGENTS.md and to the hook's list.
  - Test: `TestPluginOAuth` (fake authorization server and MCP server: dynamic registration, PKCE, code exchange, refresh and write-back, "authentication expired" on a rejected refresh, no token left in the DB).

Departures from the decision (decided during implementation; the intent of the decision is unchanged):
- **Location:** the user's plugins are not in the data directory but in the user's config directory (`$XDG_CONFIG_HOME/kairocode/plugins/<name>`, or `~/.config/kairocode/plugins/<name>` if that is unset). It is the same place as `kairocode.json` and skills, and it survives deleting the data directory (DB). The credentials file (when there is no store) is `credentials.json` (0600) in the same place.
- **Where values are asked for:** values do not go through ACP (so they never pass between the record and the client). The terminal UI (`kairo`) hands the terminal to the CLI as above. For `/plugin connect` in other ACP clients (Zed and so on), the user is told to run `kairocode plugin connect <name>` in a terminal.
- **macOS keychain:** `security add-generic-password` cannot read the value from standard input, so the value is passed as a command argument. For that short time, it may be visible in the process list to processes of the same user. The same is true when writing back a refreshed OAuth token. Linux's `secret-tool` takes it on standard input.
- **Judging an authentication failure:** "authentication expired" is set only when the server of a remote (URL-only) plugin returns HTTP 401 or 403 to a POST that carries a message. `headers.RoundTrip` returns this as a typed error (`errRefused`); it is not judged from the text. A refused POST was not received by the server, so it can be said not to have run.
  - Addendum (2026-10-26, checked against the real GitHub MCP server; approved by the user)
    - **Judgment:** GitHub returns `400` with `WWW-Authenticate: Bearer error="invalid_token"` for a malformed token, not 401. RFC 6750's `invalid_token` is a rejection at the token-check stage (the request was not processed), so a 4xx to a POST with `error="invalid_token"` is also treated as "refused", the same as 401 and 403 (`turnedAway`). The "401 and 403" in 4 above includes this.
    - **Display:** when refused, the server's answer is shown as is, without rephrasing. The list state is `refused: HTTP 400 Bad Request: <body> (WWW-Authenticate: …); kairocode plugin connect <name>`, and the reply to the model is the same answer with "did not run" and `kairocode plugin connect` added. When a refresh is rejected, it is the token endpoint's answer (status and body). 4 above and "authentication expired" below refer to this display.
    - Only 3 things are changed: it is joined into one line, the body and `WWW-Authenticate` are each cut at 300 characters, and the credentials sent (the stored value, the OAuth access token, and for a refresh also the refresh token and client secret) are masked before cutting (invariant 6). A masked value shows only its first and last 4 characters if it is 20 characters or longer (`ghp_…wxyz`, so you can tell which value was sent), and `***` if shorter (the user's suggestion, 2026-10-26). Up to the first 2 KiB of the body is read for up to 2 seconds; if it cannot be read fully, the last word cut off midway is dropped (so the head of a credential is not left). The answer is carried by a `refusal` error and judged as `errRefused`.
    - Tests: `TestTurnedAway`, `TestAnswerOf`, `TestAnswerOfCut`, `TestMasked`, `TestPluginRefused`.
  - A 401 on the event stream (GET), the text of a JSON-RPC error (such as "403 Forbidden" returned by a tool that ran), and errors of locally started servers do not count. If the outcome is unknown, it is treated as unknown as before (invariant 4).
- **Effect type declarations:** a manifest's `tools` applies only to the tools of that plugin's server (the manifest's path). It does not apply to other servers with the same name.
- **Updates:** the plugin's version and commit go into the server's fingerprint (`Spec`). After `update`, a turn in progress treats it as a different server from the one listed at its start, and does not call its tools.
- Tests: `TestPluginTool` (not connected → connected, the key reaches the server and is not left in the DB, effect types, skills), `TestPluginRefused` (a 401 on a call returns the answer and `kairocode plugin connect` to the model, and the list shows that answer; reconnecting restores it; a 401 at start is the same; GitHub's style of 400 with `invalid_token` is also treated as refused, and the answer is shown as is with the key masked), `TestProjectPluginAsks` (project plugins do not start before the user is asked; skills-only plugins), `TestProjectPluginGetsNoUsersKey`, `TestPluginRefusedOnlyWhenTurnedAway`, `TestPluginDeclsAndErrors`, `TestPluginInstallConnectRemove` (pinned to a git commit, value from standard input, cancelling, refusing a delete that goes outside).
