package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// The sessions of a folder: the latest used first, each titled by its
// first message, with its turns counted; another folder's are left out.
func TestSessions(t *testing.T) {
	userSettings(t, `{}`)
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Text: "a"}, {Text: "b"}, {Text: "c"}, {Text: "d"},
	}})
	ctx := context.Background()
	ws, elsewhere := repo(t), repo(t)
	first, _ := r.CreateSession(ctx, "one", workflows.SessionInput{Dir: ws})
	turn(t, r, first, "fix   the\nlogin bug")
	turn(t, r, first, "and its test")
	second, _ := r.CreateSession(ctx, "two", workflows.SessionInput{Dir: ws})
	turn(t, r, second, "write the README")
	other, _ := r.CreateSession(ctx, "three", workflows.SessionInput{Dir: elsewhere})
	turn(t, r, other, "elsewhere")

	list, err := r.Sessions(ctx, ws, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != second || list[1].ID != first {
		t.Fatalf("the list %+v", list)
	}
	if list[1].Title != "fix the login bug" || list[1].Turns != 2 || list[0].Title != "write the README" || list[0].Turns != 1 {
		t.Fatalf("the sessions %+v", list)
	}
	if all, _ := r.Sessions(ctx, "", 0); len(all) != 3 || all[0].ID != other {
		t.Fatalf("all %+v", all)
	}
	if one, _ := r.Sessions(ctx, ws, 1); len(one) != 1 || one[0].ID != second {
		t.Fatalf("limit 1 %+v", one)
	}
}
