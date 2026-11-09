# 0040 Rate limits per provider: requests and tokens a minute, and the waits a 429 asks for

- Status: Accepted (2026-11-09)
- Date: 2026-11-09
- Related: requirements FR-27 (rate limits), ADR 0024 (decision 6, replaced with the rest of it by 0039, which left rate limits out), 0039 (the ledger), 0035 (OpenAI-compatible providers), 0008 (one daemon), kairo's ADR 0059 (`Limit`, `Rate`), kairo#20, invariants 2, 4, 6 and 7, M11

## Context

FR-27 says to respect each provider's limits of requests a minute (RPM) and tokens a minute (TPM). Sessions, subagents and flows run side by side in one daemon, and a flow of ten agents sends ten model calls at once: with a low tier of an API key, or a free hosted model, they come back 429 ("too many requests").

What happens today:

- **Anthropic's API:** the SDK asks again twice, after the wait the 429 asks for (`retry-after`). After that, the `model` action fails retryably, and kairo tries the step at most three times, 200 ms and then 400 ms apart. A turn that keeps meeting 429s fails within seconds, although waiting a minute would have done.
- **OpenAI-compatible APIs:** a 429 fails the action retryably at once, with kairo's same three tries, and the server's `Retry-After` is not read.
- **The calls of one provider do not know of each other:** ten agents each meet the limit, and each asks again on its own.

What kairo offers (ADR 0059):

- `kairo.Limit(n)` and `kairo.Rate(perMinute)` limit an action's steps by its `Destination`, fixed when the action is declared. The `model` action serves every provider (the engine is in its input), so a provider cannot be its destination.
- kairo has no limit of tokens, and no way for a step's failure to say how long to wait before the next try.

So, as ADR 0024 decided before it was replaced, the limits are kept in the daemon, around the call to the provider.

## Decision

1. **Settings:** `rateLimits` in the user's `kairocode.json`, by provider:

   ```json
   {"rateLimits": {"anthropic": {"rpm": 50, "tpm": 30000}, "groq": {"rpm": 30, "tpm": 6000}}}
   ```

   - **Which provider:** `anthropic` is the Claude API (the `api` engine); another name is an OpenAI-compatible provider of `providers` (the `oai:<name>` engine).
   - **Defaults:** a limit not written is no limit.
   - **A project's `rateLimits`:** read too, and the smaller of each is taken, so a project may slow its calls and never speed them past the user's.
   - **When it is read:** at each call, so a change takes effect at the next call. A limit decides only when a call starts, never what it does or returns, so it is not recorded (invariant 2 is about what the workflow branches on).
2. **Before a call, it waits its turn, in the action.** The daemon keeps, for each provider, the start of each call and the tokens of each reply in the last minute.
   - **Requests:** a call waits while the provider had `rpm` calls start in the last minute.
   - **Tokens:** a call waits while the replies of the last minute used `tpm` tokens or more (input and output, cache reads and writes included). The tokens of a request are known only from its reply, so one call can go past `tpm`, and the next ones wait for it.
   - **The order:** calls waiting for one provider start in the order they began to wait.
   - **The calls counted:** every model call through `providerFor` (`model`, compaction, `decide`), so a turn's judge and its agents share the limit.
   - **When the action is stopped:** the wait ends with the action's context (a turn cancelled, the daemon closing).
3. **A 429 pauses the provider, and the call waits and asks again, in the action.**
   - **How long:** for the time the reply asks (`retry-after`, `Retry-After`, in seconds or as a date), or 10 seconds when it does not say.
   - **For whom:** every call to that provider waits until then, not only the one that met it.
   - **How many times:** the call is asked again at most 5 times, and waits at most 5 minutes in all; after that, it fails retryably, as today.
   - **Why it is safe to ask again:** a 429 is a refusal before anything is generated, so nothing is paid for and nothing ran; ask again in the same attempt changes nothing that is recorded.
   - **The Anthropic SDK's own retries** are turned off (`MaxRetries(0)`), so the 429s are counted and paused here once.
   - **Errors:** the provider adapters return a `*provider.RateLimitError` with the wait the reply asked for. Other retryable errors (5xx, a stream cut short) are unchanged.
4. **Invariant 7 is not changed.** Invariant 7 is about waiting for a person or an outside event, which goes through `kairo.WaitFor` and lets go of the process. These waits are short, inside an action that is running, for a provider that is busy: the action holds its step's lease as any running action does. They are bounded: a minute for the limits, 5 minutes for 429s, within the action's timeout of an hour.
5. **What is shown:**
   - The terminal UI's status line says "waiting for anthropic's rate limit" while a call of the session waits more than a second (a live event of the session, not a record).
   - `/usage` shows the rate limits set, and the 429s met today by provider.
6. **CLI engines are not limited:** Claude Code, codex, copilot and ACP agents call their APIs themselves.

Where this shows in the implementation:
- `internal/provider`: `RateLimitError`, from the Anthropic adapter (the SDK's error with its response's headers) and the OpenAI-compatible adapter.
- `internal/runtime`:
  - `ratelimit.go`: the windows of each provider, the queue, the pauses.
  - `metered` (ADR 0039): waits before a call, counts its tokens after, and asks again after a 429.
  - reading `rateLimits`.
  - the live event while waiting.
- `internal/api` and `internal/tui`: the event and the status line; `/usage`'s rows.

The kairo side: [kairo#20](https://github.com/r-hashi01/kairo/issues/20) asks for a step's destination given when it is called (not when its action is declared), a limit of tokens, and a failure that says when to try again. When kairo has them, the waits move there; the code here carries `// kairo#20:` comments until then.

## Consequences

- Turns, agents and flows that share a provider stay within its limits, and a 429 makes them wait as long as the provider asks, instead of failing after three quick tries.
- A call may now wait up to a minute (limits) or 5 minutes (429s) before it starts. A turn shows it is waiting.
- A busy provider slows every session of the daemon that uses it, which is what its limit asks.
- Tests (to add):
  - `TestRateLimitRequests`: with `rpm` 2, a third call in a minute waits until the first is a minute old (on a clock the test moves).
  - `TestRateLimitTokens`: with `tpm`, a call waits while the last minute's replies used it up.
  - `TestRateLimit429`: a fake server's 429 with `Retry-After: 2` pauses the provider; a call made meanwhile by another session waits too; the call is asked again and answers; after 5 429s, the action fails retryably.
  - `TestRateLimitProjectCannotLoosen`.
  - `TestRateLimitCancelled`: a waiting call ends when its turn is cancelled.
  - The adapters: `Retry-After` in seconds and as a date, for both.

## Implementation (2026-11-09)

- `internal/runtime/ratelimit.go`:
  - `rateLimiter` keeps one window for each provider: the starts and the replies' tokens of the last minute, and the pause a 429 set.
  - A call takes the provider's gate (a channel of one), waits there until the window lets it start, and gives the gate up once it has started. Calls waiting at the gate are served in the order they came.
  - The clock and the wait are the limiter's own, so its tests move the clock instead of waiting.
- `metered` (ADR 0039) goes through `rateLimiter.complete`, and so do `model`, compaction and `decide`.
  - The settings are read at each call, from the session's folder (the run's). A run with no session, such as a notify run's flow, has the user's settings alone.
  - Only an answered call's tokens are counted, and they are what the ledger records too.
- **Changed from the decision: how long a call waits.**
  - A call waits **5 minutes in all**, whatever it waits for: its turn at the gate, the windows, or a pause another call's 429 set. The decision bounded only the 429s' waits; the durability review found that a single `Retry-After` of an hour (a daily quota) held the gate, and every call to the provider behind it, until the action's timeout of an hour.
  - A 429 that asks for more than what is left fails the call at once, with the provider's message. The provider is still held back for as long as it asked, and the next calls fail at once too, saying for how long ("lab's rate limit asks to wait 1h0m0s, past the 5m0s a call waits: try again later"). This is as fast as before this decision.
  - A call waiting at the gate behind others says so ("other calls first"). Every wait says, however it ends, that it no longer waits (a cancelled one too).
- **Changed from the decision: Anthropic's 529 ("overloaded") is waited for as a 429 is.** With the SDK's own retries turned off, a 529 would otherwise fail after kairo's quick tries, where the SDK used to ask again.
- **Changed from the decision: other failures are tried again differently.** Decision 3 said 5xx and a stream cut short were unchanged. With the SDK's retries off (it asked twice, 0.5 s and 1 s apart, for 408, 409, 5xx and lost connections), they had only kairo's three tries 200 ms apart. The model actions (`model`, `history.compact`, `flow.decide`) now try five times, a second apart and doubling (about 15 seconds in all), for every retryable failure. The SDK's `x-should-retry` header is no longer read.
- **The provider's name:** `anthropic` for the `api` engine, the name after `oai:` for an OpenAI-compatible provider.
- **What is shown:**
  - A wait of a second or more is sent as a `waiting` event of the session. Over ACP it goes as the notice `_kairocode/waiting`, which other clients ignore, and the terminal UI's status line shows it until the call starts or the turn ends.
  - `/usage` lists the providers with limits set in the folder's settings, or with 429s today, and how many 429s each met today.
  - The count of 429s is the daemon's, so it starts again when the daemon does.
  - The event is the session's, not the call's: with agents of one session waiting side by side, one call's end of waiting clears what the status line says of another's until that one says again.
- `// kairo#20:` marks where kairo's support would replace this (the head of `ratelimit.go`).
- **Tests:**
  - `TestRateLimitRequests`, `TestRateLimitTokens`, `TestRateLimit429` (including the pause for another call, and giving up after `rateTries`), `TestRateLimitLongWaitFailsAtOnce` (an hour's `Retry-After`, for the call and the next), `TestRateLimitCancelled`, `TestRateLimitCancelledSaysSo`, `TestRateLimitProjectCannotLoosen`: on the limiter's clock (the cancelled ones on the real one).
  - `TestRateLimit429Turn`: a turn on a fake OpenAI-compatible server that answers 429 with `Retry-After: 1`; the call is asked again after a second, and `/usage` counts it.
  - `TestRetryAfter`, `TestRateLimitErrors`: both adapters; the Anthropic SDK asks once.
  - The terminal UI's `TestRateLimitWaitShown`.

## Alternatives considered

- **kairo's `Rate` and `Limit` on the `model` action.** One limit for every provider together, and no tokens: a fast local model would be held to a hosted provider's limit.
- **One action per provider** (`model.anthropic`, `model.groq`), each with its kairo `Rate`. Providers are named in the user's settings and change while the daemon runs, and a turn's call names its action in the record: a provider renamed would change the turn's calls. And kairo has no tokens a minute, nor a wait asked for by a 429.
- **Fail and let kairo try again later.** kairo's backoff is fixed when the action is declared (200 ms, doubled), and three tries take under a second: a 429 asking for 30 seconds still fails the turn.
- **Estimate a request's tokens before it is sent** (characters divided by four), to never go past `tpm`. The estimate is wrong for images and for some languages, and the provider's own count arrives with the reply; going past by one call and waiting after is what the providers' own clients do.
