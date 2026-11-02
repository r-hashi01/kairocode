package cli

import (
	"bufio"
	"cmp"
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
	"github.com/r-hashi01/kairocode/internal/plugins"
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
	usage := errors.New("usage: kairocode plugin [-dir DIR] [list | install <name[@marketplace], git URL or path> | update <name> | remove <name> | connect <name> | disconnect <name> | marketplace ...]")
	switch verb {
	case "", "list":
		return pluginList(ctx, *data, abs, stdout)
	case "install":
		if len(rest) != 1 {
			return usage
		}
		return pluginInstall(ctx, *data, abs, rest[0], os.Stdin, stdout)
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
	case "marketplace":
		return pluginMarketplace(ctx, rest, stdout)
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
	text := plugins.PluginsText(list)
	if more := plugins.AvailableText("kairocode plugin install"); more != "" {
		text += "\n" + more
	}
	_, err = io.WriteString(stdout, text)
	return err
}

// pluginInstall installs a plugin (ADR 0027): name or name@market of a
// marketplace, a git URL, or a folder. Its credentials are asked for
// next, if its marketplace says so and this is a terminal.
func pluginInstall(ctx context.Context, data, dir, arg string, in *os.File, stdout io.Writer) error {
	got, err := plugins.InstallPlugin(ctx, arg)
	if err != nil {
		return err
	}
	at := ""
	if got.Version != "" {
		at = " " + got.Version
	}
	fmt.Fprintf(stdout, "Installed %s%s from %s.\n", got.Name, at, got.From)
	if got.Auth == "none" {
		return nil
	}
	if got.ConnectNow && isTerminal(in) {
		return pluginConnect(ctx, data, dir, got.Name, in, stdout)
	}
	fmt.Fprintf(stdout, "It needs credentials: kairocode plugin connect %s\n", got.Name)
	return nil
}

// isTerminal says f is a terminal.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// pluginUpdate brings an installed plugin up to date.
func pluginUpdate(ctx context.Context, name string, stdout io.Writer) error {
	before, after, err := plugins.UpdatePlugin(ctx, name)
	if err != nil {
		return err
	}
	if before == after {
		fmt.Fprintf(stdout, "%s is up to date (%s).\n", name, cmp.Or(after, "no version"))
		return nil
	}
	fmt.Fprintf(stdout, "Updated %s: %s → %s. Turns under way go on with what they started with.\n", name, cmp.Or(before, "?"), cmp.Or(after, "?"))
	return nil
}

// pluginRemove removes an installed plugin; its credentials stay until
// disconnected.
func pluginRemove(name string, stdout io.Writer) error {
	if err := plugins.RemovePlugin(name); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Removed %s. Its credentials are kept until kairocode plugin disconnect %s.\n", name, name)
	return nil
}

// pluginMarketplace adds, lists, updates or removes a marketplace.
func pluginMarketplace(ctx context.Context, args []string, stdout io.Writer) error {
	usage := errors.New("usage: kairocode plugin marketplace [list | add <git URL, owner/repo or folder> | update <name> | remove <name>]")
	verb := "list"
	if len(args) > 0 {
		verb, args = args[0], args[1:]
	}
	if (verb == "list") != (len(args) == 0) || len(args) > 1 {
		return usage
	}
	switch verb {
	case "list":
		_, err := io.WriteString(stdout, plugins.MarketplacesText())
		return err
	case "add":
		m, err := plugins.AddMarketplace(ctx, args[0])
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Added the marketplace %s (%d plugins). kairocode plugin list shows them.\n", m.Name, len(m.Plugins))
		return nil
	case "update":
		before, after, err := plugins.UpdateMarketplace(ctx, args[0])
		if err != nil {
			return err
		}
		if before == after {
			fmt.Fprintf(stdout, "%s is up to date (%s).\n", args[0], after)
		} else {
			fmt.Fprintf(stdout, "Updated %s: %s → %s. kairocode plugin update <name> brings its plugins up to it.\n", args[0], before, after)
		}
		return nil
	case "remove":
		if err := plugins.RemoveMarketplace(args[0]); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Removed the marketplace %s. The plugins installed from it stay.\n", args[0])
		return nil
	}
	return usage
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
	b, err := os.ReadFile(filepath.Join(p.Dir, plugins.ManifestFile))
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
	return cmp.Or(m.Auth.Type, "none"), keys, nil
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
