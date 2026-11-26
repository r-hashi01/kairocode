package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/config"
	"github.com/r-hashi01/kairocode/internal/store"
)

// Storage retention (ADR 0045): what something present reaches is kept;
// what nothing reaches goes after a grace; some things are kept for a
// while only. The sweep only ever removes what is not reached, so one cut
// off is begun again from the start.

// retention is the user's retention settings, in days (CacheSize in GB).
type retention struct {
	Grace     int     `json:"grace"`
	Usage     int     `json:"usage"`
	Items     int     `json:"items"`
	Caches    int     `json:"caches"`
	CacheSize float64 `json:"cacheSize"`
	Off       bool    `json:"off"`
}

// minUsageDays keeps a week's limit countable whatever the setting says.
const minUsageDays = 8

// sweepEvery is how often the daemon sweeps by itself.
const sweepEvery = 24 * time.Hour

// retentionSettings is retention of the user's own settings (a project
// cannot shorten them), with the defaults for what it does not say.
func retentionSettings() retention {
	r := retention{Grace: 7, Usage: 90, Items: 180, Caches: 30, CacheSize: 5}
	l, ok, err := config.User()
	if !ok || err != nil {
		return r
	}
	var cfg struct {
		Retention retention `json:"retention"`
	}
	if l.Decode(&cfg) != nil {
		return r
	}
	set := cfg.Retention
	for _, f := range []struct {
		to *int
		v  int
	}{{&r.Grace, set.Grace}, {&r.Usage, set.Usage}, {&r.Items, set.Items}, {&r.Caches, set.Caches}} {
		if f.v > 0 {
			*f.to = f.v
		}
	}
	if set.CacheSize > 0 {
		r.CacheSize = set.CacheSize
	}
	r.Usage = max(r.Usage, minUsageDays)
	r.Off = set.Off
	return r
}

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

var (
	// hashRe finds the hashes of kc_message in text; treeRe, git's object
	// names (a snapshot's tree). Taking more than is meant only keeps more.
	hashRe = regexp.MustCompile(`sha256:[0-9a-f]{64}`)
	treeRe = regexp.MustCompile(`\b[0-9a-f]{40}(?:[0-9a-f]{24})?\b`)
)

// sweepRoots is what is present (ADR 0045, 1): the hashes and git object
// names in the records of every run kairo holds, and in the kc_ tables
// kept apart from them; and the runs there are. A new place that names a
// message outside kairo's records must be read here (TestSweepRootsCoverTables).
type sweepRoots struct {
	hashes map[string]bool
	trees  map[string]bool
	runs   map[string]bool
}

func (r *Runtime) sweepRoots(ctx context.Context) (*sweepRoots, error) {
	roots := &sweepRoots{hashes: map[string]bool{}, trees: map[string]bool{}, runs: map[string]bool{}}
	take := func(b []byte) {
		for _, h := range hashRe.FindAll(b, -1) {
			roots.hashes[string(h)] = true
		}
		for _, t := range treeRe.FindAll(b, -1) {
			roots.trees[string(t)] = true
		}
	}
	var walk func(info kairo.RunInfo) error
	walk = func(info kairo.RunInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		roots.runs[info.RunID] = true
		take(info.Input)
		take(info.Output)
		take(info.Meta)
		take([]byte(info.Error))
		if info.Workflow == "" {
			return nil // a call: no runs under it
		}
		children, err := r.K.Children(ctx, info.RunID)
		if err != nil {
			return err
		}
		for _, c := range children {
			if err := walk(c); err != nil {
				return err
			}
		}
		return nil
	}
	// Every root at once, in one read: paged by the run after the last
	// one seen, a page whose last run kairo removed meanwhile (a finished
	// tree past KeepFinished) would come back empty, and the roots after it,
	// the newest, would go unread.
	all, err := r.K.List(ctx, kairo.Filter{})
	if err != nil {
		return nil, err
	}
	for _, info := range all {
		if err := walk(info); err != nil {
			return nil, err
		}
	}
	if err := r.Store.RootTexts(ctx, func(b []byte) error { take(b); return nil }); err != nil {
		return nil, err
	}
	return roots, nil
}

// sweepMu lets one sweep run at a time.
var sweepMu sync.Mutex

// Sweep removes what nothing present reaches, and what is past its time
// (ADR 0045); with dry, it only says what it would remove.
func (r *Runtime) Sweep(ctx context.Context, dry bool) (api.SweepReport, error) {
	return r.sweep(ctx, dry, time.Now())
}

// sweepStep, if set, is called after each message the sweep removes: a
// crash test stops the process there.
var sweepStep func()

// sweep is Sweep as of now.
func (r *Runtime) sweep(ctx context.Context, dry bool, now time.Time) (api.SweepReport, error) {
	sweepMu.Lock()
	defer sweepMu.Unlock()
	// No fork is under way from what is present to the last removal: a
	// fork's new session holds what it names before the sweep looks
	// (forkMu).
	r.forkMu.Lock()
	defer r.forkMu.Unlock()
	if r.cfg.sweepHeld != nil {
		r.cfg.sweepHeld()
	}
	set := retentionSettings()
	rep := api.SweepReport{Dry: dry}
	roots, err := r.sweepRoots(ctx)
	if err != nil {
		return rep, fmt.Errorf("sweep: what is present: %w", err)
	}
	grace := now.Add(-days(set.Grace))
	// Each part goes on whatever another came to: each removes only what
	// is unreached or past its time, and one that cannot run here (no
	// sandbox for git, say) must not keep the others from running.
	var errs []error
	step := func(what string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("sweep: %s: %w", what, err))
		}
	}
	step("messages", r.sweepMessages(ctx, roots, grace, dry, &rep))
	step("snapshots", r.sweepSnapshots(ctx, roots, grace, dry, &rep))
	step("rows of sessions", r.sweepSessionRows(ctx, dry, &rep))
	if !dry {
		n, err := r.Store.SweepUsage(ctx, now.Add(-days(set.Usage)))
		rep.Usage = n
		step("usage", err)
	}
	step("items", r.sweepItems(ctx, now.Add(-days(set.Items)), dry, &rep))
	step("check caches", r.sweepCaches(now.Add(-days(set.Caches)), int64(set.CacheSize*(1<<30)), dry, &rep))
	if !dry && ctx.Err() == nil {
		step("its time", r.Store.SetSwept(ctx, "all", now))
	}
	return rep, errors.Join(errs...)
}

// sweepMessages removes the messages of the content kinds that no root
// reaches, through the hashes each reached message names, written last
// before grace.
func (r *Runtime) sweepMessages(ctx context.Context, roots *sweepRoots, grace time.Time, dry bool, rep *api.SweepReport) error {
	marked := map[string]bool{}
	queue := make([]string, 0, len(roots.hashes))
	for h := range roots.hashes {
		queue = append(queue, h)
	}
	for len(queue) > 0 {
		h := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if marked[h] {
			continue
		}
		marked[h] = true
		_, body, err := r.Store.Get(ctx, h)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		for _, n := range hashRe.FindAll(body, -1) {
			if !marked[string(n)] {
				queue = append(queue, string(n))
			}
		}
	}
	// The images given to engines as files (imageFile): each named by the
	// hash of the image's own message (its Ref). A cache: imageFile writes
	// one again when it is missing.
	imgs := filepath.Join(filepath.Dir(r.cfg.DB), "images")
	if entries, err := os.ReadDir(imgs); err == nil {
		for _, e := range entries {
			name, _, _ := strings.Cut(e.Name(), ".")
			info, err := e.Info()
			if err != nil || !info.Mode().IsRegular() || marked["sha256:"+name] || !info.ModTime().Before(grace) {
				continue
			}
			rep.Images++
			if !dry {
				if err := os.Remove(filepath.Join(imgs, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
			}
		}
	}
	unswept, err := r.Store.Unswept(ctx, grace)
	if err != nil {
		return err
	}
	for _, h := range unswept {
		if marked[h] {
			continue
		}
		if dry {
			if _, body, err := r.Store.Get(ctx, h); err == nil {
				rep.Messages++
				rep.MessageBytes += int64(len(body))
			}
			continue
		}
		n, gone, err := r.Store.SweepMessage(ctx, h, grace)
		if err != nil {
			return err
		}
		if gone {
			rep.Messages++
			rep.MessageBytes += n
			if sweepStep != nil {
				sweepStep()
			}
		}
	}
	return nil
}

// sweepSnapshots prunes each snapshot store of the trees no root names,
// older than grace. A store another borrows from (its alternates name it)
// is left as it is: the borrower's trees may need its objects, and it no
// longer grows (ADR 0037's addendum).
func (r *Runtime) sweepSnapshots(ctx context.Context, roots *sweepRoots, grace time.Time, dry bool, rep *api.SweepReport) error {
	base := filepath.Join(filepath.Dir(r.cfg.DB), "snapshot")
	entries, err := os.ReadDir(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var stores []string
	borrowed := map[string]bool{}
	for _, e := range entries {
		dir := filepath.Join(base, e.Name())
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "config")); err != nil {
			continue
		}
		stores = append(stores, dir)
		alt, _ := os.ReadFile(filepath.Join(dir, "objects", "info", "alternates"))
		for _, line := range strings.Fields(string(alt)) {
			borrowed[realPath(filepath.Dir(line))] = true
		}
	}
	trees := make([]string, 0, len(roots.trees))
	for t := range roots.trees {
		trees = append(trees, t)
	}
	slices.Sort(trees)
	for _, s := range stores {
		if borrowed[realPath(s)] {
			continue
		}
		if dry {
			rep.Stores++
			continue
		}
		freed, err := pruneStore(ctx, s, trees, grace)
		if err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
		rep.Stores++
		rep.StoreBytes += freed
	}
	return nil
}

// pruneStore keeps in store the trees of trees it has, and prunes what
// else is older than grace, holding the store to itself: a snapshot taken
// meanwhile may use an old object again (git freshens what it finds,
// rather than write it anew), which gc must not then remove.
func pruneStore(ctx context.Context, store string, trees []string, grace time.Time) (int64, error) {
	unlock, err := lockStore(store, true)
	if err != nil {
		return 0, err
	}
	defer unlock()
	before := dirSize(store)
	keep, err := keepTree(ctx, store, trees)
	if err != nil {
		return 0, err
	}
	if keep == "" {
		if _, err := storeGit(ctx, store, nil, "update-ref", "-d", "refs/kairocode/keep"); err != nil {
			return 0, err
		}
	} else if _, err := storeGit(ctx, store, nil, "update-ref", "refs/kairocode/keep", keep); err != nil {
		return 0, err
	}
	if _, err := storeGit(ctx, store, nil, "gc", "--quiet", "--prune="+grace.UTC().Format(time.RFC3339)); err != nil {
		return 0, err
	}
	return max(before-dirSize(store), 0), nil
}

// realPath is path with links resolved, as far as it can be.
func realPath(path string) string {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		return p
	}
	return filepath.Clean(path)
}

// keepTree is a tree in store that holds, as subtrees, those of trees
// that are trees there; "" when none is.
func keepTree(ctx context.Context, store string, trees []string) (string, error) {
	if len(trees) == 0 {
		return "", nil
	}
	out, err := storeGit(ctx, store, []byte(strings.Join(trees, "\n")+"\n"), "cat-file", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, line := range strings.Split(out, "\n") {
		name, kind, _ := strings.Cut(line, " ")
		if kind == "tree" {
			fmt.Fprintf(&b, "040000 tree %s\t%s\n", name, name)
		}
	}
	if b.Len() == 0 {
		return "", nil
	}
	t, err := storeGit(ctx, store, []byte(b.String()), "mktree")
	return strings.TrimSpace(t), err
}

// storeGit runs git on a snapshot store, with stdin, in the sandbox with
// the store writable (as snapshots are taken).
func storeGit(ctx context.Context, store string, stdin []byte, args ...string) (string, error) {
	return gitEnv(ctx, store, store, []string{"GIT_DIR=" + store}, stdin, append([]string{"-c", "gc.auto=0"}, args...)...)
}

// dirSize is how many bytes the regular files under dir hold.
func dirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// sweepSessionRows removes the rows about sessions kairo no longer holds:
// gone, their tree removed once KeepFinished passed after they closed.
func (r *Runtime) sweepSessionRows(ctx context.Context, dry bool, rep *api.SweepReport) error {
	rows, err := r.Store.SessionRows(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		session := sessionOfRun(row.Run)
		if session == "" {
			continue
		}
		if _, err := r.K.Get(ctx, session); !errors.Is(err, kairo.ErrUnknownRun) {
			if err != nil {
				return err
			}
			continue // there still
		}
		rep.Rows++
		if !dry {
			if err := r.Store.DeleteSessionRow(ctx, row); err != nil {
				return err
			}
		}
	}
	return nil
}

// sessionOfRun is the session ("s/<id>") a run is of, "" if none.
func sessionOfRun(run string) string {
	rest, ok := strings.CutPrefix(run, "s/")
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, "/")
	if id == "" {
		return ""
	}
	return "s/" + id
}

// sweepItems removes the items that ended before before, and their
// branches: not one of an item taken whose branch is not merged.
func (r *Runtime) sweepItems(ctx context.Context, before time.Time, dry bool, rep *api.SweepReport) error {
	rows, err := r.Store.ItemsEndedBefore(ctx, before)
	if err != nil {
		return err
	}
	for _, row := range rows {
		rep.Items++
		if dry {
			continue
		}
		var res api.ItemResult
		if b, err := r.Store.ItemResult(ctx, row.ID); err == nil && b != nil {
			_ = json.Unmarshal(b, &res)
		}
		if row.Branch != "" && row.Repo != "" && res.Commit != "" {
			// Only the branch as the item left it: one a person has gone on
			// with since, or made again by that name, is theirs.
			if tip, err := gitIn(ctx, row.Repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+row.Branch); err == nil && strings.TrimSpace(tip) == res.Commit {
				merged := row.Base != "" && func() bool {
					_, err := gitIn(ctx, row.Repo, "merge-base", "--is-ancestor", "refs/heads/"+row.Branch, "refs/heads/"+row.Base)
					return err == nil
				}()
				if row.Ended != "taken" || merged {
					if _, err := gitIn(ctx, row.Repo, "branch", "-D", row.Branch); err != nil {
						r.logf("sweep: the branch of item %s: %v", row.Name, err)
					} else {
						rep.Branches++
					}
				}
			}
		}
		if err := r.Store.DeleteItem(ctx, row.ID); err != nil {
			return err
		}
	}
	return nil
}

// sweepCaches removes the check caches not used since before, then the
// least recently used until they hold no more than size bytes.
func (r *Runtime) sweepCaches(before time.Time, size int64, dry bool, rep *api.SweepReport) error {
	if r.cfg.CacheDir == "" {
		return nil
	}
	base := filepath.Join(r.cfg.CacheDir, "checks")
	entries, err := os.ReadDir(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	type cache struct {
		dir  string
		used time.Time
		size int64
	}
	var caches []cache
	var total int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !e.IsDir() {
			continue
		}
		c := cache{filepath.Join(base, e.Name()), info.ModTime(), dirSize(filepath.Join(base, e.Name()))}
		caches = append(caches, c)
		total += c.size
	}
	slices.SortFunc(caches, func(a, b cache) int { return a.used.Compare(b.used) })
	for _, c := range caches {
		if !c.used.Before(before) && total <= size {
			break
		}
		rep.Caches++
		rep.CacheBytes += c.size
		total -= c.size
		if !dry {
			if err := os.RemoveAll(c.dir); err != nil {
				return err
			}
		}
	}
	return nil
}

// sweepDue sweeps in the background if it is on (the daemon's), and the
// last sweep was sweepEvery ago or more.
func (r *Runtime) sweepDue() {
	if !r.cfg.Sweep {
		return
	}
	r.background(func(ctx context.Context) {
		if retentionSettings().Off {
			return
		}
		last, err := r.Store.Swept(ctx, "all")
		if err != nil || time.Since(last) < sweepEvery {
			return
		}
		rep, err := r.Sweep(ctx, false)
		if err != nil {
			if ctx.Err() == nil {
				r.logf("%v", err)
			}
			return
		}
		r.logf("swept: %s", rep.String())
	})
}

// lockStore takes the lock a snapshot store's users share and its pruning
// holds alone (exclusive), and returns its release. One daemon owns a data
// directory; the lock is a flock on a file beside the store, so it holds
// between the goroutines of that process too (each OpenFile is an open
// file description of its own). On NFS, where Linux makes flock a lock of
// the process, it would not: a data directory there is not supported.
func lockStore(store string, exclusive bool) (func(), error) {
	f, err := os.OpenFile(store+".gc.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil // closing it lets go of the lock
}
