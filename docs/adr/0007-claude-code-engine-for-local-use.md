# 0007 Locally, the user's own Claude Code (subscription) can be used as the model engine

- Status: Accepted (2026-10-09)
- Date: 2026-10-09
- Related: requirements FR-18, FR-19, NFR-1, NFR-7; ADR 0001, 0002 (ACP), 0005 (sandbox), 0006 (conversation storage)

## Context

The user's policy (2026-10-09): in the end, run it with an API key. For local development now, run it on a Claude Code subscription. Two engines, the Claude API and Claude Code, are enough.

Anthropic's documents say the following (checked on 2026-10-09).

- "Third-party developers are not allowed to offer Claude.ai login in their apps, or to route requests on behalf of users with the credentials of a Free, Pro or Max plan" (https://code.claude.com/docs/en/legal-and-compliance)
- There is also an exception: "this does not prevent users themselves from logging in to an unmodified Claude Code binary with their own subscription"

We also checked how billing is handled (https://support.claude.com/en/articles/15036540-use-the-claude-agent-sdk-with-your-claude-plan, updated 2026-10-07).

- On 2026-05-14, it was announced that use of `claude -p` and the Agent SDK would move outside the subscription's allowance (a monthly credit counted at API prices) from June 15.
- That change was stopped on June 15. `claude -p` logged in with a plan is still used from the subscription's usage allowance.
- From 2026-10-07, the Max and Team plans come with a monthly API credit. The user is not on a Max plan, so this ADR does not deal with it.

This handling once nearly changed and then stopped, so we treat it as something that may change again.

So KairoCode does not touch subscription credentials, and is limited to starting, headless, the **unmodified `claude`** the user has logged in to. Whether to make this a feature of a distributed product is decided after checking with Anthropic.

On 2026-10-09 we prototyped with `claude` 2.1.295 and confirmed the following.

- With `claude -p --tools "" --strict-mcp-config --mcp-config <config>`, the built-in tools disappear and only the MCP tools passed in remain. The model itself answered "there are only echo and slow, and I can't read files".
- The tool_use ID comes in `_meta["claudecode/toolUseId"]` of MCP's `tools/call`.
- When `claude` is forcibly stopped while a tool is running and continued with `--resume <ID>`, the following happens.
  - Finished tool calls do not come again.
  - The call that was running comes again **with the same arguments and a new tool_use ID**.
- A session started with `--session-id <uuid>` keeps the same ID after `--resume`, and the conversation's record stays in `~/.claude/projects/<working directory>/<ID>.jsonl`.

## Decision

### 1. Two model engines

| Engine | How it runs | When it is used |
|---|---|---|
| `api` (default) | KairoCode calls the model one call at a time and runs the loop (ADR 0001, 0006) | API key. The final way of distribution |
| `claude-code` | The user's `claude` runs the loop, and KairoCode offers tools over MCP | Local development. Only when the user chooses it explicitly |

- The engine is chosen in the configuration (`engine` in `kairocode.json`, or `KAIROCODE_ENGINE`). `claude-code` is not made the default.
- It can also be chosen per agent with `engine` in the agent definition (ADR 0002).

### 2. A turn on the `claude-code` engine

- A turn is a single action, `claude.turn` (effect `Unprotected`). This action starts `claude` in the following form.

  ```
  claude -p <prompt> --session-id|--resume <uuid> --tools "" \
    --setting-sources "" --disable-slash-commands --strict-mcp-config \
    --mcp-config <KairoCode's MCP> --allowedTools mcp__kairocode__<tool>,… \
    --system-prompt <the agent's prompt> [--model <model>] \
    --output-format stream-json --verbose --include-partial-messages   (stdin is /dev/null)
  ```

  - `--setting-sources ""` and `--disable-slash-commands`: the user's settings, hooks, plugins and skills are not loaded. When we checked on 2026-10-09, without these the user's global hooks (a plugin's SessionStart and so on) ran inside `claude -p` too and inserted extra context into KairoCode's turn. With them, hooks did not run, the only plugins were the ones built into Claude Code, and `apiKeySource` stayed `none` (the subscription). `--bare` also cuts hooks, but it limits authentication to API keys only, so it is not used.
  - `stream-json` needs `--verbose`. If stdin is left open, `claude -p` waits 3 seconds for input.
  - `--model` is added only when the agent specifies a model. Otherwise it is the default model of the user's Claude Code.

  - **The session ID:** a UUID made by a fixed rule from KairoCode's session ID (invariant 8). The first turn starts with `--session-id`, and from the second on with `--resume`.
  - **Streaming:** the `stream-json` output is streamed as live output with `TaskContext.Emit`. The final response and usage are taken from the result line and recorded.
  - **Environment variables:** variables that indicate running inside Claude Code (`CLAUDECODE` and so on) are removed from the child process. This is so that even when KairoCode itself is being developed inside Claude Code, it is not refused as nested.
- **Resuming after a crash:** what to do is decided by Claude Code's own record (the transcript, `projects/*/<uuid>.jsonl` in `$CLAUDE_CONFIG_DIR` or `~/.claude`).
  - If there is no transcript, it starts with `--session-id`; if there is one, it continues with `--resume`.
  - A turn marker (`[kairocode turn <turn ID>]`) is appended to the end of the prompt. If the marker is already in the transcript, the prompt is not sent again; "continue from where you were interrupted" is sent instead.
  - What is lost is only the model call that was running (NFR-1). However, if it crashes between `claude` finishing and KairoCode recording the response, the recorded response is the answer to "continue" (the original response is in the transcript).
- **Stopping a leftover `claude`:** `claude` is started in its own process group, and its PID is recorded in `kc_claude_proc`. Before redoing a turn, if the recorded PID is still running as this session's `claude`, it is stopped. On Linux, the child is made to stop when the parent dies (`Pdeathsig`). On macOS, the child outlives the parent, so this way of stopping is relied on.

### 3. Tie tool calls to kairo's records

- KairoCode's MCP server is served over HTTP by the runtime process. A per-turn token tells which turn a call belongs to.
- Each MCP call becomes a kairo run. Its ID is not the `tool_use ID` but `<turn ID>/mcp/<first 24 characters of sha256(tool name + normalized arguments)>.<which occurrence of the same combination>`.
  - Reason: a call that is made again after a crash comes with a new tool_use ID (the prototype above).
  - When a new call comes, it first looks for a run with the same name and arguments **whose result has not been confirmed to have reached Claude Code**, and joins it if there is one. If not, it starts a new run for the next occurrence.
  - If the run it joined is stopped at blocked (needs confirmation) (a real step whose result is unknown), it returns without waiting a result saying "waiting for a person's confirmation; stop here and tell the user". Waiting would keep holding the `claude` process (invariant 7).
  - "Reached" is confirmed when either of the following is seen. Both rely on Claude Code moving on only after it has received the result.
    - The next MCP call came from the same `claude` process.
    - That process printed its result line and ended.
  - Writing out the result does not count as "reached". In a test on 2026-10-09, that form recorded a response to a dead process as "reached", and about once in 60 runs a real tool ran twice. This is because the server side is late to notice that the connection was cut.
  - A run is started with `Submit` and waited on with `Await`, so that one run is not driven by two handlers. A run that another handler is waiting on right now is not joined (two calls with the same content issued in parallel are two calls).
  - **What was settled in M1 (2026-10-09):**
    - The record of calls is kept, apart from kairo's runs, in a table of our own too (`kc_mcp_call`: which `claude` process it came from, and whether it was confirmed to have reached). Even if kairo deletes old runs (r-hashi01/kairo#4), the way calls are chosen does not change. When the result of a deleted run is needed, the model is told "the result was lost; do not do it again, tell the user".
    - If a call with the same content as a real call marked "reached" comes from a different `claude` process, it is not run silently; a person is asked "run it again?". "Reached" only means that Claude Code held the result in memory (if it crashes partway through several calls in one response, the result may not be left in the transcript). Read-only tools and idempotent tools are simply run again.
    - In exchange, after a crash, when someone legitimately wants to make the same real call again, they are asked too.
- The tools themselves (effect types, permission decisions, the sandbox) are the same as on the `api` engine (ADR 0005).

### 4. Waiting for approval

- A tool that needs approval does not keep the MCP call waiting (implemented in M1; `TestClaudeEngineAsksForApproval`). If it waited, the `claude` process would stay alive the whole time the approval is waited for (NFR-4).
- Instead, it returns a result showing that it is waiting for approval, and following the system prompt's instructions, the model ends the turn.
- The workflow waits for the approval with `kairo.WaitFor`. The answer is recorded in `kc_mcp_grant` (per turn and per key of the call's content), and the next `claude.turn` tells the answer and does `--resume`. When `claude` issues the same call again, it goes through or is refused according to the recorded answer.

- Addendum (2026-10-09, M1's review): one answer is used by only one MCP call's run (`kc_mcp_grant.used_by`). If `claude` issues a call with the same content again, the user is asked again. Also, an MCP call that was cut off partway and became "waiting for a person's confirmation" stays listed in `kairocode approvals` even after the turn ends, and is settled with `kairocode approve` (`TestCrashClaudeEngine`).

### 5. What cannot be used with this engine

- The conversation is held by Claude Code's record (the transcript). ADR 0006's conversation chain is only for the `api` engine. Only the user's messages and the turn's final response are put in `kc_message`.
- Replay from the records alone (FR-25) cannot be used with this engine.
- RPM, TPM and cost limits follow the subscription's limits.

### 6. Credentials

- KairoCode does not read Claude Code's credentials. It does not store them. It does not leave them in records (NFR-7, invariant 6).
- Variables with names that look like API keys are removed from the child process's environment (ADR 0005). Otherwise `claude` might be billed with that key rather than the user's subscription.

## Consequences

- Local development can be run within the subscription. The `api` engine stays as the final path, as it is.
- The durability of the `claude-code` engine is per turn. What KairoCode reliably keeps is "a real tool does not run twice".
- Tests:
  - Instead of `claude`, a fake command that makes fixed MCP calls is used. The real `claude` is not called in tests (invariant 9).
  - Two turns run with `--session-id` and `--resume` of the same ID, and the MCP tool's result reaches the response (`TestClaudeEngineTurns`).
  - A real tool called again with a new ID after a crash does not run twice (`TestClaudeEngineResumeDoesNotRepeatARealCall`; checked by running it 300 times).
  - If the same process makes the same call twice, it runs twice (`TestClaudeEngineRepeatedCallRunsAgain`).
  - When KairoCode is SIGKILLed while a real tool is running and reopened, it stops the leftover `claude`, does `--resume` and sends "continue". The same call joins the blocked (needs confirmation) run, does not run again, and "waiting for a person's confirmation" goes back to the model (`TestCrashClaudeEngine`).
  - A turn ends waiting for approval and resumes after the approval (`TestClaudeEngineAsksForApproval`).
- Checks with the real `claude` are done by hand (because they use the user's subscription allowance).

## Alternatives considered

- **Use the subscription's OAuth token from KairoCode's HTTP client:** this falls directly under the prohibition in the document above.
- **Run it through ACP's Claude adapter (`@agentclientprotocol/claude-agent-acp`):**
  - From 0.18.0, this adapter runs edits and bash inside Claude Code and does not pass them to the client. In ACP v2, the very mechanism that leaves file and terminal operations to the client goes away.
  - Removing the built-in tools would rely on a `_meta` extension.
  - ACP is used as the entry point on KairoCode's user side (ADR 0002).
- **Use Claude Code's built-in tools as they are:** the effects would happen outside KairoCode's records and sandbox. After a crash, "nothing happens twice" could not be kept.

## Addendum (2026-10-27): the input of a turn with images (ADR 0026)

Only for a turn whose user message has images, the prompt is passed not as the argument of `-p <prompt>` but with `-p --input-format stream-json`, as one line of JSON on stdin (`{"type":"user","message":{"role":"user","content":[text, images…]}}`), and stdin is closed. How the output is read, `--session-id` and `--resume`, and cutting the built-in tools do not change. Turns without images stay as they are. This is because a single string prompt cannot carry images (the Agent SDK's documents). On 2026-10-27, with the real `claude` (2.1.311), we confirmed that it finishes the turn and exits even with a closed stdin, and that with `--resume` it remembers the earlier images.


## Addendum (2026-10-28, narrowing when the same call is asked about)

- The same real call from an earlier claude run is asked about as "it may have been called again after a crash" only when the earlier run was cut off. If the earlier run returned its result and ended (for example, it ended to wait for an approval), the result given is in the session Claude Code saved, so the same call is a new call.
- Before, it always asked if the run was a different one. Claude Code run by KairoCode becomes a new run every time an approval is answered, so calling the same command again within the same turn was asked about even though nothing had crashed (the user's comment, 2026-10-28).
- A mark for a run that returned its result and ended (that run's token) is written once in `kc_message` with the kind `mcp-run-ended` (the same way of writing as a hook's answer; the storage format does not change). If it crashes before writing, there is no mark, and it asks as before.
- Tests: `TestClaudeEngineSameCallAfterARunThatEnded`. `TestClaudeEngineRepeatAfterInterruptionAsks` and `TestClaudeEngineApprovalServesOneRun` (it asks after a cut-off) still pass as they are.

## Addendum (2026-10-28, how long Claude Code waits for KairoCode's tools)

- By default, Claude Code waited only 1 minute for KairoCode's MCP calls, and gave up with "The operation timed out." after that (checked on 2026-10-28: `sleep 58` went through and `sleep 70` was cut off). On KairoCode's side the command keeps running, but the result does not reach the model. Long tests and `bash_wait` (ADR 0028; waits up to a day) could not be used with the Claude Code engine.
- When starting a turn's `claude`, the wait is set to `bash_wait`'s limit (one day) plus one minute. It is passed both as `timeout` of KairoCode's server in `--mcp-config` and as the environment variable `MCP_TOOL_TIMEOUT` (if the user has set something longer, that is kept). The server's `timeout` also extends the wait while nothing is returned (default 5 minutes) by the same amount.
- Tests: `TestClaudeWaitsForLongCalls` (the values the fake `claude` received). Checking with the real `claude` comes after the daemon is replaced with the new build.
