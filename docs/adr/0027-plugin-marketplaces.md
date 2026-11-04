# 0027 Plugin marketplaces: a bundled catalog, and catalogs you can add

- Status: Accepted (2026-10-26)
- Date: 2026-10-26
- Related: ADR 0025 (plugins), ADR 0010 (the terminal UI), ADR 0002 (ACP)

## Context

ADR 0025 made plugins installable with `kairocode plugin install <git URL or local path>`. But there is no list anywhere of what can be installed, and a user who does not know a URL does not know what to install. With nothing installed, `/plugin` only says "you can install with install".

Codex (checked with 0.163.0) and Claude Code both have the user install through a "marketplace".
- A marketplace is a catalog (`marketplace.json`) listing plugin names and where they are.
- One bundled with the product is there from the start and can be chosen without any setup. Codex's `openai-bundled` is one.
- The user can add a git repository or a local folder as a catalog (`codex plugin marketplace add`).
- Installing is done with `codex plugin add <name>@<catalog>`, or from `/plugins` on screen.
- Each row of a catalog has a timing for authentication (at install / at first use).

Also, ADR 0025 decided `/plugin install` and `remove` inside the conversation, but only the CLI was implemented.

## Decision

1. **Catalog format (`kairocode-marketplace.json`):** placed at the top level of a git repository or folder.
   ```json
   {
     "name": "acme",
     "description": "Acme plugins",
     "plugins": [
       {"name": "tracker", "description": "Acme issue tracking", "source": "./plugins/tracker", "connect": "install"},
       {"name": "docs", "source": "https://github.com/acme/docs-plugin.git"}
     ]
   }
   ```
   - If `source` starts with `./`, it is a folder inside the catalog. Anything outside the catalog (`..`, a symbolic link pointing outside) is refused. Otherwise it is a git URL.
   - `connect` is `install` (go straight to connect after installing) or `use` (guide the user at first use; the default).
   - Catalog and plugin names must be usable as folder names (no `/` or `\`, not starting with `.`).
2. **The bundled catalog `kairocode`:** embedded in the binary (`internal/runtime/catalog`, `go:embed`). At first it has these 2. We checked the manifests against the real servers.
   - `github`: the GitHub MCP server (key: the value of `gh auth token`, or a personal access token).
   - `exa`: Exa web search (key).
3. **Adding catalogs:** `kairocode plugin marketplace add|list|update|remove`.
   - `add <git URL, owner/repo, local folder>`. A local folder is read in place (for trying it while building it). A git one is cloned into `marketplaces/<name>` in the user's config directory and pinned to that commit. The list of added catalogs is `marketplaces.json`.
   - `update <name>` moves a git one to the latest commit. The bundled one changes with the binary.
   - The name `kairocode` is the bundled catalog's name, so it cannot be added.
4. **Installing:** `kairocode plugin install <name>` or `<name>@<catalog>`.
   - With a name only, catalogs are searched in order: bundled, then in the order added. If the name is in 2 or more, the user is asked to choose with `@`.
   - A plugin inside a catalog is copied into the user's plugin directory. Where it came from (catalog and name) is kept in `.kairocode-source`. Those with a git URL are cloned as before.
   - git URLs and local paths are still accepted as before. If a local folder is not a git repository, it is copied (before, it tried to clone and failed).
   - If `connect` is `install`, the plugin needs authentication, and it was run from a terminal, it goes on to connect. Otherwise it points the user to `kairocode plugin connect <name>`.
   - `update <name>`, for one installed from a catalog, re-reads the catalog (moving a git one to the latest first) and replaces it.
5. **Screen:** `/plugin` and `kairocode plugin list` show what is installed, followed by what is not yet installed, per catalog (name, description, kind of authentication).
   - Add `/plugin install`, `update`, `remove` and `marketplace` inside the conversation (missing from ADR 0025's implementation). The ACP agent does them directly (no secrets pass through).
   - `/plugin install` on the `kairo` screen hands the terminal to `kairocode plugin install`, as `connect` does (so it can go straight to connect after installing).
6. **Remote catalogs (hosted on a server) are not decided in this ADR.** They need a server to host them, so they are decided separately when needed.

Where this shows up in the implementation: `internal/runtime/marketplace.go` (reading and writing catalogs, installing, replacing, removing), `internal/runtime/catalog/` (the bundled catalog), `internal/cli/plugin.go`, `internal/acp/agent.go` (the `/plugin` subcommands), `internal/tui` (`/plugin install` in the terminal).

Decided during implementation (the intent of the decision is unchanged):
- **Version fingerprint:** for a plugin installed by copying, the first 12 characters of the hash of its files (paths and contents) are written to `.kairocode-commit`. Cloned ones use the commit as before. This goes into the server's fingerprint (ADR 0025's `Spec`), so even if the content changes without a version bump, a turn in progress does not call the changed server.
- **The markers are KairoCode's:** `.kairocode-source` and `.kairocode-commit` brought in by a plugin are not used; they are rewritten at install. Symbolic links inside a plugin are not copied.
- **A catalog's `source`:** anything not starting with `./` must be a git URL: `https://`, `http://`, `ssh://`, `git://` or `git@`. A catalog cannot point at a folder on the user's machine.
- **Local paths for ACP's `/plugin install` and `marketplace add`:** only absolute paths are accepted (a relative path would resolve against the ACP agent process's location, not the session's folder).
- **Credentials in URLs:** for URLs like `https://user:token@…`, `user:token@` is dropped in what is shown (`From`, `marketplace list`). It is stored as is (git uses it).
- Tests: `TestCatalog`, `TestMarketplaceInstall`, `TestMarketplaceGit`, `TestCopiedPluginIsAtItsFiles`, `TestUpdateFromAMarketplaceGoneBad`, `TestPluginChange` (ACP), `TestPluginCommand` (the terminal UI).

Addendum (2026-10-26, approved by the user): **Claude Code catalogs can also be added.**
- Context: the bundled catalog has only 2 entries, and the `/plugin` screen is nearly empty. KairoCode can already read Claude Code plugins (`.claude-plugin/plugin.json`, with its `.mcp.json` and skills). So if Claude Code catalogs (`.claude-plugin/marketplace.json`; for example `anthropics/claude-plugins-official`, 315 entries) can be added, the content grows all at once. The option rejected under "Alternatives considered" below is taken in, on the grounds that the plugin side can already be read.
- **How it is read:** if a repository or folder added with `marketplace add` has no `kairocode-marketplace.json` but has `.claude-plugin/marketplace.json`, that is read. Each row's `source` is read as follows.
  - `./<path>`: a folder inside the catalog (as before).
  - `{"source": "github", "repo": "owner/repo"}`, `{"source": "url", "url": …}`: a git URL.
  - `{"source": "git-subdir", "url": …, "path": …}`: a folder inside a git repository. It is cloned, and then only that folder is copied (a `path` pointing outside the repository is refused).
  - In all of these, if there is `ref` (a branch or tag) or `sha` (a commit), it is pinned there.
  - Anything else (npm and so on) is shown in the list as "cannot be installed" with a reason, and is not installed.
- `path`, `ref` and `sha` are likewise added to KairoCode's catalog format (when `source` is a git URL).
- **Rows with `strict: false`:** for a row where the plugin has no `plugin.json` and the catalog row defines the plugin (for example `skills: ["./a", "./b"]`), at install `.claude-plugin/plugin.json` is written from that row and the listed skill folders are laid out under `skills/<name>`. It only rearranges them into a form KairoCode can read; the content is not changed.
- **What is and is not used:** of a Claude Code plugin, KairoCode uses only skills and the MCP server in `.mcp.json` (at most 1). Commands, subagents, hooks, LSP and output styles are not used. The `/plugin` details and list show "what KairoCode does not use". MCP tools do not declare effect types, so as "real" they ask for approval every time (invariant 3, same as ADR 0025).
- **Links are not followed:** a git one is cloned and then `path` (or the whole thing if there is none) is taken out by copying. Links and `.git` are not brought along (the version is that repository's commit). Rearranging also does not write to or move anything through a link.
- **A Claude Code `plugin.json` does not decide effect types:** even if it has KairoCode's fields (`tools`, `mcp`, `auth`), they are not used. The MCP server is only the one in `.mcp.json`, and its tools are "real".
- For testing, `file://` is accepted as a catalog's git URL only inside tests.
- Tests: `TestClaudeMarketplace`, `TestClaudePluginStaysInItsFolder`, `TestPluginsRPC` (ACP), `TestPluginsScreen` and `TestPluginsScreenUnsupported` (the terminal's `/plugin` screen).

## Consequences

- Without any setup, the user can see in `/plugin` what can be installed, and install it with `/plugin install github`. Catalogs of a company or of peers can also be added.
- What is installed from a catalog is pinned to its content at that time, and does not change until replaced with `update`. A turn in progress continues with the version from its start, as in ADR 0025.
- Tests
  - All manifests in the bundled catalog can be read and declare effect types (`TestCatalog`).
  - Add a local catalog, install by name only and with `@`, have the user choose for a name that is in 2 catalogs, replace with `update`, delete with `remove`. A `source` pointing outside the catalog is refused (`TestMarketplaceInstall`).
  - Add a git catalog, pin it, and move it forward with `update`.
  - A local folder that is not git can be installed as is.
  - `/plugin` shows what is not yet installed, per catalog.

## Alternatives considered

- **Only a bundled list (catalogs cannot be added):** the smallest implementation. But there is no way to distribute a company's or peers' plugins, and in the end people pass URLs around.
- **Read the Codex and Claude Code catalog formats (`.agents/plugins/marketplace.json`, `.claude-plugin/marketplace.json`) as they are:** the format of the plugins inside (MCP settings, effect type declarations) differs, so even if they can be read they do not work as is. To match ADR 0025, which made the plugin format `kairocode-plugin.json`, the catalog also gets its own format.
- **Remote catalogs from the start:** hosting the official catalog on a server. It needs a place to host it and operations, so for now the bundled one is enough.
