package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/sandbox/sandboxtest"
	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// itemRepo is a git repository with one commit holding the project's
// settings (the model may write and send items; the check "fixed"), and
// two lines: quick (an agent) and gated (an agent whose work must pass
// "fixed").
func itemRepo(t *testing.T, settings string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	if settings == "" {
		settings = `{"permission": {"write": "allow", "edit": "allow", "line": "allow"}, "checks": {"fixed": {"command": "test -f fixed"}}}`
	}
	dir := repo(t)
	putFiles(t, dir, map[string]string{
		ConfigFile:                    settings,
		".kairocode/lines/quick.json": `{"description": "one agent", "stations": [{"name": "build", "agent": "build"}]}`,
		".kairocode/lines/gated.json": `{"description": "one gated agent", "stations": [{"name": "build", "agent": "build", "checks": ["fixed"]}]}`,
	})
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.name", "T"},
		{"config", "user.email", "t@example.com"},
		{"add", "-A"},
		{"commit", "-q", "-m", "first"},
	} {
		gitT(t, dir, args...)
	}
	return sameDir(dir)
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func lineCall(in string) provider.FakeReply {
	return provider.FakeReply{Tools: []provider.FakeTool{{Name: "line", Input: json.RawMessage(in)}}}
}

// waitItem waits until conversation's item is in one of states.
func waitItem(t *testing.T, ctx context.Context, r *Runtime, conversation string, states ...string) api.Item {
	t.Helper()
	for {
		items, err := r.Items(ctx, conversation)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			for _, s := range states {
				if it.State == s {
					return it
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("no item %v: %+v", states, items)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// waitTurns waits until session has had n turns sent.
func waitTurns(t *testing.T, ctx context.Context, r *Runtime, session string, n int) {
	t.Helper()
	for {
		if got, _ := r.sent(ctx, session); got >= n {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("turns of %s: fewer than %d", session, n)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// The conversation sends a request down a standard line (ADR 0043): the
// tool returns at once with the item; the item runs in a worktree and a
// session of its own, its work committed to its branch, not the
// conversation's folder; the conversation is told once that it is done.
func TestItemStart(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	ws := itemRepo(t, "")
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1,
		Script: []provider.FakeReply{lineCall(`{"line": "quick", "request": "write the notes"}`), {Text: "Sent it."}, {Text: "Noted."}},
		Steps:  map[string][]provider.FakeReply{`You are at the station "build"`: {writeCall("notes.txt", "notes\n"), {Text: "wrote the notes"}}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "conv", workflows.SessionInput{Dir: ws})
	n, _ := r.Send(ctx, s, "send the notes down a line")
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	if reply, _ := r.Reply(ctx, out.Reply); reply != "Sent it." {
		t.Fatalf("reply %q", reply)
	}
	if conv := strings.Join(conversationOf(t, r, s, true), "\n"); !strings.Contains(conv, "Started the item quick-1 on the line quick") {
		t.Fatalf("conversation:\n%s", conv)
	}
	it := waitItem(t, ctx, r, s, api.ItemDone, api.ItemStopped, api.ItemFailed)
	if it.State != api.ItemDone || it.Name != "quick-1" || it.Branch != "kairo/quick-1" || it.Line != "quick" ||
		!strings.HasPrefix(it.Dir, sameDir(filepath.Join(filepath.Dir(r.cfg.DB), "worktrees"))) {
		t.Fatalf("item %+v", it)
	}
	if _, err := os.Stat(filepath.Join(ws, "notes.txt")); err == nil {
		t.Fatal("the item wrote in the conversation's folder")
	}
	if msg := gitT(t, ws, "log", "-1", "--format=%s", "kairo/quick-1"); msg != "quick: write the notes" {
		t.Fatalf("the branch's commit: %q", msg)
	}
	if got := gitT(t, ws, "show", "kairo/quick-1:notes.txt"); got != "notes" {
		t.Fatalf("notes on the branch: %q", got)
	}
	// Told once: the conversation's next turn is the notice.
	waitTurns(t, ctx, r, s, 2)
	if _, err := r.WaitTurn(ctx, s, 1); err != nil {
		t.Fatal(err)
	}
	conv := strings.Join(conversationOf(t, r, s, false), "\n")
	if strings.Count(conv, "[KairoCode] The item quick-1 (line quick) is done") != 1 || !strings.Contains(conv, "The line quick is done") {
		t.Fatalf("conversation:\n%s", conv)
	}
	if err := r.tellItem(ctx, it.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.sent(ctx, s); got != 2 {
		t.Fatalf("told again: %d turns", got)
	}
	// The items tool lists it; an item's own session is given no line.
	text, _ := itemEnv{r: r, conversation: s}.List(ctx)
	if !strings.Contains(text, "quick-1  line quick  done") {
		t.Fatalf("items: %s", text)
	}
	if !r.isItem(ctx, it.ID) || r.isItem(ctx, s) {
		t.Fatal("which session is an item's")
	}
}

// What an item waits for is told to the conversation and answered from
// it (ADR 0043): its project's check is asked about before it first runs.
// Its end is told after.
func TestItemWaitsInConversation(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	ws := itemRepo(t, "")
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1,
		Script: []provider.FakeReply{lineCall(`{"line": "gated", "request": "make the file fixed"}`), {Text: "Sent it."}, {Text: "Noted."}, {Text: "Noted again."}},
		Steps:  map[string][]provider.FakeReply{`You are at the station "build"`: {writeCall("fixed", "x"), {Text: "fixed"}}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "conv", workflows.SessionInput{Dir: ws})
	r.Send(ctx, s, "go")
	it := waitItem(t, ctx, r, s, api.ItemWaiting)
	p, err := r.ItemsPending(ctx, s)
	if err != nil || len(p) != 1 || p[0].Item != it.Name || !strings.Contains(p[0].Subject, "The project's check") {
		t.Fatalf("the items' waits: %+v %v", p, err)
	}
	if own, _ := r.Pending(ctx, s); len(own) != 0 {
		t.Fatalf("the conversation's own waits: %+v", own)
	}
	waitTurns(t, ctx, r, s, 2)
	if _, err := r.WaitTurn(ctx, s, 1); err != nil {
		t.Fatal(err)
	}
	if conv := strings.Join(conversationOf(t, r, s, false), "\n"); !strings.Contains(conv, "[KairoCode] The item gated-1 (line gated) waits for the person") {
		t.Fatalf("conversation:\n%s", conv)
	}
	if err := r.Answer(ctx, p[0].Key, "Run it in KairoCode's sandbox"); err != nil {
		t.Fatal(err)
	}
	if it = waitItem(t, ctx, r, s, api.ItemDone, api.ItemStopped, api.ItemFailed); it.State != api.ItemDone {
		t.Fatalf("item %+v", it)
	}
	waitTurns(t, ctx, r, s, 3)
	if _, err := r.WaitTurn(ctx, s, 2); err != nil {
		t.Fatal(err)
	}
	if conv := strings.Join(conversationOf(t, r, s, false), "\n"); !strings.Contains(conv, "[KairoCode] The item gated-1 (line gated) is done") {
		t.Fatalf("conversation:\n%s", conv)
	}
}

// A line put together on the spot is asked about every time, its stations
// shown, whatever the rules allow; with lines.onTheSpot "deny" it is
// refused, not asked.
func TestLineOnTheSpot(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	spot := lineCall(`{"stations": [{"name": "build", "agent": "build", "checks": ["fixed"]}], "request": "make it"}`)
	ws := itemRepo(t, "")
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{spot, {Text: "Not sent."}}})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "conv", workflows.SessionInput{Dir: ws})
	n, _ := r.Send(ctx, s, "go")
	p := waitPending(t, r, s, 1)
	if p[0].Tool != "line" || !strings.Contains(p[0].Rule, "puts a line together on the spot: build [agent build; checks fixed]") {
		t.Fatalf("pending %+v", p[0])
	}
	if err := r.Approve(ctx, p[0].Key, false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	if items, _ := r.Items(ctx, s); len(items) != 0 {
		t.Fatalf("items %+v", items)
	}

	ws2 := itemRepo(t, `{"permission": {"line": "allow"}, "lines": {"onTheSpot": "deny"}, "checks": {"fixed": {"command": "test -f fixed"}}}`)
	s2, _ := r.CreateSession(ctx, "conv2", workflows.SessionInput{Dir: ws2})
	n, _ = r.Send(ctx, s2, "go")
	if _, err := r.WaitTurn(ctx, s2, n); err != nil {
		t.Fatal(err)
	}
	if conv := strings.Join(conversationOf(t, r, s2, true), "\n"); !strings.Contains(conv, `lines.onTheSpot is \"deny\"`) && !strings.Contains(conv, `lines.onTheSpot is "deny"`) {
		t.Fatalf("conversation:\n%s", conv)
	}
	if items, _ := r.Items(ctx, s2); len(items) != 0 {
		t.Fatalf("items %+v", items)
	}
}

// At most api.MaxItemsRunning items run at once in a repository: another
// is refused, naming those running.
func TestItemCap(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	ws := itemRepo(t, "")
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	r := open(t, filepath.Join(dir, "k.db"), &provider.Fake{HoldAt: -1, HoldFile: filepath.Join(dir, "holding"), HoldOn: `You are at the station "build"`,
		ReleaseFile: release, Script: []provider.FakeReply{{Text: "ok"}}, Replies: map[string]provider.FakeReply{`You are at the station "build"`: {Text: "built"}}})
	defer os.WriteFile(release, nil, 0o600)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "conv", workflows.SessionInput{Dir: ws})
	spec := tools.ItemSpec{Line: "quick", Request: "build"}
	for i := range api.MaxItemsRunning {
		if _, err := r.StartItem(ctx, s, "call"+string(rune('a'+i)), spec); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.StartItem(ctx, s, "call-more", spec); !errors.Is(err, ErrItemsCap) || !strings.Contains(err.Error(), "quick-1, quick-2, quick-3, quick-4") {
		t.Fatalf("a fifth: %v", err)
	}
	// The same call again is the item it started, not a new one.
	again, err := r.StartItem(ctx, s, "calla", spec)
	if err != nil || again.Name != "quick-1" {
		t.Fatalf("again: %+v %v", again, err)
	}
}

// An item whose branch was merged is taken: cleaned up as it is listed
// (worktree and branch gone, session closed). One dropped with changes is
// asked to be forced; dropped, it is cleaned up too. The conversation's
// items still list both, with how they ended.
func TestItemCleanedUp(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	ws := itemRepo(t, "")
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{{Text: "ok"}},
		Steps: map[string][]provider.FakeReply{`You are at the station "build"`: {writeCall("notes.txt", "notes\n"), {Text: "wrote"}}}})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "conv", workflows.SessionInput{Dir: ws})
	one, err := r.StartItem(ctx, s, "one", tools.ItemSpec{Line: "quick", Request: "one"})
	if err != nil {
		t.Fatal(err)
	}
	two, err := r.StartItem(ctx, s, "two", tools.ItemSpec{Line: "quick", Request: "two"})
	if err != nil {
		t.Fatal(err)
	}
	for {
		items, _ := r.Items(ctx, s)
		if len(items) == 2 && items[0].State == api.ItemDone && items[1].State == api.ItemDone {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("items %+v", items)
		case <-time.After(20 * time.Millisecond):
		}
	}
	gitT(t, ws, "merge", "-q", "--no-edit", one.Branch)
	// Listing it changes nothing (the items tool reads it); taking does.
	if items, _ := r.Items(ctx, s); items[0].State != api.ItemDone {
		t.Fatalf("taken by a listing: %+v", items[0])
	}
	if err := r.TakeItems(ctx, s); err != nil {
		t.Fatal(err)
	}
	items, err := r.Items(ctx, s)
	if err != nil || items[0].State != api.ItemTaken || items[1].State != api.ItemDone {
		t.Fatalf("items %+v %v", items, err)
	}
	if _, err := os.Stat(one.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the taken item's worktree: %v", err)
	}
	if branchExists(ctx, ws, one.Branch) {
		t.Fatal("the taken item's branch is there")
	}
	if info, err := r.K.Get(ctx, one.ID); err != nil || !info.Finished() {
		t.Fatalf("the taken item's session: %+v %v", info, err)
	}

	os.WriteFile(filepath.Join(two.Dir, "more.txt"), []byte("more"), 0o644)
	if err := r.DropItem(ctx, s, two.Name, false); !errors.Is(err, ErrWorktreeChanged) {
		t.Fatalf("dropped with changes: %v", err)
	}
	if err := r.DropItem(ctx, s, two.Name, true); err != nil {
		t.Fatal(err)
	}
	items, _ = r.Items(ctx, s)
	if len(items) != 2 || items[1].State != api.ItemDropped {
		t.Fatalf("items %+v", items)
	}
	if _, err := os.Stat(two.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the dropped item's worktree: %v", err)
	}
	if !branchExists(ctx, ws, two.Branch) {
		t.Fatal("the dropped item's branch, not merged, is gone")
	}
}

// crashItemChild starts an item and is held where KC_CRASH_ITEM_CHILD
// says: once its worktree is made, or once its session is.
func crashItemChild() {
	ctx := context.Background()
	fake, err := provider.LoadFake(os.Getenv("KC_FAKE"))
	if err != nil {
		panic(err)
	}
	r, err := Open(ctx, Config{DB: os.Getenv("KC_DB"), Provider: fake, Owner: "crash", CacheDir: os.Getenv("KC_CACHE")})
	if err != nil {
		panic(err)
	}
	hold := func(string) {
		os.WriteFile(os.Getenv("KC_HOLD"), nil, 0o600)
		select {}
	}
	switch os.Getenv("KC_CRASH_ITEM_CHILD") {
	case "worktree":
		afterItemWorktree = hold
	case "session":
		afterItemSession = hold
	case "commit":
		tools.AfterGitCommit = hold
	}
	s, err := r.CreateSession(ctx, "conv", workflows.SessionInput{Dir: os.Getenv("KC_DIR")})
	if err != nil {
		panic(err)
	}
	if _, err := r.StartItem(ctx, s, "the-call", tools.ItemSpec{Line: "quick", Request: "write the notes"}); err != nil {
		panic(err)
	}
	select {}
}

// Killed while an item starts (after its worktree is made, and after its
// session is), the start made again makes no second worktree, session or
// line's turn: one of each, and its agent called once (ADR 0043).
func TestCrashStartingItem(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	for _, at := range []string{"worktree", "session", "commit"} {
		t.Run(at, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			db := filepath.Join(dir, "k.db")
			ws := itemRepo(t, "")
			prompts := filepath.Join(dir, "prompts")
			fake := provider.Fake{HoldAt: -1, PromptLog: prompts, Script: []provider.FakeReply{{Text: "ok"}},
				Steps: map[string][]provider.FakeReply{`You are at the station "build"`: {writeCall("notes.txt", "notes\n"), {Text: "wrote"}}}}
			fakePath := filepath.Join(dir, "fake.json")
			b, _ := json.Marshal(fake)
			os.WriteFile(fakePath, b, 0o600)
			hold := filepath.Join(dir, "holding")
			child := exec.Command(os.Args[0], "-test.run=^$")
			child.Env = append(os.Environ(), "KC_CRASH_ITEM_CHILD="+at, "KC_DB="+db, "KC_DIR="+ws, "KC_FAKE="+fakePath, "KC_HOLD="+hold, "KC_CACHE="+t.TempDir())
			child.Stderr = os.Stderr
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			waitForFile(t, hold, child)
			if err := child.Process.Kill(); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			r, err := Open(ctx, Config{DB: db, Provider: &fake, Owner: "crash", CacheDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			s := SessionID("conv")
			it, err := r.StartItem(ctx, s, "the-call", tools.ItemSpec{Line: "quick", Request: "write the notes"})
			if err != nil {
				t.Fatal(err)
			}
			if it.Name != "quick-1" {
				t.Fatalf("item %+v", it)
			}
			if it = waitItem(t, ctx, r, s, api.ItemDone, api.ItemStopped, api.ItemFailed); it.State != api.ItemDone {
				t.Fatalf("item %+v", it)
			}
			if items, _ := r.Items(ctx, s); len(items) != 1 {
				t.Fatalf("items %+v", items)
			}
			if wts := strings.Count(gitT(t, ws, "worktree", "list", "--porcelain"), "worktree "); wts != 2 {
				t.Fatalf("%d worktrees, want the main checkout and one", wts)
			}
			if n, _ := r.sent(ctx, it.ID); n != 1 {
				t.Fatalf("the item's session was sent %d events, want its line once", n)
			}
			log, _ := os.ReadFile(prompts)
			if n := strings.Count(string(log), `You are at the station "build"`); n != 2 {
				t.Fatalf("the agent was called %d times, want 2 (its two steps, once):\n%s", n, log)
			}
			// One commit on the branch, said as made: one cut off by the
			// kill is found, not made again.
			if n := gitT(t, ws, "rev-list", "--count", "main.."+it.Branch); n != "1" {
				t.Fatalf("%s commits on the branch, want 1", n)
			}
			out, err := r.WaitTurn(ctx, it.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if reply, _ := r.Reply(ctx, out.Reply); !strings.Contains(reply, "Committed to the branch") {
				t.Fatalf("reply %q", reply)
			}
		})
	}
}

// Two items started at once (two line calls of one reply) take two names
// and two worktrees; a name given that a worktree or a branch has is
// refused; a line whose files are not committed is refused before
// anything is made, as the item's worktree starts from HEAD.
func TestItemsAtOnce(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	ws := itemRepo(t, "")
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	r := open(t, filepath.Join(dir, "k.db"), &provider.Fake{HoldAt: -1, HoldFile: filepath.Join(dir, "holding"), HoldOn: `You are at the station "build"`,
		ReleaseFile: release, Script: []provider.FakeReply{{Text: "ok"}}, Replies: map[string]provider.FakeReply{`You are at the station "build"`: {Text: "built"}}})
	defer os.WriteFile(release, nil, 0o600)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "conv", workflows.SessionInput{Dir: ws})
	type started struct {
		it  api.Item
		err error
	}
	got := make(chan started, 2)
	for _, key := range []string{"a", "b"} {
		go func() {
			it, err := r.StartItem(ctx, s, key, tools.ItemSpec{Line: "quick", Request: "build " + key})
			got <- started{it, err}
		}()
	}
	one, two := <-got, <-got
	if one.err != nil || two.err != nil || one.it.Name == two.it.Name || one.it.Dir == two.it.Dir {
		t.Fatalf("at once: %+v %v; %+v %v", one.it, one.err, two.it, two.err)
	}

	gitT(t, ws, "branch", "kairo/taken")
	if _, err := r.StartItem(ctx, s, "c", tools.ItemSpec{Line: "quick", Request: "x", Name: "taken"}); err == nil || !strings.Contains(err.Error(), "taken is taken") {
		t.Fatalf("a name a branch has: %v", err)
	}
	putFiles(t, ws, map[string]string{".kairocode/lines/new.json": `{"stations": [{"name": "build", "agent": "build"}]}`})
	if _, err := r.StartItem(ctx, s, "d", tools.ItemSpec{Line: "new", Request: "x"}); err == nil || !strings.Contains(err.Error(), "not committed") {
		t.Fatalf("a line not committed: %v", err)
	}
	if items, _ := r.Items(ctx, s); len(items) != 2 {
		t.Fatalf("items %+v", items)
	}
}
