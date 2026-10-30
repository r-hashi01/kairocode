package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/r-hashi01/kairocode/internal/acp"
)

func sessionInfo(id string, turns, waiting, jobs int) acp.SessionInfo {
	var s acp.SessionInfo
	s.ID = id
	s.Meta.KairoCode.Turns, s.Meta.KairoCode.Waiting, s.Meta.KairoCode.Jobs = turns, waiting, jobs
	return s
}

// A new session's header says what goes on in the folder: what waits for
// the user, what runs in the background, the flows; nothing when nothing
// does, and nothing once the user has said something.
func TestAroundUnderTheHeader(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".kairocode", "workflows"), 0o755)
	for _, f := range []string{"review", "release"} {
		os.WriteFile(filepath.Join(dir, ".kairocode", "workflows", f+".star"), []byte("def main(input):\n    return 1\n"), 0o644)
	}
	run := func(o Options, list []acp.SessionInfo) string {
		a := &fakeAgent{list: list}
		d := &driver{t: t, m: newModel(context.Background(), a, o)}
		a.d = d
		d.send(tea.WindowSizeMsg{Width: 100, Height: 24})
		d.run(d.m.Init())
		return d.out()
	}
	out := run(Options{Dir: dir}, []acp.SessionInfo{
		sessionInfo("s/a", 3, 2, 1), sessionInfo("s/b", 1, 0, 1), sessionInfo("s/c", 4, 0, 0), sessionInfo("s/new", 0, 0, 0),
	})
	for _, want := range []string{
		"↺ 2 requests waiting for you in 1 session · /resume to answer",
		"⏵ 2 commands running in the background",
		"⌁ 2 flows · release · review · /flow <name>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if !strings.Contains(out, "ϟ kairo") || strings.Index(out, "ϟ kairo") > strings.Index(out, "↺") {
		t.Errorf("not under the header:\n%s", out)
	}

	if out := run(Options{Dir: dir}, []acp.SessionInfo{sessionInfo("s/c", 4, 0, 0)}); !strings.Contains(out, "↺ 1 session here to take up again · /resume") {
		t.Errorf("a session to take up:\n%s", out)
	}
	empty := t.TempDir()
	if out := run(Options{Dir: empty}, []acp.SessionInfo{sessionInfo("s/new", 0, 0, 0)}); strings.ContainsAny(out, "↺⏵⌁") {
		t.Errorf("nothing goes on, yet:\n%s", out)
	}
	if out := run(Options{Dir: dir, Session: "s/a"}, nil); strings.ContainsAny(out, "↺⏵⌁") {
		t.Errorf("a session taken up:\n%s", out)
	}
}
