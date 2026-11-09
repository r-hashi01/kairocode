# 0039 Usage and limits: what has been used, shown as Claude Code and Codex show it, and limits that stop a turn

- Status: Accepted (2026-11-09)
- Date: 2026-11-09
- Related: requirements FR-26 (limits), design section 5 ("budget decisions are made from recorded values") and section 9, ADR 0024 (replaced by this one), 0020 (the turn's version), 0007 and 0014 (CLI engines), invariants 2, 5 and 6, M10

## Context

ADR 0024 proposed limits per turn and per session (steps, time, tokens, cost) and rate limits per provider. It was put on hold until it was needed. The user took it up again for M10 and asked for more than limits: a person should see what they have used so far, against their limits, in the way Claude Code and Codex show it.

- Claude Code's `/usage` shows one row per limit: a bar, how much of it is used (as a percentage), and when it resets ("Current session", "Current week").
- Codex's `/status` shows the same kind of rows ("5h limit", "Weekly limit": a bar, the percentage used, when it resets), with the tokens the session has used.

In both, the limit is over time across sessions, not only per session. Someone paying for an API key thinks in the same way: "no more than $20 today".

What KairoCode has today:

- **Limits:** only a limit on the steps of a turn (`MaxSteps`, 50).
- **Usage:** each model call records its usage (`msg.Usage`) in its output. `Runtime.Usage` adds it up for one session from kairo's records, and `kairocode usage -session` and the terminal UI's status line show the session's tokens.
- **Gaps:**
  - Nothing adds up usage across sessions.
  - kairo deletes the record of a finished tree after `KeepFinished` (24 hours), so a week cannot be added up from kairo's records.
  - There is no cost: no prices.

## Decision

1. **A ledger of usage (`kc_usage`).** It is a new table in the store, with one row for each model call:
   - **Key and columns:** the row's key is the call's step (run ID and attempt). It holds the session, the folder, the engine and model, the time, and the four token counts.
   - **Who writes it:** the `model` action writes the row after the provider answers, in the same action. Claude Code's and ACP engines' turns write a row when their CLI reports usage.
   - **A call run again:** a call made again after a crash is another attempt, so it gets its own row. Its tokens were spent again, so they are counted again. Writing a row twice for the same attempt changes nothing (`INSERT OR IGNORE`).
   - **No secrets:** the row holds counts and names only (invariant 6).
   - **Rows from before:** usage from before the ledger existed is not added up. The ledger starts empty.
2. **Prices.**
   - **Where they come from:** cost is tokens times a price per million tokens (input, output, cache read, cache write). The price comes from a table of known models in `internal/provider` (Anthropic's, OpenAI's), which `models.<name>.price` in the user's `kairocode.json` overrides or adds to. Local models (Ollama) cost 0 unless a price is set.
   - **Prices are not in the ledger:** fixing the table fixes the amounts shown. The turn records the prices it uses (point 4).
   - **CLI engines:** for Claude Code, whose result reports the run's cost, the reported cost is stored in the row and used as it is.
   - **Unknown cost:** a model with no price and no reported cost has an unknown cost, and is shown as such.
3. **Settings, in the shape Codex's and Claude Code's settings have:** `limits` in `kairocode.json`:

   ```json
   {"limits": {
     "turn":    {"steps": 50, "time": "30m"},
     "session": {"tokens": 2000000, "cost": 5},
     "day":     {"tokens": 10000000, "cost": 20},
     "week":    {"cost": 100},
     "warn":    0.8
   }}
   ```

   - **The windows:**
     - `day` is the local calendar day, and `week` starts on Monday at 00:00, local time. So each has a clear time when it resets, as the rows of Claude Code and Codex do.
     - They add up all sessions of the daemon (its data directory).
     - `session` is the session.
   - **Defaults:** a limit not written is no limit, except `turn.steps`, which stays 50.
   - **The user's settings and the project's:** both are read, and the smaller limit takes effect, so a project can tighten a limit but not loosen one.
   - **Warning:** `warn` is the share of a limit at which the user is warned (0.8 by default).
4. **Decisions use only recorded values (invariant 2).**
   - **What `agent.load` records at the start of a turn:**
     - the limits;
     - the prices of the turn's model;
     - what has been spent in each window and in the session, read from the ledger;
     - when each window ends.
   - **Within the turn:** each model reply's recorded usage is added to these, and its cost is computed from the recorded prices: arithmetic in the workflow.
   - **When it is checked:** before each model call, if anything spent has reached its limit, the turn stops.
   - **`turn.time`:** it is measured with `ctx.Now()` from the turn's start. Time spent waiting for a person (approvals, reviews, questions) is not counted.
   - **A window that resets during a turn:** the turn goes on counting against what it read at its start, and the next turn reads the new window.
5. **When a limit is reached:**
   - **The turn:** it ends with `Stopped: "limit"`. The reply says which limit was reached, how much was used, and when it resets (for `day` and `week`), or how to raise it.
   - **The clients:** ACP gets `stopReason`, and the terminal UI a line.
   - **The next turn:** while a session or window limit is still reached, it stops before its first model call with the same message. Only `turn` limits end with the turn.
   - **Unknown cost:** a model whose cost is unknown does not count towards `cost` limits. If one is set, `/usage` says so.
6. **Seeing it, as Claude Code's `/usage` and Codex's `/status` show it:**
   - **Where:** `/usage` in the terminal UI and as an ACP command, and `kairocode usage` with no `-session` (with `-json`).
   - **What it shows:** one row for each of this session, today and this week. Each row has:
     - a bar and the percentage used, against the tightest limit set for it (tokens or cost);
     - the amount, as "$2.05 of $5.00" or "1.2M of 2M tokens";
     - when it resets.
   - **Below the rows:** this session's tokens (input, output, cache), and today's cost by model.
   - **A row with no limit:** it shows the amount and "no limit", with no bar.
   - **The status line and warnings:** the status line keeps the session's tokens, and adds the cost when it is known. When a turn ends with anything past `warn`, the terminal UI prints one line, as Claude Code does near its limits: "78% of today's $20 used, resets at 0:00".
   - **Its source:** what `/usage` shows comes from the ledger, read by the daemon (`GET /usage`). Showing is not deciding, so it may read the ledger as it is now.
7. **Usage limits of a CLI engine's own plan are not KairoCode's.** Claude Code under a subscription (Pro, Max) and codex under ChatGPT have their own 5-hour and weekly limits, which their CLIs do not report to KairoCode. `/usage` shows what KairoCode counted, and says, for those engines, that their own limits are shown by their own `/usage` and `/status`.
8. **Version.** The turn workflow gains a place where it stops, so its version goes to 5, and version 4 is kept draining (kairo's ADR 0060). `agent.load`'s output gains fields, which stay out of the JSON when empty.

The rate limits of ADR 0024 (decision 6, FR-27) are not part of this decision. They are left for later.

Where this shows in the implementation:
- `internal/store`: `kc_usage`, writing a row, and adding rows up by session and window.
- `internal/provider`: the price table, and prices from the settings.
- `internal/runtime`:
  - limits, prices and what has been spent, in `agent.load`;
  - rows written by `model` and by the CLI engines' turns;
  - `Runtime.UsageNow` for `/usage`.
- `internal/workflows/turn.go`: the decision, and `TurnVersion` 5.
- `internal/config`: reading `limits` and taking the smaller one.
- `internal/api`: the `Usage` type of `/usage`, and how its rows are written.
- `internal/server`: `GET /usage`.
- `internal/tui`: the `/usage` screen, the status line, and the warning.
- `internal/acp`: the `/usage` command, and `stopReason`.
- `internal/cli`: `kairocode usage`.

## Consequences

- **What a person gets:** they see what they have used, today, this week and in the session, against their limits, in the same shape as in Claude Code and Codex. A turn stops at a limit and says why and until when.
- **On resume:** where a turn stops depends only on recorded values, so a resumed turn stops at the same place.
- **The ledger:** it grows by one small row per model call. It is not deleted with kairo's records, so a week can be added up, and older rows can be dropped by age later.
- **Prices:** the table goes out of date as providers change prices. The settings can override it, and `/usage` names the price it used.
- **Tests (to add):**
  - `TestUsageLedger`: each model call writes a row; a call made again after a crash (a second attempt) writes a second one; the same attempt written twice is one row.
  - `TestLimitSessionTokens`, `TestLimitDayCost`: past a limit, the turn stops before its next model call, and the next turn stops before its first; on resume (the turn run again from its record) it stops at the same place.
  - `TestLimitTurnTimeNotWaiting`: with `kairotest`'s clock, `turn.time` stops a turn, and time waiting for an approval is not counted.
  - `TestLimitsProjectCannotLoosen`.
  - `TestPrices`: the table, an override from the settings, Claude Code's reported cost, and an unknown model that does not count towards `cost` and is named in `/usage`.
  - `TestUsageNow`: the rows of `/usage` (session, day, week), reset times, and windows at the boundaries (midnight, Monday).
  - The terminal UI's `/usage` screen and the warning line.

## Implementation (2026-11-09)

- **The ledger:**
  - The `model`, compaction and `decide` actions reach the API through `providerFor`, which now wraps the provider (`metered`): each call that answers adds its row.
  - Claude Code's turns and decisions add a row with the cost they report (`total_cost_usd`), and codex's, copilot's and ACP agents' turns add a row with their tokens.
  - A row that cannot be written is logged, not the call's failure: the call was paid for, and failing it would make it again.
  - The row's session is the run's (`ownerSession`), and the folder is not kept: `/usage` reads it from the session.
- **Prices:**
  - The table has Anthropic's models only, from Anthropic's published prices. OpenAI's are not in it: a model on an OpenAI-compatible API costs what `models.<name>.price` says, or nothing on this machine, or is not known.
  - A project's `models.<name>.price` is not read.
- **What `agent.load` records:** it records `Agent.Limits` with the turn's limits, its model's price, and what had been spent, read from the ledger. The session is read from the run's ID: a new field in `agent.load`'s input would have changed its call's ID in turns under way.
  - With no limit set but steps, `Limits` is nil and the turn checks nothing.
  - `limits.turn.steps` sets `MaxSteps` for an agent that sets none, and caps one that does.
- **Changed from the decision: the turn's version is not raised.** The turn branches only on `Agent.Limits`, recorded by `agent.load`, and it is empty in every turn from before. Such a turn makes the same calls it always made, and a turn under way keeps the `agent.load` it recorded. This is how `Agent.Context` (ADR 0020) and `Agent.Loopback` (ADR 0031) were added. A stopped turn's reply is a `flow.put` call, made only in turns that have limits.
- **`turn.time`:** it reads `ctx.Now()` only when it is set.
  - A step's calls being decided count as waiting: approvals, questions, and the checks around them.
  - The tools' run counts.
  - A subagent's own waits count for its parent.
- **What a turn does not see:**
  - A subagent's spending is in the ledger, and its parent sees it at its next turn.
  - Turns running side by side each read the ledger at their start, so together they can go past a window's limit by up to one turn each.
  - A CLI engine's turn is checked only before it starts.
  - Not every model call is preceded by a check. Not checked: compacting after a reply that was too long for the context, `decide` calls (auto mode's judge, hooks' decisions), and a flow's own turn (its agents' turns are checked). What they use is in the ledger, and the next check counts it.
  - Claude Code reports a cost under a subscription (Pro, Max) too, where it is a figure and not a charge. It is counted towards `cost` limits as reported.
- **Clients:**
  - ACP's `stopReason` for a limit is `max_turn_requests`, as for steps, and the reply says which limit.
  - `/usage` is a command of the ACP agent (the terminal UI and editors alike).
  - The terminal UI's status line shows the session's cost, and it warns once of a limit past `warn`, after a turn (a new warning is said again).
  - `GET /usage?session=`; `kairocode usage` without `-session` prints `/usage`, and with it, the session's rows and then its turns as before (`-json` keeps the turns' form).
- **Tests:**
  - `TestUsageLedger`.
  - `TestLimitSessionTokens`: the next turn, and another session.
  - `TestLimitDayCost`: two sessions; a project's price and looser limit not taken; the warning.
  - `TestLimitTurnTimeNotWaiting`: real time (a two-second wait for an approval, a two-second command), skipped with `-short`. The runtime's tests do not run on `kairotest`'s clock; the limit is checked against the recorded `ctx.Now`, so a slow machine only makes the command count more.
  - `TestLimitsProjectCannotLoosen`: steps too.
  - `TestLimitsRecordedAtTurnStart`: a limit raised while the turn waits does not change where it stops (invariant 2).
  - `TestPrices`: the table, dated ids, the user's price, a local server, Claude Code's cost, a model not known, and `/usage`'s text.
  - `TestWindows`.
  - The terminal UI's `TestUsageWarning`.
  - No crash test was added: where a turn stops is computed from recorded values only, which `TestLimitsRecordedAtTurnStart` checks.

## Alternatives considered

- **Add up windows from kairo's records** (as `Runtime.Usage` does for a session). Finished trees are deleted after 24 hours, so a week cannot be added up, and reading every session's runs at each turn's start is slow.
- **Rolling windows** (the last 5 hours, the last 7 days), as Claude Code's session limit does. With a rolling window, the time the limit is free again depends on when each earlier call was made. Calendar days and weeks reset at one time that is easy to show and to set a budget by.
- **Decide inside the `model` action** (as in 0024's alternatives). It would stop on a total read at that moment, not recorded, so a resumed turn could stop elsewhere.
- **Only show usage, with no limits.** The requirement (FR-26) and the user ask for both.
