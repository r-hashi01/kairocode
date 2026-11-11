package api

import (
	"fmt"
	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/workflows"
	"net/http"
	"slices"
	"strings"
	"time"
)

import ()

// Image is an image the user gives with a message (ADR 0026).
type Image struct {
	MediaType string
	Data      []byte
}

// SessionLog is a session's record.
type SessionLog struct {
	Session string             `json:"session"`
	Dir     string             `json:"dir"`
	Engine  string             `json:"engine,omitempty"`
	From    *workflows.ForkRef `json:"from,omitempty"`
	Usage   msg.Usage          `json:"usage"`
	Turns   []LogEntry         `json:"turns"`
}

// LogEntry is a run of the record: a turn, a call, a wait, a child
// workflow (with its own runs).
type LogEntry struct {
	At     time.Time  `json:"at"`
	End    time.Time  `json:"end,omitzero"`
	Kind   string     `json:"kind"` // turn, model, tool, permission, wait, workflow, step
	Name   string     `json:"name"`
	Status string     `json:"status"`
	Text   string     `json:"text,omitempty"`
	Usage  *msg.Usage `json:"usage,omitempty"`
	Runs   []LogEntry `json:"runs,omitempty"`
}

// Text is the record as lines to read.
func (l SessionLog) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Session %s in %s", strings.TrimPrefix(l.Session, "s/"), l.Dir)
	if l.Engine != "" {
		fmt.Fprintf(&b, " (%s)", l.Engine)
	}
	if l.From != nil {
		fmt.Fprintf(&b, ", forked from turn %d of %s", l.From.Turn, strings.TrimPrefix(l.From.Session, "s/"))
	}
	fmt.Fprintf(&b, "\nUsed: %s\n", usageText(l.Usage))
	for _, t := range l.Turns {
		b.WriteString("\n" + l.entriesText([]LogEntry{t}))
	}
	return b.String()
}

// Head is the record's first lines, without its turns; TurnText, one
// turn's lines.
func (l SessionLog) Head() string {
	return SessionLog{Session: l.Session, Dir: l.Dir, Engine: l.Engine, From: l.From, Usage: l.Usage}.Text()
}

// TurnText is turn t's lines, as Text writes them.
func (l SessionLog) TurnText(t LogEntry) string {
	return l.entriesText([]LogEntry{t})
}

// Settings are a session's provider and model: those its next turn runs
// with. Model "" is the provider's default.
type Settings struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Effort   string `json:"effort,omitempty"` // "": the provider's default
	Mode     string `json:"mode"`             // manual, plan (ADR 0016)
}

// Modes are the modes a session can be in (ADR 0016).
var Modes = []string{workflows.ModeManual, workflows.ModeAuto, workflows.ModePlan}

// FailedNote is put before the first message of a session forked from one
// that failed (ADR 0021): the turn cut short may have changed files.
const FailedNote = "[The session this one goes on from stopped on an error during a turn. That turn may have changed the working files; look before going on.]"

// StoppedCancelled is why a turn the user stopped ended, as clients are
// told it (a turn's Stopped).
const StoppedCancelled = "cancelled"

// StoppedLimit is why a turn that reached one of its limits ended (ADR
// 0039): its reply says which, and is not streamed.
const StoppedLimit = "limit"

// SessionInfo is a session as a list of them shows it (kairocode
// sessions, /resume, ACP's session/list).
type SessionInfo struct {
	ID      string    `json:"id"` // the run's: s/<id>
	Dir     string    `json:"dir"`
	Title   string    `json:"title"` // its first message, on a line
	Turns   int       `json:"turns"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"` // its last turn's last change
	Closed  bool      `json:"closed,omitempty"`
	From    string    `json:"from,omitempty"` // forked from (s/<id>)
	// Waiting is what its turns under way wait for from a person
	// (approvals, questions); Jobs, its commands in the background not
	// ended (ADR 0028). What a client shows as going on.
	Waiting int `json:"waiting,omitempty"`
	Jobs    int `json:"jobs,omitempty"`
	// Working: a turn of it is under way.
	Working bool `json:"working,omitempty"`
	// Of a repository's listing (ADR 0037): its folder's repository (the
	// main checkout), the worktree's name ("" for the main checkout), its
	// branch, and how it differs (Tree) from the main checkout's HEAD.
	// Gone: its folder was removed; it takes no more turns.
	Repo     string    `json:"repo,omitempty"`
	Worktree string    `json:"worktree,omitempty"`
	Branch   string    `json:"branch,omitempty"`
	Tree     *TreeDiff `json:"tree,omitempty"`
	Gone     bool      `json:"gone,omitempty"`
}

// TreeDiff is how a worktree differs from the main checkout's HEAD: files
// changed and lines added and removed (what is not committed included),
// files git does not track, and commits ahead.
type TreeDiff struct {
	Files     int `json:"files"`
	Added     int `json:"added"`
	Removed   int `json:"removed"`
	Untracked int `json:"untracked"`
	Ahead     int `json:"ahead"`
}

// State is what the session is doing, in words: gone, closed, working,
// waiting for you, or idle; with its commands in the background.
func (s SessionInfo) State() string {
	var st string
	switch {
	case s.Gone:
		return "folder removed"
	case s.Closed:
		return "closed"
	case s.Waiting > 0:
		st = fmt.Sprintf("waits for you (%d)", s.Waiting)
	case s.Working:
		st = "working"
	default:
		st = "idle"
	}
	if s.Jobs > 0 {
		st += fmt.Sprintf(", %d in the background", s.Jobs)
	}
	return st
}

// Place is where a session of a repository's listing works: "main", or
// its worktree's name; with its branch.
func (s SessionInfo) Place() string {
	p := "main"
	if s.Worktree != "" {
		p = s.Worktree
	}
	if s.Branch != "" {
		p += " (" + s.Branch + ")"
	}
	return p
}

// String is the difference in short: "3 files +40 -2, 1 untracked, 2
// commits ahead", or "no changes".
func (t TreeDiff) String() string {
	var parts []string
	if t.Files > 0 {
		parts = append(parts, fmt.Sprintf("%d %s +%d -%d", t.Files, plural(t.Files, "file"), t.Added, t.Removed))
	}
	if t.Untracked > 0 {
		parts = append(parts, fmt.Sprintf("%d untracked", t.Untracked))
	}
	if t.Ahead > 0 {
		parts = append(parts, fmt.Sprintf("%d %s ahead", t.Ahead, plural(t.Ahead, "commit")))
	}
	if len(parts) == 0 {
		return "no changes"
	}
	return strings.Join(parts, ", ")
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// Worktree is a worktree made for a session (ADR 0037): its folder, name
// and branch. Uncommitted: the folder it was made from had changes, which
// it does not have.
type Worktree struct {
	Dir         string `json:"dir"`
	Name        string `json:"name"`
	Branch      string `json:"branch"`
	Uncommitted bool   `json:"uncommitted,omitempty"`
}

// WorktreeRemoved is what removing a worktree did: its branch deleted,
// or kept because it is not merged.
type WorktreeRemoved struct {
	Branch     string `json:"branch"`
	BranchKept bool   `json:"branchKept,omitempty"`
}

// SessionUsage is what a session's model calls used, as they recorded it.
type SessionUsage struct {
	Session string      `json:"session"`
	Turns   []TurnUsage `json:"turns"`
	Total   msg.Usage   `json:"total"`
}

// TurnUsage is what one turn's model calls used so far.
type TurnUsage struct {
	N        int       `json:"n"`
	Finished bool      `json:"finished"`
	Usage    msg.Usage `json:"usage"`
}

// entriesText is entries, and the runs in each, as lines to read.
func (SessionLog) entriesText(es []LogEntry) string {
	var b strings.Builder
	var write func(es []LogEntry, depth int)
	write = func(es []LogEntry, depth int) {
		for _, e := range es {
			pad := strings.Repeat("  ", depth)
			line := fmt.Sprintf("%s%s %s %s: %s", pad, e.At.Local().Format("15:04:05"), e.Kind, e.Name, e.Status)
			if e.Usage != nil && (e.Usage.Input > 0 || e.Usage.Output > 0) {
				line += " [" + usageText(*e.Usage) + "]"
			}
			if e.Text != "" {
				line += "\n" + pad + "    " + strings.ReplaceAll(e.Text, "\n", "\n"+pad+"    ")
			}
			b.WriteString(line + "\n")
			write(e.Runs, depth+1)
		}
	}
	write(es, 0)
	return b.String()
}

func usageText(u msg.Usage) string {
	return fmt.Sprintf("in %d, out %d, cache read %d, cache write %d tokens", u.Input, u.Output, u.CacheRead, u.CacheWrite)
}

// CheckImages refuses, before anything is stored, what the model would:
// a kind it does not read, more than MaxImageBytes, more than MaxImages.
// The bytes must be of the kind said. Errors name no content.
func CheckImages(images []Image) error {
	if len(images) > msg.MaxImages {
		return fmt.Errorf("%w: %d images, at most %d in a message", ErrBadImage, len(images), msg.MaxImages)
	}
	total := 0
	for _, im := range images {
		total += len(im.Data)
	}
	if total > msg.MaxRequestImageBytes {
		return fmt.Errorf("%w: the images are %d bytes together, at most %d", ErrBadImage, total, msg.MaxRequestImageBytes)
	}
	for i, im := range images {
		if !slices.Contains(msg.ImageTypes, im.MediaType) {
			return fmt.Errorf("%w: image %d is %q; %s are read", ErrBadImage, i+1, im.MediaType, strings.Join(msg.ImageTypes, ", "))
		}
		if len(im.Data) == 0 {
			return fmt.Errorf("%w: image %d is empty", ErrBadImage, i+1)
		}
		if len(im.Data) > msg.MaxImageBytes {
			return fmt.Errorf("%w: image %d is %d bytes, at most %d", ErrBadImage, i+1, len(im.Data), msg.MaxImageBytes)
		}
		if got := http.DetectContentType(im.Data); got != im.MediaType {
			return fmt.Errorf("%w: image %d is said to be %s, but is %s", ErrBadImage, i+1, im.MediaType, got)
		}
	}
	return nil
}
