package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// /flow alone opens the folder's flows on a screen of its own, narrowed as
// typed, not in the conversation; Enter shows one (what it is given, its
// chart), and Enter again puts it in the input to run with what to give
// it; r looks back at its runs; esc goes back, then leaves.
func TestFlowsScreen(t *testing.T) {
	dir := t.TempDir()
	put := func(rel, src string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put(".kairocode/workflows/triage.star", "DESCRIPTION = \"sort the issues\"\nARGS = \"<repo>\"\n\ndef main(input):\n    agent(prompt = \"triage \" + input[\"args\"])\n")
	put(".kairocode/workflows/nightly.star", "DESCRIPTION = \"the nightly check\"\n\ndef main(input):\n    agent(prompt = \"check\")\n")
	put(".kairocode/workflows/drafts/release.star", "DESCRIPTION = \"cut a release\"\n\ndef main(input):\n    agent(prompt = \"release\")\n")
	d, a := start(t, Options{Dir: dir})
	typed(d, "/flow")
	if !d.m.View().AltScreen {
		t.Fatal("not on a screen of its own")
	}
	v := d.view()
	for _, want := range []string{"Flows", "nightly", "the nightly check", "triage <repo>", "sort the issues", "release draft"} {
		if !strings.Contains(v, want) {
			t.Fatalf("%q not in\n%s", want, v)
		}
	}
	for _, k := range strings.Split("sort", "") {
		d.send(key(k))
	}
	if v := d.view(); strings.Contains(v, "nightly") || !strings.Contains(v, "triage") {
		t.Fatalf("narrowed:\n%s", v)
	}
	d.send(key("enter"))
	v = d.view()
	for _, want := range []string{"Flow · triage", "/flow triage <repo>", "Enter to run it", "r its runs"} {
		if !strings.Contains(v, want) {
			t.Fatalf("%q not in the flow:\n%s", want, v)
		}
	}
	d.send(key("enter"))
	if d.m.flows != nil || d.m.View().AltScreen {
		t.Fatal("still open")
	}
	if got := d.m.input.Value(); got != "/flow triage " || len(a.prompts) != 0 {
		t.Fatalf("the input %q, sent %v", got, a.prompts)
	}
	if !strings.Contains(d.m.note, "<repo>") {
		t.Fatalf("the note %q", d.m.note)
	}
	if strings.Contains(d.out(), "sort the issues") {
		t.Fatalf("the list went into the conversation:\n%s", d.out())
	}

	// A draft is tried; r is its runs; esc goes back, then leaves.
	d.m.input.Reset()
	typed(d, "/flow")
	for _, k := range strings.Split("release", "") {
		d.send(key(k))
	}
	d.send(key("enter"))
	if v := d.view(); !strings.Contains(v, "Enter to try it") || strings.Contains(v, "e to change it") {
		t.Fatalf("the draft:\n%s", v)
	}
	d.send(key("r"))
	if d.m.flows != nil || d.m.runs == nil || d.m.runs.filter != "release" {
		t.Fatalf("r: flows %v, runs %+v", d.m.flows, d.m.runs)
	}
	d.send(key("esc"))
	d.send(key("esc"))
	typed(d, "/flow")
	d.send(key("enter"))
	d.send(key("esc"))
	if d.m.flows == nil || d.m.flows.shown != nil {
		t.Fatal("esc did not go back to the list")
	}
	d.send(key("esc"))
	if d.m.flows != nil || len(a.prompts) != 0 {
		t.Fatalf("still open, or sent: %v", a.prompts)
	}
}
