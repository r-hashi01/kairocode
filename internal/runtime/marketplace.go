package runtime

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Plugin marketplaces (ADR 0027): catalogs of plugins, by name and where
// each is. The one KairoCode comes with is built in; the user adds others
// (a git repository, kept at a commit, or a folder, read where it is).
// Installing copies a plugin of one into the user's plugins, or clones it.

// MarketplaceManifest is the file a marketplace has at its top.
const MarketplaceManifest = "kairocode-marketplace.json"

// BuiltIn is the name of the marketplace KairoCode comes with.
const BuiltIn = "kairocode"

//go:embed catalog
var catalog embed.FS

// Marketplace is a catalog of plugins.
type Marketplace struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Plugins     []MarketPlugin `json:"plugins"`
	// Where it is: built in, a folder, or a git repository's clone.
	Source  string `json:"-"` // as added ("" when built in)
	Commit  string `json:"-"` // a git one's
	BuiltIn bool   `json:"-"`
	Claude  bool   `json:"-"` // Claude Code's (.claude-plugin/marketplace.json)
	files   fs.FS  // its files (a folder's: dir)
	dir     string // its folder ("" when built in)
}

// MarketPlugin is a plugin of a marketplace.
type MarketPlugin struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Source: ./<path> in the marketplace, or a git URL: then Path, a
	// folder in the repository (else all of it); Ref, a branch or a tag;
	// SHA, the commit (else the ref's latest).
	Source string `json:"source"`
	Path   string `json:"path,omitempty"`
	Ref    string `json:"ref,omitempty"`
	SHA    string `json:"sha,omitempty"`
	// Connect: when its credentials are asked for: install (as it is
	// installed), or use (when it is used; the default).
	Connect string `json:"connect,omitempty"`
	// Unsupported: why it cannot be installed (a Claude Code one's source
	// KairoCode does not take, say).
	Unsupported string        `json:"-"`
	claude      *claudeLayout // a Claude Code marketplace's: how to lay it out
}

// addedMarket is a marketplace the user added, as marketplaces.json keeps
// it.
type addedMarket struct {
	Name   string `json:"name"`
	Source string `json:"source"`           // a folder (absolute) or a git URL
	Local  bool   `json:"local,omitempty"`  // a folder, read where it is
	Commit string `json:"commit,omitempty"` // a git one's, as it is kept
}

// UserPluginsDir is the folder of the user's plugins.
func UserPluginsDir() string { return filepath.Join(userConfigDir(), "plugins") }

func marketsFile() string { return filepath.Join(userConfigDir(), "marketplaces.json") }
func marketsDir() string  { return filepath.Join(userConfigDir(), "marketplaces") }

// folderName says name can be a folder's: not empty, no separator, not
// starting with a dot.
func folderName(name string) bool {
	return name != "" && !strings.ContainsAny(name, `/\`) && !strings.HasPrefix(name, ".")
}

// readMarket reads a marketplace's manifest from files.
func readMarket(files fs.FS, where string) (Marketplace, error) {
	b, err := fs.ReadFile(files, MarketplaceManifest)
	if err != nil {
		return readClaudeMarket(files, where)
	}
	var m Marketplace
	if err := json.Unmarshal(b, &m); err != nil {
		return Marketplace{}, fmt.Errorf("%s: %s: %w", where, MarketplaceManifest, err)
	}
	if !folderName(m.Name) {
		return Marketplace{}, fmt.Errorf("%s: the marketplace's name %q cannot be a folder's", where, m.Name)
	}
	for _, p := range m.Plugins {
		if !folderName(p.Name) {
			return Marketplace{}, fmt.Errorf("%s: a plugin's name %q cannot be a folder's", where, p.Name)
		}
		if p.Connect != "" && p.Connect != "install" && p.Connect != "use" {
			return Marketplace{}, fmt.Errorf("%s: %s: connect is install or use, not %q", where, p.Name, p.Connect)
		}
	}
	for i, p := range m.Plugins {
		if !strings.HasPrefix(p.Source, "./") {
			m.Plugins[i].Unsupported = checkGitSource(p)
		}
	}
	m.files = files
	return m, nil
}

func readAdded() ([]addedMarket, error) {
	b, err := os.ReadFile(marketsFile())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []addedMarket
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", marketsFile(), err)
	}
	return out, nil
}

func writeAdded(list []addedMarket) error {
	if err := os.MkdirAll(userConfigDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(list, "", "  ")
	tmp := marketsFile() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, marketsFile())
}

// builtIn is the marketplace KairoCode comes with.
func builtIn() Marketplace {
	files, _ := fs.Sub(catalog, "catalog")
	m, err := readMarket(files, "the built-in marketplace")
	if err != nil {
		panic(err) // TestCatalog
	}
	m.BuiltIn = true
	return m
}

// Marketplaces are the marketplaces: the built-in one, then the user's in
// the order they were added. One that cannot be read is in errs.
func Marketplaces() (list []Marketplace, errs []error) {
	list = append(list, builtIn())
	added, err := readAdded()
	if err != nil {
		return list, []error{err}
	}
	for _, a := range added {
		dir := a.Source
		if !a.Local {
			dir = filepath.Join(marketsDir(), a.Name)
		}
		m, err := readMarket(os.DirFS(dir), a.Name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		m.Name, m.Source, m.Commit, m.dir = a.Name, a.Source, a.Commit, dir
		list = append(list, m)
	}
	return list, errs
}

// ownerRepo is GitHub's owner/repo shorthand.
var ownerRepo = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// AddMarketplace adds a marketplace: a folder (read where it is), or a git
// repository (a URL, or GitHub's owner/repo), cloned and kept at its
// commit.
func AddMarketplace(ctx context.Context, src string) (Marketplace, error) {
	added, err := readAdded()
	if err != nil {
		return Marketplace{}, err
	}
	taken := func(name string) error {
		if name == BuiltIn || slices.ContainsFunc(added, func(a addedMarket) bool { return a.Name == name }) {
			return fmt.Errorf("a marketplace named %s is there already", name)
		}
		return nil
	}
	if abs, err := filepath.Abs(src); err == nil {
		if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
			m, err := readMarket(os.DirFS(abs), abs)
			if err != nil {
				return Marketplace{}, err
			}
			if err := taken(m.Name); err != nil {
				return Marketplace{}, err
			}
			m.Source, m.dir = abs, abs
			return m, writeAdded(append(added, addedMarket{Name: m.Name, Source: abs, Local: true}))
		}
	}
	if ownerRepo.MatchString(src) {
		src = "https://github.com/" + src + ".git"
	}
	if err := os.MkdirAll(marketsDir(), 0o700); err != nil {
		return Marketplace{}, err
	}
	tmp, err := os.MkdirTemp(marketsDir(), ".add-")
	if err != nil {
		return Marketplace{}, err
	}
	defer os.RemoveAll(tmp)
	if _, err := gitIn(ctx, marketsDir(), "clone", "--quiet", "--", src, tmp); err != nil {
		return Marketplace{}, err
	}
	commit, err := gitIn(ctx, tmp, "rev-parse", "HEAD")
	if err != nil {
		return Marketplace{}, err
	}
	m, err := readMarket(os.DirFS(tmp), src)
	if err != nil {
		return Marketplace{}, err
	}
	if err := taken(m.Name); err != nil {
		return Marketplace{}, err
	}
	dir := filepath.Join(marketsDir(), m.Name)
	os.RemoveAll(dir) // one left by a removal that did not finish
	if err := os.Rename(tmp, dir); err != nil {
		return Marketplace{}, err
	}
	m.Source, m.Commit, m.dir = src, commit[:12], dir
	return m, writeAdded(append(added, addedMarket{Name: m.Name, Source: src, Commit: commit[:12]}))
}

// UpdateMarketplace brings a git marketplace to its latest commit: the
// commits before and after.
func UpdateMarketplace(ctx context.Context, name string) (before, after string, err error) {
	added, err := readAdded()
	if err != nil {
		return "", "", err
	}
	i := slices.IndexFunc(added, func(a addedMarket) bool { return a.Name == name })
	switch {
	case name == BuiltIn:
		return "", "", fmt.Errorf("%s is built in: it changes with KairoCode", name)
	case i < 0:
		return "", "", fmt.Errorf("no marketplace %q", name)
	case added[i].Local:
		return "", "", fmt.Errorf("%s is a folder, read where it is: nothing to update", name)
	}
	dir := filepath.Join(marketsDir(), name)
	if _, err := gitIn(ctx, dir, "pull", "--quiet", "--ff-only"); err != nil {
		return "", "", err
	}
	head, err := gitIn(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	before, added[i].Commit = added[i].Commit, head[:12]
	return before, added[i].Commit, writeAdded(added)
}

// RemoveMarketplace removes a marketplace the user added. The plugins
// installed from it stay.
func RemoveMarketplace(name string) error {
	added, err := readAdded()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(added, func(a addedMarket) bool { return a.Name == name })
	if name == BuiltIn {
		return fmt.Errorf("%s is built in", name)
	}
	if i < 0 {
		return fmt.Errorf("no marketplace %q", name)
	}
	gone := added[i]
	if err := writeAdded(slices.Delete(added, i, i+1)); err != nil {
		return err
	}
	if !gone.Local {
		return os.RemoveAll(filepath.Join(marketsDir(), name))
	}
	return nil
}

// Installed is a plugin as it was installed.
type Installed struct {
	Name    string
	From    string // the marketplace, or the URL or folder
	Version string
	Auth    string // none, key, oauth
	// ConnectNow: its marketplace says to ask for its credentials as it
	// is installed.
	ConnectNow bool
}

// pluginOrigin is where an installed plugin came from
// (.kairocode-source), for update.
type pluginOrigin struct {
	Marketplace string `json:"marketplace,omitempty"`
	Plugin      string `json:"plugin,omitempty"`
}

const originFile = ".kairocode-source"

// InstallPlugin installs a plugin into the user's: name or name@market of
// a marketplace, a git URL, or a folder.
func InstallPlugin(ctx context.Context, arg string) (Installed, error) {
	markets, _ := Marketplaces()
	if name, market, ok := cutLast(arg, "@"); ok && slices.ContainsFunc(markets, func(m Marketplace) bool { return m.Name == market }) {
		return installFrom(ctx, markets, market, name)
	}
	if abs, err := filepath.Abs(arg); err == nil {
		if _, err := os.Stat(abs); err == nil {
			return installDirect(ctx, abs)
		}
	}
	if strings.ContainsAny(arg, "/:") {
		return installDirect(ctx, arg)
	}
	var in []string
	for _, m := range markets {
		if slices.ContainsFunc(m.Plugins, func(p MarketPlugin) bool { return p.Name == arg }) {
			in = append(in, m.Name)
		}
	}
	switch len(in) {
	case 0:
		return Installed{}, fmt.Errorf("no marketplace has a plugin %q (kairocode plugin list shows those that do)", arg)
	case 1:
		return installFrom(ctx, markets, in[0], arg)
	}
	var choices []string
	for _, m := range in {
		choices = append(choices, arg+"@"+m)
	}
	return Installed{}, fmt.Errorf("%s is in more than one marketplace: %s", arg, strings.Join(choices, " or "))
}

// remoteGit says a marketplace's plugin source is a git repository away:
// a marketplace does not point to the user's own folders.
func remoteGit(src string) bool {
	if fileURLs && strings.HasPrefix(src, "file://") {
		return true
	}
	for _, p := range []string{"https://", "http://", "ssh://", "git://", "git@"} {
		if strings.HasPrefix(src, p) {
			return true
		}
	}
	return false
}

// fileURLs lets a marketplace's plugin be a file:// repository: in tests
// only (ADR 0027).
var fileURLs bool

// shown is a source as it is shown: a URL without the credentials it may
// carry (user:token@).
func shown(src string) string {
	if u, err := url.Parse(src); err == nil && u.User != nil {
		u.User = nil
		return u.String()
	}
	return src
}

func cutLast(s, sep string) (before, after string, ok bool) {
	i := strings.LastIndex(s, sep)
	if i <= 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// installFrom installs market's plugin name.
func installFrom(ctx context.Context, markets []Marketplace, market, name string) (Installed, error) {
	i := slices.IndexFunc(markets, func(m Marketplace) bool { return m.Name == market })
	m := markets[i]
	j := slices.IndexFunc(m.Plugins, func(p MarketPlugin) bool { return p.Name == name })
	if j < 0 {
		return Installed{}, fmt.Errorf("the marketplace %s has no plugin %q", market, name)
	}
	mp := m.Plugins[j]
	dest := filepath.Join(UserPluginsDir(), name)
	if _, err := os.Stat(dest); err == nil {
		return Installed{}, fmt.Errorf("a plugin named %s is installed already: kairocode plugin update %s", name, name)
	}
	tmp, err := fetchPlugin(ctx, m, mp)
	if err != nil {
		return Installed{}, err
	}
	defer os.RemoveAll(tmp)
	got, err := placePlugin(tmp, dest, name, pluginOrigin{Marketplace: market, Plugin: name})
	if err != nil {
		return Installed{}, err
	}
	got.From, got.ConnectNow = market, mp.Connect == "install"
	if got.Version == "" && m.Commit != "" {
		got.Version = m.Commit
	}
	return got, nil
}

// installDirect installs from a git URL, or a folder: cloned if it is a
// git repository (to be updated), else copied.
func installDirect(ctx context.Context, src string) (Installed, error) {
	var tmp string
	var err error
	if fi, serr := os.Stat(src); serr == nil && fi.IsDir() && !isGit(src) {
		tmp, err = copyTree(os.DirFS(src), src)
	} else {
		tmp, err = cloneOut(ctx, src)
	}
	if err != nil {
		return Installed{}, err
	}
	defer os.RemoveAll(tmp)
	name := strings.TrimSuffix(filepath.Base(strings.TrimRight(src, "/")), ".git")
	if m, ok := manifestOf(tmp); ok && m.Name != "" {
		name = m.Name
	}
	if !folderName(name) {
		return Installed{}, fmt.Errorf("the plugin's name %q cannot be a folder's", name)
	}
	dest := filepath.Join(UserPluginsDir(), name)
	if _, err := os.Stat(dest); err == nil {
		return Installed{}, fmt.Errorf("a plugin named %s is installed already: kairocode plugin update %s", name, name)
	}
	got, err := placePlugin(tmp, dest, name, pluginOrigin{})
	got.From = shown(src)
	return got, err
}

func isGit(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// manifestOf reads a plugin's manifest in dir (KairoCode's, else Claude
// Code's name).
func manifestOf(dir string) (pluginManifest, bool) {
	var m pluginManifest
	if b, err := os.ReadFile(filepath.Join(dir, PluginManifest)); err == nil && json.Unmarshal(b, &m) == nil {
		return m, true
	}
	if b, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json")); err == nil && json.Unmarshal(b, &m) == nil {
		return m, true
	}
	return m, false
}

// placePlugin moves a plugin's files at tmp to dest (one there is put
// aside, then removed), with where it came from.
func placePlugin(tmp, dest, name string, origin pluginOrigin) (Installed, error) {
	m, _ := manifestOf(tmp)
	// KairoCode's manifest names its plugin; a Claude Code one goes by
	// its marketplace's name for it, as in Claude Code.
	if _, err := os.Stat(filepath.Join(tmp, PluginManifest)); err == nil && m.Name != "" && m.Name != name {
		return Installed{}, fmt.Errorf("the plugin's manifest names it %q, not %s", m.Name, name)
	}
	// The markers are KairoCode's: none the plugin brings is taken.
	os.Remove(filepath.Join(tmp, originFile))
	os.Remove(filepath.Join(tmp, ".kairocode-commit"))
	if isGit(tmp) {
		os.Remove(filepath.Join(tmp, ".kairocode-at")) // a clone's own: not taken
	}
	// What it is at goes into its servers' fingerprint (ADR 0025): a
	// clone's commit, else a hash of its files, so that turns under way
	// do not call a server changed under them.
	at := ""
	if b, err := os.ReadFile(filepath.Join(tmp, ".kairocode-at")); err == nil { // a folder of a repository's commit
		at = string(b)
		os.Remove(filepath.Join(tmp, ".kairocode-at"))
	} else if isGit(tmp) {
		commit, err := gitIn(context.Background(), tmp, "rev-parse", "HEAD")
		if err != nil {
			return Installed{}, err
		}
		at = commit[:12]
	} else {
		h, err := treeHash(tmp)
		if err != nil {
			return Installed{}, err
		}
		at = h
	}
	if err := os.WriteFile(filepath.Join(tmp, ".kairocode-commit"), []byte(at+"\n"), 0o600); err != nil {
		return Installed{}, err
	}
	if origin.Marketplace != "" {
		b, _ := json.Marshal(origin)
		if err := os.WriteFile(filepath.Join(tmp, originFile), b, 0o600); err != nil {
			return Installed{}, err
		}
	}
	old := ""
	if _, err := os.Stat(dest); err == nil {
		old = filepath.Join(filepath.Dir(dest), ".old-"+name)
		os.RemoveAll(old)
		if err := os.Rename(dest, old); err != nil {
			return Installed{}, err
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		if old != "" {
			os.Rename(old, dest)
		}
		return Installed{}, err
	}
	if old != "" {
		os.RemoveAll(old)
	}
	auth := cmpOr(m.Auth.Type, "none")
	return Installed{Name: name, Version: cmpOr(m.Version, pluginCommit(dest)), Auth: auth}, nil
}

// treeHash is a hash of the files in dir (their paths and contents, .git
// left out), its first 12 hex digits.
func treeHash(dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return fs.SkipDir
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(rel), len(b))
		h.Write(b)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

// pluginsTemp is a new folder among the user's plugins (hidden from them),
// to put a plugin's files in before they take its place.
func pluginsTemp() (string, error) {
	if err := os.MkdirAll(UserPluginsDir(), 0o700); err != nil {
		return "", err
	}
	return os.MkdirTemp(UserPluginsDir(), ".install-")
}

// copyOut copies market's folder rel out to a new folder: rel only in the
// marketplace (not above it, nor through a link out of it).
func copyOut(m Marketplace, rel string) (string, error) {
	rel = path.Clean(rel)
	if !fs.ValidPath(rel) || rel == "." {
		return "", fmt.Errorf("the marketplace %s: %q is not a folder in it", m.Name, "./"+rel)
	}
	if m.dir == "" {
		sub, err := fs.Sub(m.files, rel)
		if err != nil {
			return "", err
		}
		return copyTree(sub, m.Name+"/"+rel)
	}
	root, err := filepath.EvalSymlinks(m.dir)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(filepath.Join(m.dir, filepath.FromSlash(rel)))
	if err != nil {
		return "", fmt.Errorf("the marketplace %s has no %s", m.Name, "./"+rel)
	}
	if r, err := filepath.Rel(root, real); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("the marketplace %s: %s is outside it: not installed", m.Name, "./"+rel)
	}
	return copyTree(os.DirFS(real), real)
}

// copyTree copies files (no links, no .git) to a new folder.
func copyTree(files fs.FS, where string) (string, error) {
	tmp, err := pluginsTemp()
	if err != nil {
		return "", err
	}
	err = fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" && d.IsDir() {
			return fs.SkipDir
		}
		if p == originFile || p == ".kairocode-commit" || p == ".kairocode-at" {
			return nil // KairoCode's markers, written as it is installed
		}
		to := filepath.Join(tmp, filepath.FromSlash(p))
		switch {
		case d.IsDir():
			return os.MkdirAll(to, 0o700)
		case !d.Type().IsRegular():
			return nil // a link: not followed
		}
		b, err := fs.ReadFile(files, p)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if info, err := d.Info(); err == nil && info.Mode()&0o100 != 0 {
			mode = 0o700
		}
		return os.WriteFile(to, b, mode)
	})
	if err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("copying %s: %w", where, err)
	}
	return tmp, nil
}

// fetchPlugin puts market's plugin mp's files in a new folder: copied out
// of the marketplace, or cloned (at its ref or commit, and only its path,
// if it has them); a Claude Code one's laid out as KairoCode reads it.
func fetchPlugin(ctx context.Context, m Marketplace, mp MarketPlugin) (string, error) {
	if mp.Unsupported != "" {
		return "", fmt.Errorf("%s of %s cannot be installed: %s", mp.Name, m.Name, mp.Unsupported)
	}
	var tmp string
	var err error
	if rel, ok := strings.CutPrefix(mp.Source, "./"); ok {
		tmp, err = copyOut(m, rel)
	} else {
		tmp, err = cloneAt(ctx, mp)
	}
	if err != nil {
		return "", err
	}
	if mp.claude != nil {
		if err := layOut(tmp, mp.claude); err != nil {
			os.RemoveAll(tmp)
			return "", fmt.Errorf("%s of %s: %w", mp.Name, m.Name, err)
		}
	}
	return tmp, nil
}

// cloneAt clones mp's git source at its ref or commit, and copies out its
// path (else all of it), as a folder of a marketplace: no link and no .git
// comes with it (a plugin of a marketplace is laid out after, and must not
// be led out of its folder).
func cloneAt(ctx context.Context, mp MarketPlugin) (string, error) {
	if why := checkGitSource(mp); why != "" {
		return "", fmt.Errorf("%s cannot be installed: %s", mp.Name, why)
	}
	tmp, err := pluginsTemp()
	if err != nil {
		return "", err
	}
	args := []string{"clone", "--quiet"}
	if mp.Ref != "" && mp.SHA == "" {
		args = append(args, "--depth", "1", "--branch", mp.Ref)
	}
	if _, err := gitIn(ctx, UserPluginsDir(), append(args, "--", mp.Source, tmp)...); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	if mp.SHA != "" {
		if _, err := gitIn(ctx, tmp, "checkout", "--quiet", "--detach", mp.SHA); err != nil {
			os.RemoveAll(tmp)
			return "", err
		}
	}
	defer os.RemoveAll(tmp)
	commit, err := gitIn(ctx, tmp, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	var out string
	if rel := path.Clean(strings.TrimPrefix(mp.Path, "./")); rel == "." {
		out, err = copyTree(os.DirFS(tmp), mp.Source)
	} else {
		out, err = copyOut(Marketplace{Name: mp.Name, dir: tmp}, rel)
	}
	if err != nil {
		return "", err
	}
	// Its version is the repository's commit, not a hash of the copy.
	if err := os.WriteFile(filepath.Join(out, ".kairocode-at"), []byte(commit[:12]), 0o600); err != nil {
		os.RemoveAll(out)
		return "", err
	}
	return out, nil
}

// cloneOut clones src to a new folder.
func cloneOut(ctx context.Context, src string) (string, error) {
	tmp, err := pluginsTemp()
	if err != nil {
		return "", err
	}
	if _, err := gitIn(ctx, UserPluginsDir(), "clone", "--quiet", "--", src, tmp); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	return tmp, nil
}

// UpdatePlugin brings an installed plugin up to date: one of a marketplace
// is installed again as the marketplace has it now (a git one brought to
// its latest commit first); a git one is pulled. The versions before and
// after.
func UpdatePlugin(ctx context.Context, name string) (before, after string, err error) {
	if !folderName(name) {
		return "", "", fmt.Errorf("no plugin %q", name)
	}
	dir := filepath.Join(UserPluginsDir(), name)
	if _, err := os.Stat(dir); err != nil {
		return "", "", fmt.Errorf("no plugin %q is installed", name)
	}
	m, _ := manifestOf(dir)
	before = cmpOr(m.Version, pluginCommit(dir))
	var origin pluginOrigin
	if b, err := os.ReadFile(filepath.Join(dir, originFile)); err == nil {
		_ = json.Unmarshal(b, &origin)
	}
	if origin.Marketplace != "" {
		markets, _ := Marketplaces()
		i := slices.IndexFunc(markets, func(m Marketplace) bool { return m.Name == origin.Marketplace })
		if i < 0 {
			return "", "", fmt.Errorf("%s came from the marketplace %s, which is not there now", name, origin.Marketplace)
		}
		if markets[i].Commit != "" {
			if _, _, err := UpdateMarketplace(ctx, origin.Marketplace); err != nil {
				return "", "", err
			}
			markets, _ = Marketplaces()
			i = slices.IndexFunc(markets, func(m Marketplace) bool { return m.Name == origin.Marketplace })
			if i < 0 {
				return "", "", fmt.Errorf("the marketplace %s could not be read at its latest commit", origin.Marketplace)
			}
		}
		mk := markets[i]
		j := slices.IndexFunc(mk.Plugins, func(p MarketPlugin) bool { return p.Name == origin.Plugin })
		if j < 0 {
			return "", "", fmt.Errorf("the marketplace %s has no plugin %q now", mk.Name, origin.Plugin)
		}
		tmp, err := fetchPlugin(ctx, mk, mk.Plugins[j])
		if err != nil {
			return "", "", err
		}
		defer os.RemoveAll(tmp)
		got, err := placePlugin(tmp, dir, name, origin)
		if err != nil {
			return "", "", err
		}
		return before, cmpOr(got.Version, mk.Commit), nil
	}
	if !isGit(dir) {
		return "", "", fmt.Errorf("%s was copied from a folder: remove it and install it again", name)
	}
	if _, err := gitIn(ctx, dir, "pull", "--quiet", "--ff-only"); err != nil {
		return "", "", err
	}
	head, err := gitIn(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(filepath.Join(dir, ".kairocode-commit"), []byte(head[:12]+"\n"), 0o600); err != nil {
		return "", "", err
	}
	m, _ = manifestOf(dir)
	return before, cmpOr(m.Version, head[:12]), nil
}

// RemovePlugin removes an installed plugin; its credentials stay until
// disconnected.
func RemovePlugin(name string) error {
	if !folderName(name) {
		return fmt.Errorf("no plugin %q", name)
	}
	dir := filepath.Join(UserPluginsDir(), name)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("no plugin %q is installed", name)
	}
	return os.RemoveAll(dir)
}

// CatalogPlugin is a plugin of a marketplace, as /plugin shows it.
type CatalogPlugin struct {
	Name        string `json:"name"`
	Marketplace string `json:"marketplace"`
	Description string `json:"description,omitempty"`
	Installed   bool   `json:"installed,omitempty"` // one of the user's has its name
	// Unsupported: why it cannot be installed.
	Unsupported string `json:"unsupported,omitempty"`
}

// MarketInfo is a marketplace, as /plugin shows it.
type MarketInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source,omitempty"` // "" built in
	Commit      string `json:"commit,omitempty"`
	BuiltIn     bool   `json:"builtIn,omitempty"`
	Claude      bool   `json:"claude,omitempty"` // Claude Code's
	Plugins     int    `json:"plugins"`
}

// Catalog is every marketplace's plugins, the marketplaces, and why any
// could not be read.
func Catalog() (plugins []CatalogPlugin, markets []MarketInfo, errs []string) {
	list, bad := Marketplaces()
	for _, m := range list {
		markets = append(markets, MarketInfo{Name: m.Name, Description: m.Description, Source: shown(m.Source),
			Commit: m.Commit, BuiltIn: m.BuiltIn, Claude: m.Claude, Plugins: len(m.Plugins)})
		for _, p := range m.Plugins {
			_, err := os.Stat(filepath.Join(UserPluginsDir(), p.Name))
			if !folderName(p.Name) {
				err = os.ErrNotExist
			}
			plugins = append(plugins, CatalogPlugin{Name: p.Name, Marketplace: m.Name, Description: p.Description,
				Installed: err == nil, Unsupported: p.Unsupported})
		}
	}
	for _, err := range bad {
		errs = append(errs, err.Error())
	}
	return plugins, markets, errs
}

// AvailableText lists the marketplaces' plugins not installed, by
// marketplace, and how to install one (install: the command's words).
func AvailableText(install string) string {
	plugins, markets, errs := Catalog()
	var b strings.Builder
	for _, m := range markets {
		var todo []CatalogPlugin
		width := 0
		for _, p := range plugins {
			if p.Marketplace == m.Name && !p.Installed && p.Unsupported == "" {
				todo = append(todo, p)
				width = max(width, len(p.Name))
			}
		}
		if len(todo) == 0 {
			continue
		}
		head := m.Name
		switch {
		case m.BuiltIn:
			head += " (built in)"
		case m.Description != "":
			head += " (" + m.Description + ")"
		}
		fmt.Fprintf(&b, "  %s:\n", head)
		for _, p := range todo {
			fmt.Fprintf(&b, "    %-*s  %s\n", width, p.Name, p.Description)
		}
	}
	for _, err := range errs {
		fmt.Fprintf(&b, "  (a marketplace could not be read: %s)\n", err)
	}
	if b.Len() == 0 {
		return ""
	}
	return fmt.Sprintf("Available (%s <name>):\n%s", install, b.String())
}

// MarketplacesText lists the marketplaces.
func MarketplacesText() string {
	markets, errs := Marketplaces()
	var b strings.Builder
	for _, m := range markets {
		where := "built in"
		switch {
		case m.Commit != "":
			where = shown(m.Source) + " at " + m.Commit
		case m.Source != "":
			where = m.Source + " (a folder, read where it is)"
		}
		if m.Claude {
			where += ", Claude Code's"
		}
		fmt.Fprintf(&b, "%s: %s, %d plugins\n", m.Name, where, len(m.Plugins))
	}
	for _, err := range errs {
		fmt.Fprintf(&b, "(could not be read: %v)\n", err)
	}
	return b.String()
}

// gitIn runs git in dir, its output given back.
func gitIn(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
