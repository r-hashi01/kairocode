package flowchart

import "strings"

// Math is a condition as a formula reads: ≠ for !=, = for ==, ≤ and ≥,
// ∧ ∨ ¬ for and, or and not, ∈ and ∉ for in and not in. What is in quotes
// stays as it is.
func Math(cond string) string {
	var b strings.Builder
	words := []struct{ from, to string }{
		{"!=", "≠"}, {"==", "="}, {"<=", "≤"}, {">=", "≥"},
		{" not in ", " ∉ "}, {" and ", " ∧ "}, {" or ", " ∨ "}, {" in ", " ∈ "},
	}
	for i := 0; i < len(cond); {
		c := cond[i]
		if c == '"' || c == '\'' { // a string: as it is
			j := i + 1
			for j < len(cond) && cond[j] != c {
				if cond[j] == '\\' {
					j++
				}
				j++
			}
			j = min(j+1, len(cond))
			b.WriteString(cond[i:j])
			i = j
			continue
		}
		if strings.HasPrefix(cond[i:], "not ") && (i == 0 || !isName(cond[i-1])) {
			b.WriteString("¬")
			i += len("not ")
			continue
		}
		done := false
		for _, w := range words {
			if strings.HasPrefix(cond[i:], w.from) {
				b.WriteString(w.to)
				i += len(w.from)
				done = true
				break
			}
		}
		if !done {
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

func isName(c byte) bool {
	return c == '_' || c == '.' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
