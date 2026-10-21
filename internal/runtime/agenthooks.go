package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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
	path := filepath.Join(userConfigDir(), ConfigFile)
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
				path, filepath.Join(userConfigDir(), ConfigFile))
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
	f, err := os.CreateTemp(filepath.Dir(path), ".settings-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
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
		strings.Join(found, ", "), filepath.Join(userConfigDir(), ConfigFile))
}
