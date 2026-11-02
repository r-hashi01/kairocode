package runtime

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/r-hashi01/kairocode/internal/atomicfile"
	"github.com/r-hashi01/kairocode/internal/config"
	"github.com/r-hashi01/kairocode/internal/opencode"
)

// Agents' own hooks (ADR 0018). KairoCode runs Claude Code's hooks in its
// sandbox on every engine, and keeps each agent's own hooks off, since they
// would run outside it with no one asked: Claude Code's by
// --setting-sources "", codex's by features.hooks = false, copilot's by
// disableAllHooks in its settings (copilot has no flag for one run). The
// user's kairocode.json can release this:
//
//	{"agents": {"ownHooks": true}}
//
// codex and copilot then run their own hooks as they would by themselves.
// Only the user's kairocode.json says so: a project's cannot turn on hooks
// that run outside the sandbox.

// copilotMark, in copilot's settings directory, says KairoCode wrote its
// disableAllHooks, so that releasing takes back only what KairoCode wrote.
const copilotMark = "kairocode-disabled-hooks"

// ownHooks is the user's agents.ownHooks.
func ownHooks() (bool, error) {
	path := config.UserFile()
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var cfg struct {
		Agents struct {
			OwnHooks bool `json:"ownHooks"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return cfg.Agents.OwnHooks, nil
}

// copilotHooks turns copilot's own hooks off (own false), or takes back
// KairoCode's turning them off (own true). Other settings are kept. env is
// the registry's for copilot: a COPILOT_HOME of its is where copilot reads.
//
// Another process writing copilot's settings at the same moment (copilot
// itself, the user) can lose its change: the file has no lock to share.
func (r *Runtime) copilotHooks(own bool, env map[string]string) error {
	home := r.cfg.CopilotHome
	if home == "" {
		return nil // tests (NoCLIs): the user's copilot is not run
	}
	if h := env["COPILOT_HOME"]; h != "" && filepath.IsAbs(h) {
		home = h
	}
	r.copilotHooksMu.Lock()
	defer r.copilotHooksMu.Unlock()
	path := filepath.Join(home, "settings.json")
	// A link (a dotfiles manager's) is kept: its target is what is written.
	if p, err := filepath.EvalSymlinks(path); err == nil {
		path = p
	}
	mark := filepath.Join(home, copilotMark)
	_, err := os.Stat(mark)
	marked := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if own && !marked {
		return nil
	}
	settings := map[string]json.RawMessage{}
	perm := fs.FileMode(0o600)
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(b, &settings); err != nil {
			if own {
				// Nothing of KairoCode's can be found in it to take back.
				r.logf("copilot's %s is not JSON: KairoCode's disableAllHooks, if still there, is left", path)
				return os.Remove(mark)
			}
			return fmt.Errorf("copilot's %s is not JSON, so KairoCode cannot turn copilot's own hooks off "+
				"(ADR 0018): correct it, or let copilot run them with \"agents\": {\"ownHooks\": true} in %s",
				path, config.UserFile())
		}
		if fi, err := os.Stat(path); err == nil {
			perm = fi.Mode().Perm()
		}
	}
	var disabled bool
	if v, ok := settings["disableAllHooks"]; ok {
		_ = json.Unmarshal(v, &disabled)
	}
	if own {
		// Take back what KairoCode wrote; if the user has since set it
		// otherwise, it is theirs.
		if disabled {
			delete(settings, "disableAllHooks")
			if err := writeJSONFile(path, settings, perm); err != nil {
				return err
			}
		}
		return os.Remove(mark)
	}
	if disabled {
		return nil // already off: the user's, or KairoCode's
	}
	settings["disableAllHooks"] = json.RawMessage("true")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	// The mark first: if KairoCode stops between the two, releasing finds
	// no disableAllHooks to take back, and only removes the mark.
	if err := os.WriteFile(mark, []byte("KairoCode wrote disableAllHooks in settings.json (ADR 0018).\n"), 0o600); err != nil {
		return err
	}
	r.logf("copilot's own hooks are turned off in %s (ADR 0018)", path)
	return writeJSONFile(path, settings, perm)
}

// writeJSONFile replaces path with v as indented JSON, all at once.
func writeJSONFile(path string, v any, perm fs.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(path, append(b, '\n'), perm)
}

// copilotRepoHooks refuses a working directory whose repository defines
// hooks for copilot (.github/hooks/*.json, or "hooks" in
// .github/copilot/settings.json or settings.local.json): copilot runs them
// outside KairoCode's sandbox with no one asked, even with
// disableAllHooks and in a folder it was not told to trust (checked with
// copilot 1.0.101 over ACP), and nothing turns them off for one run. The
// user's agents.ownHooks lets copilot run them.
func copilotRepoHooks(dir string) error {
	if dir == "" {
		return nil
	}
	var found []string
	for _, d := range opencode.ProjectDirs(dir) {
		gh := filepath.Join(d, ".github")
		hooks := filepath.Join(gh, "hooks")
		entries, err := os.ReadDir(hooks)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			found = append(found, hooks) // not read: copilot may read it
		default:
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".json") {
					found = append(found, filepath.Join(hooks, e.Name()))
				}
			}
		}
		for _, name := range []string{"settings.json", "settings.local.json"} {
			path := filepath.Join(gh, "copilot", name)
			b, err := os.ReadFile(path)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			var s struct {
				Hooks map[string]json.RawMessage `json:"hooks"`
			}
			// Not read: copilot may read it all the same.
			if err != nil || json.Unmarshal(b, &s) != nil || len(s.Hooks) > 0 {
				found = append(found, path)
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf("this repository defines hooks for copilot (%s), which copilot would run outside KairoCode's sandbox "+
		"with no one asked, and which cannot be turned off for one run (ADR 0018): use another provider here, "+
		"or let copilot run them with \"agents\": {\"ownHooks\": true} in %s",
		strings.Join(found, ", "), config.UserFile())
}

// codexMCPNames are the MCP servers codex's own settings at path name
// (ADR 0022): codex, run over ACP, starts them by itself, outside
// KairoCode. TOML has many ways to write a table's name, and a cloned
// repository's settings may use any: only the plain ones are read
// ([mcp_servers.<name>...], [mcp_servers."<name>"...], a dotted
// mcp_servers.<name>.key =, the name of letters, digits, _ and -, or in
// quotes also spaces and dots), and any other line that names
// mcp_servers, or a table or key with an escape in it, is an error: codex
// is then not started, rather than one of its servers missed. An error
// names the file and the line, not what is on it (it may hold a secret).
func codexMCPNames(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key := line // a table's name, or a key
		if strings.HasPrefix(line, "[") {
			key = line
		} else if k, _, ok := strings.Cut(line, "="); ok {
			key = k
		}
		if strings.Contains(key, "\\") && (strings.HasPrefix(line, "[") || strings.Contains(line, "=")) {
			return nil, fmt.Errorf("%s:%d: a table or key with an escape: KairoCode cannot tell whether it is an MCP server, so codex is not started here", path, i+1)
		}
		if !strings.Contains(line, "mcp_servers") {
			continue
		}
		m := codexServerLine.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("%s:%d: mcp_servers is written in a way KairoCode does not read, so it cannot turn codex's own MCP servers off: codex is not started here", path, i+1)
		}
		name := cmp.Or(m[1], m[2], m[3])
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names, nil
}

// codexServerLine is a line read as naming an MCP server: a table
// [mcp_servers.<name>] or [mcp_servers."<name>"] (with more after, a dot
// and plain keys), or a key mcp_servers.<name>.<key> =; a comment may
// follow a table.
var codexServerLine = regexp.MustCompile(`^(?:\[mcp_servers\.(?:([A-Za-z0-9_-]+)|"([A-Za-z0-9_ .-]+)")(?:\.[A-Za-z0-9_-]+)*\]\s*(?:#.*)?|mcp_servers\.(?:([A-Za-z0-9_-]+))\.[A-Za-z0-9_.-]+\s*=.*)$`)

// codexHome is where codex, started with env (the registry's for it, over
// this process's), keeps its settings: CODEX_HOME, else ~/.codex of its
// HOME.
func codexHome(env map[string]string) string {
	if h := cmp.Or(env["CODEX_HOME"], os.Getenv("CODEX_HOME")); h != "" {
		return h
	}
	home := env["HOME"]
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".codex")
}

// ownMCP is the user's agents.ownMCP: codex may start the MCP servers of
// the user's own codex settings (not a project's).
func ownMCP() (bool, error) {
	path := config.UserFile()
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var cfg struct {
		Agents struct {
			OwnMCP bool `json:"ownMCP"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return cfg.Agents.OwnMCP, nil
}

// codexMCPOff are the MCP servers of codex's own settings to turn off for
// a session in dir (ADR 0022): those of the project's .codex/config.toml
// always (a cloned repository's commands, run with no one asked), and the
// user's own (codexHome's config.toml) unless agents.ownMCP. Their calls
// would not go through KairoCode; servers the user wants are given
// through KairoCode instead (kairocode.json's mcp).
func codexMCPOff(dir, codexHome string) ([]string, error) {
	var off []string
	add := func(path string) error {
		names, err := codexMCPNames(path)
		for _, n := range names {
			if !slices.Contains(off, n) {
				off = append(off, n)
			}
		}
		return err
	}
	if dir != "" {
		for _, d := range opencode.ProjectDirs(dir) {
			if err := add(filepath.Join(d, ".codex", "config.toml")); err != nil {
				return nil, err
			}
		}
	}
	own, err := ownMCP()
	if err != nil {
		return nil, err
	}
	if !own && codexHome != "" {
		if err := add(filepath.Join(codexHome, "config.toml")); err != nil {
			return nil, err
		}
	}
	return off, nil
}
