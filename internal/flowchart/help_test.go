package flowchart

import (
	"strings"
	"testing"
)

// /flow <name> --help says what a flow is to be given: its ARGS, or the
// lines that read its words; and its phases (ADR 0003, 追記).
func TestHelp(t *testing.T) {
	withArgs := "DESCRIPTION = \"Comment on the issue to fix first\"\nARGS = \"<owner/repo> [label]\"\ndef main(input):\n    phase(\"read\")\n    owner, repo = input[\"args\"].split(\"/\")\n    phase(\"comment\")\n    return \"done\"\n"
	h, err := Help("triage", []byte(withArgs), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"triage\n  Comment on the issue to fix first", "/flow triage <owner/repo> [label]", "Phases: read → comment"} {
		if !strings.Contains(h, want) {
			t.Fatalf("%q not in\n%s", want, h)
		}
	}
	noArgs := "def main(input):\n    if True:\n        w = input[\"args\"] or \"world\"\n    return \"hi \" + w\n"
	h, _ = Help("hi", []byte(noArgs), true)
	for _, want := range []string{"(a draft: /flow hi --draft tries it", "/flow hi --draft <words>", `3: w = input["args"] or "world"`} {
		if !strings.Contains(h, want) {
			t.Fatalf("%q not in\n%s", want, h)
		}
	}
	h, _ = Help("plain", []byte("def main(input):\n    return \"x\"\n"), false)
	if !strings.Contains(h, "it reads no words") {
		t.Fatalf("no words:\n%s", h)
	}
}
