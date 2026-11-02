package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/r-hashi01/kairocode/internal/api"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/r-hashi01/kairocode/internal/flowchart"
	"github.com/r-hashi01/kairocode/internal/flowfile"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// preview is what a call would do, for the user asked about it: for a
// flow, what it does, its shape, how it changes the flow there was, and its
// script, in named sections ("" for other calls, or a script that does not
// parse).
func (r *Runtime) preview(ctx context.Context, in workflows.PermissionInput) string {
	if in.Tool == tools.HTTPTool.Name {
		return r.httpPreview(ctx, in)
	}
	if in.Tool != tools.FlowWriteTool.Name && in.Tool != tools.FlowPublishTool.Name {
		return ""
	}
	args, err := r.toolArgs(ctx, in.Call)
	if err != nil {
		return ""
	}
	var a struct {
		Name   string           `json:"name"`
		Source string           `json:"source"`
		Asked  []flowchart.Need `json:"asked"`
	}
	if json.Unmarshal(args, &a) != nil {
		return ""
	}
	if in.Tool == tools.FlowPublishTool.Name { // what is published: the draft as it is (ADR 0030)
		root, err := os.OpenRoot(in.Call.Dir)
		if err != nil {
			return ""
		}
		src, err := tools.DraftOf(tools.Env{Root: root, Dir: in.Call.Dir}, a.Name)
		root.Close()
		if err != nil {
			return ""
		}
		a.Source = string(src)
	}
	c, err := flowchart.Build(a.Name, []byte(a.Source))
	if err != nil {
		return ""
	}
	// What it is, whether it is what was asked, what it reaches: kept in
	// the record. The script, and the flow there now it replaces, are
	// stored by hash: its chart and its changes are drawn from them when
	// the question is listed (the record stays small, NFR-5).
	// What was asked is the model's words: kept short in the record.
	if len(a.Asked) > maxAsked {
		a.Asked = a.Asked[:maxAsked]
	}
	for i := range a.Asked {
		a.Asked[i].Need = clip(a.Asked[i].Need, 200)
		a.Asked[i].Phase = clip(a.Asked[i].Phase, 60)
	}
	before, beforeSrc, there := r.flowBefore(in.Call.Dir, a.Name)
	var removed []string
	if before != nil {
		removed = flowchart.Removed(before, c)
	}
	overview, orefs := flowchart.Overview(c, flowchart.OverviewInput{Asked: a.Asked, Note: r.stepReach, Removed: removed})
	p := overview + withRefs(orefs)
	h, err := r.Store.Put(ctx, store.KindContent, []byte(a.Source))
	if err != nil {
		return p
	}
	p += scriptRef + h + nameRef + a.Name // the name too, to try it (ADR 0017)
	switch {
	case before != nil:
		bh, err := r.Store.Put(ctx, store.KindContent, beforeSrc)
		if err != nil {
			bh = unreadable // it is replaced still
		}
		p += oldRef + bh
	case there:
		p += oldRef + unreadable
	}
	return p
}

// scriptRef, at the end of a recorded preview, names a flow's script by
// its hash in the store; sectionSep starts a named section of it (the
// first is the summary).
const (
	scriptRef  = "\n\x00script:"
	sectionSep = "\n\x00section:"
	// refsSep ends a section's text: then for each of its lines, the
	// script's lines it is ("first-last", 0-0 for none), by commas.
	refsSep = "\n\x00refs:"
	// nameRef, after scriptRef's hash, gives the flow's name; oldRef,
	// after it, the hash of the flow there it replaces (unreadable: one
	// that could not be read as a flow).
	nameRef    = "\x00name:"
	oldRef     = "\x00old:"
	unreadable = "!"
)

type PreviewSection = api.PreviewSection

// withRefs is a section's refs as recorded after its text.
func withRefs(refs []flowchart.Lines) string {
	parts := make([]string, len(refs))
	for i, l := range refs {
		parts[i] = fmt.Sprintf("%d-%d", l.First, l.Last)
	}
	return refsSep + strings.Join(parts, ",")
}

// section is a recorded section's text and refs.
func section(name, s string) PreviewSection {
	text, rs, ok := strings.Cut(s, refsSep)
	sec := PreviewSection{Name: name, Text: text}
	if !ok {
		return sec
	}
	for _, r := range strings.Split(rs, ",") {
		var f, l int
		if _, err := fmt.Sscanf(r, "%d-%d", &f, &l); err != nil || f < 0 || l < 0 {
			return PreviewSection{Name: name, Text: text}
		}
		sec.Refs = append(sec.Refs, [2]int{f, l})
	}
	return sec
}

// previewScript is the flow a recorded preview names: its script's hash
// and its name (empty for a preview of none, or one recorded before the
// name was).
func previewScript(p string) (hash, name string) {
	hash, name, _ = previewScripts(p)
	return hash, name
}

// previewScripts is previewScript, and the hash of the flow it replaces
// ("": none; unreadable: one that could not be read).
func previewScripts(p string) (hash, name, old string) {
	_, ref, ok := strings.Cut(p, scriptRef)
	if !ok {
		return "", "", ""
	}
	hash, rest, _ := strings.Cut(ref, nameRef)
	name, old, _ = strings.Cut(rest, oldRef)
	return hash, name, old
}

// fullPreview is a recorded preview with the script it names put in: as
// one text, and in its sections.
func (r *Runtime) fullPreview(ctx context.Context, p string) (string, []PreviewSection) {
	if p == "" {
		return "", nil
	}
	h, name, old := previewScripts(p)
	p, _, hasScript := strings.Cut(p, scriptRef)
	parts := strings.Split(p, sectionSep)
	sections := []PreviewSection{section("Overview", parts[0])}
	for _, s := range parts[1:] { // as recorded before the chart was drawn here
		name, text, _ := strings.Cut(s, "\n")
		sections = append(sections, section(name, text))
	}
	if hasScript {
		if _, src, err := r.Store.Get(ctx, h); err == nil {
			if len(parts) == 1 && name != "" {
				sections = append(sections, r.reviewSections(ctx, name, string(src), old)...)
			}
			sections = append(sections, PreviewSection{Name: "Script", Text: strings.TrimRight(string(src), "\n")})
		}
	}
	text := sections[0].Text
	for _, s := range sections[1:] {
		text += "\n\n── " + s.Name + " ──\n" + s.Text
	}
	return text, sections
}

// flowBefore is the flow name as it is in dir now, its script, and
// whether there is one (nil but there: it cannot be read as a flow). Only
// a plain file of at most flowMax bytes is read.
func (r *Runtime) flowBefore(dir, name string) (*flowchart.Chart, []byte, bool) {
	if !flowchart.ValidName(name) {
		return nil, nil, false
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, false
	}
	defer root.Close()
	path := flowfile.Path(name, false)
	fi, err := root.Lstat(path)
	if err != nil {
		return nil, nil, false
	}
	if !fi.Mode().IsRegular() || fi.Size() > flowMax {
		return nil, nil, true
	}
	src, err := root.ReadFile(path)
	if err != nil {
		return nil, nil, true
	}
	c, err := flowchart.Build(name, src)
	if err != nil {
		return nil, nil, true
	}
	return c, src, true
}

// maxAsked is the most of what was asked shown in a flow's review.
const maxAsked = 20

// flowMax is the most of a flow there now read to show what replaces it.
const flowMax = 1 << 20

// stepReach says what a flow's tool step reaches, for the user deciding
// on it: an outside service, what cannot be undone, or files.
func (r *Runtime) stepReach(n flowchart.Node) (string, flowchart.Reach) {
	if n.Kind != flowchart.Tool {
		return "", flowchart.ReachNone
	}
	d, ok := r.tool(n.Title)
	if server, _, isMCP := strings.Cut(strings.TrimPrefix(n.Title, "mcp__"), "__"); strings.HasPrefix(n.Title, "mcp__") && isMCP {
		return "sends to " + server + " · outside service", flowchart.ReachOutside
	}
	switch {
	case !ok:
		return "outside service", flowchart.ReachOutside
	case d.Effect == tools.Real && n.Title == "bash":
		return "runs a command · cannot be undone", flowchart.ReachOutside
	case d.Effect == tools.Real:
		return "cannot be undone", flowchart.ReachOutside
	case d.Effect == tools.Idempotent:
		return "changes files", flowchart.ReachFiles
	}
	return "", flowchart.ReachNone
}

func firstLineOf(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// indent is s with each line after prefix.
func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// prettyJSON is raw indented, or raw as it is.
func prettyJSON(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// clipLines is s in at most n lines.
func clipLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = append(lines[:n], fmt.Sprintf("… %d more lines", len(lines)-n))
	}
	return strings.Join(lines, "\n")
}

// reviewSections are a flow's review drawn from its script: its chart
// (each tool that reaches out marked, with what it sends; each question
// with its options), and what it changes of the flow there (old, its
// hash), a line a step, each with its before and after.
func (r *Runtime) reviewSections(ctx context.Context, name, src, old string) []PreviewSection {
	c, err := flowchart.Build(name, []byte(src))
	if err != nil {
		return nil
	}
	tree, refs := flowchart.TreeLines(c, nil)
	var mark func([]flowchart.Node)
	mark = func(nodes []flowchart.Node) {
		for i := range nodes {
			n := &nodes[i]
			switch n.Kind {
			case flowchart.Tool:
				if _, reach := r.stepReach(*n); reach == flowchart.ReachOutside {
					n.Title += " ⚠"
				}
				if len(n.Uses) > 0 {
					n.Text, n.Code = "sends "+strings.Join(n.Uses, ", "), false
				}
			case flowchart.Ask:
				if len(n.Options) > 0 {
					n.Text += "  [" + strings.Join(n.Options, " | ") + "]"
				}
			}
			mark(n.Then)
			mark(n.Else)
			mark(n.Body)
			for _, b := range n.Branches {
				mark(b)
			}
		}
	}
	mark(c.Nodes)
	chart, err := json.Marshal(c)
	if err != nil {
		return nil
	}
	flow := PreviewSection{Name: "Flow", Text: tree, Chart: chart}
	for _, l := range refs {
		flow.Refs = append(flow.Refs, [2]int{l.First, l.Last})
	}
	out := []PreviewSection{flow}
	switch old {
	case "":
		return out
	case unreadable:
		return append(out, PreviewSection{Name: "Changes",
			Text: "replaces  " + flowfile.Path(name, false) + "  (not readable as a flow)"})
	}
	// It replaces a flow: said, whether or not its changes can be drawn.
	replaces := PreviewSection{Name: "Changes", Text: "replaces  " + flowfile.Path(name, false)}
	_, before, err := r.Store.Get(ctx, old)
	if err != nil {
		return append(out, replaces)
	}
	b, err := flowchart.Build(name, before)
	if err != nil {
		return append(out, replaces)
	}
	plain, err := flowchart.Build(name, []byte(src)) // not marked as the chart is
	if err != nil {
		return append(out, replaces)
	}
	cs, same := flowchart.Changes(b, plain, string(before), src)
	sec := PreviewSection{Name: "Changes"}
	var lines []string
	add := func(line, detail string, src flowchart.Lines) {
		lines = append(lines, line)
		sec.Details = append(sec.Details, detail)
		sec.Refs = append(sec.Refs, [2]int{src.First, src.Last})
	}
	add("replaces  "+flowfile.Path(name, false), "", flowchart.Lines{})
	add("", "", flowchart.Lines{})
	if len(cs) == 0 {
		add("  no step changed", "", flowchart.Lines{})
	}
	diff := func(sign, text string) string {
		var out []string
		for _, l := range strings.Split(text, "\n") {
			out = append(out, sign+" "+l)
		}
		return strings.Join(out, "\n")
	}
	for _, ch := range cs {
		line := ch.Mark + " " + ch.Label
		if ch.Phase != "" {
			line += "  (" + ch.Phase + ")"
		}
		var d []string
		if ch.Before != "" {
			d = append(d, "before", diff("-", ch.Before))
		}
		if ch.After != "" {
			d = append(d, "after", diff("+", ch.After))
		}
		add(line, strings.Join(d, "\n"), ch.Src)
	}
	if same > 0 {
		add(fmt.Sprintf("… %d the same", same), "", flowchart.Lines{})
	}
	sec.Text = strings.Join(lines, "\n")
	return append(out, sec)
}

// httpPreview is what an http call sends, for the person who is asked:
// its URL, the headers the model set, and its body (cut), which the
// subject (host and path) leaves out.
func (r *Runtime) httpPreview(ctx context.Context, in workflows.PermissionInput) string {
	args, err := r.toolArgs(ctx, in.Call)
	if err != nil {
		return ""
	}
	var a struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	}
	if json.Unmarshal(args, &a) != nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "POST %s\n", a.URL)
	for _, k := range slices.Sorted(maps.Keys(a.Headers)) {
		fmt.Fprintf(&b, "%s: %s\n", k, clip(a.Headers[k], 200))
	}
	if a.Body != "" {
		b.WriteString("\n" + clip(a.Body, 2000))
	}
	return strings.TrimRight(b.String(), "\n")
}
