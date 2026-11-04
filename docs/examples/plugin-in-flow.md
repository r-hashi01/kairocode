# Example: call a plugin's tool from a flow (GitHub)

A plugin's (ADR 0025) tools can be called from a flow's (ADR 0003) `tool(...)` with the same name as in a conversation (`<plugin>_<tool>`). Agents run by the flow's `agent(...)` can use the same tools too.

## 1. Install the plugin

```sh
kairocode plugin install github   # then it asks for a token (the value of gh auth token)
```

## 2. Permission rules

The same permission rules as in a conversation apply to calls inside a flow. If you write nothing, every tool asks before it is called. To let the read-only ones through without asking, write this in `~/.config/kairocode/kairocode.json`.

```json
{
  "permission": {
    "github_list_*": "allow",
    "github_get_*": "allow",
    "github_search_*": "allow",
    "github_issue_read": "allow",
    "github_pull_request_read": "allow"
  }
}
```

This is the same set as the tools that the bundled github plugin declares as "read-only". The other tools (the ones that write, such as `github_add_issue_comment`) ask, following the default rule (`"*": "ask"`).

## 3. The flow

`.kairocode/workflows/triage.star`:

```python
DESCRIPTION = "Read the open issues, choose the one to fix first, and comment the plan on it"

def main(input):
    repo = input["args"] or "r-hashi01/kairocode"
    owner, name = repo.split("/")

    phase("read")
    issues = tool("github_list_issues", owner = owner, repo = name, state = "OPEN")
    if not issues.ok:
        return {"ok": False, "message": issues.error}

    phase("choose")
    pick = agent(agent = "plan", prompt = "From the following issues, choose the one to fix first, and write its number and the reason:\n" + issues.output)

    phase("comment")
    number = decide("What is the number of the chosen issue?\n" + pick.output, type = "number")
    if not number.ok:
        return {"ok": False, "message": number.error}
    done = tool("github_add_issue_comment", owner = owner, repo = name,
                issue_number = number.value, body = pick.output)
    return {"ok": done.ok, "message": done.output or done.error}
```

```sh
kairocode run "/flow triage r-hashi01/kairocode"   # or, in a conversation, /flow triage r-hashi01/kairocode
```

- `github_list_issues` only reads, so it runs without asking, as the rules say.
- `github_add_issue_comment` writes, so it asks for approval. While waiting for approval, it releases the process, and even if it crashes, it does not comment again (when the outcome is unknown, it asks a person "was it done?").
- The argument names follow that tool's schema. You can check the tool names in the details of `/plugin`, and the arguments by asking the model in a conversation.

## 4. Make it from words

If you ask `/flow new choose the issue to fix first from the open issues and comment on it`, the model writes a flow draft (`.kairocode/workflows/drafts/`) using the installed plugins' tools (ADR 0030). Try the draft with `/flow triage --draft`, and publish it with `/flow triage --publish` (a review appears then). A try (ADR 0017) does not start MCP servers or plugins. It calls none of their tools, neither the ones that read nor the ones that write; it puts "what it would have called them with" in the try's result, and the flow moves on (that call returns as a success, and `.output` is a sentence saying "not called because this is a try").
