package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/config"
	"github.com/r-hashi01/kairocode/internal/lines"
	"github.com/r-hashi01/kairocode/internal/permission"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// Work items (ADR 0043): a conversation sends a request down a line; the
// item is a session of its own, in a worktree of its own, whose first turn
// runs the line. The conversation goes on, and is told when the item ends
// or waits for the person.

// onTheSpot is the permission subject of a line put together on the spot.
const onTheSpot = "(on the spot)"

// itemStale is how long an item done or stopped may be left before it is
// marked, to be taken or dropped.
const itemStale = 7 * 24 * time.Hour

// afterItemWorktree and afterItemSession, in tests, are called once an
// item's worktree, and then its session, are made (where a crash test
// stops the process).
var afterItemWorktree, afterItemSession func(id string)

// kindItemTold is what the store keeps of a notice sent to a conversation
// about an item: sent once.
const kindItemTold = "item-told"

var (
	ErrItemsCap   = errors.New("too many items running")
	ErrNotAnItem  = errors.New("no such item")
	ErrItemsNoGit = errors.New("a line runs in a worktree: the conversation's folder is in no git repository with a commit")
)

// itemSessionName is the id of the session an item started by the call
// key of conversation has (AGENTS.md, invariant 8): made from them alone,
// so a start made again after a crash finds the session it made.
func itemSessionName(conversation, key string) string {
	h := sha256.Sum256([]byte("kairocode/item:" + conversation + "\x00" + key))
	return hex.EncodeToString(h[:16])
}

// isItem says whether session is a work item's.
func (r *Runtime) isItem(ctx context.Context, session string) bool {
	if session == "" {
		return false
	}
	_, err := r.Store.Item(ctx, session)
	return err == nil
}

// standardLines is what the line tool's description adds: the project's
// lines, as they are when the turn starts (agent.load, invariant 2).
func standardLines(dir string) string {
	all, err := lines.List(dir)
	if err != nil || len(all) == 0 {
		return "\n\nThe project has no standard lines (.kairocode/lines): put one together for the request, or do the work in this turn."
	}
	var b strings.Builder
	b.WriteString("\n\nThe project's standard lines:")
	for _, e := range all {
		if e.Err != nil {
			continue
		}
		fmt.Fprintf(&b, "\n- %s: %s (%s)", e.Name, e.Line.Description, stationsText(e.Line.Stations))
	}
	return b.String()
}

// stationsText says a line's stations on one line.
func stationsText(st []workflows.Station) string {
	parts := make([]string, len(st))
	for i, s := range st {
		var what []string
		switch {
		case s.Agent != "":
			what = append(what, "agent "+s.Agent)
		case s.Flow != "":
			what = append(what, "flow "+s.Flow)
		}
		if len(s.Checks) > 0 {
			what = append(what, "checks "+strings.Join(s.Checks, ", "))
		}
		parts[i] = s.Name + " [" + strings.Join(what, "; ") + "]"
	}
	return strings.Join(parts, " → ")
}

// stationsOf says the stations of a line call put together on the spot.
func stationsOf(args json.RawMessage) string {
	var in tools.ItemSpec
	_ = json.Unmarshal(args, &in)
	var st []workflows.Station
	if json.Unmarshal(in.Stations, &st) != nil {
		return string(in.Stations)
	}
	return stationsText(st)
}

// onTheSpotSetting is lines.onTheSpot, the user's and the project's: the
// stricter of them ("deny" over "ask").
func onTheSpotSetting(dir string) permission.Action {
	out := permission.Ask
	layers, _ := config.Layers(dir)
	for _, l := range layers {
		var cfg struct {
			Lines struct {
				OnTheSpot string `json:"onTheSpot"`
			} `json:"lines"`
		}
		if l.Decode(&cfg) == nil && cfg.Lines.OnTheSpot == string(permission.Deny) {
			out = permission.Deny
		}
	}
	return out
}

// itemEnv is the items of the conversation a tool call is in.
type itemEnv struct {
	r            *Runtime
	conversation string
	key          string // the call's idempotency key
}

func (e itemEnv) Start(ctx context.Context, spec tools.ItemSpec) (string, error) {
	it, err := e.r.StartItem(ctx, e.conversation, e.key, spec)
	if err != nil {
		return "", err
	}
	line := it.Line
	if line == "" {
		line = "put together on the spot"
	}
	return fmt.Sprintf("Started the item %s on the line %s (%s), in the worktree %s on the branch %s. It runs behind this conversation: "+
		"you are told when it ends or needs the person. The items tool lists the items.", it.Name, line, strings.Join(it.Stations, " → "), it.Dir, it.Branch), nil
}

func (e itemEnv) List(ctx context.Context) (string, error) {
	items, err := e.r.Items(ctx, e.conversation)
	if err != nil {
		return "", err
	}
	return ItemsText(items), nil
}

// ItemsText says what items are doing, a line each.
func ItemsText(items []api.Item) string {
	if len(items) == 0 {
		return "No items: none was sent down a line from this conversation."
	}
	var b strings.Builder
	for _, it := range items {
		line := it.Line
		if line == "" {
			line = "(on the spot)"
		}
		fmt.Fprintf(&b, "%s  line %s  %s", it.Name, line, it.State)
		if it.Station != "" {
			fmt.Fprintf(&b, " at %s", it.Station)
		}
		if it.Why != "" {
			fmt.Fprintf(&b, " (%s)", firstLine(it.Why))
		}
		if it.Waiting > 0 {
			fmt.Fprintf(&b, ", %d waiting for you", it.Waiting)
		}
		fmt.Fprintf(&b, "  branch %s  tokens in %d out %d", it.Branch, it.Usage.Input, it.Usage.Output)
		if it.Stale {
			b.WriteString("  [left a week: take or drop it]")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// StartItem sends spec's request down its line from conversation, for the
// call key: its worktree, its session, its line's turn. Each step looks for
// what it would make first, so a start made again (a crash between) makes
// nothing twice.
func (r *Runtime) StartItem(ctx context.Context, conversation, key string, spec tools.ItemSpec) (api.Item, error) {
	dir, err := r.sessionDir(ctx, conversation)
	if err != nil {
		return api.Item{}, err
	}
	main, _, err := repoOf(ctx, dir)
	if err != nil {
		return api.Item{}, ErrItemsNoGit
	}
	name := itemSessionName(conversation, key)
	id := SessionID(name)
	row, err := r.Store.Item(ctx, id)
	if errors.Is(err, store.ErrNoItem) {
		if row, err = r.newItem(ctx, conversation, dir, main, id, spec); err != nil {
			return api.Item{}, err
		}
	} else if err != nil {
		return api.Item{}, err
	}
	var line workflows.Line
	if err := json.Unmarshal(row.Def, &line); err != nil {
		return api.Item{}, err
	}
	if row.Dir == "" {
		wt, err := r.MakeWorktree(ctx, dir, row.Name)
		if err != nil {
			return api.Item{}, err
		}
		base, _ := gitIn(ctx, dir, "symbolic-ref", "--short", "HEAD")
		start, _ := gitIn(ctx, wt.Dir, "rev-parse", "HEAD")
		if err := r.Store.SetItemWorktree(ctx, id, wt.Dir, wt.Branch, base, start); err != nil {
			return api.Item{}, err
		}
		row.Dir, row.Branch, row.Base, row.Start = wt.Dir, wt.Branch, base, start
		if afterItemWorktree != nil {
			afterItemWorktree(id)
		}
	}
	if _, err := r.K.Get(ctx, id); errors.Is(err, kairo.ErrUnknownRun) {
		var conv workflows.SessionInput
		if info, err := r.K.Get(ctx, conversation); err == nil {
			_ = json.Unmarshal(input(info), &conv)
		}
		if _, err := r.CreateSession(ctx, name, workflows.SessionInput{Dir: row.Dir, Engine: conv.Engine, Model: conv.Model, Effort: conv.Effort,
			Item: &workflows.ItemRef{Of: conversation, Line: row.Line}}); err != nil {
			return api.Item{}, err
		}
		if afterItemSession != nil {
			afterItemSession(id)
		}
	} else if err != nil {
		return api.Item{}, err
	}
	if n, err := r.sent(ctx, id); err != nil {
		return api.Item{}, err
	} else if n == 0 {
		if _, err := r.RunLine(ctx, id, line, row.Request); err != nil {
			return api.Item{}, fmt.Errorf("the item's worktree (from HEAD) cannot run the line: %w", err)
		}
	}
	return r.itemState(ctx, row), nil
}

// newItem checks spec's line for the conversation's folder, and keeps the
// item it starts, with the name its worktree is to have, before anything
// is made: a start made again makes it under that name.
func (r *Runtime) newItem(ctx context.Context, conversation, dir, main, id string, spec tools.ItemSpec) (store.ItemRow, error) {
	var line workflows.Line
	if spec.Line != "" {
		l, err := lines.Read(dir, spec.Line)
		if err != nil {
			return store.ItemRow{}, err
		}
		line = l
	} else if err := json.Unmarshal(spec.Stations, &line.Stations); err != nil {
		return store.ItemRow{}, fmt.Errorf("stations: %w", err)
	}
	if err := checkLine(dir, line); err != nil {
		return store.ItemRow{}, err
	}
	// The stop-gap of ADR 0043 until a queue: so many at once in a
	// repository.
	kept, err := r.Store.ItemsIn(ctx, main)
	if err != nil {
		return store.ItemRow{}, err
	}
	var running []string
	for _, k := range kept {
		if s := r.itemState(ctx, k).State; s == api.ItemRunning || s == api.ItemWaiting {
			running = append(running, k.Name)
		}
	}
	if len(running) >= api.MaxItemsRunning {
		return store.ItemRow{}, fmt.Errorf("%w: %d run in this repository (%s); wait for one to end", ErrItemsCap, len(running), strings.Join(running, ", "))
	}
	used, err := r.Store.ItemNames(ctx, main)
	if err != nil {
		return store.ItemRow{}, err
	}
	name := spec.Name
	if name != "" {
		if !worktreeName.MatchString(name) {
			return store.ItemRow{}, ErrWorktreeName
		}
		if used[name] {
			return store.ItemRow{}, fmt.Errorf("an item named %s was sent before: name it otherwise", name)
		}
	} else {
		base := line.Name
		if base == "" {
			base = "item"
		}
		for n := 1; ; n++ {
			name = fmt.Sprintf("%s-%d", base, n)
			if _, err := os.Stat(filepath.Join(r.worktreesOf(main), name)); !used[name] && errors.Is(err, os.ErrNotExist) && !branchExists(ctx, main, worktreeBranch+name) {
				break
			}
		}
	}
	def, err := json.Marshal(line)
	if err != nil {
		return store.ItemRow{}, err
	}
	return r.Store.AddItem(ctx, store.ItemRow{ID: id, Of: conversation, Line: line.Name, Def: def, Request: spec.Request,
		Name: name, Repo: main, Started: time.Now()})
}

// Items are the items conversation sent, the first first. Items whose
// branch was merged are taken, and cleaned up, as they are listed.
func (r *Runtime) Items(ctx context.Context, conversation string) ([]api.Item, error) {
	rows, err := r.Store.ItemsOf(ctx, conversation)
	if err != nil {
		return nil, err
	}
	out := make([]api.Item, 0, len(rows))
	for _, row := range rows {
		it := r.itemState(ctx, row)
		if row.Ended == "" && it.State != api.ItemRunning && it.State != api.ItemWaiting && r.merged(ctx, row) {
			if err := r.endItem(ctx, row, api.ItemTaken, false); err == nil {
				row.Ended = api.ItemTaken
				it = r.itemState(ctx, row)
			} else {
				r.logf("cleaning up the item %s, taken: %v", row.Name, err)
			}
		}
		out = append(out, it)
	}
	return out, nil
}

// itemState is what an item is doing, from its line's turn.
func (r *Runtime) itemState(ctx context.Context, row store.ItemRow) api.Item {
	it := api.Item{ID: row.ID, Of: row.Of, Name: row.Name, Line: row.Line, Request: row.Request, Dir: row.Dir, Branch: row.Branch,
		State: api.ItemRunning, Started: row.Started}
	var line workflows.Line
	_ = json.Unmarshal(row.Def, &line)
	it.Stations = []string{}
	for _, s := range line.Stations {
		it.Stations = append(it.Stations, s.Name)
	}
	if row.Ended != "" {
		it.State = row.Ended
	}
	run, ok, err := r.TurnRun(ctx, row.ID, 0)
	if err != nil || !ok {
		return it
	}
	if !run.Finished() {
		if row.Ended == "" {
			if p, _ := r.Pending(ctx, row.ID); len(p) > 0 {
				it.State, it.Waiting = api.ItemWaiting, len(p)
			}
		}
		return it
	}
	out, err := r.WaitTurn(ctx, row.ID, 0)
	switch {
	case err != nil:
		if row.Ended == "" {
			it.State = api.ItemFailed
		}
		it.Why = err.Error()
	default:
		it.Steps, it.Usage = out.Steps, out.Usage
		if row.Ended == "" {
			it.State = api.ItemDone
		}
		if out.Stopped != "" {
			if row.Ended == "" {
				it.State = api.ItemStopped
			}
			it.Why = out.Stopped
			if n := len(out.Line); n > 0 {
				it.Station, it.Why = out.Line[n-1].Name, out.Line[n-1].Why
			}
		}
	}
	if row.Ended == "" && time.Since(run.Updated) > itemStale {
		it.Stale = true
	}
	return it
}

// ItemsPending are the requests the items of conversation wait for, each
// naming its item (ADR 0043): answered as any request is, by the person.
func (r *Runtime) ItemsPending(ctx context.Context, conversation string) ([]Pending, error) {
	rows, err := r.Store.ItemsOf(ctx, conversation)
	if err != nil {
		return nil, err
	}
	var out []Pending
	for _, row := range rows {
		if row.Ended != "" {
			continue
		}
		p, err := r.Pending(ctx, row.ID)
		if err != nil {
			return nil, err
		}
		for _, x := range p {
			x.Item = row.Name
			out = append(out, x)
		}
	}
	return out, nil
}

// tellItem tells the conversation of item session what came to it since
// it was last told: each request it waits for, and its line's end. Each
// is sent once: kept as told once sent (a process stopped between sends
// it again, once).
func (r *Runtime) tellItem(ctx context.Context, session string) error {
	row, err := r.Store.Item(ctx, session)
	if errors.Is(err, store.ErrNoItem) {
		return nil
	} else if err != nil {
		return err
	}
	if row.Ended != "" {
		return nil
	}
	it := r.itemState(ctx, row)
	line := row.Line
	if line == "" {
		line = "put together on the spot"
	}
	tell := func(what, text string) error {
		marker := store.Hash(kindItemTold, []byte(row.ID+"\x00"+what))
		if _, _, err := r.Store.Get(ctx, marker); err == nil {
			return nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if _, err := r.Send(ctx, row.Of, text); err != nil {
			return fmt.Errorf("telling %s of the item %s: %w", row.Of, row.Name, err)
		}
		_, err := r.Store.Put(ctx, kindItemTold, []byte(row.ID+"\x00"+what))
		return err
	}
	switch it.State {
	case api.ItemWaiting:
		p, err := r.Pending(ctx, row.ID)
		if err != nil {
			return err
		}
		var errs []error
		for _, x := range p {
			text := fmt.Sprintf("[KairoCode] The item %s (line %s) waits for the person: %s %s. They answer it with /items, or kairocode approvals %s.",
				row.Name, line, x.Tool, firstLine(x.Subject), row.Of)
			errs = append(errs, tell("wait\x00"+x.Key, text))
		}
		return errors.Join(errs...)
	case api.ItemDone, api.ItemStopped, api.ItemFailed:
		var text string
		if out, err := r.WaitTurn(ctx, row.ID, 0); err == nil {
			text, _ = r.Reply(ctx, out.Reply)
		}
		if text == "" {
			text = it.Why
		}
		return tell("end", fmt.Sprintf("[KairoCode] The item %s (line %s) is %s, on the branch %s:\n\n%s", row.Name, line, it.State, row.Branch, text))
	}
	return nil
}

// merged says whether item row's work was taken: its branch has commits
// of its own, and they are in the branch the item started from.
func (r *Runtime) merged(ctx context.Context, row store.ItemRow) bool {
	if row.Branch == "" || row.Base == "" || row.Start == "" || !branchExists(ctx, row.Repo, row.Branch) {
		return false
	}
	tip, err := gitIn(ctx, row.Repo, "rev-parse", "refs/heads/"+row.Branch)
	if err != nil || tip == row.Start {
		return false // nothing done on it
	}
	_, err = gitIn(ctx, row.Repo, "merge-base", "--is-ancestor", tip, "refs/heads/"+row.Base)
	return err == nil
}

// lineCommitAction commits the worktree of an item whose line passed
// (ADR 0043), as git_commit does: done again after the commit was made, it
// finds nothing to commit, and makes no second one. Not an item's session
// (a line run elsewhere), it commits nothing.
func (r *Runtime) lineCommitAction(t *kairo.TaskContext, in workflows.LineCommitInput) (workflows.LineCommitOutput, error) {
	if _, err := r.Store.Item(t, in.Session); errors.Is(err, store.ErrNoItem) {
		return workflows.LineCommitOutput{}, nil
	} else if err != nil {
		return workflows.LineCommitOutput{}, err
	}
	root, err := os.OpenRoot(in.Dir)
	if err != nil {
		return workflows.LineCommitOutput{Note: "Not committed: " + err.Error()}, nil
	}
	defer root.Close()
	env := tools.Env{Root: root, Dir: in.Dir}
	args, _ := json.Marshal(map[string]string{"message": in.Message})
	p := tools.GitCommitTool.Plan(t, env, args)
	if p.Fail != nil {
		if strings.HasPrefix(p.Fail.Output, "nothing to commit") {
			return workflows.LineCommitOutput{Note: "Nothing left to commit on the branch: the stations committed their work, or changed nothing."}, nil
		}
		return workflows.LineCommitOutput{Note: "Not committed: " + p.Fail.Output}, nil
	}
	res := tools.GitCommitTool.Apply(t, env, p.Data)
	if res.Unknown {
		return workflows.LineCommitOutput{}, kairo.Unknown(errors.New(res.Output))
	}
	if res.IsError {
		return workflows.LineCommitOutput{Note: "Not committed: " + res.Output}, nil
	}
	head, _ := gitIn(t, in.Dir, "rev-parse", "--short", "HEAD")
	return workflows.LineCommitOutput{Commit: head, Note: "Committed to the branch: " + head + "."}, nil
}

// DropItem drops item id of conversation: cleaned up, its worktree removed
// (with changes not committed, only if force), its branch deleted once
// merged.
func (r *Runtime) DropItem(ctx context.Context, conversation, item string, force bool) error {
	rows, err := r.Store.ItemsOf(ctx, conversation)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.ID == item || row.Name == item {
			if row.Ended != "" {
				return nil
			}
			return r.endItem(ctx, row, api.ItemDropped, force)
		}
	}
	return ErrNotAnItem
}

// endItem cleans item row up, as how (taken, dropped): its session
// closed, its worktree removed. Its row stays, with how it ended.
func (r *Runtime) endItem(ctx context.Context, row store.ItemRow, how string, force bool) error {
	if run, ok, err := r.TurnRun(ctx, row.ID, 0); err == nil && ok && !run.Finished() {
		if err := r.K.Cancel(ctx, run.RunID); err != nil {
			return err
		}
	}
	if row.Dir != "" {
		if _, err := os.Stat(row.Dir); err == nil {
			if _, err := r.RemoveWorktree(ctx, row.Dir, force); err != nil {
				return err
			}
		}
	}
	if err := r.CloseSession(ctx, row.ID); err != nil && !errors.Is(err, kairo.ErrUnknownRun) {
		r.logf("closing the item %s: %v", row.Name, err)
	}
	return r.Store.EndItem(ctx, row.ID, how)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
