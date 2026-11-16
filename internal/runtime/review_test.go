package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/config"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/sandbox/sandboxtest"
	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// buildThen is a line of an agent that writes a file, then st.
func buildThen(st workflows.Station) workflows.Line {
	return workflows.Line{Name: "ship", Stations: []workflows.Station{{Name: "build", Agent: "build"}, st}}
}

// buildWrites is the fake's script of the agent at the station build: it
// writes path, then says it did.
func buildWrites(path string) map[string][]provider.FakeReply {
	return map[string][]provider.FakeReply{`You are at the station "build"`: {writeCall(path, "one\n"), {Text: "wrote " + path}}}
}

// waitReview waits for the person's review the line's turn n of s waits
// for, or its end (nil).
func waitReview(t *testing.T, ctx context.Context, r *Runtime, s string, n int) *Pending {
	t.Helper()
	for {
		ps, _ := r.Pending(ctx, s)
		for _, p := range ps {
			if p.Tool == "review" {
				return &p
			}
		}
		if run, ok, _ := r.TurnRun(ctx, s, n); ok && run.Finished() {
			return nil
		}
		select {
		case <-ctx.Done():
			t.Fatalf("no review, and the turn has not ended: %+v", ps)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func statuses(out workflows.TurnOutput) string {
	var s []string
	for _, r := range out.Line {
		s = append(s, r.Name+" "+r.Status)
	}
	return strings.Join(s, ", ")
}

// A person's review (ADR 0044): it shows the change; yes takes it; no
// with words sends the item back to the station named back, which is given
// them, and the review comes again; the rounds spent stop the item.
func TestLineReviewPerson(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	ws := itemRepo(t, "")
	prompts := filepath.Join(t.TempDir(), "prompts")
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, PromptLog: prompts, Steps: buildWrites("a.txt")})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	line := buildThen(workflows.Station{Name: "review", Review: workflows.ReviewPerson, Back: "build"})

	s, _ := r.CreateSession(ctx, "taken", workflows.SessionInput{Dir: ws})
	n, err := r.RunLine(ctx, s, line, "write a")
	if err != nil {
		t.Fatal(err)
	}
	p := waitReview(t, ctx, r, s, n)
	if p == nil || p.Subject != "review" || !strings.Contains(p.Rule, "review the change of the line ship: 1 lines in 1 files") ||
		!strings.Contains(p.Preview, "a.txt") || !strings.Contains(p.Preview, "+one") || !strings.Contains(p.Preview, "wrote a.txt") {
		t.Fatalf("the review %+v", p)
	}
	if err := r.Approve(ctx, p.Key, false, "write two lines"); err != nil {
		t.Fatal(err)
	}
	p = waitReview(t, ctx, r, s, n)
	if p == nil {
		t.Fatal("no second review")
	}
	if err := r.Approve(ctx, p.Key, true, ""); err != nil {
		t.Fatal(err)
	}
	out, err := r.WaitTurn(ctx, s, n)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stopped != "" || statuses(out) != "build passed, review sent back, build passed, review passed" {
		t.Fatalf("stopped %q: %s", out.Stopped, statuses(out))
	}
	b, _ := os.ReadFile(prompts)
	if !strings.Contains(string(b), "Your work was sent back by a review, with these words: <review> write two lines </review>") {
		t.Fatalf("the words were not given:\n%s", b)
	}

	// The rounds spent (1), a second no stops the item at the review.
	s2, _ := r.CreateSession(ctx, "refused", workflows.SessionInput{Dir: itemRepo(t, "")})
	n, _ = r.RunLine(ctx, s2, line, "write a")
	for range 2 {
		p := waitReview(t, ctx, r, s2, n)
		if p == nil {
			t.Fatal("no review")
		}
		if err := r.Approve(ctx, p.Key, false, "no"); err != nil {
			t.Fatal(err)
		}
	}
	out, err = r.WaitTurn(ctx, s2, n)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stopped != workflows.StopStation || statuses(out) != "build passed, review sent back, build passed, review stopped" ||
		!strings.HasPrefix(out.Line[3].Why, "sent back, with no round left: no") {
		t.Fatalf("stopped %q: %s (%+v)", out.Stopped, statuses(out), out.Line)
	}
}

// A model's review: a reviewer agent reads the change, and the model's
// typed verdict passes it, or sends it back.
func TestLineReviewModel(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	line := buildThen(workflows.Station{Name: "review", Review: workflows.ReviewModel, Back: "build", Prompt: "Look for tests."})
	for _, c := range []struct {
		verdict, want string
		stopped       bool
	}{
		{"pass", "build passed, review passed", false},
		{"send back", "build passed, review sent back, build passed, review stopped", true},
	} {
		t.Run(c.verdict, func(t *testing.T) {
			t.Parallel()
			prompts := filepath.Join(t.TempDir(), "prompts")
			r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, PromptLog: prompts, Steps: buildWrites("a.txt"),
				Replies: map[string]provider.FakeReply{"You review a change": {Text: "It has no test."}},
				Decide:  json.RawMessage(`{"value": "` + c.verdict + `"}`)})
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			s, _ := r.CreateSession(ctx, "model", workflows.SessionInput{Dir: itemRepo(t, "")})
			n, err := r.RunLine(ctx, s, line, "write a")
			if err != nil {
				t.Fatal(err)
			}
			if p := waitReview(t, ctx, r, s, n); p != nil {
				t.Fatalf("a person was asked: %+v", p)
			}
			out, err := r.WaitTurn(ctx, s, n)
			if err != nil {
				t.Fatal(err)
			}
			if (out.Stopped != "") != c.stopped || statuses(out) != c.want {
				t.Fatalf("stopped %q: %s", out.Stopped, statuses(out))
			}
			b, _ := os.ReadFile(prompts)
			if !strings.Contains(string(b), "+one") || !strings.Contains(string(b), "What to look for: Look for tests.") {
				t.Fatalf("the reviewer was not given the change:\n%s", b)
			}
			if c.stopped && !strings.Contains(string(b), "<review> It has no test. </review>") {
				t.Fatalf("the reviewer's words were not given back:\n%s", b)
			}
		})
	}
}

// A review by risk: the model alone for a small change away from
// lines.people; a person after the model for one touching them, or larger
// than lines.reviewLines.
func TestLineReviewRisk(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	line := buildThen(workflows.Station{Name: "review", Review: workflows.ReviewRisk, Back: "build"})
	for _, c := range []struct {
		name, settings, path, why string
	}{
		{"small", `{"permission": {"write": "allow"}, "lines": {"people": ["secret/*"]}}`, "a.txt", ""},
		{"people", `{"permission": {"write": "allow"}, "lines": {"people": ["secret/*"]}}`, "secret/k.txt", "it touches secret/*"},
		{"large", `{"permission": {"write": "allow"}, "lines": {"reviewLines": 0}}`, "a.txt", ""},
		{"size", `{"permission": {"write": "allow"}, "lines": {"reviewLines": 1}}`, "b.txt", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fake := &provider.Fake{HoldAt: -1, Steps: map[string][]provider.FakeReply{
				`You are at the station "build"`: {writeCall(c.path, "one\ntwo\n"), {Text: "wrote"}}},
				Replies: map[string]provider.FakeReply{"You review a change": {Text: "Fine."}}, Decide: json.RawMessage(`{"value": "pass"}`)}
			r := open(t, filepath.Join(t.TempDir(), "k.db"), fake)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			s, _ := r.CreateSession(ctx, "risk", workflows.SessionInput{Dir: itemRepo(t, c.settings)})
			n, err := r.RunLine(ctx, s, line, "write")
			if err != nil {
				t.Fatal(err)
			}
			p := waitReview(t, ctx, r, s, n)
			person := c.name == "people" || c.name == "size"
			if (p != nil) != person {
				t.Fatalf("a person asked: %+v, want %v", p, person)
			}
			if p != nil {
				if c.why != "" && !strings.Contains(p.Rule, c.why) || c.name == "size" && !strings.Contains(p.Rule, "more than lines.reviewLines (1)") {
					t.Fatalf("why: %q", p.Rule)
				}
				if err := r.Approve(ctx, p.Key, true, ""); err != nil {
					t.Fatal(err)
				}
			}
			out, err := r.WaitTurn(ctx, s, n)
			if err != nil || out.Stopped != "" {
				t.Fatalf("%v %q: %s", err, out.Stopped, statuses(out))
			}
		})
	}
}

// A change touching lines.people gets a person's review before the line
// ends, whatever the line says; a change elsewhere does not. A project's
// settings only add to the user's paths.
func TestPeoplePathsAlways(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	line := workflows.Line{Name: "plain", Stations: []workflows.Station{{Name: "build", Agent: "build"}}}
	settings := `{"permission": {"write": "allow"}, "lines": {"people": ["secret/*"]}}`
	for _, path := range []string{"secret/k.txt", "a.txt"} {
		r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Steps: buildWrites(path)})
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		s, _ := r.CreateSession(ctx, "people", workflows.SessionInput{Dir: itemRepo(t, settings)})
		n, err := r.RunLine(ctx, s, line, "write")
		if err != nil {
			t.Fatal(err)
		}
		p := waitReview(t, ctx, r, s, n)
		if path == "a.txt" {
			if p != nil {
				t.Fatalf("a person asked for %s: %+v", path, p)
			}
			continue
		}
		if p == nil || p.Subject != workflows.PeopleStation || !strings.Contains(p.Rule, "it touches secret/*, a path that always needs a person") {
			t.Fatalf("the review %+v", p)
		}
		if err := r.Approve(ctx, p.Key, true, ""); err != nil {
			t.Fatal(err)
		}
		out, err := r.WaitTurn(ctx, s, n)
		if err != nil || out.Stopped != "" || statuses(out) != "build passed, people passed" {
			t.Fatalf("%v %q: %s", err, out.Stopped, statuses(out))
		}
	}
	user := config.Layer{Path: "user", Data: []byte(`{"lines": {"people": ["auth/*"], "reviewLines": 100}}`)}
	project := config.Layer{Path: "project", Project: true, Data: []byte(`{"lines": {"people": [], "reviewLines": 900}}`)}
	if p := policyOf([]config.Layer{user, project}); len(p.People) != 1 || p.People[0] != "auth/*" || p.ReviewLines != 100 {
		t.Fatalf("policy %+v", p)
	}
}

// waitEnded waits until every item of conversation has ended its line.
func waitEnded(t *testing.T, ctx context.Context, r *Runtime, conversation string, n int) []api.Item {
	t.Helper()
	for {
		items, _ := r.Items(ctx, conversation)
		ended := 0
		for _, it := range items {
			if it.State != api.ItemRunning && it.State != api.ItemWaiting && it.State != api.ItemQueued {
				ended++
			}
		}
		if len(items) == n && ended == n {
			return items
		}
		select {
		case <-ctx.Done():
			t.Fatalf("items %+v", items)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// A line whose items end stopped or failed lines.stopAfter times in a row
// stops itself: it takes no new item, its conversation is told once, and
// it runs again only once a person starts it again; an item done between
// starts the count again (ADR 0044).
func TestLineStopsItself(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	ws := itemRepo(t, `{"permission": {"line": "allow"}, "lines": {"stopAfter": 2}}`)
	putFiles(t, ws, map[string]string{
		".kairocode/workflows/maybe.star": "def main(input):\n    return {\"ok\": input[\"args\"] == \"pass\", \"message\": \"asked to \" + input[\"args\"]}\n",
		".kairocode/lines/breaks.json":    `{"stations": [{"name": "try", "flow": "maybe"}]}`,
	})
	gitT(t, ws, "add", "-A")
	gitT(t, ws, "commit", "-q", "-m", "a line that breaks")
	script := make([]provider.FakeReply, 12)
	for k := range script {
		script[k] = provider.FakeReply{Text: "Noted."}
	}
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: script})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, _ := r.CreateSession(ctx, "conv", workflows.SessionInput{Dir: ws})
	n := 0
	send := func(request string) error {
		n++
		_, err := r.StartItem(ctx, s, "call"+strconv.Itoa(n), tools.ItemSpec{Line: "breaks", Request: request})
		return err
	}
	for _, req := range []string{"fail", "pass", "fail"} { // a done item between: not stopped
		if err := send(req); err != nil {
			t.Fatal(err)
		}
		waitEnded(t, ctx, r, s, n)
	}
	if err := send("fail"); err != nil { // the second in a row
		t.Fatal(err)
	}
	waitEnded(t, ctx, r, s, n)
	if err := send("pass"); !errors.Is(err, ErrLineStopped) || !strings.Contains(err.Error(), "its last 2 items did not pass") {
		t.Fatalf("a new item on a stopped line: %v", err)
	}
	l, err := r.Lines(ctx, ws)
	if err != nil || len(l) != 3 || l[0].Name != "breaks" || !l[0].Stopped {
		t.Fatalf("lines %+v %v", l, err)
	}
	// Told once: the notice of the stop is in the conversation, once.
	for {
		conv := strings.Join(conversationOf(t, r, s, false), "\n")
		if c := strings.Count(conv, "[KairoCode] The line breaks stopped itself"); c == 1 {
			break
		} else if c > 1 {
			t.Fatalf("told %d times:\n%s", c, conv)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("not told:\n%s", conv)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err := r.RestartLine(ctx, ws, "breaks"); err != nil {
		t.Fatal(err)
	}
	n-- // the refused one kept nothing
	if err := send("fail"); err != nil {
		t.Fatalf("after a restart: %v", err)
	}
	waitEnded(t, ctx, r, s, n)
	if l, _ := r.Lines(ctx, ws); l[0].Stopped {
		t.Fatalf("stopped again after one: %+v", l[0])
	}
}

// reviewLine is the line the crash test runs: an agent, then a review by
// KC_CRASH_REVIEW_CHILD's reviewer.
func reviewLine(by string) workflows.Line {
	return buildThen(workflows.Station{Name: "review", Review: by, Back: "build"})
}

func crashReviewChild() {
	ctx := context.Background()
	fake, err := provider.LoadFake(os.Getenv("KC_FAKE"))
	if err != nil {
		panic(err)
	}
	r, err := Open(ctx, Config{DB: os.Getenv("KC_DB"), Provider: fake, Owner: "crash", CacheDir: os.Getenv("KC_CACHE")})
	if err != nil {
		panic(err)
	}
	s, err := r.CreateSession(ctx, "rev", workflows.SessionInput{Dir: os.Getenv("KC_DIR")})
	if err != nil {
		panic(err)
	}
	by := os.Getenv("KC_CRASH_REVIEW_CHILD")
	if _, err := r.RunLine(ctx, s, reviewLine(by), "write a"); err != nil {
		panic(err)
	}
	if by == workflows.ReviewPerson { // held where a person's review waits
		for {
			if ps, _ := r.Pending(ctx, s); len(ps) > 0 {
				os.WriteFile(os.Getenv("KC_HOLD"), nil, 0o600)
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	select {}
}

// Killed while a person's review waits, or while a reviewer agent works,
// the line resumes there: the review is asked once, the agent before it
// is not run again, and nothing is asked twice (ADR 0044).
func TestCrashDuringReview(t *testing.T) {
	t.Parallel()
	sandboxtest.Require(t)
	if testing.Short() {
		t.Skip("crash test: starts and kills a process")
	}
	for _, by := range []string{workflows.ReviewPerson, workflows.ReviewModel} {
		t.Run(by, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			db := filepath.Join(dir, "k.db")
			ws := itemRepo(t, "")
			prompts := filepath.Join(dir, "prompts")
			hold := filepath.Join(dir, "holding")
			fake := provider.Fake{HoldAt: -1, PromptLog: prompts, Steps: buildWrites("a.txt"),
				Replies: map[string]provider.FakeReply{"You review a change": {Text: "Fine."}}, Decide: json.RawMessage(`{"value": "pass"}`)}
			held := fake
			if by == workflows.ReviewModel {
				held.HoldFile, held.HoldOn = hold, "You review a change"
			}
			fakePath := filepath.Join(dir, "fake.json")
			b, _ := json.Marshal(held)
			os.WriteFile(fakePath, b, 0o600)
			child := exec.Command(os.Args[0], "-test.run=^$")
			child.Env = append(os.Environ(), "KC_CRASH_REVIEW_CHILD="+by, "KC_DB="+db, "KC_DIR="+ws, "KC_FAKE="+fakePath, "KC_HOLD="+hold, "KC_CACHE="+t.TempDir())
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
			s := SessionID("rev")
			if by == workflows.ReviewPerson {
				ps, _ := r.Pending(ctx, s)
				if len(ps) != 1 || ps[0].Tool != "review" {
					t.Fatalf("pending %+v", ps)
				}
				if err := r.Approve(ctx, ps[0].Key, true, ""); err != nil {
					t.Fatal(err)
				}
			}
			out, err := r.WaitTurn(ctx, s, 0)
			if err != nil {
				t.Fatal(err)
			}
			if out.Stopped != "" || statuses(out) != "build passed, review passed" {
				t.Fatalf("stopped %q: %s", out.Stopped, statuses(out))
			}
			log, _ := os.ReadFile(prompts)
			if n := strings.Count(string(log), `You are at the station "build"`); n != 2 {
				t.Fatalf("the agent at build was called %d times, want 2 (its two steps, once):\n%s", n, log)
			}
			if by == workflows.ReviewModel {
				if n := strings.Count(string(log), "You review a change"); n != 2 {
					t.Fatalf("the reviewer was called %d times, want 2 (cut off, again)", n)
				}
			}
		})
	}
}
