package opencode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// project are the directories from the repository's root (the nearest
// directory above dir holding .git) down to dir. Outside a repository it
// is dir alone: nothing above it is the project's.
func project(dir string) []string {
	var up []string
	for d := dir; ; d = filepath.Dir(d) {
		up = append(up, d)
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			slices.Reverse(up)
			return up
		}
		if filepath.Dir(d) == d {
			return []string{dir}
		}
	}
}

// Dirs are opencode's configuration directories for a working directory,
// in the order their definitions apply, the last winning: the user's
// ($XDG_CONFIG_HOME/opencode or ~/.config/opencode), then each .opencode
// from the repository's root (the nearest directory above holding .git)
// down to dir, the nearest last. Those that do not exist are left out.
//
// opencode walks up from dir and applies what it finds in that order, so
// there a .opencode higher up wins over a nearer one; here the nearer one
// wins, as for its settings files (opencode.json).
func Dirs(dir string) []string {
	var out []string
	config := os.Getenv("XDG_CONFIG_HOME")
	if config == "" {
		if home, err := os.UserHomeDir(); err == nil {
			config = filepath.Join(home, ".config")
		}
	}
	if config != "" {
		out = append(out, filepath.Join(config, "opencode"))
	}
	for _, d := range project(dir) {
		out = append(out, filepath.Join(d, ".opencode"))
	}
	return slices.DeleteFunc(out, func(d string) bool {
		info, err := os.Stat(d)
		return err != nil || !info.IsDir()
	})
}

// markdown lists the .md files under sub/ and subs/ of each directory, by
// name: the path under that directory without .md ("team/review" for
// agents/team/review.md). The files of every directory are returned in
// order, for the caller to merge.
func markdown(dirs []string, sub string) ([]file, error) {
	var out []file
	for _, d := range dirs {
		for _, s := range []string{sub, sub + "s"} {
			root := filepath.Join(d, s)
			err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
				if err != nil {
					if path == root && os.IsNotExist(err) {
						return fs.SkipDir
					}
					return err
				}
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
					return nil
				}
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				out = append(out, file{name: filepath.ToSlash(strings.TrimSuffix(rel, ".md")), path: path})
				return nil
			})
			if err != nil && !os.IsNotExist(err) {
				return nil, err
			}
		}
	}
	return out, nil
}

type file struct{ name, path string }

// read reads a definition: its frontmatter and its body (trimmed).
func (f file) read() (*Value, string, error) {
	b, err := os.ReadFile(f.path)
	if err != nil {
		return nil, "", err
	}
	front, body, err := Split(b)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", f.path, err)
	}
	v, err := Parse(front)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", f.path, err)
	}
	return v, strings.TrimSpace(body), nil
}

// Agent is an agent definition (opencode's agents/*.md).
type Agent struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Mode is "primary", "subagent" or "all" (the default).
	Mode string `json:"mode,omitempty"`
	// Model is "provider/model".
	Model string `json:"model,omitempty"`
	// Prompt, if not empty, takes the place of the default system prompt.
	Prompt string `json:"prompt,omitempty"`
	// Steps caps the model calls of a turn (0: the default).
	Steps   int  `json:"steps,omitempty"`
	Disable bool `json:"disable,omitempty"`
	Hidden  bool `json:"hidden,omitempty"`
	// Workflow is a flow of .kairocode/workflows that runs the agent's turns
	// in place of the model loop (KairoCode's, ADR 0003).
	Workflow string `json:"workflow,omitempty"`
	// Permission are permission rules in kairocode.json's form, each object
	// a layer after the one before (tools first, then permission, file by
	// file): of the rules that match a call, the last wins.
	Permission []json.RawMessage `json:"permission,omitempty"`
}

// Agents reads the agent definitions of dirs. A name defined in several
// files takes each field from the last file that sets it, and the
// permission layers of all of them in order.
func Agents(dirs []string) (map[string]Agent, error) {
	files, err := markdown(dirs, "agent")
	if err != nil {
		return nil, err
	}
	out := map[string]Agent{}
	for _, f := range files {
		v, body, err := f.read()
		if err != nil {
			return nil, err
		}
		name := f.name
		if n := v.Get("name").String(); n != "" {
			name = n
		}
		a := out[name]
		a.Name = name
		if s := v.Get("description").String(); s != "" {
			a.Description = s
		}
		if s := v.Get("mode").String(); s != "" {
			switch s {
			case "primary", "subagent", "all":
				a.Mode = s
			default:
				return nil, fmt.Errorf("%s: mode %q is not primary, subagent or all", f.path, s)
			}
		}
		if s := v.Get("model").String(); s != "" {
			a.Model = s
		}
		if s := v.Get("workflow").String(); s != "" {
			a.Workflow = s
		}
		if body != "" {
			a.Prompt = body
		}
		for _, k := range []string{"steps", "maxSteps"} {
			if v.Get(k) == nil {
				continue
			}
			n, ok := v.Get(k).Int()
			if !ok || n <= 0 {
				return nil, fmt.Errorf("%s: %s must be a positive number", f.path, k)
			}
			if k == "steps" || a.Steps == 0 {
				a.Steps = n
			}
		}
		for _, k := range []string{"disable", "hidden"} {
			if v.Get(k) == nil {
				continue
			}
			b, ok := v.Get(k).Bool()
			if !ok {
				return nil, fmt.Errorf("%s: %s must be true or false", f.path, k)
			}
			if k == "disable" {
				a.Disable = b
			} else {
				a.Hidden = b
			}
		}
		if t := v.Get("tools"); t != nil {
			layer, err := toolsLayer(t)
			if err != nil {
				return nil, fmt.Errorf("%s: tools: %w", f.path, err)
			}
			a.Permission = append(a.Permission, layer)
		}
		if p := v.Get("permission"); p != nil {
			layer, err := permissionLayer(p)
			if err != nil {
				return nil, fmt.Errorf("%s: permission: %w", f.path, err)
			}
			a.Permission = append(a.Permission, layer)
		}
		out[name] = a
	}
	return out, nil
}

// editTools are the tools opencode's "edit" permission covers, by their
// names here.
var editTools = []string{"edit", "write"}

// toolsLayer turns tools (name: true|false, names may be globs) into
// permission rules: true allows, false denies (opencode's old form).
func toolsLayer(t *Value) (json.RawMessage, error) {
	if !t.IsMap() {
		return nil, fmt.Errorf("want a map of tool: true|false")
	}
	var b bytes.Buffer
	b.WriteByte('{')
	n := 0
	for _, k := range t.Keys {
		on, ok := t.Map[k].Bool()
		if !ok {
			return nil, fmt.Errorf("%s: want true or false", k)
		}
		action := "deny"
		if on {
			action = "allow"
		}
		for _, name := range toolNames(k) {
			if n > 0 {
				b.WriteByte(',')
			}
			n++
			key, _ := json.Marshal(name)
			fmt.Fprintf(&b, "%s:%q", key, action)
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// toolNames are the tools a permission key names here: opencode's "edit",
// "write" and "patch" all mean every tool that changes files.
func toolNames(key string) []string {
	switch key {
	case "edit", "write", "patch":
		return editTools
	}
	return []string{key}
}

// permissionLayer is a permission map (or one action for all) in
// kairocode.json's form, with opencode's "edit" written for each tool it
// covers.
func permissionLayer(p *Value) (json.RawMessage, error) {
	if !p.IsMap() {
		return json.Marshal(p.String())
	}
	var b bytes.Buffer
	b.WriteByte('{')
	n := 0
	for _, k := range p.Keys {
		for _, name := range toolNames(k) {
			if n > 0 {
				b.WriteByte(',')
			}
			n++
			key, _ := json.Marshal(name)
			b.Write(key)
			b.WriteByte(':')
			b.Write(p.Map[k].JSON())
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// Command is a command definition (opencode's commands/*.md).
type Command struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Agent and Model, if set, run the command's turn.
	Agent string `json:"agent,omitempty"`
	Model string `json:"model,omitempty"`
	// Subtask asks for the command to run as a subagent (M3; for now it
	// runs as a turn of the session).
	Subtask  bool   `json:"subtask,omitempty"`
	Template string `json:"template"`
}

// Commands reads the command definitions of dirs; a later file of a name
// replaces the fields it sets.
func Commands(dirs []string) (map[string]Command, error) {
	files, err := markdown(dirs, "command")
	if err != nil {
		return nil, err
	}
	out := map[string]Command{}
	for _, f := range files {
		v, body, err := f.read()
		if err != nil {
			return nil, err
		}
		c := out[f.name]
		c.Name = f.name
		if s := v.Get("description").String(); s != "" {
			c.Description = s
		}
		if s := v.Get("agent").String(); s != "" {
			c.Agent = s
		}
		if s := v.Get("model").String(); s != "" {
			c.Model = s
		}
		if v.Get("subtask") != nil {
			b, ok := v.Get("subtask").Bool()
			if !ok {
				return nil, fmt.Errorf("%s: subtask must be true or false", f.path)
			}
			c.Subtask = b
		}
		if body != "" {
			c.Template = body
		}
		if c.Template == "" {
			return nil, fmt.Errorf("%s: the command has no template (its body)", f.path)
		}
		out[f.name] = c
	}
	return out, nil
}
