package acp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/plugins"
)

// builtinCommands are the slash commands KairoCode itself has.
var builtinCommands = []map[string]any{
	{"name": "undo", "description": "Put the working files back as they were before the last turn"},
	{"name": "redo", "description": "Put back what the last undo took away"},
	{"name": "compact", "description": "Summarize the conversation and go on from the summary (the earlier messages are kept)"},
	{"name": "fork", "description": "Start another session from a turn of this one (the last, unless named); --files puts the files back as they were after it", "input": map[string]any{"hint": "[turn] [--files]"},
		"_meta": options(CommandOption{Name: "--files", Description: "put the files back as they were after the turn"})},
	{"name": "flow", "description": "Run a flow of .kairocode/workflows (/flow alone lists them; /flow new, a new one; /flow new --from-session, one that repeats this session; /flow runs, the runs to look back at)", "input": map[string]any{"hint": "<name> [what to give it] | new <what it does> | new --from-session [what to make of it] | runs | <name> --help"},
		"_meta": options(
			CommandOption{Name: "new", Hint: "<what it does>", Description: "have a new flow written: a draft, drawn, then tried"},
			CommandOption{Name: "runs", Description: "the flows' runs, to look back at"})},
	{"name": "provider", "description": "Switch who runs the model from the next turn: anthropic, claude-code, codex, copilot, oai:<name> (/provider alone lists them)", "input": map[string]any{"hint": "[provider]"}},
	{"name": "model", "description": "Switch the model from the next turn (/model alone lists the provider's)", "input": map[string]any{"hint": "[model]"}},
	{"name": "effort", "description": "Switch how hard the model thinks, where the provider has it (/effort alone lists them)", "input": map[string]any{"hint": "[effort]"}},
	{"name": "login", "description": "Sign in to the provider (/login alone lists the ways)", "input": map[string]any{"hint": "[provider] [way]"}},
	{"name": "skills", "description": "The skills here (/<skill> runs one), and SKILL.md files not taken, and why"},
	{"name": "mcp", "description": "The MCP servers and custom tools here: how each runs, and their tools"},
	{"name": "plugin", "description": "The plugins here, their state, and those to install; install, connect, update or remove <name>", "input": map[string]any{"hint": "[list | install <name> | update <name> | remove <name> | connect <name> | disconnect <name> | marketplace …]"},
		"_meta": options(
			CommandOption{Name: "list", Description: "the plugins here, their state, and those to install"},
			CommandOption{Name: "install", Hint: "<name[@marketplace]>", Description: "install a plugin"},
			CommandOption{Name: "update", Hint: "<name>", Description: "update a plugin"},
			CommandOption{Name: "remove", Hint: "<name>", Description: "remove a plugin"},
			CommandOption{Name: "connect", Hint: "<name>", Description: "give a plugin its credentials (in a terminal)"},
			CommandOption{Name: "disconnect", Hint: "<name>", Description: "take a plugin's credentials away"},
			CommandOption{Name: "marketplace", Description: "the marketplaces plugins come from"},
			CommandOption{Name: "marketplace add", Hint: "<git URL, owner/repo or folder>", Description: "add a marketplace"},
			CommandOption{Name: "marketplace update", Hint: "<name>", Description: "update a marketplace"},
			CommandOption{Name: "marketplace remove", Hint: "<name>", Description: "remove a marketplace"})},
	{"name": "log", "description": "The session's record: each turn, model call, tool call and result, what waited, and what was used"},
}

// options is a command's _meta with the words it takes after its name,
// for KairoCode's terminal to offer as they are typed.
func options(o ...CommandOption) map[string]any {
	return map[string]any{OptionsMeta: o}
}

// PluginsView is what the terminal's /plugin shows.
type PluginsView struct {
	Installed    []api.PluginInfo    `json:"installed"`
	Catalog      []api.CatalogPlugin `json:"catalog"`
	Marketplaces []api.MarketInfo    `json:"marketplaces"`
	Errors       []string            `json:"errors,omitempty"`
}

// offerCommands tells the client the slash commands: KairoCode's, then the
// project's (opencode's commands/*.md).
func (a *agent) offerCommands(ctx context.Context, session string) {
	a.update(session, a.commandsUpdate(ctx, session))
}

// afterNew is what is sent once a new session's id is answered: its
// updates before would be of a session the client does not know yet (Zed
// drops them).
func (a *agent) afterNew(ctx context.Context, w *sessionWatch, session string) func() {
	update := a.commandsUpdate(ctx, session)
	return func() {
		a.update(session, update)
		w.release()
	}
}

// commandsUpdate is the session's commands, as available_commands_update.
func (a *agent) commandsUpdate(ctx context.Context, session string) map[string]any {
	all := append([]map[string]any(nil), builtinCommands...)
	cs, err := a.b.Commands(ctx, session)
	if err != nil {
		a.o.Logger.Warn("acp: the project's commands", "err", err)
	}
	for _, c := range cs {
		if slices.ContainsFunc(builtinCommands, func(b map[string]any) bool { return b["name"] == c.Name }) {
			continue // KairoCode's own
		}
		desc := c.Description
		if desc == "" {
			desc = "/" + c.Name
		}
		if c.Skill {
			desc = "(skill) " + desc
		}
		hint := c.Hint
		if hint == "" {
			hint = "arguments"
		}
		all = append(all, map[string]any{"name": c.Name, "description": desc, "input": map[string]any{"hint": hint}})
	}
	return map[string]any{"sessionUpdate": "available_commands_update", "availableCommands": all}
}

// loginCommand answers /login [provider] [way] in text, for a client
// without its own /login: the ways, or the agent's own way taken; a
// command is the user's to run in a terminal.
func (a *agent) loginCommand(ctx context.Context, session string, args []string) string {
	var provider, way string
	switch len(args) {
	case 0:
		st, err := a.b.Settings(ctx, session)
		if err != nil {
			return err.Error()
		}
		provider = st.Provider
	case 1:
		provider = args[0]
	default:
		provider, way = args[0], args[1]
	}
	ms, err := a.b.LoginMethods(ctx, provider)
	if err != nil {
		return "Could not sign in to " + provider + ": " + err.Error()
	}
	for _, m := range ms {
		if way == "" && len(ms) > 1 || way != "" && m.ID != way {
			continue
		}
		switch {
		case len(m.Command) > 0:
			return "Run this in a terminal to sign in to " + provider + ": " + strings.Join(m.Command, " ")
		case m.Agent:
			if err := a.b.Login(ctx, provider, m.ID, false); err != nil {
				return "Could not sign in to " + provider + ": " + err.Error()
			}
			return "Signed in to " + provider + "."
		}
		return m.Name + ": " + m.Description
	}
	if len(ms) == 0 {
		return provider + " has no way to sign in here."
	}
	lines := []string{"Ways to sign in to " + provider + " (/login " + provider + " <way>):"}
	for _, m := range ms {
		line := "- " + m.ID + ": " + m.Name
		if m.Description != "" {
			line += " — " + m.Description
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// settingsCommand answers /provider and /model: with a value, it switches
// the setting and tells the client the new ones; alone, it lists what
// there is, as the agent's message.
func (a *agent) settingsCommand(ctx context.Context, session, id, value string) (any, error) {
	var opts []map[string]any
	var err error
	if value != "" {
		opts, err = a.setConfig(ctx, session, id, value)
	} else {
		opts, err = a.configOptions(ctx, session)
	}
	if err != nil {
		var re *rpcError
		if errors.As(err, &re) { // a value there is not: said, not failed
			a.update(session, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock(re.Message)})
			return map[string]any{"stopReason": "end_turn"}, nil
		}
		return nil, err
	}
	a.update(session, map[string]any{"sessionUpdate": "config_option_update", "configOptions": opts})
	var b strings.Builder
	for _, o := range opts {
		if o["id"] != id {
			continue
		}
		cur := o["currentValue"]
		if value != "" {
			fmt.Fprintf(&b, "%s: %v (from the next turn)", o["name"], cur)
			break
		}
		fmt.Fprintf(&b, "%s:\n", o["name"])
		for _, v := range o["options"].([]map[string]any) {
			mark := "  "
			if v["value"] == cur {
				mark = "* "
			}
			line := fmt.Sprintf("%s%v", mark, v["value"])
			if d, ok := v["description"]; ok {
				line += fmt.Sprintf(" (%v)", d)
			}
			b.WriteString(line + "\n")
		}
		fmt.Fprintf(&b, "/%s <%s> switches from the next turn.", id, id)
	}
	a.update(session, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock(b.String())})
	return map[string]any{"stopReason": "end_turn"}, nil
}

// listFlows answers /flow alone: the flows there are, as the agent's
// message.
func (a *agent) listFlows(ctx context.Context, session string) (any, error) {
	fs, err := a.b.Flows(ctx, session)
	if err != nil {
		return nil, err
	}
	text := "There are no flows yet: they are .kairocode/workflows/<name>.star. /flow new <what it does> has one written (a draft, drawn, then tried)."
	if len(fs) > 0 {
		lines := []string{"Flows here. /flow <name> [what to give it] runs one; /flow <name> --help says what it takes and does; /flow runs, the runs to look back at; /flow new <what it does>, a new one; /flow new --from-session, one that repeats this session:"}
		for _, f := range fs {
			name := f.Name
			if f.Args != "" {
				name += " " + f.Args
			}
			if f.Draft {
				lines = append(lines, "- "+name+" (draft: /flow "+f.Name+" --draft tries it, --publish publishes it)"+describe(f.Description))
				continue
			}
			lines = append(lines, "- "+name+describe(f.Description))
		}
		text = strings.Join(lines, "\n")
	}
	a.update(session, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock(text)})
	return map[string]any{"stopReason": "end_turn"}, nil
}

func describe(d string) string {
	if d == "" {
		return ""
	}
	return ": " + d
}

// skillsText is /skills: each skill (/name to run it) and where it is, and
// the SKILL.md files not taken, and why.
func (a *agent) skillsText(ctx context.Context, session string) string {
	sk, err := a.b.Skills(ctx, session)
	if err != nil {
		return "Could not read the skills: " + err.Error()
	}
	var b strings.Builder
	if len(sk.Skills) == 0 {
		b.WriteString("No skills here. A skill is a SKILL.md in .kairocode/skills/<name>/ (or .claude/skills, .agents/skills, .opencode/skills).\n")
	} else {
		b.WriteString("Skills (/<name> runs one):\n")
		for _, s := range sk.Skills {
			line := "- /" + s.Name
			if s.Description != "" {
				line += " — " + s.Description
			}
			fmt.Fprintf(&b, "%s\n    %s\n", line, s.Path)
		}
	}
	if len(sk.Problems) > 0 {
		b.WriteString("\nNot taken:\n")
		for _, p := range sk.Problems {
			fmt.Fprintf(&b, "- %s: %s\n", p.Path, p.Why)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// pluginText is /plugin's answer. Connecting asks for credentials, which
// do not go through ACP (nor the record): kairo's terminal runs kairocode
// plugin connect itself; another client's user runs it in a terminal.
func (a *agent) pluginText(ctx context.Context, session string, args []string) string {
	if len(args) > 0 && (args[0] == "connect" || args[0] == "disconnect") {
		if len(args) != 2 {
			return "/plugin " + args[0] + " <name>"
		}
		return "Run kairocode plugin " + args[0] + " " + args[1] + " in a terminal, in this folder: credentials are asked for there, and kept in the system's key store."
	}
	if len(args) > 0 && args[0] != "list" {
		return pluginChange(ctx, args)
	}
	list, err := a.b.Plugins(ctx, session)
	if err != nil {
		return "The plugins could not be listed: " + err.Error()
	}
	text := plugins.PluginsText(list)
	if more := plugins.AvailableText("/plugin install"); more != "" {
		text += "\n" + more
	}
	return strings.TrimRight(text, "\n")
}

// pluginChange is /plugin install, update, remove and marketplace (ADR
// 0027): done here, as no credential goes through them.
func pluginChange(ctx context.Context, args []string) string {
	// A folder is named by its absolute path: a relative one would be
	// this process's, not the session's.
	if last := args[len(args)-1]; len(args) > 1 && !filepath.IsAbs(last) {
		if _, err := os.Stat(last); err == nil {
			return last + " is a folder here: give its absolute path."
		}
	}
	one := len(args) == 2
	said := plugins.Said{Prefix: "/plugin"}
	switch {
	case args[0] == "install" && one:
		got, err := plugins.InstallPlugin(ctx, args[1])
		if err != nil {
			return "Not installed: " + err.Error()
		}
		text := said.Installed(got)
		if got.Auth != "none" {
			text += " " + said.NeedsCredentials(got.Name)
		}
		return text
	case args[0] == "update" && one:
		before, after, err := plugins.UpdatePlugin(ctx, args[1])
		if err != nil {
			return "Not updated: " + err.Error()
		}
		return said.Updated(args[1], before, after)
	case args[0] == "remove" && one:
		if err := plugins.RemovePlugin(args[1]); err != nil {
			return "Not removed: " + err.Error()
		}
		return said.Removed(args[1])
	case args[0] == "marketplace" && (len(args) == 1 || args[1] == "list") && len(args) <= 2:
		return strings.TrimRight(plugins.MarketplacesText(), "\n")
	case args[0] == "marketplace" && len(args) == 3 && args[1] == "add":
		m, err := plugins.AddMarketplace(ctx, args[2])
		if err != nil {
			return "Not added: " + err.Error()
		}
		return said.MarketAdded(m)
	case args[0] == "marketplace" && len(args) == 3 && args[1] == "update":
		before, after, err := plugins.UpdateMarketplace(ctx, args[2])
		if err != nil {
			return "Not updated: " + err.Error()
		}
		return said.MarketUpdated(args[2], before, after)
	case args[0] == "marketplace" && len(args) == 3 && args[1] == "remove":
		if err := plugins.RemoveMarketplace(args[2]); err != nil {
			return "Not removed: " + err.Error()
		}
		return said.MarketRemoved(args[2])
	}
	return "/plugin [list | install <name[@marketplace]> | update <name> | remove <name> | connect <name> | disconnect <name> | marketplace [list | add <git URL, owner/repo or folder> | update <name> | remove <name>]]"
}

// mcpText is /mcp's answer: each MCP server, custom tool and server of
// codex's own, and how it runs, with its tools and their effects.
func (a *agent) mcpText(ctx context.Context, session string) string {
	list, err := a.b.MCP(ctx, session)
	if err != nil {
		return "The MCP servers could not be listed: " + err.Error()
	}
	if len(list) == 0 {
		return "No MCP server or custom tool is configured here. Add one to kairocode.json's \"mcp\" (or \"tools\" for a command), or Claude Code's .mcp.json."
	}
	var b strings.Builder
	head := map[string]string{"mcp": "MCP servers", "tool": "Custom tools", "codex": "Codex's own MCP servers"}
	last := ""
	for _, m := range list {
		if m.Kind != last {
			if last != "" {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "%s:\n", head[m.Kind])
			last = m.Kind
		}
		whose := "yours"
		if m.Project {
			whose = "the project's"
		}
		if m.Kind == "codex" {
			whose = m.Source
		}
		fmt.Fprintf(&b, "- %s (%s): %s\n", m.Name, whose, m.State)
		for _, t := range m.Tools {
			fmt.Fprintf(&b, "    %s  %s\n", t.Name, t.Effect)
		}
	}
	b.WriteString("\nA tool declared read may run again after an interruption; any other is real, and asked about if cut off. Declare effects in kairocode.json's \"tools\".")
	return b.String()
}

// flowRunsText is /flow runs' answer for a client with no screen for it:
// the folder's flows' runs, the latest first (ADR 0029).
func (a *agent) flowRunsText(ctx context.Context, session string) string {
	runs, err := a.b.FlowRuns(ctx, a.dirOf(session), "", "", 30)
	if err != nil {
		return "The flows' runs could not be listed: " + err.Error()
	}
	if len(runs) == 0 {
		return "No flow has run here (or none is kept)."
	}
	var b strings.Builder
	for _, f := range runs {
		fmt.Fprintf(&b, "%s\n    kairocode flow show %s\n", f.Line(), f.ID)
	}
	return strings.TrimRight(b.String(), "\n")
}
