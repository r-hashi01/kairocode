package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/r-hashi01/kairocode/internal/runtime"
	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// lineCommand sends a request down a standard line the person names (ADR
// 0043), from a session (-session), or from a new one in the folder that
// stays open to be told of the item. With -keep, it keeps the line an item
// ran as a line of the project.
func lineCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("line", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", ".", "working directory")
	data := fs.String("data", runtime.DefaultDataDir(), "data directory")
	session := fs.String("session", "", "the conversation that sends it (default: a new session in the folder)")
	name := fs.String("name", "", "the item's worktree and branch name (default: <line>-<n>)")
	keep := fs.String("keep", "", "keep the line this item ran as .kairocode/lines/<name>.json")
	prov := fs.String("provider", os.Getenv("KAIROCODE_ENGINE"), "with no -session: who runs the model, as run's")
	model := fs.String("model", os.Getenv("KAIROCODE_MODEL"), "with no -session: the model, as run's")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := connect(ctx, *data)
	if err != nil {
		return err
	}
	defer c.Close()
	if *keep != "" {
		if *session == "" || fs.NArg() != 1 {
			return errors.New(`usage: kairocode line -keep <item> -session ID <name>`)
		}
		path, err := c.KeepLine(ctx, sessionArg(*session), *keep, fs.Arg(0))
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "kept as %s: review it, and commit it to make it the project's\n", path)
		return nil
	}
	if fs.NArg() < 2 {
		return errors.New(`usage: kairocode line [-session ID] <line> "<request>"`)
	}
	s := sessionArg(*session)
	if *session == "" {
		abs, err := filepath.Abs(*dir)
		if err != nil {
			return err
		}
		if s, err = c.CreateSession(ctx, workflows.SessionInput{Dir: abs, Engine: *prov, Model: *model}); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "session %s (told of the item; kairocode items %s)\n", s, s)
	}
	it, err := c.StartItem(ctx, s, tools.ItemSpec{Line: fs.Arg(0), Request: strings.Join(fs.Args()[1:], " "), Name: *name})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "item %s on the line %s (%s): worktree %s, branch %s\n", it.Name, it.Line, strings.Join(it.Stations, " → "), it.Dir, it.Branch)
	return nil
}

// itemsCommand lists the work items a session sent (ADR 0043), or drops
// one.
func itemsCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("items", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", runtime.DefaultDataDir(), "data directory")
	asJSON := fs.Bool("json", false, "as JSON")
	drop := fs.String("drop", "", "drop this item: its worktree removed, its session closed")
	force := fs.Bool("force", false, "with -drop: though its worktree has changes not committed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: kairocode items [-json] [-drop ITEM [-force]] <session>")
	}
	c, err := connect(ctx, *data)
	if err != nil {
		return err
	}
	defer c.Close()
	s := sessionArg(fs.Arg(0))
	if *drop != "" {
		if err := c.DropItem(ctx, s, *drop, *force); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "dropped %s\n", *drop)
		return nil
	}
	items, err := c.Items(ctx, s)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(items)
	}
	_, err = fmt.Fprint(stdout, runtime.ItemsText(items))
	return err
}

// sessionArg is a session named on the command line, with or without its
// "s/".
func sessionArg(s string) string {
	if s == "" || strings.HasPrefix(s, "s/") {
		return s
	}
	return runtime.SessionID(s)
}
