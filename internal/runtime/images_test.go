package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// testPNG is a small image, of colour c.
func testPNG(t *testing.T, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := range 4 {
		for y := range 4 {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// Images not taken are refused before anything is stored, saying why and
// not what they hold (ADR 0026).
func TestCheckImages(t *testing.T) {
	red := testPNG(t, color.RGBA{255, 0, 0, 255})
	many := make([]Image, msg.MaxImages+1)
	for i := range many {
		many[i] = Image{MediaType: "image/png", Data: red}
	}
	for _, c := range []struct {
		name   string
		images []Image
		ok     bool
	}{
		{"a png", []Image{{MediaType: "image/png", Data: red}}, true},
		{"none", nil, true},
		{"too many", many, false},
		{"a kind not read", []Image{{MediaType: "image/svg+xml", Data: []byte("<svg/>")}}, false},
		{"not what it says", []Image{{MediaType: "image/jpeg", Data: red}}, false},
		{"empty", []Image{{MediaType: "image/png"}}, false},
		{"too large", []Image{{MediaType: "image/png", Data: append(bytes.Clone(red), make([]byte, msg.MaxImageBytes)...)}}, false},
		{"too large together", slices.Repeat([]Image{{MediaType: "image/png", Data: append(bytes.Clone(red), make([]byte, msg.MaxImageBytes-len(red))...)}}, 5), false},
	} {
		err := CheckImages(c.images)
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
		if err != nil && (!errors.Is(err, ErrBadImage) || strings.Contains(err.Error(), base64.StdEncoding.EncodeToString(red)[:16])) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// An image goes to the model with the message, and with the conversation
// in later turns; it is stored once, apart, and the message holds its
// hash. The record shows its kind and size, not its bytes.
func TestImagesReachTheModel(t *testing.T) {
	dir := t.TempDir()
	images := filepath.Join(dir, "images")
	r := open(t, filepath.Join(dir, "k.db"), &provider.Fake{HoldAt: -1, ImageLog: images,
		Script: []provider.FakeReply{{Text: "red"}, {Text: "still red"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := r.CreateSession(ctx, "img", workflows.SessionInput{Dir: repo(t)})
	if err != nil {
		t.Fatal(err)
	}
	red := testPNG(t, color.RGBA{255, 0, 0, 255})
	n, err := r.SendImages(ctx, s, "what colour?", []Image{{MediaType: "image/png", Data: red}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	turn(t, r, s, "and now?")
	want := "image/png:" + strconv.Itoa(len(red))
	if b, _ := os.ReadFile(images); string(b) != want+"\n"+want+"\n" {
		t.Fatalf("the model was given %q", b)
	}

	ms, err := r.History(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	p := ms[0].Parts[1]
	if p.Type != msg.Image || p.MediaType != "image/png" || p.Ref != store.Hash(store.KindImage, red) || p.Data != nil {
		t.Fatalf("the image part: %+v", p)
	}
	// Stored as it reads back: without the bytes.
	b, err := json.Marshal(ms[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Store.Get(ctx, store.Hash(store.KindMessage, b)); err != nil || bytes.Contains(b, []byte(base64.StdEncoding.EncodeToString(red)[:16])) {
		t.Errorf("the stored message: %v\n%s", err, b)
	}
	l, err := r.Log(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	text := l.Text()
	if !strings.Contains(text, "[image: image/png, 1 KB]") || strings.Contains(text, base64.StdEncoding.EncodeToString(red)[:16]) {
		t.Errorf("the record:\n%s", text)
	}
}

// An engine that takes no images is given them as files, named in the
// prompt: written once, by their content's hash.
func TestImagesAsFiles(t *testing.T) {
	dir := t.TempDir()
	r := open(t, filepath.Join(dir, "k.db"), &provider.Fake{HoldAt: -1})
	ctx := context.Background()
	red := testPNG(t, color.RGBA{255, 0, 0, 255})
	ref, err := r.Store.Put(ctx, store.KindImage, red)
	if err != nil {
		t.Fatal(err)
	}
	h, err := r.Store.PutMessage(ctx, msg.Message{Role: msg.User, Parts: []msg.Part{
		{Type: msg.Text, Text: "what colour?"}, {Type: msg.Image, MediaType: "image/png", Ref: ref}}})
	if err != nil {
		t.Fatal(err)
	}
	in := workflows.ClaudeTurnInput{Message: h, Turn: "s/img/t1"}
	prompt, err := r.cliPrompt(ctx, in, false, false)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "images", strings.TrimPrefix(ref, "sha256:")+".png")
	if !strings.Contains(prompt, "Attached images (files):\n"+path) {
		t.Fatalf("prompt %q", prompt)
	}
	if b, err := os.ReadFile(path); err != nil || !bytes.Equal(b, red) {
		t.Fatalf("the file: %v", err)
	}
	// Given as parts, they are not named.
	if prompt, _ := r.cliPrompt(ctx, in, false, true); strings.Contains(prompt, "Attached images") {
		t.Errorf("prompt %q", prompt)
	}
}

// Claude Code is given the images as content, by stream-json on stdin; a
// message without images is given as before (-p).
func TestClaudeEngineImages(t *testing.T) {
	e := setupFakeClaude(t, fakeClaudeRun{Reply: "red"}, fakeClaudeRun{Reply: "ok"})
	r := openClaude(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := r.CreateSession(ctx, "cci", workflows.SessionInput{Dir: repo(t)})
	if err != nil {
		t.Fatal(err)
	}
	red := testPNG(t, color.RGBA{255, 0, 0, 255})
	n, err := r.SendImages(ctx, s, "what colour?", []Image{{MediaType: "image/png", Data: red}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitTurn(ctx, s, n); err != nil {
		t.Fatal(err)
	}
	turn(t, r, s, "thanks")
	st := e.starts(t)
	if len(st) != 2 {
		t.Fatalf("claude was started as %+v", st)
	}
	if !strings.HasPrefix(st[0].Prompt, "what colour?\n\n[kairocode turn ") || len(st[0].Images) != 1 || st[0].Images[0] != "image/png:"+strconv.Itoa(len(red)) {
		t.Errorf("the first start: %+v", st[0])
	}
	if !strings.HasPrefix(st[1].Prompt, "thanks\n\n[kairocode turn ") || len(st[1].Images) != 0 {
		t.Errorf("the second start: %+v", st[1])
	}
}

// A model call is given the latest images, as many as it takes; older ones
// are said in words, not sent again, so a long session does not grow past
// what the model takes.
func TestImagesKeptToWhatACallTakes(t *testing.T) {
	r := open(t, filepath.Join(t.TempDir(), "k.db"), &provider.Fake{HoldAt: -1})
	ctx := context.Background()
	var ms []msg.Message
	for i := range msg.MaxRequestImages + 1 {
		data := testPNG(t, color.RGBA{uint8(i), 0, 0, 255})
		ref, err := r.Store.Put(ctx, store.KindImage, data)
		if err != nil {
			t.Fatal(err)
		}
		ms = append(ms, msg.Message{Role: msg.User, Parts: []msg.Part{{Type: msg.Image, MediaType: "image/png", Ref: ref}}})
	}
	out, err := r.withImages(ctx, ms)
	if err != nil {
		t.Fatal(err)
	}
	if p := out[0].Parts[0]; p.Type != msg.Text || !strings.Contains(p.Text, "not sent again") {
		t.Errorf("the oldest: %+v", p)
	}
	for i, m := range out[1:] {
		if p := m.Parts[0]; p.Type != msg.Image || len(p.Data) == 0 {
			t.Fatalf("image %d: %+v", i+1, p)
		}
	}
}
