package runtime

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Claude Code's marketplaces (.claude-plugin/marketplace.json; ADR 0027,
// 追記): read as KairoCode's, their plugins installed as Claude Code's
// plugins, which KairoCode reads (their skills, and their .mcp.json's one
// server).

// ClaudeMarketplaceManifest is where a Claude Code marketplace has its
// catalog.
const ClaudeMarketplaceManifest = ".claude-plugin/marketplace.json"

// claudeMarket is a Claude Code marketplace's catalog, as far as it is read.
type claudeMarket struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Metadata    struct {
		Description string `json:"description"`
		PluginRoot  string `json:"pluginRoot"`
	} `json:"metadata"`
	Plugins []claudeEntry `json:"plugins"`
}

// claudeEntry is a plugin of a Claude Code marketplace.
type claudeEntry struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Version     string          `json:"version"`
	Source      json.RawMessage `json:"source"`
	Strict      *bool           `json:"strict"`
	Skills      json.RawMessage `json:"skills"`
	MCPServers  json.RawMessage `json:"mcpServers"`
}

// claudeLayout is what a Claude Code marketplace's entry says of its
// plugin's files, for it to be laid out as KairoCode reads it.
type claudeLayout struct {
	Name, Description, Version string
	Skills                     []string        // folders of skills (./a)
	MCPServers                 json.RawMessage // an object: its .mcp.json's servers
}

// readClaudeMarket reads a Claude Code marketplace's catalog.
func readClaudeMarket(files fs.FS, where string) (Marketplace, error) {
	b, err := fs.ReadFile(files, ClaudeMarketplaceManifest)
	if err != nil {
		return Marketplace{}, fmt.Errorf("%s has neither %s nor %s", where, MarketplaceManifest, ClaudeMarketplaceManifest)
	}
	var cm claudeMarket
	if err := json.Unmarshal(b, &cm); err != nil {
		return Marketplace{}, fmt.Errorf("%s: %s: %w", where, ClaudeMarketplaceManifest, err)
	}
	if !folderName(cm.Name) {
		return Marketplace{}, fmt.Errorf("%s: the marketplace's name %q cannot be a folder's", where, cm.Name)
	}
	m := Marketplace{Name: cm.Name, Description: cmpOr(cm.Description, cm.Metadata.Description), Claude: true, files: files}
	root := strings.TrimPrefix(path.Clean(cm.Metadata.PluginRoot), "./")
	for _, e := range cm.Plugins {
		if e.Name == "" {
			continue
		}
		mp := MarketPlugin{Name: e.Name, Description: e.Description}
		mp.Unsupported = claudeSource(&mp, e.Source, root)
		if !folderName(e.Name) || !extName.MatchString(e.Name) {
			mp.Unsupported = "its name cannot be a KairoCode plugin's (letters, digits, - and _)"
		}
		lay := &claudeLayout{Name: e.Name, Description: e.Description, Version: e.Version}
		if len(e.Skills) > 0 {
			var one string
			if json.Unmarshal(e.Skills, &lay.Skills) != nil && json.Unmarshal(e.Skills, &one) == nil {
				lay.Skills = []string{one}
			}
		}
		if len(e.MCPServers) > 0 && e.MCPServers[0] == '{' {
			lay.MCPServers = e.MCPServers
		}
		mp.claude = lay
		m.Plugins = append(m.Plugins, mp)
	}
	return m, nil
}

// gitRef is a ref or a commit KairoCode checks out: no option, no path out.
var gitRef = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)

// claudeSource reads a Claude Code entry's source into mp, or says why it
// cannot be installed.
func claudeSource(mp *MarketPlugin, raw json.RawMessage, root string) string {
	var rel string
	if json.Unmarshal(raw, &rel) == nil {
		if !strings.HasPrefix(rel, "./") {
			return fmt.Sprintf("its source %q is not a folder of the marketplace", rel)
		}
		if root != "" && root != "." {
			rel = "./" + path.Join(root, rel)
		}
		mp.Source = rel
		return ""
	}
	var s struct {
		Source string `json:"source"`
		Repo   string `json:"repo"`
		URL    string `json:"url"`
		Path   string `json:"path"`
		Ref    string `json:"ref"`
		SHA    string `json:"sha"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return "its source is not one KairoCode reads"
	}
	switch s.Source {
	case "github":
		if !ownerRepo.MatchString(s.Repo) {
			return fmt.Sprintf("its GitHub repository %q is not owner/repo", s.Repo)
		}
		mp.Source = "https://github.com/" + s.Repo + ".git"
	case "url", "git-subdir":
		mp.Source, mp.Path = s.URL, s.Path
	default:
		return fmt.Sprintf("its source is %s, which KairoCode does not install", cmpOr(s.Source, "of no kind"))
	}
	mp.Ref, mp.SHA = s.Ref, s.SHA
	return checkGitSource(*mp)
}

// checkGitSource says why a git source cannot be installed, if it cannot.
func checkGitSource(mp MarketPlugin) string {
	switch {
	case !remoteGit(mp.Source):
		return fmt.Sprintf("its source %q is not a git URL", shown(mp.Source))
	case mp.Ref != "" && !gitRef.MatchString(mp.Ref):
		return fmt.Sprintf("its ref %q is not one", mp.Ref)
	case mp.SHA != "" && !regexp.MustCompile(`^[0-9a-f]{7,64}$`).MatchString(mp.SHA):
		return fmt.Sprintf("its sha %q is not a commit's", mp.SHA)
	case mp.Path != "" && !fs.ValidPath(strings.TrimPrefix(path.Clean(mp.Path), "./")):
		return fmt.Sprintf("its path %q is not a folder in its repository", mp.Path)
	}
	return ""
}

// layOut lays a Claude Code plugin's files at dir out as KairoCode reads
// them: a .claude-plugin/plugin.json (the entry's, when the plugin has
// none), its .mcp.json from the entry, and the skills the entry names in
// skills/<name>. Nothing in them is changed.
func layOut(dir string, lay *claudeLayout) error {
	// Its files come with no link (copyTree); none is followed here either.
	for _, rel := range append([]string{".claude-plugin/plugin.json", ".mcp.json", "skills"}, lay.Skills...) {
		if err := noLink(dir, rel); err != nil {
			return err
		}
	}
	manifest := filepath.Join(dir, ".claude-plugin", "plugin.json")
	if _, err := os.Stat(manifest); err != nil {
		if err := os.MkdirAll(filepath.Dir(manifest), 0o700); err != nil {
			return err
		}
		b, _ := json.Marshal(map[string]string{"name": lay.Name, "description": lay.Description, "version": lay.Version})
		if err := os.WriteFile(manifest, b, 0o600); err != nil {
			return err
		}
	}
	if len(lay.MCPServers) > 0 {
		if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); err != nil {
			b, _ := json.Marshal(map[string]json.RawMessage{"mcpServers": lay.MCPServers})
			if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), b, 0o600); err != nil {
				return err
			}
		}
	}
	for _, s := range lay.Skills {
		rel := path.Clean(strings.TrimPrefix(s, "./"))
		if !fs.ValidPath(rel) || rel == "." {
			return fmt.Errorf("a skill's folder %q is not in the plugin", s)
		}
		if path.Dir(rel) == "skills" {
			continue // where KairoCode reads it
		}
		from := filepath.Join(dir, filepath.FromSlash(rel))
		if fi, err := os.Lstat(from); err != nil || !fi.IsDir() {
			return fmt.Errorf("the skill's folder %s is not in the plugin", s)
		}
		to := filepath.Join(dir, "skills", path.Base(rel))
		if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
			return err
		}
		if _, err := os.Stat(to); err == nil {
			return fmt.Errorf("two skills' folders are named %s", path.Base(rel))
		}
		if err := os.Rename(from, to); err != nil {
			return err
		}
	}
	return nil
}

// noLink says why rel in dir cannot be written or moved: a part of it is
// a link (or rel is not in dir).
func noLink(dir, rel string) error {
	rel = path.Clean(strings.TrimPrefix(rel, "./"))
	if !fs.ValidPath(rel) {
		return fmt.Errorf("%q is not in the plugin", rel)
	}
	at := dir
	for _, part := range strings.Split(rel, "/") {
		at = filepath.Join(at, part)
		fi, err := os.Lstat(at)
		if err != nil {
			return nil // not there: nothing further is
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s in the plugin is a link: not followed", rel)
		}
	}
	return nil
}

// claudeUnused is what of a Claude Code plugin at dir KairoCode does not
// use: its commands, subagents, hooks, language servers and output styles.
func claudeUnused(dir string) []string {
	var out []string
	var m map[string]json.RawMessage
	if b, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json")); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	has := func(key string, files ...string) bool {
		if _, ok := m[key]; ok {
			return true
		}
		for _, f := range files {
			if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
				return true
			}
		}
		return false
	}
	for _, c := range []struct {
		what, key string
		files     []string
	}{
		{"commands", "commands", []string{"commands"}},
		{"subagents", "agents", []string{"agents"}},
		{"hooks", "hooks", []string{"hooks", "hooks.json"}},
		{"language servers", "lspServers", []string{".lsp.json"}},
		{"output styles", "outputStyles", []string{"output-styles"}},
		{"its plugin.json's MCP servers", "mcpServers", nil},
	} {
		if has(c.key, c.files...) {
			out = append(out, c.what)
		}
	}
	return out
}
