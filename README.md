# KairoCode

A coding agent that picks up where it left off after a crash, never runs a side effect twice, and can wait hours for your approval without holding a process.

KairoCode runs each session as a workflow on [kairo](https://github.com/r-hashi01/kairo), a durable workflow runtime. It is the same kind of tool as Claude Code or opencode, with these guarantees:

- **It survives crashes.** `kill -9` the process and the next one continues from where it stopped. Model calls and tool runs that already finished are not repeated.
- **Side effects happen once.** If the outcome of a real effect — a `git push`, an HTTP POST — is unknown, KairoCode does not retry it on its own; it asks you whether it took effect. File writes are checked against content hashes before going on.
- **It can wait as long as it takes.** While waiting for an approval it lets go of the process. You can answer later, from the terminal, an editor or the CLI.
- **The same rules on any engine.** Besides the Claude API, it can drive your own Claude Code, Codex or GitHub Copilot CLI, or any ACP agent. Permission rules, the sandbox and the record stay KairoCode's.
- **Procedures become durable flows.** Write a fixed procedure as a Starlark flow (`.kairocode/workflows/*.star`), or have one written from a description; look at its chart, try it, then use it. A flow that crashes midway does not redo the steps it finished.

> This is the first release (v0.1). Commands and settings may still change.

## Platforms

- macOS (uses `sandbox-exec`; nothing else to install)
- Linux (needs `bwrap` from [bubblewrap](https://github.com/containers/bubblewrap), and user namespaces)
- Windows is not supported (there is no sandbox for it).

## Install

| How | Command |
|---|---|
| Homebrew (macOS, Linux) | `brew install r-hashi01/tap/kairocode` |
| Go | `go install github.com/r-hashi01/kairocode/cmd/kairocode@latest` (does not create the `kairo` link) |
| Binaries | Download the tar.gz for your OS and CPU from [Releases](https://github.com/r-hashi01/kairocode/releases) and put `kairocode` and `kairo` on your `PATH` |
| Source | `git clone https://github.com/r-hashi01/kairocode && cd kairocode && make build` (Go 1.27.2 or later; builds `bin/kairocode` and `bin/kairo`) |

`kairo` is the same as `kairocode chat`: it opens the terminal UI. `kairocode version` prints the version.

The macOS binaries are not notarized by Apple. If one you downloaded with a browser is blocked, run `xattr -d com.apple.quarantine kairocode kairo` (binaries installed with Homebrew or `curl` run as they are).

## Getting started

Pick an engine for the model.

| Engine | What it uses | How to choose it |
|---|---|---|
| `anthropic` (default) | The Claude API, with whatever credentials Anthropic's SDK reads (`ANTHROPIC_API_KEY`, ...) | Nothing to do |
| `claude-code` | Your own Claude Code (signed in with your subscription) | `/provider`, or `KAIROCODE_ENGINE=claude-code` |
| `codex` | Your own Codex CLI | Same |
| `copilot` | Your own GitHub Copilot CLI | Same |

`/provider` in the chat can also install an agent from the ACP registry and use it. Choose the model with `/model` (or `KAIROCODE_MODEL`).

## Usage

```sh
kairo                                # the terminal UI; asks whether to trust a folder the first time
kairocode run "fix the tests"        # do one task without a conversation, then exit
kairocode run -detach "…"            # stop where a person must approve, and exit (exit code 3)
kairocode resume <session>           # go on from there
kairocode approvals <session>        # the approvals waiting
kairocode approve [-deny] <key>      # answer an approval (from any terminal)
kairocode answer <key> <answer>      # answer a flow's question
kairocode log <session>              # the session's record: turns, model input and output, tools, waits, usage
kairocode sessions                   # the sessions of this folder
kairocode fork [-files] <session> [turn]   # go on from a past turn, as another session
kairocode plugin install <name>      # install a plugin (MCP servers and skills); connect gives it credentials
kairocode usage -session <session>   # the tokens the model calls used
```

`kairocode help` lists every command. One daemon runs the sessions of each data directory (`~/.local/share/kairocode` by default). Commands start it as needed, and it stops after 10 minutes without use.

### In the terminal UI

- `/help`, `/model`, `/provider`, `/effort`, `/login`
- `/undo`, `/redo`: put the working files back as they were before the last turn, and back again (the record is kept)
- `/compact`: summarize the conversation and go on from the summary (also done on its own near the context limit)
- `/fork`: go on from a past turn, as another session
- `/flow`: flows (see below)
- `/plugin`, `/skills`, `/mcp`: plugins, skills, MCP servers
- `/log`, `/resume`, `/review`
- Type a space after a command's name to see what it takes (`/plugin install <name>`, `/flow new`, your flows' names, `/model`'s models, ...). Tab or ↑↓ picks one; Enter sends what you typed.
- Shift+Tab switches the approval mode: manual, auto, plan. The input's `>` takes the mode's color.
- Esc stops the current turn. Ctrl-C twice leaves.
- `@path` attaches a file. Shift+Enter adds a new line.
- Images: Ctrl+V pastes the clipboard's image (Cmd+V too, when the clipboard holds only an image), or drop an image file on the terminal. It shows as `[Image #1]` in the input and is sent with the message. On Linux this needs `wl-paste` or `xclip`.
- A large paste (over 10 lines or 1,000 characters) is folded in the input as `[Pasted text #1 +42 lines]` and sent in full.
- Messages typed during a turn are sent when it ends.
- Long commands (tests, builds, servers) can run in the background: the model reads their output or waits for them later. They keep running when the daemon is replaced.
- Notifications: when the terminal is not focused and KairoCode waits for an approval or a question, or a turn ends, it tells you with a terminal notification (iTerm2, WezTerm, Ghostty, kitty) or the bell. Set `"notify"` in `~/.config/kairocode/kairocode.json` to `"unfocused"` (default), `"always"` or `"never"`.
- If a session stops on a storage error, a new session goes on from its last finished turn (the old session's record is kept).

### From an editor (ACP)

`kairocode acp` is an [ACP](https://agentclientprotocol.com) agent over standard input and output. In Zed or another editor that speaks ACP, set the command to `kairocode` and the arguments to `acp`.

## Flows

Write a fixed procedure in Starlark, in `.kairocode/workflows/<name>.star`.

```python
DESCRIPTION = "Review the changes, then summarize them"

def main(input):
    phase("review")
    r = agent(agent = "plan", prompt = "Review the current diff: " + input["args"])
    if not r.ok:
        return {"ok": False, "message": r.error}
    return {"ok": True, "message": r.output}
```

- What a flow can call: `agent` (a subagent), `parallel`, `tool`, `ask` (ask a person), `decide` (a typed decision), `phase`.
- Run one with `/flow <name> <args>`. `/flow new <description>` has one written from your words; you check its chart and try it before it is saved.
- In the terminal UI, `/flow` alone opens the list of flows. Pick one to see how it is used and its chart, then run it, change it on its chart (`e`), or look at its past runs (`r`). The last row, "make a new flow", starts `/flow new`.
- `kairocode flow graph <name>` draws a flow, `kairocode flow show -follow <session>` shows one as it runs, and `kairocode flow runs` lists past runs.
- If it crashes midway, the `agent` and `tool` calls that finished are not run again.

## Permissions and the sandbox

Write rules per tool in `kairocode.json` (yours in `~/.config/kairocode/kairocode.json`, the project's at the repository's root). The format is opencode's; a later rule wins.

```json
{
  "permission": {
    "bash": { "*": "ask", "git status*": "allow", "rm -rf*": "deny" },
    "edit": "allow"
  }
}
```

Commands KairoCode runs run in the OS sandbox. They can write only the working directory and the temporary directory, and even there `.git`, `.kairocode`, `.claude` and the like are protected. There is no network. Running a command outside the sandbox asks you every time by default.

For commands that talk to a local server, such as tests, the model can pick a local-only sandbox (`loopback`). It reaches ports on this machine, but not other machines, and not KairoCode's own approval endpoint. If you would rather decide each time, set `"sandbox": {"loopback": "ask"}`, or `"deny"`.

A project's settings (permissions, hooks, MCP servers) are not read until you say you trust its folder. Hooks (commands) in Claude Code's settings ask before their first run whether to run inside the sandbox, outside it, or not at all. `"agents": {"claudeHooks": "off"}` stops reading them.

### What `kairocode.json` sets

| Key | Meaning | Where |
|---|---|---|
| `permission` | Rules per tool (above) | Yours, the project's |
| `sandbox.loopback` | The local-only sandbox: `allow` (default), `ask` or `deny` | Yours (a project can only make it stricter) |
| `hooks` | Flows run at points such as the end of a turn | Yours, the project's |
| `mcp`, `tools` | MCP servers, and your own tools written as commands | Yours, the project's |
| `lsp` | Language servers that check the files written (`false` turns them off) | Yours, the project's |
| `agents.claudeHooks` | Claude Code's hooks: `ask` (default) or `off` | Yours (a project can only turn them `off`) |
| `http` | Credentials for the `http` and `fetch` tools (`auth`), and whether local addresses are allowed (`allowPrivate`) | Yours |
| `models.<name>.context` | A model's context length (when summarizing starts) | Yours |
| `flowHistory` | How many flow runs to keep (`keep`) and for how many days (`days`) | Yours |
| `notify`, `language` | How to notify, and the language shown | Yours |

## Development

How the work is done is in [AGENTS.md](AGENTS.md), design decisions in [docs/adr](docs/adr/README.md), and progress in [docs/milestones.md](docs/milestones.md) (these are in Japanese).

```sh
scripts/check.sh            # gofmt, vet, build, all tests (-race, including the crash tests), invariants, ADRs
scripts/check.sh --quick    # the light version
```

## License

[MIT](LICENSE). What is in `third_party/` follows its own license: the macOS sandbox policy in `third_party/codex-sandbox` comes from OpenAI Codex, under the Apache License 2.0 ([LICENSE](third_party/codex-sandbox/LICENSE), [NOTICE](third_party/codex-sandbox/NOTICE)).
