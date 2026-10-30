package opencode

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

var (
	argPattern   = regexp.MustCompile(`(?i)\[Image\s+\d+\]|"[^"]*"|'[^']*'|[^\s"']+`)
	quotePattern = regexp.MustCompile(`^["']|["']$`)
	placeholder  = regexp.MustCompile(`\$(\d+)`)
	shellPattern = regexp.MustCompile("!`([^`]+)`")
)

// Expand makes the prompt of command c run with args, as opencode does, in
// this order:
//
//  1. args split into words; a quoted group is one word, its quotes cut;
//  2. $1, $2, ... are the words; the highest of them in the template takes
//     the rest of the words too, and one past the last word is empty;
//  3. $ARGUMENTS is args as they came;
//  4. a template with neither gets args after it, on its own paragraph;
//  5. each !`command` is replaced with what run prints (args can hold one:
//     opencode runs those too);
//  6. the text is trimmed;
//  7. each @path of a file under dir is followed by the file, appended.
//
// may says whether the project's permission rules let a tool (bash, read)
// act on a subject; one they deny is not run or read, as a tool call would
// not be.
func Expand(c Command, args, dir string, run func(cmd string) (string, error), may func(tool, subject string) bool) (string, error) {
	words := argPattern.FindAllString(args, -1)
	for i, w := range words {
		words[i] = quotePattern.ReplaceAllString(w, "")
	}
	text := c.Template
	last := 0
	for _, m := range placeholder.FindAllStringSubmatch(text, -1) {
		if n, _ := strconv.Atoi(m[1]); n > last {
			last = n
		}
	}
	text = placeholder.ReplaceAllStringFunc(text, func(m string) string {
		n, _ := strconv.Atoi(m[1:])
		switch {
		case n < 1 || n > len(words):
			return ""
		case n == last:
			return strings.Join(words[n-1:], " ")
		}
		return words[n-1]
	})
	hasArguments := strings.Contains(text, "$ARGUMENTS")
	text = strings.ReplaceAll(text, "$ARGUMENTS", args)
	if last == 0 && !hasArguments && strings.TrimSpace(args) != "" {
		text += "\n\n" + args
	}
	var err error
	text = shellPattern.ReplaceAllStringFunc(text, func(m string) string {
		if err != nil {
			return m
		}
		cmd := shellPattern.FindStringSubmatch(m)[1]
		if !may("bash", cmd) {
			err = fmt.Errorf("the permission rules deny the command %q", cmd)
			return m
		}
		var out string
		out, err = run(cmd)
		return out
	})
	if err != nil {
		return "", err
	}
	text = strings.TrimSpace(text)
	return text + attachments(text, dir, may), nil
}

// maxAttachment is the largest file @path attaches.
const maxAttachment = 256 << 10

// attachments are the files the text names as @path (relative to dir,
// inside it), each written out after it once. A name that is not such a
// file stays plain text.
func attachments(text, dir string, may func(tool, subject string) bool) string {
	var b strings.Builder
	seen := map[string]bool{}
	for _, name := range mentions(text) {
		if seen[name] {
			continue
		}
		seen[name] = true
		path := filepath.Join(dir, filepath.FromSlash(name))
		if path != dir && !strings.HasPrefix(path, dir+string(filepath.Separator)) {
			continue
		}
		real, err := filepath.EvalSymlinks(path)
		if err != nil || !strings.HasPrefix(real, evalOr(dir)+string(filepath.Separator)) || !may("read", name) {
			continue
		}
		// What is read is the file checked: real, opened once.
		f, err := os.Open(real)
		if err != nil {
			continue
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxAttachment {
			f.Close()
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, maxAttachment))
		f.Close()
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "\n\n<file path=%q>\n%s\n</file>", name, strings.TrimRight(string(data), "\n"))
	}
	return b.String()
}

func evalOr(dir string) string {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return real
	}
	return dir
}

// mentions are the @names of text, by opencode's rule: an @ not after a
// letter, digit, _ or `, then a name of no spaces, backquotes or commas,
// whose dots each sit between other characters (so a sentence's last
// period is not part of it).
func mentions(text string) []string {
	var out []string
	rs := []rune(text)
	for i := 0; i < len(rs); i++ {
		if rs[i] != '@' {
			continue
		}
		if i > 0 && (rs[i-1] == '`' || rs[i-1] == '_' || unicode.IsLetter(rs[i-1]) || unicode.IsDigit(rs[i-1])) {
			continue
		}
		j := i + 1
		for j < len(rs) && !unicode.IsSpace(rs[j]) && rs[j] != '`' && rs[j] != ',' {
			j++
		}
		name := string(rs[i+1 : j])
		// Dots end the name unless something follows them.
		name = strings.TrimRight(name, ".")
		if name != "" {
			out = append(out, name)
		}
		i = j - 1
	}
	return out
}
