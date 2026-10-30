package acp

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Files a prompt points to (@path in the terminal, or an editor's
// resource_link): read here and put in the user's message, so the model
// has them as they are when it is asked. Only files in the session's
// folder are read.

const (
	attachMax     = 256 << 10 // a file's bytes put in at most
	attachEntries = 200       // a folder's entries listed at most
)

// attachment is what a link to path puts in the message: the file's text,
// a folder's entries, or why it is not there.
func attachment(dir, uri string) string {
	path := uri
	if u, err := url.Parse(uri); err == nil && u.Scheme == "file" {
		path = u.Path
	} else if err == nil && u.Scheme != "" {
		return "" // not a file: the link stays as it was written
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	rel, err := inside(dir, path)
	if err != nil {
		return fmt.Sprintf("[%s: %v]", uri, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Sprintf("[%s: it could not be read]", rel)
	}
	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return fmt.Sprintf("[%s: it could not be read]", rel)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "<folder path=%q>\n", rel)
		for i, e := range entries {
			if i == attachEntries {
				fmt.Fprintf(&b, "… and %d more\n", len(entries)-i)
				break
			}
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			b.WriteString(name + "\n")
		}
		b.WriteString("</folder>")
		return b.String()
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("[%s: it could not be read]", rel)
	}
	defer f.Close()
	body := make([]byte, attachMax+1)
	n, _ := f.Read(body)
	for n < len(body) {
		m, err := f.Read(body[n:])
		n += m
		if err != nil || m == 0 {
			break
		}
	}
	body = body[:n]
	if bytes.IndexByte(body[:min(len(body), 8<<10)], 0) >= 0 {
		return fmt.Sprintf("[%s: a binary file (%d bytes), not put in]", rel, info.Size())
	}
	cut := ""
	if len(body) > attachMax {
		body, cut = body[:attachMax], fmt.Sprintf("\n… cut at %d of its %d bytes", attachMax, info.Size())
	}
	return fmt.Sprintf("<file path=%q>\n%s%s\n</file>", rel, strings.TrimRight(string(body), "\n"), cut)
}

// inside is path relative to dir, when it is in dir (links followed).
func inside(dir, path string) (string, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("the session's folder could not be read")
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("it does not exist")
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("it is outside the session's folder: not read")
	}
	return rel, nil
}
