package tui

import (
	"encoding/json"
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

// The list's last row makes a new flow: /flow new in the input, to say
// what it is to do. With no flows, it is the row chosen.
func TestFlowsScreenNew(t *testing.T) {
	for _, c := range []struct {
		name string
		flow bool
		keys []string
	}{{"after the flows", true, []string{"up"}}, {"no flows", false, nil}} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if c.flow {
				os.MkdirAll(filepath.Join(dir, ".kairocode/workflows"), 0o755)
				os.WriteFile(filepath.Join(dir, ".kairocode/workflows/nightly.star"), []byte("def main(input):\n    agent(prompt = \"check\")\n"), 0o644)
			}
			d, a := start(t, Options{Dir: dir})
			typed(d, "/flow")
			if v := d.view(); !strings.Contains(v, "make a new flow") {
				t.Fatalf("no row to make one:\n%s", v)
			}
			for _, k := range c.keys {
				d.send(key(k))
			}
			d.send(key("enter"))
			if d.m.flows != nil || d.m.input.Value() != "/flow new " || len(a.prompts) != 0 {
				t.Fatalf("open %v, the input %q, sent %v", d.m.flows != nil, d.m.input.Value(), a.prompts)
			}
		})
	}
}

// An update that only settles a question's call with its answer (for Zed)
// is not written here: the screen shows the answer its own way.
func TestSettledCallNotWritten(t *testing.T) {
	d, _ := start(t, Options{})
	d.send(updateMsg{Kind: "tool_call_update", ToolCallID: "ask_0_0", Status: "completed", Text: "Answered: left",
		Meta: json.RawMessage(`{"kairocode/settled":true}`)})
	if out := d.out(); strings.Contains(out, "Answered") || strings.Contains(out, "tool call") {
		t.Fatalf("written:\n%s", out)
	}
}
