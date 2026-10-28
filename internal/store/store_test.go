package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/r-hashi01/kairocode/internal/msg"
	_ "modernc.org/sqlite"
)

func open(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := New(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	return s, db
}

func rows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM kc_message`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Content is written once, by its hash, and never changed (design 2.2,
// NFR-5, invariant 5): the same content again adds no row, and the row
// keeps what was first written.
func TestPutOnce(t *testing.T) {
	ctx := context.Background()
	s, db := open(t)
	h1, err := s.Put(ctx, KindSystem, []byte("you are"))
	if err != nil {
		t.Fatal(err)
	}
	h2, err := s.Put(ctx, KindSystem, []byte("you are"))
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 || rows(t, db) != 1 {
		t.Fatalf("the same content twice: %s, %s, %d rows", h1, h2, rows(t, db))
	}
	if h1 != Hash(KindSystem, []byte("you are")) || !strings.HasPrefix(h1, "sha256:") {
		t.Fatalf("hash %s", h1)
	}
	// The kind is part of the key: the same bytes of another kind are
	// another row, and neither is changed by the other.
	h3, err := s.Put(ctx, KindTools, []byte("you are"))
	if err != nil {
		t.Fatal(err)
	}
	if h3 == h1 || rows(t, db) != 2 {
		t.Fatalf("another kind: %s, %d rows", h3, rows(t, db))
	}
	if k, b, err := s.Get(ctx, h1); err != nil || k != KindSystem || string(b) != "you are" {
		t.Fatalf("get: %q %q %v", k, b, err)
	}
	// A row put in the way under a hash is not overwritten by Put.
	if _, err := db.Exec(`UPDATE kc_message SET body = 'other' WHERE hash = ?`, h1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, KindSystem, []byte("you are")); err != nil {
		t.Fatal(err)
	}
	if _, b, _ := s.Get(ctx, h1); string(b) != "other" {
		t.Fatalf("Put wrote over a row: %q", b)
	}
}

func TestGetMissingAndWrongKind(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	if _, _, err := s.Get(ctx, "sha256:none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	h, err := s.PutJSON(ctx, KindTools, []string{"read"})
	if err != nil {
		t.Fatal(err)
	}
	var m msg.Message
	if err := s.GetJSON(ctx, h, KindMessage, &m); err == nil {
		t.Fatal("content of another kind read as a message")
	}
}

// A message comes back as it was put; an image's bytes are not stored with
// it, only its reference (ADR 0026).
func TestMessageRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	in := msg.Message{Role: msg.User, Parts: []msg.Part{
		{Type: msg.Text, Text: "look"},
		{Type: msg.Image, MediaType: "image/png", Ref: "sha256:img", Data: []byte("bytes")},
	}}
	h, err := s.PutMessage(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	_, body, err := s.Get(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "Ynl0ZXM") || strings.Contains(string(body), "bytes") {
		t.Fatalf("the image's bytes are stored: %s", body)
	}
	out, err := s.Message(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	if out.Role != msg.User || len(out.Parts) != 2 || out.Parts[0].Text != "look" ||
		out.Parts[1].Ref != "sha256:img" || out.Parts[1].Data != nil {
		t.Fatalf("got %+v", out)
	}
	// The same message with its bytes or without is one row.
	in.Parts[1].Data = nil
	if h2, _ := s.PutMessage(ctx, in); h2 != h {
		t.Fatalf("the image's bytes change the hash: %s, %s", h, h2)
	}
}

// A conversation is read oldest first through its links; a Base link (a
// summary, ADR 0020) stands for all before it, which stays stored.
func TestConversation(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	put := func(text string) string {
		h, err := s.PutMessage(ctx, msg.Message{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: text}}})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	a, b, c, sum, d := put("a"), put("b"), put("c"), put("summary"), put("d")
	l1, _ := s.Extend(ctx, "", []string{a, b})
	l2, _ := s.Extend(ctx, l1, []string{c})
	if got, err := s.Conversation(ctx, l2); err != nil || !slices.Equal(got, []string{a, b, c}) {
		t.Fatalf("conversation: %v %v", got, err)
	}
	l3, _ := s.Compacted(ctx, l2, sum)
	l4, _ := s.Extend(ctx, l3, []string{d})
	if got, err := s.Conversation(ctx, l4); err != nil || !slices.Equal(got, []string{sum, d}) {
		t.Fatalf("after the summary: %v %v", got, err)
	}
	// What the summary stands for is still there, from its link.
	if got, _ := s.Conversation(ctx, l2); !slices.Equal(got, []string{a, b, c}) {
		t.Fatalf("before the summary: %v", got)
	}
	ms, err := s.Messages(ctx, []string{sum, d})
	if err != nil || len(ms) != 2 || ms[0].Text() != "summary" || ms[1].Text() != "d" {
		t.Fatalf("messages: %+v %v", ms, err)
	}
	if _, err := s.Conversation(ctx, "sha256:none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing link: %v", err)
	}
}

// An answer to an MCP call lets one run, never two: the run it went to gets
// it again; another run of the same call does not (ADR 0007).
func TestGrantLetsOneRun(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	if found, _, _, err := s.UseGrant(ctx, "t", "k", "r1"); err != nil || found {
		t.Fatalf("no answer yet: %v %v", found, err)
	}
	if err := s.Grant(ctx, "t", "ask1", "k", true, "yes"); err != nil {
		t.Fatal(err)
	}
	// The same request answered again keeps the first answer.
	if err := s.Grant(ctx, "t", "ask1", "k", false, "no"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		found, ok, reason, err := s.UseGrant(ctx, "t", "k", "r1")
		if err != nil || !found || !ok || reason != "yes" {
			t.Fatalf("r1: %v %v %q %v", found, ok, reason, err)
		}
	}
	if found, _, _, err := s.UseGrant(ctx, "t", "k", "r2"); err != nil || found {
		t.Fatalf("a second run used the answer: %v %v", found, err)
	}
	// Another call's answer is not this one's.
	if err := s.Grant(ctx, "t", "ask2", "other", true, ""); err != nil {
		t.Fatal(err)
	}
	if found, _, _, _ := s.UseGrant(ctx, "t", "k", "r2"); found {
		t.Fatal("an answer about another call was used")
	}
}
