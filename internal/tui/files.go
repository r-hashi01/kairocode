package tui

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// @ mentions: @path points to a file (or a folder) of the session's
// folder; the agent puts it in the message. Typing @ completes the paths.

const filesMax = 20000 // paths listed for completion at most

// filesMsg: the folder's paths, for completion.
type filesMsg struct{ paths []string }

// skipDir: folders not walked for completion (their files can still be
// typed).
var skipDir = map[string]bool{".git": true, "node_modules": true, "vendor": true, "target": true, "dist": true, "build": true, "__pycache__": true}

// listFiles walks dir for its paths (relative, a folder's with a /).
func listFiles(dir string) tea.Cmd {
	return func() tea.Msg {
		var out []string
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || path == dir {
				return nil
			}
			if len(out) >= filesMax {
				return filepath.SkipAll
			}
			rel, _ := filepath.Rel(dir, path)
			if d.IsDir() {
				if skipDir[d.Name()] || strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				out = append(out, rel+"/")
				return nil
			}
			out = append(out, rel)
			return nil
		})
		return filesMsg{paths: out}
	}
}

// completion is a choice the input can be completed with.
type completion struct {
	label, desc string
	value       string // the input as it is when chosen
	// offered: a command's option. Enter sends what is typed (/flow alone,
	// /provider codex), unless one was moved to; tab takes it.
	offered bool
}

// mentionAt is the @word the input ends in: what is before it and what is
// typed of the path.
func mentionAt(v string) (before, typed string, ok bool) {
	i := strings.LastIndexAny(v, " \n\t")
	word := v[i+1:]
	if !strings.HasPrefix(word, "@") {
		return "", "", false
	}
	return v[:i+1], word[1:], true
}

// fileCompletions are the paths that fit what is typed after @: those
// that begin with it, then those whose name does, then those that have it.
func (m *model) fileCompletions(v string) []completion {
	before, typed, ok := mentionAt(v)
	if !ok || m.files == nil {
		return nil
	}
	t := strings.ToLower(typed)
	var starts, names, has []string
	for _, f := range m.files {
		l := strings.ToLower(f)
		switch {
		case strings.HasPrefix(l, t):
			starts = append(starts, f)
		case strings.HasPrefix(strings.ToLower(filepath.Base(strings.TrimSuffix(f, "/"))), t):
			names = append(names, f)
		case strings.Contains(l, t):
			has = append(has, f)
		}
	}
	var out []completion
	for _, f := range append(append(starts, names...), has...) {
		if len(out) == 6 {
			break
		}
		out = append(out, completion{label: "@" + f, value: before + "@" + f + " "})
	}
	return out
}

// mention is an @path in what is sent; a space in it is escaped ("\ "),
// as a terminal pastes a path dropped on it.
var mention = regexp.MustCompile(`(?:^|\s)@((?:\\\s|\S)+)`)

// imageExts are the files sent as images (ADR 0026): from anywhere, not
// only the session's folder (a screenshot is on the desktop).
var imageExts = []string{".png", ".jpg", ".jpeg", ".gif", ".webp"}

// IsImage says whether path is a file sent as an image.
func IsImage(path string) bool {
	return slices.Contains(imageExts, strings.ToLower(filepath.Ext(path)))
}

// imagePath is an image file path as pasted (a file dropped on the
// terminal: quoted, or its spaces escaped), or "" if it is not one.
func imagePath(pasted string) string {
	p := strings.TrimSpace(pasted)
	if len(p) >= 2 && (p[0] == '\'' || p[0] == '"') && p[len(p)-1] == p[0] {
		p = p[1 : len(p)-1]
	} else {
		p = strings.ReplaceAll(p, "\\ ", " ")
	}
	if !filepath.IsAbs(p) || !IsImage(p) {
		return ""
	}
	if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() {
		return ""
	}
	return p
}

// mentionOf is @path for a path, its spaces escaped.
func mentionOf(path string) string { return "@" + strings.ReplaceAll(path, " ", "\\ ") }

// Mentioned are the files and folders of dir that text points to (@path).
func Mentioned(dir, text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range mention.FindAllStringSubmatch(text, -1) {
		p := strings.ReplaceAll(strings.TrimRight(m[1], ".,;:!?)"), "\\ ", " ")
		abs := filepath.Join(dir, p)
		if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, "~/") {
			p = filepath.Join(home, p[2:])
		}
		if filepath.IsAbs(p) && IsImage(p) {
			abs = p
		}
		if seen[abs] {
			continue
		}
		if _, err := os.Stat(abs); err == nil {
			seen[abs] = true
			out = append(out, abs)
		}
	}
	return out
}
