// Package locale is the language a person reads KairoCode's reviews in:
// the words of a flow's review (what it reaches, what a try did), not its
// labels (REACHES, models), which stay as they are, as a pull request's
// do.
package locale

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Languages.
const (
	English  = "en"
	Japanese = "ja"
)

// Env names the language over all else (KAIROCODE_LANG=ja).
const Env = "KAIROCODE_LANG"

// Lang is the language to show: KAIROCODE_LANG, else "language" in the
// user's kairocode.json, else the terminal's locale (LC_ALL, LC_MESSAGES,
// LANG); English where none says.
func Lang() string {
	if l := os.Getenv(Env); l != "" {
		return of(l)
	}
	if l := configured(); l != "" {
		return of(l)
	}
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if l := os.Getenv(k); l != "" {
			return of(l)
		}
	}
	return English
}

// of is the language a locale or a name is of.
func of(l string) string {
	if strings.HasPrefix(strings.ToLower(l), "ja") {
		return Japanese
	}
	return English
}

// configured is "language" in the user's kairocode.json ("" if none).
func configured() string { return UserSetting("language") }

// UserSetting is the string setting name in the user's kairocode.json
// ($XDG_CONFIG_HOME/kairocode, else ~/.config/kairocode); "" if none.
// Only the user's: a project does not set what is the reader's.
func UserSetting(name string) string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	b, err := os.ReadFile(filepath.Join(dir, "kairocode", "kairocode.json"))
	if err != nil {
		return ""
	}
	var c map[string]json.RawMessage
	if json.Unmarshal(b, &c) != nil {
		return ""
	}
	var v string
	if json.Unmarshal(c[name], &v) != nil {
		return ""
	}
	return v
}

// phrase is a phrase of a review and what it is in a language.
type phrase struct {
	re *regexp.Regexp
	to string
}

func p(re, to string) phrase { return phrase{regexp.MustCompile(re), to} }

// japanese are the review's phrases in Japanese, the longer first. The
// labels before a row's value are not among them.
var japanese = []phrase{
	p(`runs a command · cannot be undone`, "コマンドを実行 · 取り消せない"),
	p(`sends to (\S+) · outside service`, "${1} に送る · 外部サービス"),
	p(`outside service`, "外部サービス"),
	p(`cannot be undone`, "取り消せない"),
	p(`changes files`, "ファイルを変える"),
	p(`replies of agents at once`, "並列のエージェントの返事"),
	p(`reply of agent (\S+)`, "agent ${1} の返事"),
	p(`result of tool (\S+)`, "tool ${1} の結果"),
	p(`decision of the model`, "モデルの判断"),
	p(`your answer`, "あなたの答え"),
	p(`your words`, "実行時に渡す言葉"),
	p(`✓ yes, in (.+)$`, "✓ はい（${1} で）"),
	p(`✓ yes$`, "✓ はい"),
	p(`(\s{2,})⚠ no$`, "${1}⚠ いいえ"),
	p(`(\s{2,})none$`, "${1}なし"),
	p(`\(\+ more in a loop\)`, "（繰り返しの中でさらに増える）"),
	p(`not asked first$`, "確認せずに動く"),
	p(`may not end$`, "終わらないかもしれない"),
	p(`✓ nothing found$`, "✓ 見つからない"),
	p(`\? not given by the model$`, "? モデルが示していない"),
	p(`→ no phase (.+)$`, "→ 段 ${1} が無い"),
	p(`<words>`, "<言葉>"),
	p(`… (\d+) the same$`, "… ${1} 段は同じ"),
	p(`no step changed$`, "変わった段は無い"),
	p(`\(not readable as a flow\)`, "（フローとして読めない）"),
	p(`(\s{2,})not called$`, "${1}呼んでいない"),
	p(`(\s{2,})replied$`, "${1}返事"),
	p(`✓ nothing$`, "✓ 何もしていない"),
	p(`✗ stopped: `, "✗ 止まった: "),
	p(`^sends `, "送る: "),
}

// Review is text of a review (a line at a time) in lang. Only KairoCode's
// own words are put in another: a line that quotes the script (a string
// in quotes) or is a block shown as it is (indented eight spaces: what an
// agent replied, what a call would have been given) is left as it is, as
// what is approved must read as what is written and sent.
func Review(lang, text string) string {
	if lang != Japanese || text == "" {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if strings.Contains(l, `"`) || strings.HasPrefix(l, "        ") {
			continue
		}
		for _, ph := range japanese {
			l = ph.re.ReplaceAllString(l, ph.to)
		}
		lines[i] = l
	}
	return strings.Join(lines, "\n")
}
