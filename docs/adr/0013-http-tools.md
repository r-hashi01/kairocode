# 0013 HTTP(S) tools: `fetch` for GET and `http` for POST

- Status: Accepted (2026-10-21)
- Date: 2026-10-19
- Related: design section 10 (M4's HTTP(S) tools), AGENTS.md invariants 3, 4 and 6, ADR 0002 (opencode's webfetch), 0005 (sandbox)

## Context

M4 includes "HTTP(S) tools". They let the model read documents (opencode's `webfetch`) and call APIs (re-running CI, commenting on an Issue, and so on). There is one effect type per tool (invariant 3). But the effect of HTTP changes with the method (GET only reads, POST is a real effect). Authentication secrets (tokens) also have to be passed without leaving them in the record (invariant 6).

## Decision

1. **Only GET and POST. There are 2 tools** (`internal/tools/fetch.go`, `http.go`).
   - `fetch` (effect `Read`): GET. Takes a URL and optional headers (`Accept` and so on) and returns the body. HTML is turned into text, large bodies are cut, and the cut is noted (the same shape as opencode's `webfetch`; `format` is `text`, `markdown` or `html`).
   - `http` (effect `Real`): POST. Takes a URL, headers and a body, and returns the status, some headers, and the body.
     - Some APIs, like GraphQL, send read-only queries with POST too. We do not try to tell from the body whether it only reads; every POST is treated as `Real` (falling to the safe side). So even a read-only POST asks a person if its outcome is unknown.
     - PUT, PATCH, DELETE and so on are not handled. When needed, use bash's `curl` with escalation (ADR 0005).
     - If the outcome is unknown (cut off before the response), it is `kairo.Unknown` and goes to blocked (needs confirmation) (invariant 4).
     - Whether to add an idempotency key header (`Idempotency-Key`) is up to the caller. KairoCode does not retry automatically.
2. **Permission rules are written by host** (`permission`).
   - Example: `{"fetch": {"*": "allow"}, "http": {"api.github.com/*": "ask", "*": "deny"}}`.
   - The default is `ask` for `fetch` and `ask` for `http`. The subject of the decision (`Subject`) is `<host><path>` (the method is decided by the tool).
3. **Secrets are added inside the action by per-host authentication in the configuration.**
   - It is written in `http.auth` in `kairocode.json`. Example: `{"api.github.com": {"header": "Authorization", "env": "GITHUB_TOKEN", "prefix": "Bearer "}}`.
   - The value is read from the environment variable inside the action. It does not go into the tool's input, result, record or logs.
   - If the response contains the same value, it is masked.
   - This setting can be written only in the user's own configuration, not in the project configuration (for the same reason as ADR 0009's addendum: so a project cannot make the user's token be sent to an arbitrary host).
4. **Requests go out from the KairoCode process (the daemon),** not from a command inside the sandbox.
   - Only http and https destinations. `localhost`, private addresses and link-local (`169.254.0.0/16` and so on) are refused by default (allowed with the setting `http.allowPrivate`).
   - Redirects are followed up to 5 times, and the redirect target's host also goes through the permission decision.
   - The time limit is 30 seconds, and the body limit is 5 MB.

## Consequences

- The model can read documents and call APIs. Read-only ones have effect `Read`, so calling them again on resume does no harm.
- Ones with an effect ask a person if the outcome is unknown.
- Tokens do not stay in the record.
- Tests (added at implementation)
  - With an `httptest` server, `fetch` returns the body, turns HTML into text, and cuts large bodies.
  - An `http` POST arrives only once. Killing the process before the response leads to blocked (needs confirmation), and it is not sent again without checking (`TestCrash...`).
  - Host rules (allow, ask, deny), and the decision also applies to redirect targets.
  - Private addresses are refused by default.
  - The authentication header is sent, and the value is not in the record (input, result, `kc_message`) (in addition to `TestNoSecretsInSources`, scan the record).
  - `http.auth` in the project configuration is not used.

## Alternatives considered

- **Handle every method with `http`:** Adding PUT, PATCH and DELETE keeps the same effect type, `Real`. But we do not yet see where they would be used, and they add to the permission rules and how reviews are shown. We will add them if a case comes up that GET and POST cannot cover (2026-10-21; narrowed to GET and POST after discussion with the user).
- **Look at the POST body and make read-only queries (GraphQL's `query`) `Read`:** Each API has different conventions, and a wrong call treats a real effect as read-only (invariants 3 and 4).
- **One tool whose effect changes with the method:** There is one effect type per tool, decided by its declaration (invariant 3). Changing it by method can lead to the mistake of treating something real as read-only.
- **Leave it to bash's `curl`:** The sandbox cuts off the network, so allowing it needs escalation. Neither the effect type nor the handling of secrets can be written as a tool.
- **Use kairo's HTTP(S) actions (ADR 0052):** That is a mechanism for running the action's processing itself somewhere else. It is a different thing from a tool that calls an outside API.

## Implementation (2026-10-21)

- **Tools:** `internal/tools/fetch.go` (`Read`), `http.go` (`Real`); the shared part is `net.go`. `tools.Env.Net` holds the user's configuration (`allowPrivate`, `auth`).
- **Configuration:** `userNet` in `internal/runtime/netconfig.go` reads only `http` in the user's `kairocode.json` when running the tool. The project's `http` is not read.
  - Example: `{"http": {"auth": {"api.github.com": {"header": "Authorization", "env": "GITHUB_TOKEN", "prefix": "Bearer "}}}}`.
  - Authentication is sent only over https. If the environment variable is empty, it does not send and returns a failure (writing only the variable's name).
  - The same value in the response becomes `[redacted]`.
- **Subject of the decision:** `<host (lowercase, including port)><path>` (`subject`). The query part (from `?` on) is not included. Rules are written like `api.github.com/*`.
- **Private addresses:** The address is checked after the name is resolved, right before connecting (closing the loophole of pointing DNS at a private address). No proxy is used.
- **Redirects:** `fetch` follows up to 5 within the same host. It does not follow redirects to other hosts or from https to http, and returns the redirect target to the model. If the model goes to fetch it, it goes through the normal decision. `http` does not follow redirects and returns the status and `Location`.
- **Unknown outcome:** `http` makes it "outcome unknown" (`Unknown`) if no answer comes after the connection is made, and stops at blocked (needs confirmation). A failure before connecting is a "not sent" failure. Connections are not reused, so Go does not resend a POST on its own either.
- **Display:** Responses are read up to 5 MB, and the model is shown up to 100,000 characters (noting the cut). Non-text content (images and so on) is described only by type and size. HTML is turned into Markdown or text by a small converter of our own (no added dependency). If the status is 400 or above, the result is returned as a failure.
- **ACP:** The kind for `fetch` and `http` is `fetch`.
- **Tests:** `TestFetch`, `TestHTTPPost`, `TestHTTPUnknown`, `TestNetAuth`, `TestNetPrivateRefused`, `TestNetRedirects` (`internal/tools`), `TestNetSubject`, `TestNetSettingsAreTheUsers`, `TestCrashHTTPPost` (`internal/runtime`; we confirmed it fails if the effect of `http` is made `Read`).
- **No secrets left in the record:** The value is read from the environment variable inside the action and goes into neither the tool's input nor its result (the result is masked). There is not yet a test that scans the runtime record (DB), because there is no way to pass the test's TLS certificate in from the runtime. The tool unit test (`TestNetAuth`) checks that the value does not appear in the result.
- **Fixed after points raised by durability-reviewer**
  - **The model could choose the subject of the decision (fixed):** `subject` looked at `path` and `command` first, so adding `path` to a `fetch` or `http` call made something other than the URL the subject, slipping past the rules. `fetch` and `http` now use only the URL's host and path as the subject (`subject` takes the tool's name). The ACP title is also just the URL. Other tools' subjects are unchanged.
  - **`..` could get out of a rule's path (fixed):** A URL whose path has a `.` or `..` segment (including `%2e%2e`) is not sent and returns a failure.
  - **The person approving could not see what would be sent (fixed):** When asking about `http`, it shows the URL (including the query part), the headers the model added, and the body (up to 2,000 characters) (`preview`).
  - **Added private address ranges:** `0.0.0.0/8`, `100.64.0.0/10` (CGNAT, Tailscale, cloud metadata), `192.0.0.0/24`, `198.18.0.0/15`, `240.0.0.0/4`, IPv6 that carries IPv4 (`::/96`, NAT64, 6to4).
  - Tests: `TestNetSubject` (arguments added by the model do not change the subject), `TestHTTPPreview`, `TestNetDotSegments`, `TestNetPrivateRefused` (ranges), `TestHTTPUnknown` (timeout after connecting).
- **Accepted weaknesses**
  - `fetch` follows redirects within the same host without re-checking the rules. A `fetch` rule written with a path (`example.com/public/*`) can be escaped by a redirect within the same host. `fetch` only reads, so we accept this.
  - Masking covers only the value itself. A value returned in a changed form (escaped, base64 and so on) cannot be masked.
  - A rule's `*` also matches `/`. Writing a host like `*.github.com/*` also matches `evil.example/.github.com/x`. Write hosts without `*`.
