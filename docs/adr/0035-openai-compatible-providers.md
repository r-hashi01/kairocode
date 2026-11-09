# 0035 OpenAI-compatible APIs as providers, local models among them

- Status: Proposed
- Date: 2026-11-09
- Related: requirement FR-18 (providers through adapters), design section 11 item 2, ADR 0002 (only Claude, at first), 0006 (the conversation as `kc_message`), 0013 (credentials sent over https only), 0014 (providers; section 6 planned `openai` on the Responses API), 0020 (compacting at the model's context), 0025 (credentials kept out of the record), invariants 2, 5, 6 and 9

## Context

- M8 asks for "an adapter for OpenAI-compatible APIs (including local models), written with the standard library's HTTP".
- "OpenAI-compatible" in practice means the Chat Completions API (`POST /v1/chat/completions`, with `tools`, and streaming as server-sent events). Ollama, LM Studio, llama.cpp's server, vLLM, OpenRouter, Groq, Together, DeepSeek and others speak it. OpenAI's own Responses API is spoken by OpenAI alone.
- ADR 0014 section 6 planned an `openai` provider on the Responses API. One adapter for Chat Completions reaches OpenAI too, and every server above.
- Today the API engine is Claude's alone: the `model` action calls `internal/provider`'s Anthropic adapter, and KairoCode runs the loop (tools, permissions, the record). An OpenAI-compatible API would run in the same loop: only the adapter differs.
- Where the requests go, and with which key, is new configuration. A project's settings must not choose it: a cloned repository could send the conversation (the code in it) to a server of its own, with a key from your environment.

## Decision

1. **One adapter, for Chat Completions** (`internal/provider/openai.go`). It is written with `net/http` and `encoding/json`, with no added dependency.
   - **Requests:**
     - The system prompt is a `system` message. KairoCode's tools are `tools` of type `function`.
     - `Force` maps to `tool_choice: {"type": "function", …}` and `NoTools` to `tool_choice: "none"`.
     - `Effort` maps to `reasoning_effort`, sent only when set.
     - Images in a message are `image_url` parts with a `data:` URL.
     - Tool results are `tool` messages carrying the call's id.
   - **Streaming:** the request has `stream: true` and `stream_options: {"include_usage": true}`. Text is given to `OnText` as it comes. Tool calls are put together from their pieces.
   - **Usage:**
     - `prompt_tokens`, `completion_tokens` and `prompt_tokens_details.cached_tokens` are read.
     - A server that sends no usage gives zeros, not an error.
   - **Stop reasons:**
     - `stop` → end, `tool_calls` → tool use, `length` → max tokens, `content_filter` → refusal.
     - A reply that has tool calls is tool use, whatever reason the server gave (some local servers say `stop`).
   - **Errors:**
     - 429, 5xx and a cut connection are retried, as with Anthropic.
     - Other 4xx are permanent.
     - A 400 whose message speaks of the context length is `ErrContextTooLong`, so the conversation is compacted (ADR 0020).
   - **Reasoning text** some servers send (`reasoning_content`) is not kept: only the reply's text and its calls are.
2. **Providers are named in the user's own settings, and two are there from the start.** In `~/.config/kairocode/kairocode.json`:

   ```json
   {"providers": {
     "lab": {"baseURL": "https://llm.example.com/v1", "keyEnv": "LAB_API_KEY",
             "models": {"qwen3-coder": {"context": 262144}}}
   }}
   ```

   - **Built in:**
     - `openai`: `https://api.openai.com/v1` with `OPENAI_API_KEY`.
     - `ollama`: `http://localhost:11434/v1` with no key.
     - A user's entry of the same name replaces a built-in one.
   - **The key is named, never written:** `keyEnv` names an environment variable, read inside the `model` action as the Anthropic key is (invariant 6). It is not in any input, output, log or error.
   - **Where the key goes:** a key is sent over `https` only, or to a loopback address (ADR 0013's rule). A provider that would send it over plain `http` elsewhere is refused, saying why.
   - **A project's `kairocode.json` cannot name or change providers.** Its `providers` are ignored with a warning: a cloned repository does not choose where the conversation goes.
3. **How it runs:**
   - **The engine:** a provider of this kind runs as the engine `oai:<name>` in KairoCode's own loop. It is like the API engine: its tool calls are KairoCode's, so a try of a flow may use it, and it answers a flow's `decide` with a forced tool.
   - **Choosing it:**
     - `/provider` lists the configured providers.
     - `/model` lists what `GET <baseURL>/models` returns and takes free input.
     - `-provider oai:<name>` and `KAIROCODE_ENGINE` work as for the others.
     - A switch is recorded as an event to the session (ADR 0014).
   - **The record:**
     - The engine and the model are recorded in the turn's input.
     - The base URL and the key are read in the `model` action from the user's settings, as the key is: they say where to send the call, not what the workflow does.
     - The model call's input is the list of message hashes (invariant 5), as for Anthropic. Its output is recorded as today.
   - **The context:** a model's `context` comes from the provider's `models` setting, else 128,000 tokens. `provider.ContextOf` takes the provider as well as the model.
4. **ADR 0014 section 6's `openai` line is replaced:** `openai` is this adapter's built-in provider, on Chat Completions. The Responses API is not used.

Where this shows in the implementation:
- `internal/provider/openai.go` (the adapter).
- `internal/config` (reading `providers`, the user's only).
- `internal/runtime`: the engine `oai:<name>` in `factsOf`, choosing the adapter in the `model` action, `/provider` and `/model` lists.
- `internal/workflows`: no change. The engine is a value the turn already records.
- Docs: a README section on local models.

## Consequences

- **What it gives:** OpenAI's models, other hosted ones, and local models (Ollama, LM Studio, llama.cpp, vLLM) run with KairoCode's tools, permissions, sandbox and record, and with the same guarantees as Claude on the API.
- **What depends on the model:** how well a model calls tools is its own. A model or server without tool calling fails the turn with the server's error, shown to the user.
- **Tests** (no network: an `httptest` server answers with recorded streams, invariant 9):
  - `TestOpenAIAdapter`: a streamed reply with text and two tool calls in pieces; usage; stop reasons, including tool calls under `stop`; forced and no tools; images; `reasoning_effort`.
  - `TestOpenAIErrors`: 429 and 5xx retried, other 4xx permanent, the context-length 400 as `ErrContextTooLong`.
  - `TestOpenAIProviderTurn`: a session on `oai:<name>` runs a turn with tool calls through KairoCode's tools, and resumes from the record after a crash with the model not called again for steps already recorded (`TestCrash…`).
  - `TestOpenAIKeyNotRecorded`: the key is in no record, log or error.
  - `TestOpenAIKeyHTTPSOnly`: no key over plain `http` except to loopback.
  - `TestProjectCannotNameProviders`: a project's `providers` are ignored.

## Alternatives considered

- **The Responses API for `openai`, as ADR 0014 planned.** Only OpenAI speaks it, so a second adapter would still be needed for every other server. Chat Completions covers OpenAI as well.
- **An SDK (`openai-go`).** It adds a dependency, and the protocol is small: one endpoint and server-sent events.
- **Providers in a project's settings.** Convenient for a team, but a cloned repository would choose where the code is sent, with a key from your environment. A team can share a snippet for the user's settings instead.
- **Reaching local models through an ACP agent or a CLI (e.g. opencode as the engine).** It would be another agent's loop, not KairoCode's, with the limits of ADR 0014 and 0015.
