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
	"github.com/r-hashi01/kairocode/internal/msg"
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
	ErrItemsCap   = api.ErrItemsCap
	ErrNotAnItem  = api.ErrNotAnItem
	ErrItemsNoGit = api.ErrItemsNoGit
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
	var b strings.Builder
	// What a station's checks may name: the checks declared, by name.
	if c, err := readChecks(dir); err == nil && c != nil && len(c.Defs) > 0 {
		names := make([]string, len(c.Defs))
		for i, d := range c.Defs {
			names[i] = d.Name
		}
		fmt.Fprintf(&b, "\n\nA station's checks name checks declared in kairocode.json, and only those: %s.", strings.Join(names, ", "))
	} else {
		b.WriteString("\n\nNo checks are declared in kairocode.json: a station has no checks.")
	}
	all, err := lines.List(dir)
	if err != nil || len(all) == 0 {
		b.WriteString("\nThe project has no standard lines (.kairocode/lines): put one together for the request, or do the work in this turn.")
		return b.String()
	}
	b.WriteString("\nThe project's standard lines:")
	for _, e := range all {
		if e.Err != nil {
			continue
		}
		fmt.Fprintf(&b, "\n- %s: %s (%s)", e.Name, e.Line.Description, stationsText(e.Line.Stations))
	}
	return b.String()
}

// lineCanRun checks the line a line call names or puts together, for the
// top folder of the repository the conversation's folder dir is in (as the
// item's worktree has it).
func lineCanRun(ctx context.Context, dir string, args json.RawMessage) error {
	if _, top, err := repoOf(ctx, dir); err == nil {
		dir = top
	}
	var in tools.ItemSpec
	if err := json.Unmarshal(args, &in); err != nil {
		return err
	}
	var line workflows.Line
	switch {
	case in.Line != "":
		l, err := lines.Read(dir, in.Line)
		if err != nil {
			return err
		}
		line = l
	case len(in.Stations) > 0:
		if err := json.Unmarshal(in.Stations, &line.Stations); err != nil {
			return fmt.Errorf("stations: %w", err)
		}
	default:
		return nil // the tool says what is missing
	}
	return checkLine(dir, line)
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
			fmt.Fprintf(&b, " for you (%d)", it.Waiting)
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
// call key: its worktree, then its session, whose first turn runs the line.
// Each step looks for what it would make first, so a start made again (a
// crash between) makes nothing twice. An error once the item is kept is
// not settled (tools.ErrNotSettled): the call is made again with its key,
// and finishes what it began. A line the worktree cannot run (its files not
// committed) ends the item, failed and cleaned up: that is settled.
func (r *Runtime) StartItem(ctx context.Context, conversation, key string, spec tools.ItemSpec) (api.Item, error) {
	name := itemSessionName(conversation, key)
	id := SessionID(name)
	row, err := r.Store.Item(ctx, id)
	if err == nil { // kept before: finished from where it was
		return r.finishItem(ctx, row)
	} else if !errors.Is(err, store.ErrNoItem) {
		return api.Item{}, notSettled(err)
	}
	dir, err := r.sessionDir(ctx, conversation)
	if err != nil {
		return api.Item{}, err
	}
	main, top, err := repoOf(ctx, dir)
	if err != nil {
		return api.Item{}, ErrItemsNoGit
	}
	if row, err = r.newItem(ctx, conversation, top, main, id, spec); err != nil {
		return api.Item{}, err
	}
	return r.finishItem(ctx, row)
}

// finishItem makes what item row has not got yet: its worktree, then its
// session, whose first turn runs the line. Each step looks for what it
// would make first, so done again (a crash between) it makes nothing
// twice. An error is not settled (tools.ErrNotSettled): it is done again.
// A line the worktree cannot run ends the item, failed and cleaned up:
// that is settled.
func (r *Runtime) finishItem(ctx context.Context, row store.ItemRow) (api.Item, error) {
	if row.Ended != "" { // ended as it started (failed): said, not started again
		return r.itemState(ctx, row), fmt.Errorf("the item %s %s", row.Name, row.Ended)
	}
	var line workflows.Line
	if err := json.Unmarshal(row.Def, &line); err != nil {
		return api.Item{}, err
	}
	if row.Dir == "" {
		dir, err := r.sessionDir(ctx, row.Of)
		if err != nil {
			return api.Item{}, notSettled(err)
		}
		if row, err = r.itemWorktree(ctx, dir, row); err != nil {
			return api.Item{}, notSettled(err)
		}
	}
	_, getErr := r.K.Get(ctx, row.ID)
	if row.Made && errors.Is(getErr, kairo.ErrUnknownRun) {
		// Made before, its record let go of since (a session closed or
		// failed, KeepFinished): never made again, which would run its line
		// twice. The item ends, closed; its worktree is left to the person.
		if err := r.Store.EndItem(ctx, row.ID, api.ItemClosed); err != nil {
			return api.Item{}, notSettled(err)
		}
		row.Ended = api.ItemClosed
		return r.itemState(ctx, row), fmt.Errorf("the item %s %s", row.Name, row.Ended)
	}
	if !row.Made && errors.Is(getErr, kairo.ErrUnknownRun) {
		// What the worktree has (HEAD's files) must run the line: else the
		// item ends here, failed.
		if err := checkLine(row.Dir, line); err != nil {
			why := "the worktree, from HEAD, cannot run the line (commit what it uses first): " + err.Error()
			if err := r.endItem(ctx, row, api.ItemFailed, true); err != nil {
				return api.Item{}, notSettled(err)
			}
			return api.Item{}, errors.New(why)
		}
		var conv workflows.SessionInput
		if info, err := r.K.Get(ctx, row.Of); err == nil {
			_ = json.Unmarshal(input(info), &conv)
		}
		request, err := r.Store.PutMessage(ctx, msg.Message{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: row.Request}}})
		if err != nil {
			return api.Item{}, notSettled(err)
		}
		if _, err := r.CreateSession(ctx, strings.TrimPrefix(row.ID, "s/"), workflows.SessionInput{Dir: row.Dir, Engine: conv.Engine, Model: conv.Model, Effort: conv.Effort,
			Item: &workflows.ItemRef{Of: row.Of, Line: row.Line, Run: &line, Request: request}}); err != nil {
			if _, gerr := r.K.Get(ctx, row.ID); gerr != nil { // not made by another at once
				return api.Item{}, notSettled(err)
			}
		}
		getErr = nil // made now
	}
	if getErr != nil {
		return api.Item{}, notSettled(getErr)
	}
	if !row.Made {
		if err := r.Store.SetItemMade(ctx, row.ID); err != nil {
			return api.Item{}, notSettled(err)
		}
		row.Made = true
		if afterItemSession != nil {
			afterItemSession(row.ID)
		}
	}
	return r.itemState(ctx, row), nil
}

// FinishItems finishes the starts of items a process stopped before
// finishing: kept, with no session yet (a person's /line has no call made
// again to finish it). The daemon does it as it starts.
func (r *Runtime) FinishItems(ctx context.Context) error {
	rows, err := r.Store.ItemsKept(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, row := range rows {
		if row.Made { // its session was made: finished, whatever came of it since
			if _, err := r.K.Get(ctx, row.ID); errors.Is(err, kairo.ErrUnknownRun) {
				_, _ = r.finishItem(ctx, row) // ends it, closed
			}
			continue
		}
		if _, err := r.finishItem(ctx, row); err != nil {
			errs = append(errs, fmt.Errorf("finishing the start of the item %s: %w", row.Name, err))
		}
	}
	return errors.Join(errs...)
}

// notSettled marks err as a start not finished: the call is made again.
func notSettled(err error) error { return fmt.Errorf("%w: %w", tools.ErrNotSettled, err) }

// itemWorktree makes item row's worktree, under the name kept for it, and
// notes it: one item at a time in this runtime (itemMu), so no two take
// one name.
func (r *Runtime) itemWorktree(ctx context.Context, dir string, row store.ItemRow) (store.ItemRow, error) {
	r.itemMu.Lock()
	defer r.itemMu.Unlock()
	wt, err := r.MakeWorktree(ctx, dir, row.Name)
	if err != nil {
		return row, err
	}
	base, _ := gitIn(ctx, dir, "symbolic-ref", "--short", "HEAD")
	start, _ := gitIn(ctx, wt.Dir, "rev-parse", "HEAD")
	if err := r.Store.SetItemWorktree(ctx, row.ID, wt.Dir, wt.Branch, base, start); err != nil {
		return row, err
	}
	row.Dir, row.Branch, row.Base, row.Start = wt.Dir, wt.Branch, base, start
	if afterItemWorktree != nil {
		afterItemWorktree(row.ID)
	}
	return row, nil
}

// newItem checks spec's line for the repository's top folder (as the
// item's worktree has it, wherever in it the conversation is), and keeps the
// item it starts, with the name its worktree is to have, before anything
// is made: a start made again makes it under that name. One item at a time
// is kept in this runtime (itemMu): two starts at once (two calls of one
// reply) take two names, and the cap counts each.
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
	// The worktree has HEAD's files: what the line uses must be there.
	if st, _ := gitIn(ctx, dir, "status", "--porcelain", "--", ".kairocode", ".opencode", ConfigFile); st != "" {
		return store.ItemRow{}, fmt.Errorf("the item's worktree starts from HEAD, and these are not committed: commit them first:\n%s", st)
	}
	def, err := json.Marshal(line)
	if err != nil {
		return store.ItemRow{}, err
	}
	r.itemMu.Lock()
	defer r.itemMu.Unlock()
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
	free := func(name string) bool {
		_, err := os.Stat(filepath.Join(r.worktreesOf(main), name))
		return !used[name] && errors.Is(err, os.ErrNotExist) && !branchExists(ctx, main, worktreeBranch+name)
	}
	name := spec.Name
	if name != "" {
		if !worktreeName.MatchString(name) {
			return store.ItemRow{}, ErrWorktreeName
		}
		if !free(name) {
			return store.ItemRow{}, fmt.Errorf("%s is taken (an item, a worktree or the branch %s%s): name it otherwise", name, worktreeBranch, name)
		}
	} else {
		base := line.Name
		if base == "" {
			base = "item"
		}
		for n := 1; ; n++ {
			if name = fmt.Sprintf("%s-%d", base, n); free(name) {
				break
			}
		}
	}
	return r.Store.AddItem(ctx, store.ItemRow{ID: id, Of: conversation, Line: line.Name, Def: def, Request: spec.Request,
		Name: name, Repo: main, Started: time.Now()})
}

// Items are the items conversation sent, the first first, as they are: it
// changes nothing (the items tool reads them).
func (r *Runtime) Items(ctx context.Context, conversation string) ([]api.Item, error) {
	rows, err := r.Store.ItemsOf(ctx, conversation)
	if err != nil {
		return nil, err
	}
	out := make([]api.Item, 0, len(rows))
	for _, row := range rows {
		out = append(out, r.itemState(ctx, row))
	}
	return out, nil
}

// TakeItems cleans up conversation's items whose branch was merged: taken
// (ADR 0043). /items and kairocode items do it as they list, and the
// daemon as it starts (TakeAllItems).
func (r *Runtime) TakeItems(ctx context.Context, conversation string) error {
	rows, err := r.Store.ItemsOf(ctx, conversation)
	if err != nil {
		return err
	}
	return r.takeItems(ctx, rows)
}

// TakeAllItems cleans up every item whose branch was merged.
func (r *Runtime) TakeAllItems(ctx context.Context) error {
	rows, err := r.Store.ItemsKept(ctx)
	if err != nil {
		return err
	}
	return r.takeItems(ctx, rows)
}

func (r *Runtime) takeItems(ctx context.Context, rows []store.ItemRow) error {
	var errs []error
	for _, row := range rows {
		if row.Ended != "" {
			continue
		}
		if s := r.itemState(ctx, row).State; s == api.ItemRunning || s == api.ItemWaiting || !r.merged(ctx, row) {
			continue
		}
		if err := r.endItem(ctx, row, api.ItemTaken, false); err != nil {
			err = fmt.Errorf("cleaning up the item %s, taken: %w", row.Name, err)
			r.logf("%v", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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
		// Once KairoCode is on a kairo with SignalID (kairo's ADR 0071;
		// the update waits: docs/milestones.md, future work), it makes this once. Until
		// then a process stopped between sending and keeping it as told
		// sends it again, once (ADR 0043, implementation).
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
			what := strings.TrimSpace(x.Tool + " " + firstLine(x.Subject))
			if x.Kind == api.KindQuestion {
				what = "a question: " + strings.TrimRight(firstLine(x.Subject), ":.")
			}
			text := fmt.Sprintf("[KairoCode] The item %s (line %s) waits for the person: %s. They answer it with /items, or kairocode approvals %s.",
				row.Name, line, what, row.Of)
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
	row, err := r.Store.Item(t, in.Session)
	if errors.Is(err, store.ErrNoItem) || err == nil && sameDir(row.Dir) != sameDir(in.Dir) {
		return workflows.LineCommitOutput{}, nil // not an item's worktree: nothing committed
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
			// Committed before a crash cut this off: HEAD is that commit.
			if head, err := gitIn(t, in.Dir, "rev-parse", "HEAD"); err == nil && head != row.Start {
				if subject, _ := gitIn(t, in.Dir, "log", "-1", "--format=%s"); subject == in.Message {
					short, _ := gitIn(t, in.Dir, "rev-parse", "--short", "HEAD")
					return workflows.LineCommitOutput{Commit: short, Note: "Committed to the branch: " + short + "."}, nil
				}
			}
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

// StartNamedItem starts an item down the standard line the person named
// (/line, kairocode line): nothing is asked, the person chose it. call
// names the request, so the same one made again starts no second item.
func (r *Runtime) StartNamedItem(ctx context.Context, conversation, call string, spec tools.ItemSpec) (api.Item, error) {
	if spec.Line == "" || len(spec.Stations) > 0 {
		return api.Item{}, errors.New("name a standard line: /line <name> <request>")
	}
	return r.StartItem(ctx, conversation, "person:"+call, spec)
}

// KeepLine writes the line item ran as the project's line name (/line
// keep): a new file in the conversation's folder, for the person to
// review and commit.
func (r *Runtime) KeepLine(ctx context.Context, conversation, item, name string) (string, error) {
	rows, err := r.Store.ItemsOf(ctx, conversation)
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		if row.ID != item && row.Name != item {
			continue
		}
		var line workflows.Line
		if err := json.Unmarshal(row.Def, &line); err != nil {
			return "", err
		}
		dir, err := r.sessionDir(ctx, conversation)
		if err != nil {
			return "", err
		}
		if err := lines.Write(dir, name, line); err != nil {
			return "", err
		}
		return filepath.Join(dir, lines.Path(name)), nil
	}
	return "", ErrNotAnItem
}
