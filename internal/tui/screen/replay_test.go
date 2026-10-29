package screen

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/r-hashi01/kairocode/internal/tui/screen/vtest"
)

// TestReplayTrace plays a trace of what the terminal was sent
// (KAIROCODE_TUI_TRACE) on the tests' terminal, and shows what it shows
// after each write, with where the screen thought its area was: to find
// where a terminal did otherwise. Run with KAIROCODE_TUI_REPLAY=<trace>.
func TestReplayTrace(t *testing.T) {
	path := os.Getenv("KAIROCODE_TUI_REPLAY")
	if path == "" {
		t.Skip("KAIROCODE_TUI_REPLAY=<trace> plays a trace")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<24)
	var term *vtest.VT
	n := 0
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "# ") && term == nil {
			var w, h int
			fmt.Sscanf(line, "# %dx%d", &w, &h)
			term = vtest.New(w, h)
			continue
		}
		if strings.HasPrefix(line, "#") {
			if os.Getenv("KAIROCODE_TUI_REPLAY_ALL") != "" {
				t.Logf("after write %d (%s):\n%s", n, line, strings.Join(term.Rows(), "\n"))
			}
			continue
		}
		s, err := strconv.Unquote(line)
		if err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		n++
		term.Write([]byte(s))
	}
	t.Logf("at the end (%d writes):\n%s", n, strings.Join(term.Rows(), "\n"))
}
