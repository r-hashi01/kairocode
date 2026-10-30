package locale

import (
	"os"
	"path/filepath"
	"testing"
)

// The language: KAIROCODE_LANG, then the user's kairocode.json, then the
// terminal's locale.
func TestLang(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	for _, k := range []string{Env, "LC_ALL", "LC_MESSAGES", "LANG"} {
		t.Setenv(k, "")
	}
	if l := Lang(); l != English {
		t.Fatalf("none: %s", l)
	}
	t.Setenv("LANG", "ja_JP.UTF-8")
	if l := Lang(); l != Japanese {
		t.Fatalf("LANG: %s", l)
	}
	os.MkdirAll(filepath.Join(cfg, "kairocode"), 0o755)
	os.WriteFile(filepath.Join(cfg, "kairocode", "kairocode.json"), []byte(`{"language": "en"}`), 0o644)
	if l := Lang(); l != English {
		t.Fatalf("the setting over the locale: %s", l)
	}
	t.Setenv(Env, "ja")
	if l := Lang(); l != Japanese {
		t.Fatalf("KAIROCODE_LANG over all: %s", l)
	}
}

// A review's words are put in Japanese; its labels stay.
func TestReview(t *testing.T) {
	in := `▌ REACHES
  ⚠ bash                    runs a command · cannot be undone
      command               "curl " + url
      asked before it       ✓ yes, in confirm
  ⚠ mcp__slack__post        sends to slack · outside service
      text                  reply of agent plan (draft)
      asked before it       ⚠ no
  files                     none
  models                    agent plan ×1 (+ more in a loop)
  + not asked               → confirm`
	want := `▌ REACHES
  ⚠ bash                    コマンドを実行 · 取り消せない
      command               "curl " + url
      asked before it       ✓ はい（confirm で）
  ⚠ mcp__slack__post        slack に送る · 外部サービス
      text                  agent plan の返事 (draft)
      asked before it       ⚠ いいえ
  files                     なし
  models                    agent plan ×1 （繰り返しの中でさらに増える）
  + not asked               → confirm`
	if got := Review(Japanese, in); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if got := Review(English, in); got != in {
		t.Fatal("English changed")
	}

	// What is written and sent stays as it is: a quoted string, a block.
	verbatim := "      text                  \"this cannot be undone\"\n        {\"content\": \"outside service\"}"
	if got := Review(Japanese, verbatim); got != verbatim {
		t.Fatalf("changed what is sent:\n%s", got)
	}
}
