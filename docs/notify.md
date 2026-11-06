# Hearing of approvals away from the machine

When a turn needs you (an approval, a question, or "did this step take effect?" about one that was cut off), KairoCode waits for as long as it takes, and lets go of the process while it waits. A terminal notification tells you if you are at the machine. To hear of it elsewhere (Slack, say), bind a flow to `approval.requested`: the daemon runs it once for each request, and the flow sends it where you want (ADR 0034).

## How it works

- `approval.requested` is a hook event, like `turn.end`: `{"hooks": {"approval.requested": ["notify"]}}` in `kairocode.json` runs `.kairocode/workflows/notify.star`.
- The daemon runs the flow once per request, whether or not a client is watching. A request that appears just before the daemon stops is told when it starts again; one already told is not told again.
- The flow runs apart from the turn. The turn goes on waiting for you, whatever the flow does; a flow that fails is logged and kept in `kairocode flow runs`, and nothing else fails with it.
- The hooks are those of the turn the request belongs to, as they were when it started. A change to `kairocode.json` applies from the next turn.
- **Nobody is asked to run it.** The flow runs only if you allowed it before: KairoCode asks about it at the start of a turn, as about the other hooks, and keeps the answer with the script's hash. Until then (or once the script changes), it is skipped. A rule `"permission": {"hook": "allow"}` allows every hook without asking.
- A request of the flow's own (one of its calls waiting for approval) is not told. It is listed in `kairocode approvals` with the others. Allow the calls the flow makes (below), so it never waits.

The flow is given the request as `input["request"]`:

| Field | What it is |
|---|---|
| `kind` | `approval`, `review` (a step cut off: did it take effect?) or `question` |
| `key` | What to answer: `kairocode approve <key>` (or `-deny`), or `kairocode answer <key> <option>` |
| `session` | The session (`s/…`) |
| `title` | The session's title (its first message, or `/flow <name>`) |
| `dir` | The working directory |
| `tool` | The tool the request is about (`bash`, `http`, ...; empty for a question) |
| `subject` | What the request shows: the command, the URL, the question |
| `rule` | Why it is asked (the rule, or "cut off while it ran") |
| `turn` | The turn it belongs to |

What leaves the machine is up to the flow: it is your script. The subject of a request is the command or the URL as the model wrote it.

## Slack

Send with Slack's Web API (`chat.postMessage`) and a bot token. The token stays out of KairoCode's record: `http.auth` adds it to the request from an environment variable, inside the daemon (ADR 0013).

1. Create a Slack app with the `chat:write` scope, install it in your workspace, and invite it to the channel. Put its bot token (`xoxb-…`) in `SLACK_BOT_TOKEN` in the environment the daemon starts in.
2. In your own `~/.config/kairocode/kairocode.json`:

   ```json
   {
     "http": {
       "auth": {"slack.com": {"header": "Authorization", "env": "SLACK_BOT_TOKEN", "prefix": "Bearer "}}
     },
     "permission": {
       "http": {"slack.com/api/chat.postMessage": "allow"}
     },
     "hooks": {"approval.requested": ["notify"]}
   }
   ```

   The `permission` rule (an `http` call is judged by its host and path) lets the flow post without asking. A hook in your own settings runs a flow of the project you work in: put the flow in each project, or bind it in the project's `kairocode.json` instead.

3. The flow, `.kairocode/workflows/notify.star` (change the channel):

   ```python
   DESCRIPTION = "Tell Slack when KairoCode needs me"

   CHANNEL = "C0123456789"

   WHAT = {
       "approval": "asks for your approval",
       "review": "asks whether a step took effect",
       "question": "has a question",
   }

   def quote(s):
       for a, b in [("\\", "\\\\"), ('"', '\\"'), ("\n", "\\n"), ("\r", "\\r"), ("\t", "\\t")]:
           s = s.replace(a, b)
       return '"' + s + '"'

   def main(input):
       r = input["request"]
       answer = "kairocode approve " + r["key"]
       if r["kind"] == "question":
           answer = "kairocode answer " + r["key"] + " <option>"
       text = "KairoCode " + WHAT.get(r["kind"], "needs you") + " in *" + r["title"] + "*\n```" + r["subject"] + "```\n" + answer
       res = tool("http", url = "https://slack.com/api/chat.postMessage",
                  headers = {"Content-Type": "application/json; charset=utf-8"},
                  body = '{"channel": ' + quote(CHANNEL) + ', "text": ' + quote(text) + '}')
       if not res.ok or '"ok":true' not in res.output:
           return {"ok": False, "message": res.error or res.output}
       return "told Slack"
   ```

4. Start a turn. KairoCode asks once whether to run the `notify` hook; allow it. From then on, each request is posted.

Slack's incoming webhooks are not supported: their URL is itself the secret, and a flow's calls (with their URLs) are recorded. A plugin's MCP tool (ADR 0025) can send too, with credentials kept in the OS credential store.

## Not covered

- Answering from Slack. Answer with `kairocode approve`, `kairocode answer`, the terminal UI or an editor.
- A flow cut off while it posts: `http` is a real call, so it is not sent again by itself. It waits for review in `kairocode approvals` like any other cut-off step, and that review is not told (it is the flow's own).
