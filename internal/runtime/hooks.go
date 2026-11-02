package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/flowchart"
	"github.com/r-hashi01/kairocode/internal/flowfile"
	"github.com/r-hashi01/kairocode/internal/permission"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// hookEvents are the events a hook can be bound to (ADR 0003; the others
// come later).
var hookEvents = []string{workflows.HookSessionStart, workflows.HookToolAfter, workflows.HookTurnEnd}

// loadHooks reads the hooks of kairocode.json, the user's then the
// project's: {"hooks": {"turn.end": ["<flow>", ...]}}; a tool.after hook
// may name its tool, {"tool": "edit", "flow": "<flow>"}. Either runs the
// project's script of that name: each is asked about before it first runs
// (and when its script changes).
func loadHooks(dir string) ([]workflows.Hook, error) {
	var out []workflows.Hook
	for _, path := range []string{filepath.Join(userConfigDir(), ConfigFile), filepath.Join(dir, ConfigFile)} {
		b, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var cfg struct {
			Hooks map[string][]json.RawMessage `json:"hooks"`
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return nil, fmt.Errorf("%s: hooks: %w", path, err)
		}
		for _, event := range slices.Sorted(func(yield func(string) bool) {
			for e := range cfg.Hooks {
				if !yield(e) {
					return
				}
			}
		}) {
			if !slices.Contains(hookEvents, event) {
				return nil, fmt.Errorf("%s: hooks: no event %q (there is %v)", path, event, hookEvents)
			}
			for _, raw := range cfg.Hooks[event] {
				var h struct {
					Tool string `json:"tool"`
					Flow string `json:"flow"`
				}
				if json.Unmarshal(raw, &h.Flow) != nil {
					if err := json.Unmarshal(raw, &h); err != nil {
						return nil, fmt.Errorf("%s: hooks: %s is neither a flow's name nor {\"tool\", \"flow\"}", path, raw)
					}
					if h.Tool != "" && event != workflows.HookToolAfter {
						return nil, fmt.Errorf("%s: hooks: only tool.after hooks name a tool", path)
					}
				}
				if !flowchart.ValidName(h.Flow) {
					return nil, fmt.Errorf("%s: hooks: %q is not a flow's name", path, h.Flow)
				}
				out = append(out, workflows.Hook{Event: event, Flow: h.Flow, Tool: h.Tool, Project: true})
			}
		}
	}
	return out, nil
}

// kindHookTrust is what the store keeps when the user allows a project's
// hook: the folder, the event, the flow, and its script's hash. A changed
// script is asked about again.
const kindHookTrust = "hook-trust"

type hookTrust struct {
	Dir    string `json:"dir"`
	Event  string `json:"event"`
	Flow   string `json:"flow"`
	Script string `json:"script"`
}

// hookCheck decides whether a project's hook may run: as the rules say,
// and, when they say to ask, without asking if the user allowed this hook
// with this script before.
func (r *Runtime) hookCheck(t *kairo.TaskContext, in workflows.PermissionInput, out workflows.PermissionOutput) (workflows.PermissionOutput, error) {
	if out.Action != string(permission.Ask) {
		return out, nil
	}
	var args struct {
		Name  string `json:"name"`
		Event string `json:"event"`
	}
	if err := json.Unmarshal(in.Call.Args, &args); err != nil {
		return out, err
	}
	src, err := flowfile.Read(in.Call.Dir, args.Name, false)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return out, err
	}
	sum := sha256.Sum256(src)
	trust, err := json.Marshal(hookTrust{Dir: in.Call.Dir, Event: args.Event, Flow: args.Name, Script: hex.EncodeToString(sum[:])})
	if err != nil {
		return out, err
	}
	script := hex.EncodeToString(sum[:])
	if _, _, err := r.Store.Get(t, store.Hash(kindHookTrust, trust)); err == nil {
		return workflows.PermissionOutput{Action: string(permission.Allow), Rule: "the user allowed this hook", Subject: out.Subject, Script: script}, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return out, err
	}
	out.Rule = "a hook of this project, not run here before"
	out.Trust, out.Script = string(trust), script
	return out, nil
}

// hookTrustAction keeps that the user allowed a hook.
func (r *Runtime) hookTrustAction(t *kairo.TaskContext, trust string) (bool, error) {
	if trust == "" {
		return true, nil
	}
	_, err := r.Store.Put(t, kindHookTrust, []byte(trust))
	return true, err
}
