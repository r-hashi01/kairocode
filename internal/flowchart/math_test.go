package flowchart

import "testing"

func TestMath(t *testing.T) {
	for in, want := range map[string]string{
		`ok.value != "通知する"`:                  `ok.value ≠ "通知する"`,
		`not r.ok or r.output.strip() == ""`:  `¬r.ok ∨ r.output.strip() = ""`,
		`a >= 3 and b <= 4`:                   `a ≥ 3 ∧ b ≤ 4`,
		`x not in ["a and b", "c != d"]`:      `x ∉ ["a and b", "c != d"]`,
		`round in range(5)`:                   `round ∈ range(5)`,
		`notice == "x"`:                       `notice = "x"`,
		`inquiry == "" or inquiry == "キャンセル"`: `inquiry = "" ∨ inquiry = "キャンセル"`,
	} {
		if got := Math(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}
