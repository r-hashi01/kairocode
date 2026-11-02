package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/r-hashi01/kairocode/internal/flowchart"
	"github.com/r-hashi01/kairocode/internal/flowfile"
)

// flowEdit is a flow being changed on its chart (/flow edit, ADR 0003):
// the script, its chart, and the box chosen. A change is made to the
// script where the box is (flowchart.SetText, Delete, Move), checked, and
// written; one that cannot be made there is asked for in words.
type flowEdit struct {
	name  string
	src   []byte
	chart *flowchart.Chart
	sel   int
	// words: the chosen box's words are being typed in the input.
	words bool
	note  string
}

// boxes are the chart's boxes that can be changed, in order.
func (e *flowEdit) boxes() []*flowchart.Node {
	var out []*flowchart.Node
	var walk func([]flowchart.Node)
	walk = func(nodes []flowchart.Node) {
		for i := range nodes {
			if nodes[i].Edit != nil {
				out = append(out, &nodes[i])
			}
			walk(nodes[i].Then)
			walk(nodes[i].Else)
			walk(nodes[i].Body)
			for _, b := range nodes[i].Branches {
				walk(b)
			}
		}
	}
	walk(e.chart.Nodes)
	return out
}

func (e *flowEdit) chosen() *flowchart.Node {
	b := e.boxes()
	if len(b) == 0 {
		return nil
	}
	e.sel = min(max(e.sel, 0), len(b)-1)
	return b[e.sel]
}

// openFlowEdit starts changing flow name of dir on its chart.
func openFlowEdit(dir, name string) (*flowEdit, error) {
	src, err := flowfile.Read(dir, name, false)
	if err != nil {
		return nil, err
	}
	c, err := flowchart.Build(name, src)
	if err != nil {
		return nil, err
	}
	return &flowEdit{name: name, src: src, chart: c}, nil
}

// apply checks a changed script, and writes it if nothing is wrong.
func (m *model) applyEdit(src []byte, err error) {
	e := m.edit
	if err != nil {
		e.note = err.Error()
		return
	}
	if problems := flowchart.Check(e.name, src); len(problems) > 0 {
		e.note = "not changed: " + problems[0]
		return
	}
	c, err := flowchart.Build(e.name, src)
	if err != nil {
		e.note = "not changed: " + err.Error()
		return
	}
	if err := flowfile.Write(m.o.Dir, e.name, src); err != nil {
		e.note = err.Error()
		return
	}
	flowchart.Diff(e.chart, c)
	e.src, e.chart, e.note = src, c, "saved"
}

// editKey is a key while a flow is changed on its chart.
func (m *model) editKey(k tea.KeyPressMsg) tea.Cmd {
	e := m.edit
	key := k.String()
	if e.words {
		switch key {
		case "enter":
			if n := e.chosen(); n != nil {
				src, err := flowchart.SetText(e.src, *n, m.input.Value())
				m.applyEdit(src, err)
			}
			e.words = false
			m.input.Reset()
			return nil
		case "esc":
			e.words = false
			m.input.Reset()
			return nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(k)
		return cmd
	}
	e.note = ""
	switch key {
	case "up", "k":
		e.sel--
	case "down", "j":
		e.sel++
	case "shift+up", "shift+down":
		if n := e.chosen(); n != nil {
			by := 1
			if key == "shift+up" {
				by = -1
			}
			src, err := flowchart.Move(e.src, *n, by)
			m.applyEdit(src, err)
			if err == nil && e.note == "saved" {
				e.sel += by
			}
		}
	case "d", "delete", "backspace":
		if n := e.chosen(); n != nil {
			src, err := flowchart.Delete(e.src, *n)
			m.applyEdit(src, err)
		}
	case "enter":
		if n := e.chosen(); n != nil {
			if n.Edit.Lit == nil {
				e.note = flowchart.ErrNotText.Error()
				return nil
			}
			e.words = true
			m.input.SetValue(n.Text)
			m.input.CursorEnd()
		}
	case "esc", "q":
		m.edit = nil
		m.block(accent.Render("✎ flow ") + bold.Render(e.name) + faint.Render("  /flow "+e.name+" runs it") + "\n" + Chart(e.chart, m.width-1, nil))
	}
	return nil
}

// editView is the chart being changed, the chosen box lit, and what keys do.
func (m *model) editView(w int) string {
	e := m.edit
	chosen := e.chosen()
	out := Chart(e.chart, w, func(n flowchart.Node) string {
		if chosen != nil && n.Edit == chosen.Edit {
			return StateRunning
		}
		return StatePending
	})
	hint := "↑↓ choose · enter change its words · d delete · shift+↑↓ move · esc done"
	if e.words {
		hint = "type its words · enter save · esc keep them"
	}
	lines := []string{out, "", faint.Render(hint)}
	if e.note != "" {
		style := warn
		if e.note == "saved" {
			style = good
		}
		lines = append(lines, style.Render(e.note))
	}
	if strings.TrimSpace(out) == "" {
		lines = lines[1:]
	}
	return strings.Join(lines, "\n")
}
