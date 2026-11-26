package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/sandbox/sandboxtest"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

const month = 30 * 24 * time.Hour

// sweepSession is a runtime with one live session in a folder of its own
// that has had a turn writing a.txt: its messages and snapshots are what
// something present reaches.
func sweepSession(t *testing.T, db string) (*Runtime, string, string, workflows.TurnOutput) {
	t.Helper()
	ws := repo(t)
	putFiles(t, ws, map[string]string{ConfigFile: `{"permission": {"write": "allow"}}`})
	r := open(t, db, &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{writeCall("a.txt", "a\n"), {Text: "wrote a"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := r.CreateSession(ctx, "live", workflows.SessionInput{Dir: ws})
	if err != nil {
		t.Fatal(err)
	}
	n, err := r.Send(ctx, s, "write a")
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	if out.Before == "" || out.After == "" || out.Before == out.After {
		t.Fatalf("no snapshots: %+v", out)
	}
	return r, ws, s, out
}

func present(t *testing.T, r *Runtime, hash string) bool {
	t.Helper()
	_, _, err := r.Store.Get(context.Background(), hash)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

func hasObject(r *Runtime, ws, name string) bool {
	_, err := storeGit(context.Background(), r.snapshotStore(ws), nil, "cat-file", "-e", name)
	return err == nil
}

// orphanTree writes into ws's snapshot store a tree no record names.
func orphanTree(t *testing.T, r *Runtime, ws string) string {
	t.Helper()
	ctx := context.Background()
	st := r.snapshotStore(ws)
	blob, err := storeGit(ctx, st, []byte("nobody's\n"), "hash-object", "-w", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := storeGit(ctx, st, []byte("100644 blob "+strings.TrimSpace(blob)+"\torphan.txt\n"), "mktree")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(tree)
}

// What something present reaches stays, whatever its age; what nothing
// reaches goes once its grace has passed, not before (ADR 0045).
func TestSweepKeepsWhatIsReached(t *testing.T) {
	t.Parallel()
	r, ws, s, out := sweepSession(t, filepath.Join(t.TempDir(), "k.db"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Reached through a kept flow run: a history, the message it lists,
	// and an image that message names.
	keptImg, _ := r.Store.Put(ctx, store.KindImage, []byte("kept image"))
	keptMsg, _ := r.Store.PutMessage(ctx, msg.Message{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: "see " + keptImg}}})
	keptHist, err := r.Store.Extend(ctx, "", []string{keptMsg})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Store.PutFlowRun(ctx, store.FlowRunRow{ID: "f/kept", Dir: ws, Name: "kept", Status: "done",
		Started: time.Now(), Ended: time.Now(), Body: []byte(`{"head": "` + keptHist + `"}`)}); err != nil {
		t.Fatal(err)
	}
	// Reached by nothing.
	lostImg, _ := r.Store.Put(ctx, store.KindImage, []byte("lost image"))
	lostMsg, _ := r.Store.PutMessage(ctx, msg.Message{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: "see " + lostImg}}})
	lostHist, _ := r.Store.Extend(ctx, "", []string{lostMsg})
	lostTree := orphanTree(t, r, ws)
	// The files images were given to engines as.
	imgs := filepath.Join(filepath.Dir(r.cfg.DB), "images")
	os.MkdirAll(imgs, 0o700)
	for _, h := range []string{keptImg, lostImg} {
		if err := os.WriteFile(filepath.Join(imgs, strings.TrimPrefix(h, "sha256:")+".png"), []byte("png"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A marker is never swept.
	marker, _ := r.Store.Put(ctx, kindLineTold, []byte("a line's stop"))
	// Rows of a session that is not there, and of one that is.
	if err := r.Store.SetClaudeProc(ctx, "s/gone", 1); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.SetClaudeProc(ctx, s, 2); err != nil {
		t.Fatal(err)
	}

	// Now: all within their grace.
	rep, err := r.sweep(ctx, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Messages != 0 || !present(t, r, lostMsg) || !hasObject(r, ws, lostTree) {
		t.Fatalf("removed within the grace: %+v", rep)
	}
	if rep.Rows != 1 {
		t.Fatalf("rows of the session gone: %+v", rep)
	}

	// A month on: what nothing reaches is gone.
	if rep, err = r.sweep(ctx, true, time.Now().Add(month)); err != nil || rep.Messages != 3 {
		t.Fatalf("dry run: %+v %v", rep, err)
	}
	if !present(t, r, lostMsg) {
		t.Fatal("a dry run removed")
	}
	if rep, err = r.sweep(ctx, false, time.Now().Add(month)); err != nil {
		t.Fatal(err)
	}
	if rep.Messages != 3 || rep.Stores != 1 {
		t.Fatalf("report %+v", rep)
	}
	for _, h := range []string{lostImg, lostMsg, lostHist} {
		if present(t, r, h) {
			t.Errorf("%s not swept", h)
		}
	}
	for _, h := range []string{keptImg, keptMsg, keptHist, marker} {
		if !present(t, r, h) {
			t.Errorf("%s swept, though reached", h)
		}
	}
	if hasObject(r, ws, lostTree) {
		t.Error("the orphan tree was not pruned")
	}
	if rep.Images != 1 || exists(filepath.Join(imgs, strings.TrimPrefix(lostImg, "sha256:")+".png")) ||
		!exists(filepath.Join(imgs, strings.TrimPrefix(keptImg, "sha256:")+".png")) {
		t.Errorf("image files: %+v", rep)
	}
	if !hasObject(r, ws, out.Before) || !hasObject(r, ws, out.After) {
		t.Fatal("a snapshot a turn names was pruned")
	}
	// The live session reads as it was, and its turn can be undone.
	if conv := strings.Join(conversation(t, r, s), "\n"); !strings.Contains(conv, "assistant: wrote a") {
		t.Fatalf("conversation:\n%s", conv)
	}
	n, err := r.Undo(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "a.txt")); err == nil {
		t.Fatal("undone after the sweep, a.txt is still there")
	}
}

// A store another borrows from is not pruned: the borrower's trees may
// need its objects (ADR 0045, 2).
func TestSweepSharedStores(t *testing.T) {
	t.Parallel()
	r, ws, _, _ := sweepSession(t, filepath.Join(t.TempDir(), "k.db"))
	ctx := context.Background()
	lost := orphanTree(t, r, ws)
	borrower := filepath.Join(filepath.Dir(r.cfg.DB), "snapshot", "borrower")
	if err := os.MkdirAll(borrower, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := storeGit(ctx, borrower, nil, "init", "--bare", "--quiet", borrower); err != nil {
		t.Fatal(err)
	}
	if err := addAlternate(borrower, filepath.Join(r.snapshotStore(ws), "objects")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.sweep(ctx, false, time.Now().Add(month)); err != nil {
		t.Fatal(err)
	}
	if !hasObject(r, ws, lost) {
		t.Fatal("a store another borrows from was pruned")
	}
}

// The usage ledger is kept retention.usage days (90), then goes.
func TestUsageRetention(t *testing.T) {
	t.Parallel()
	r, _, _, _ := sweepSession(t, filepath.Join(t.TempDir(), "k.db"))
	ctx := context.Background()
	if rep, err := r.sweep(ctx, false, time.Now().Add(month)); err != nil || rep.Usage != 0 {
		t.Fatalf("a month on: %+v %v", rep, err)
	}
	if rep, err := r.sweep(ctx, false, time.Now().Add(4*month)); err != nil || rep.Usage != 2 {
		t.Fatalf("four months on: %+v %v", rep, err)
	}
}

// An item's result is kept with it once it ends: its stations, who
// reviewed, and its change; ended items go after retention.items with
// their branches (ADR 0045, 3 and 4).
func TestItemResultKept(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	ws := itemRepo(t, "")
	putFiles(t, ws, map[string]string{".kairocode/lines/reviewed.json": `{"stations": [{"name": "build", "agent": "build"}, {"name": "review", "review": "person", "back": "build"}]}`})
	gitT(t, ws, "add", "-A")
	gitT(t, ws, "commit", "-q", "-m", "a reviewed line")
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Steps: buildWrites("a.txt")})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "conv", workflows.SessionInput{Dir: ws})
	it, err := r.StartNamedItem(ctx, s, "c1", tools.ItemSpec{Line: "reviewed", Request: "write a"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		p := waitReview(t, ctx, r, it.ID, 0)
		if p == nil {
			t.Fatal("no review")
		}
		if err := r.Approve(ctx, p.Key, i == 1, "write it again"); err != nil {
			t.Fatal(err)
		}
	}
	waitItem(t, ctx, r, s, api.ItemDone)
	if err := r.DropItem(ctx, s, it.Name, true); err != nil {
		t.Fatal(err)
	}
	it = waitItem(t, ctx, r, s, api.ItemDropped)
	res := it.Result
	if res == nil || res.How != api.ItemDropped || res.SentBack != 1 || res.Lines == 0 || res.Sum == "" || res.Usage.Output == 0 {
		t.Fatalf("result %+v", res)
	}
	var sent *workflows.StationResult
	for i := range res.Stations {
		if res.Stations[i].Status == workflows.StationSentBack {
			sent = &res.Stations[i]
		}
	}
	if sent == nil || sent.By != workflows.ReviewPerson || sent.Why != "write it again" {
		t.Fatalf("the send-back: %+v", res.Stations)
	}
	gitT(t, ws, "rev-parse", "--verify", "kairo/"+it.Name)

	// Half a year on, the item and its branch (dropped) are gone.
	rep, err := r.sweep(ctx, false, time.Now().Add(7*month))
	if err != nil || rep.Items != 1 || rep.Branches != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
	if items, _ := r.Items(ctx, s); len(items) != 0 {
		t.Fatalf("items left: %+v", items)
	}
	if out, err := exec.Command("git", "-C", ws, "rev-parse", "--verify", "--quiet", "kairo/"+it.Name).Output(); err == nil {
		t.Fatalf("the branch is still there: %s", out)
	}
}

// A kc_ table that may name a message must be one the sweep reads its
// roots from (ADR 0045): a new one is classified here, or the sweep would
// delete what it names.
func TestSweepRootsCoverTables(t *testing.T) {
	t.Parallel()
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	tables, err := r.Store.Tables(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Read by Store.RootTexts.
	roots := []string{"kc_flow_run", "kc_item", "kc_switch"}
	// Naming no message: settings, runs, counts and times; outside events,
	// whose values come from outside (ADR 0047).
	none := []string{"kc_claude_proc", "kc_engine_session", "kc_event", "kc_event_wait", "kc_kept", "kc_line", "kc_mcp_call", "kc_mcp_grant",
		"kc_message", "kc_provider_pref", "kc_sweep", "kc_usage"}
	for _, tb := range tables {
		if !slices.Contains(roots, tb) && !slices.Contains(none, tb) {
			t.Errorf("the table %s is not classified for the sweep: read it in Store.RootTexts, or list it as naming no message", tb)
		}
	}
}

// Killed in the middle of a sweep, nothing reached is removed, and the
// next sweep finishes (ADR 0045, 5).
func TestCrashDuringSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("crash test")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "k.db")
	r, _, s, _ := sweepSession(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var lost []string
	for i := range 5 {
		h, _ := r.Store.PutMessage(ctx, msg.Message{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: strings.Repeat("x", i+1)}}})
		lost = append(lost, h)
	}
	r.Close()

	step := filepath.Join(dir, "step")
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "KC_CRASH_SWEEP_CHILD=1", "KC_CRASH_DB="+db, "KC_CRASH_STEP="+step)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, step, cmd) // which waits for the child, and so reaps it
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, ctx, "the child is gone", func() bool { return syscall.Kill(cmd.Process.Pid, 0) != nil })

	r = open(t, db, &provider.Fake{HoldAt: -1})
	left := 0
	for _, h := range lost {
		if present(t, r, h) {
			left++
		}
	}
	if left == 0 || left == len(lost) {
		t.Fatalf("killed after one removal, %d of %d left", left, len(lost))
	}
	if _, err := r.sweep(ctx, false, time.Now().Add(month)); err != nil {
		t.Fatal(err)
	}
	for _, h := range lost {
		if present(t, r, h) {
			t.Errorf("%s left after the second sweep", h)
		}
	}
	if conv := strings.Join(conversation(t, r, s), "\n"); !strings.Contains(conv, "assistant: wrote a") {
		t.Fatalf("conversation:\n%s", conv)
	}
}

// crashSweepChild sweeps a month on, and stops after its first removal.
func crashSweepChild() {
	ctx := context.Background()
	r, err := Open(ctx, Config{DB: os.Getenv("KC_CRASH_DB"), Provider: &provider.Fake{HoldAt: -1}, Owner: "child"})
	if err != nil {
		panic(err)
	}
	sweepStep = func() {
		os.WriteFile(os.Getenv("KC_CRASH_STEP"), nil, 0o600)
		select {}
	}
	if _, err := r.sweep(ctx, false, time.Now().Add(month)); err != nil {
		panic(err)
	}
	os.Exit(3) // never reached
}

// Pruning a store waits while a snapshot uses it: a snapshot may use an old
// object again, which gc must not remove under it (ADR 0045).
func TestPruneWaitsForSnapshots(t *testing.T) {
	t.Parallel()
	r, ws, _, _ := sweepSession(t, filepath.Join(t.TempDir(), "k.db"))
	st := r.snapshotStore(ws)
	unlock, err := lockStore(st, false)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := pruneStore(context.Background(), st, nil, time.Now())
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("pruned while a snapshot used the store: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
