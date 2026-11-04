# Example: search the web with Exa's MCP server

KairoCode has no built-in web search tool. Search is left to MCP servers (opencode's `websearch` also uses an MCP server such as Exa). Here are 3 ways to use Exa's remote MCP server (`https://mcp.exa.ai/mcp`).

The tools of Exa's MCP server (as of 2026-10; <https://exa.ai/docs/reference/exa-mcp>):

| Tool | Default | What it does |
|---|---|---|
| `web_search_exa` | enabled | web search |
| `web_fetch_exa` | enabled | reads the contents of a URL |
| `web_search_advanced_exa` | enable with `?tools=` | search with filters |
| `agent_run` | only with an API key or OAuth | multi-step research (Exa charges for usage) |

Inside KairoCode, a tool's name is prefixed with the server's (or plugin's) name, like `exa_web_search_exa`.

## 1. Try it without a key (`kairocode.json`)

Exa can be used without a key, with a limit on the number of calls. Write this in the user's `~/.config/kairocode/kairocode.json` (ADR 0022).

```json
{
  "mcp": {
    "exa": { "type": "remote", "url": "https://mcp.exa.ai/mcp" }
  },
  "tools": {
    "exa_web_search_exa": { "effect": "read" },
    "exa_web_fetch_exa": { "effect": "read" }
  },
  "permission": {
    "exa_*": "allow"
  }
}
```

- `effect: "read"` in `tools` declares "has no effect outside". A tool without a declaration is treated as real, and if it crashes partway, a person is asked "was it run?" (invariants 3 and 4). Searching and reading do no harm however many times they run, so write `read`.
- `permission` skips the confirmation on every call. If you want to confirm, leave it out or set it to `"ask"`.
- With `/mcp`, you can check whether the server is connected, and its tools and their effect types.

An MCP server written in a project's `kairocode.json` asks how to run it on the first turn (so that a project cannot connect outside on its own).

## 2. Use it with an API key (plugin)

Do not write the key in `kairocode.json`; install it as a plugin and leave the key in the OS's credential store (ADR 0025). exa is in the catalog bundled with KairoCode (ADR 0027), so it installs by name alone. Its contents are the same as the example [`plugins/exa`](plugins/exa/kairocode-plugin.json).

```sh
kairocode plugin install exa   # installs it, then asks for the API key (the input is not shown)
kairocode plugin               # shows exa: connected, and the tools' effect types
```

In a conversation, it is `/plugin install exa`. To install from the example folder, it is `kairocode plugin install docs/examples/plugins/exa` (a folder that is not a git repository is copied as is).

- The stored key goes into the manifest's `"x-api-key": "{secret:api_key}"`. The key does not stay in records, logs, or the text of errors.
- This plugin also includes a skill that describes how to search (`skills/web-search/SKILL.md`).
- If the key becomes invalid, `/plugin` shows `refused:` and the server's answer as is, and the model is told that `kairocode plugin connect exa` is needed.
- Your own plugin can be installed with `kairocode plugin install <git URL>` if you put it in a git repository (it is pinned to a commit, and updated with `kairocode plugin update <name>`). To distribute several together, make a catalog with a `kairocode-marketplace.json`, and have people add it with `kairocode plugin marketplace add <git URL>` (ADR 0027).
- Permission rules can be written as `"exa_*": "allow"` in `permission` of `kairocode.json`, the same as for other tools.

## 3. Sign in with OAuth (plugin)

Exa also supports signing in with OAuth (`?login`; usage is charged to the Exa team). The example is [`plugins/exa-oauth`](plugins/exa-oauth/kairocode-plugin.json).

```sh
kairocode plugin install docs/examples/plugins/exa-oauth
kairocode plugin connect exa   # a browser opens; after you sign in, you return to the terminal
```

The token is kept in the store, and the daemon refreshes it before it expires.

From inside the TUI (`kairo`), `/plugin connect exa` does the same.
