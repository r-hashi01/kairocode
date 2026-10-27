package runtime

import (
	"context"

	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/store"
)

const readFlow = `
DESCRIPTION = "Read main.go"

def main(input):
    phase("read")
    f = tool("read", path = "main.go")
    return {"ok": f.ok, "message": "read it, " + input["args"]}
`

// A flow's run is kept once it ends, with its chart, each step's state and
// its steps, and listed (ADR 0029).
func TestFlowRunKept(t *testing.T) {
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/reader.star": readFlow})
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	flowTurn(t, r, ws, "reader", "twice")
	var runs []FlowRun
	waitUntil(t, ctx, "the run kept", func() bool {
		rows, _ := r.Store.FlowRuns(ctx, "", "", "", 0)
		return len(rows) == 1
	})
	runs, err := r.FlowRuns(ctx, ws, "", "", 0)
	if err != nil || len(runs) != 1 {
		t.Fatalf("%v %v", runs, err)
	}
	f := runs[0]
	if f.Name != "reader" || f.Args != "twice" || f.Status != FlowRunOK || f.From != FromCommand || f.Message != "read it, twice" || f.View != nil {
		t.Fatalf("listed: %+v", f)
	}
	whole, err := r.FlowRunByID(ctx, f.ID)
	if err != nil || whole.View == nil || len(whole.View.States) == 0 || len(whole.Steps) == 0 {
		t.Fatalf("the run: %+v %v", whole, err)
	}
	text := whole.Text()
	for _, want := range []string{"reader twice  ok", "said: read it, twice", "Steps:", "read"} {
		if !strings.Contains(text, want) {
			t.Fatalf("%q not in\n%s", want, text)
		}
	}
	if other, _ := r.FlowRuns(ctx, t.TempDir(), "", "", 0); len(other) != 0 {
		t.Fatalf("listed in another folder: %v", other)
	}
}

// A run a process did not keep (it stopped) is kept when the next opens;
// kept twice, it is one.
func TestFlowRunKeptAfterRestart(t *testing.T) {
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/reader.star": readFlow})
	db := filepath.Join(t.TempDir(), "k.db")
	r1 := open(t, db, &provider.Fake{HoldAt: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	flowTurn(t, r1, ws, "reader", "")
	waitUntil(t, ctx, "the run kept", func() bool { rows, _ := r1.Store.FlowRuns(ctx, "", "", "", 0); return len(rows) == 1 })
	if _, err := r1.db.ExecContext(ctx, `DELETE FROM kc_flow_run`); err != nil { // as if it had stopped first
		t.Fatal(err)
	}
	r1.Close()
	r2 := open(t, db, &provider.Fake{HoldAt: -1})
	waitUntil(t, ctx, "the run kept again", func() bool { rows, _ := r2.Store.FlowRuns(ctx, "", "", "", 0); return len(rows) == 1 })
	rows, _ := r2.Store.FlowRuns(ctx, "", "", "", 0)
	if err := r2.keepFlowRun(ctx, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	if rows, _ := r2.Store.FlowRuns(ctx, "", "", "", 0); len(rows) != 1 {
		t.Fatalf("%d rows: kept twice", len(rows))
	}
}

// Past flowHistory's count and days, a flow's oldest runs go.
func TestFlowRunsPruned(t *testing.T) {
	userSettings(t, `{"flowHistory": {"keep": 2, "days": 30}}`)
	if keep, days := flowHistory(); keep != 2 || days != 30 {
		t.Fatalf("flowHistory: %d %d", keep, days)
	}
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	ctx := context.Background()
	now := time.Now()
	for i, age := range []time.Duration{40 * 24 * time.Hour, 3 * time.Hour, 2 * time.Hour, time.Hour} {
		id := "s/x/" + string(rune('a'+i))
		if err := r.Store.PutFlowRun(ctx, store.FlowRunRow{ID: id, Dir: "/w", Name: "f", Status: "ok", Started: now.Add(-age), Body: []byte("{}")}); err != nil {
			t.Fatal(err)
		}
	}
	r.Store.PutFlowRun(ctx, store.FlowRunRow{ID: "s/y/other", Dir: "/w", Name: "g", Status: "ok", Started: now.Add(-40 * 24 * time.Hour), Body: []byte("{}")})
	keep, days := flowHistory()
	if err := r.Store.PruneFlowRuns(ctx, "/w", "f", false, keep, now.AddDate(0, 0, -days)); err != nil {
		t.Fatal(err)
	}
	rows, _ := r.Store.FlowRuns(ctx, "/w", "", "", 0)
	var ids []string
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	if strings.Join(ids, ",") != "s/x/d,s/x/c,s/y/other" {
		t.Fatalf("kept %v", ids)
	}
}

// A run let go past flowHistory is not kept again when the next process
// opens (kairo still has it, as long as its session waits).
func TestFlowRunsNotMadeAgain(t *testing.T) {
	userSettings(t, `{"flowHistory": {"keep": 1}}`)
	ws := repo(t)
	putFiles(t, ws, map[string]string{".kairocode/workflows/reader.star": readFlow})
	db := filepath.Join(t.TempDir(), "k.db")
	r1 := open(t, db, &provider.Fake{HoldAt: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	flowTurn(t, r1, ws, "reader", "first")
	flowTurn(t, r1, ws, "reader", "second")
	waitUntil(t, ctx, "the second kept", func() bool {
		rows, _ := r1.Store.FlowRuns(ctx, "", "", "", 0)
		return len(rows) == 1 && strings.Contains(string(rows[0].Body), `"args":"second"`)
	})
	r1.Close()
	r2 := open(t, db, &provider.Fake{HoldAt: -1})
	r2.keepMissed(ctx) // as it does as it opens
	rows, _ := r2.Store.FlowRuns(ctx, "", "", "", 0)
	if len(rows) != 1 || !strings.Contains(string(rows[0].Body), `"args":"second"`) {
		t.Fatalf("%d runs kept after the restart", len(rows))
	}
}

// Tries are counted apart from runs: trying a flow often does not push its
// runs out (ADR 0029, 追記).
func TestFlowTriesCountedApart(t *testing.T) {
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	ctx := context.Background()
	now := time.Now()
	for i := range 4 {
		r.Store.PutFlowRun(ctx, store.FlowRunRow{ID: "s/r/" + string(rune('a'+i)), Dir: "/w", Name: "f", Status: "ok",
			Started: now.Add(-time.Duration(10+i) * time.Hour), Body: []byte("{}")})
		r.Store.PutFlowRun(ctx, store.FlowRunRow{ID: "s/t/" + string(rune('a'+i)), Dir: "/w", Name: "f", Status: "ok", Try: true,
			Started: now.Add(-time.Duration(i) * time.Hour), Body: []byte("{}")})
	}
	if err := r.Store.PruneFlowRuns(ctx, "/w", "f", true, 2, now.AddDate(0, 0, -90)); err != nil {
		t.Fatal(err)
	}
	tries, _ := r.Store.FlowRuns(ctx, "/w", "f", "tries", 0)
	runs, _ := r.Store.FlowRuns(ctx, "/w", "f", "runs", 0)
	if len(tries) != 2 || len(runs) != 4 || !tries[0].Try || runs[0].Try {
		t.Fatalf("%d tries, %d runs", len(tries), len(runs))
	}
}
