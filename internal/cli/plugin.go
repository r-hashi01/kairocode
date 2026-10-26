package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"syscall"

	"github.com/r-hashi01/kairocode/internal/daemon"
	"github.com/r-hashi01/kairocode/internal/opencode"
	"github.com/r-hashi01/kairocode/internal/runtime"
)

// kairocode plugin (ADR 0025): the plugins of a directory and their state;
// install, update and remove the user's; connect and disconnect one (its
// credentials, kept in the system's key store).
func pluginCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("plugin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", ".", "working directory")
	data := fs.String("data", runtime.DefaultDataDir(), "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	rest := fs.Args()
	verb := ""
	if len(rest) > 0 {
		verb, rest = rest[0], rest[1:]
	}
	usage := errors.New("usage: kairocode plugin [-dir DIR] [list | install <git URL or path> | update <name> | remove <name> | connect <name> | disconnect <name>]")
	switch verb {
	case "", "list":
		return pluginList(ctx, *data, abs, stdout)
	case "install":
		if len(rest) != 1 {
			return usage
		}
		return pluginInstall(ctx, rest[0], stdout)
	case "update":
		if len(rest) != 1 {
			return usage
		}
		return pluginUpdate(ctx, rest[0], stdout)
	case "remove":
		if len(rest) != 1 {
			return usage
		}
		return pluginRemove(rest[0], stdout)
	case "connect":
		if len(rest) != 1 {
			return usage
		}
		return pluginConnect(ctx, *data, abs, rest[0], os.Stdin, stdout)
	case "disconnect":
		if len(rest) != 1 {
			return usage
		}
		return pluginDisconnect(ctx, *data, abs, rest[0], stdout)
	}
	return usage
}

// pluginArgv runs this kairocode's plugin command for dir (the terminal
// UI's /plugin connect).
func pluginArgv(data, dir string) []string {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	return []string{exe, "plugin", "-data", data, "-dir", dir}
}

// userPlugins is the folder of the user's plugins.
func userPlugins() string {
	if cfg := os.Getenv("XDG_CONFIG_HOME"); cfg != "" {
		return filepath.Join(cfg, "kairocode", "plugins")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "kairocode", "plugins")
}

// pluginList prints the plugins of dir and their state, from the daemon.
func pluginList(ctx context.Context, data, dir string, stdout io.Writer) error {
	c, err := connect(ctx, data)
	if err != nil {
		return err
	}
	defer c.Close()
	list, err := c.PluginsIn(ctx, dir)
	if err != nil {
		return err
	}
	_, err = io.WriteString(stdout, runtime.PluginsText(list))
	return err
}

// git runs git, its output given back.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// pluginInstall clones a plugin into the user's plugins, named by its
// manifest (or its folder), at the commit it is at; the commit is kept.
func pluginInstall(ctx context.Context, src string, stdout io.Writer) error {
	if abs, err := filepath.Abs(src); err == nil {
		if _, err := os.Stat(abs); err == nil {
			src = abs
		}
	}
	root := userPlugins()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(root, ".install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if _, err := git(ctx, root, "clone", "--quiet", "--", src, tmp); err != nil {
		return err
	}
	commit, err := git(ctx, tmp, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	name := strings.TrimSuffix(filepath.Base(strings.TrimRight(src, "/")), ".git")
	var m struct {
		Name string `json:"name"`
	}
	if b, err := os.ReadFile(filepath.Join(tmp, runtime.PluginManifest)); err == nil && json.Unmarshal(b, &m) == nil && m.Name != "" {
		name = m.Name
	} else if b, err := os.ReadFile(filepath.Join(tmp, ".claude-plugin", "plugin.json")); err == nil && json.Unmarshal(b, &m) == nil && m.Name != "" {
		name = m.Name
	}
	if !filepathSafe(name) || strings.HasPrefix(name, ".") {
		return fmt.Errorf("the plugin's name %q cannot be a folder's", name)
	}
	dest := filepath.Join(root, name)
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("a plugin named %s is installed already: kairocode plugin update %s", name, name)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".kairocode-commit"), []byte(commit[:12]+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Installed %s at %s. kairocode plugin connect %s, if it needs credentials.\n", name, commit[:12], name)
	return nil
}

// pluginUpdate brings an installed plugin to its source's latest commit.
func pluginUpdate(ctx context.Context, name string, stdout io.Writer) error {
	if !filepathSafe(name) {
		return fmt.Errorf("no plugin %q", name)
	}
	dir := filepath.Join(userPlugins(), name)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return fmt.Errorf("%s is not a plugin installed from git", name)
	}
	before, err := git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if _, err := git(ctx, dir, "pull", "--quiet", "--ff-only"); err != nil {
		return err
	}
	after, err := git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if after == before {
		fmt.Fprintf(stdout, "%s is up to date (%s).\n", name, after[:12])
		return nil
	}
	if err := os.WriteFile(filepath.Join(dir, ".kairocode-commit"), []byte(after[:12]+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Updated %s: %s → %s. Turns under way go on with what they started with.\n", name, before[:12], after[:12])
	return nil
}

// pluginRemove removes an installed plugin; its credentials stay until
// disconnected.
func pluginRemove(name string, stdout io.Writer) error {
	if !filepathSafe(name) {
		return fmt.Errorf("no plugin %q", name)
	}
	dir := filepath.Join(userPlugins(), name)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("no plugin %q is installed", name)
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Removed %s. Its credentials are kept until kairocode plugin disconnect %s.\n", name, name)
	return nil
}

func filepathSafe(name string) bool {
	return name != "" && !strings.ContainsAny(name, `/\`) && name != "." && name != ".."
}

// pluginOf finds dir's plugin of that name.
func pluginOf(dir, name string) (opencode.Plugin, error) {
	for _, p := range opencode.PluginDirs(dir) {
		if p.Name == name {
			return p, nil
		}
	}
	return opencode.Plugin{}, fmt.Errorf("no plugin %q here", name)
}

// pluginKeys are a plugin's auth type and its keys.
func pluginKeys(p opencode.Plugin) (string, []struct{ Name, Description string }, error) {
	var m struct {
		Auth struct {
			Type string `json:"type"`
			Keys []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"keys"`
		} `json:"auth"`
	}
	b, err := os.ReadFile(filepath.Join(p.Dir, runtime.PluginManifest))
	if err != nil {
		return "none", nil, nil // a Claude Code plugin: nothing to connect
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return "", nil, err
	}
	var keys []struct{ Name, Description string }
	for _, k := range m.Auth.Keys {
		keys = append(keys, struct{ Name, Description string }{k.Name, k.Description})
	}
	return cmpOr(m.Auth.Type, "none"), keys, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// pluginConnect asks for a plugin's credentials, keeps them in the key
// store, and tells the daemon, which then starts its server with them.
func pluginConnect(ctx context.Context, data, dir, name string, in *os.File, stdout io.Writer) error {
	p, err := pluginOf(dir, name)
	if err != nil {
		return err
	}
	kind, keys, err := pluginKeys(p)
	if err != nil {
		return err
	}
	switch kind {
	case "none":
		fmt.Fprintf(stdout, "%s needs no credentials.\n", name)
		return nil
	case "oauth":
		if err := runtime.PluginOAuth(ctx, dir, name, func(u string) error { return openBrowser(stdout, u) }); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Connected %s; its tokens are kept in %s.\n", name, runtime.Secrets().Where())
		return notifyConnected(ctx, data, name)
	case "key":
	default:
		return fmt.Errorf("%s: auth type %q is not known", name, kind)
	}
	store := runtime.Secrets()
	lines := bufio.NewReader(in)
	for _, k := range keys {
		prompt := k.Name
		if k.Description != "" {
			prompt += " (" + k.Description + ")"
		}
		v, err := readSecret(in, lines, stdout, prompt+": ")
		if err != nil {
			return err
		}
		if v == "" {
			return fmt.Errorf("no value for %s: not connected", k.Name)
		}
		if err := store.Set(runtime.PluginSecretKey(p, k.Name), v); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "Connected %s; its credentials are kept in %s.\n", name, store.Where())
	return notifyConnected(ctx, data, name)
}

// pluginDisconnect forgets a plugin's credentials.
func pluginDisconnect(ctx context.Context, data, dir, name string, stdout io.Writer) error {
	p, err := pluginOf(dir, name)
	if err != nil {
		return err
	}
	_, keys, err := pluginKeys(p)
	if err != nil {
		return err
	}
	store := runtime.Secrets()
	for _, k := range append(keys, struct{ Name, Description string }{Name: "oauth"}) {
		if err := store.Delete(runtime.PluginSecretKey(p, k.Name)); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "Disconnected %s.\n", name)
	return notifyConnected(ctx, data, name)
}

// notifyConnected tells the daemon, if one runs, to start the plugin's
// server again with what is kept now.
func notifyConnected(ctx context.Context, data, name string) error {
	if _, err := daemon.ReadInfo(data); err != nil {
		return nil // none runs: the next one starts with what is kept
	}
	c, err := connect(ctx, data)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.PluginConnected(ctx, name)
}

// openBrowser shows the user url: printed, and opened in the browser
// where there is a way to.
func openBrowser(stdout io.Writer, u string) error {
	fmt.Fprintf(stdout, "Sign in in your browser. If it does not open, go to:\n  %s\n", u)
	var cmd *exec.Cmd
	switch goruntime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "linux":
		cmd = exec.Command("xdg-open", u)
	default:
		return nil
	}
	_ = cmd.Start() // the URL is printed, if it does not open
	go cmd.Wait()
	return nil
}

// readSecret reads a line without showing it, if in is a terminal.
func readSecret(in *os.File, lines *bufio.Reader, stdout io.Writer, prompt string) (string, error) {
	fmt.Fprint(stdout, prompt)
	restore, err := noEcho(in)
	if err == nil {
		// Interrupted, the terminal shows what is typed again.
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		done := make(chan struct{})
		go func() {
			select {
			case s := <-sig:
				restore()
				fmt.Fprintln(stdout)
				signal.Stop(sig)
				p, _ := os.FindProcess(os.Getpid())
				p.Signal(s)
			case <-done:
			}
		}()
		defer func() {
			signal.Stop(sig)
			close(done)
			restore()
			fmt.Fprintln(stdout)
		}()
	}
	line, err := lines.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
