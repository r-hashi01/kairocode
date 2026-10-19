package flowchart

import (
	"strings"
	"testing"
)

const develop = `
DESCRIPTION = "Plan, test first, build, review"
LIMITS = {"agents": 20}

def check():
    return tool("bash", command = "scripts/check.sh")

def main(input):
    phase("plan")
    plan = agent(agent = "plan", prompt = "Plan this: " + input["args"])
    answer = ask("Go on with this plan?", options = ["Go on", "Stop"])
    if answer.value != "Go on":
        return {"ok": False, "message": "stopped"}
    phase("build")
    reviews = parallel([{"agent": "reviewer", "prompt": "review %s" % f} for f in input["args"].split(" ")])
    for round in range(5):
        agent(agent = "build", prompt = "make the tests pass")
        if check().ok:
            break
    both = parallel([{"agent": "a", "prompt": "one"}, {"agent": "b", "prompt": "two"}])
    done = decide("Is it done?")
    return "done" if done.value else agent(agent = "build", prompt = "finish")
`

// The chart follows the script: its primitives in order, the branch, the
// loop with the def it calls drawn in place, both kinds of parallel, and
// a conditional expression that calls an agent.
func TestBuild(t *testing.T) {
	c, err := Build("develop", []byte(develop))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSpace(`
develop  Plan, test first, build, review
├─ phase plan
├─ agent plan  "Plan this: " + input["args"]
├─ ask  "Go on with this plan?"
├─ if answer.value != "Go on"
│  └─ then
│     └─ return  {"ok": False, "message": "stopped"}
├─ phase build
├─ parallel for f in input["args"].split(" ")
│  └─ agent reviewer  "review %s" % f
├─ for round in range(5)
│  ├─ agent build  "make the tests pass"
│  ├─ tool bash  "scripts/check.sh"
│  └─ if check().ok
│     └─ then
│        └─ break
├─ parallel
│  ├─ agent a  "one"
│  └─ agent b  "two"
├─ decide  "Is it done?"
├─ if done.value
│  ├─ then
│  └─ else
│     └─ agent build  "finish"
└─ return  "done" if done.value else agent(agent = "build", prompt = "…`)
	if got := Tree(c, nil); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestBuildNeedsMain(t *testing.T) {
	if _, err := Build("x", []byte("X = 1\n")); err == nil {
		t.Fatal("a script with no main")
	}
	if _, err := Build("x", []byte("def main(:\n")); err == nil {
		t.Fatal("a script that does not parse")
	}
}
