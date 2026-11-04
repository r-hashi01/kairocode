// Package store keeps conversation content by the hash of its content, once
// (design 2.2, NFR-5). Workflows and their recorded calls carry only hashes.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/r-hashi01/kairocode/internal/msg"
)

// Kinds of content.
const (
	KindMessage = "message"
	KindHistory = "history" // a link of the conversation: {prev, messages}
	KindSystem  = "system"
	KindTools   = "tools"
	// KindContent is a file's content as a planned write leaves it.
	KindContent = "content"
	// KindPermissions is the permission rules as a turn started with them.
	KindPermissions = "permissions"
	// KindReviewed is a flow_publish call and the draft it was decided
	// on (ADR 0030): only that draft is published by it.
	KindReviewed = "reviewed"
	// KindImage is an image's bytes, a message's image part refers to by
	// hash (ADR 0026).
	KindImage = "image"
)

var ErrNotFound = errors.New("store: no such content")

type Store struct{ db *sql.DB }

// New makes the table if it is missing. The *sql.DB is shared with kairo's
// store, so the same file holds both.
func New(ctx context.Context, db *sql.DB) (*Store, error) {
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS kc_message (
			hash TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			body BLOB NOT NULL)`,
		// The claude process a Claude Code session runs in (ADR 0007), so one
		// left behind by a process that died can be stopped before resuming.
		`CREATE TABLE IF NOT EXISTS kc_claude_proc (
			session TEXT PRIMARY KEY,
			pid INTEGER NOT NULL)`,
		// Per provider (ADR 0014, addendum): the model and effort last chosen,
		// and the models and efforts its agent said it has (JSON lists).
		`CREATE TABLE IF NOT EXISTS kc_provider_pref (
			provider TEXT PRIMARY KEY,
			model TEXT NOT NULL DEFAULT '',
			effort TEXT NOT NULL DEFAULT '',
			models TEXT NOT NULL DEFAULT '[]',
			efforts TEXT NOT NULL DEFAULT '[]')`,
		// The sessions of the Codex and Copilot CLIs (ADR 0014): theirs are
		// ids the CLI makes, kept by the name KairoCode gives each.
		`CREATE TABLE IF NOT EXISTS kc_engine_session (
			name TEXT PRIMARY KEY,
			id TEXT NOT NULL)`,
		// The tool calls Claude Code made over MCP (ADR 0007): which run each
		// is, from which claude run (proc), and whether its result is known
		// to have reached Claude Code. Kept here as well as in kairo, which
		// may remove a finished run before its turn ends (kairo#4).
		`CREATE TABLE IF NOT EXISTS kc_mcp_call (
			run TEXT PRIMARY KEY,
			turn TEXT NOT NULL,
			key TEXT NOT NULL,
			n INTEGER NOT NULL,
			proc TEXT NOT NULL,
			acknowledged INTEGER NOT NULL DEFAULT 0)`,
		// The user's answers to MCP calls that asked (ADR 0007, 4): one per
		// request (ask), for calls of content key, each used by one run.
		`CREATE TABLE IF NOT EXISTS kc_mcp_grant (
			turn TEXT NOT NULL,
			ask TEXT NOT NULL,
			key TEXT NOT NULL,
			ok INTEGER NOT NULL,
			reason TEXT NOT NULL,
			used_by TEXT,
			PRIMARY KEY (turn, ask))`,
		// Flows' runs, kept once they end (ADR 0029): kairo removes a
		// finished tree's record after a while; this one stays.
		`CREATE TABLE IF NOT EXISTS kc_flow_run (
			id TEXT PRIMARY KEY,
			dir TEXT NOT NULL,
			name TEXT NOT NULL,
			started INTEGER NOT NULL,
			ended INTEGER NOT NULL,
			status TEXT NOT NULL,
			body BLOB NOT NULL,
			try INTEGER NOT NULL DEFAULT 0)`,
		`CREATE INDEX IF NOT EXISTS kc_flow_run_dir ON kc_flow_run (dir, started)`,
		// A session's switch of provider, model or mode not yet sent with
		// its next event (ADR 0014, addendum 2026-11-02): body is the switch's
		// JSON, seq counts its changes so only the one sent is removed.
		`CREATE TABLE IF NOT EXISTS kc_switch (
			session TEXT PRIMARY KEY,
			body BLOB NOT NULL,
			seq INTEGER NOT NULL)`,
	} {
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
	}
	// try (a try of a flow, ADR 0029 addendum): a table made before it lacks it.
	if _, err := db.ExecContext(ctx, `ALTER TABLE kc_flow_run ADD COLUMN try INTEGER NOT NULL DEFAULT 0`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		return nil, fmt.Errorf("store: %w", err)
	}
	return &Store{db: db}, nil
}

// Hash is the key content of kind is stored under.
func Hash(kind string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write(body)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// Put stores body once and returns its hash. Writing the same content again
// changes nothing, so Put is safe to repeat.
func (s *Store) Put(ctx context.Context, kind string, body []byte) (string, error) {
	h := Hash(kind, body)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO kc_message (hash, kind, body) VALUES (?, ?, ?) ON CONFLICT (hash) DO NOTHING`,
		h, kind, body); err != nil {
		return "", fmt.Errorf("store: put %s: %w", kind, err)
	}
	return h, nil
}

func (s *Store) Get(ctx context.Context, hash string) (kind string, body []byte, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT kind, body FROM kc_message WHERE hash = ?`, hash).Scan(&kind, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, fmt.Errorf("%w: %s", ErrNotFound, hash)
	}
	return kind, body, err
}

func (s *Store) PutJSON(ctx context.Context, kind string, v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return s.Put(ctx, kind, b)
}

func (s *Store) GetJSON(ctx context.Context, hash, kind string, v any) error {
	k, b, err := s.Get(ctx, hash)
	if err != nil {
		return err
	}
	if k != kind {
		return fmt.Errorf("store: %s is %s, not %s", hash, k, kind)
	}
	return json.Unmarshal(b, v)
}

func (s *Store) PutMessage(ctx context.Context, m msg.Message) (string, error) {
	return s.PutJSON(ctx, KindMessage, m)
}

func (s *Store) Message(ctx context.Context, hash string) (msg.Message, error) {
	var m msg.Message
	err := s.GetJSON(ctx, hash, KindMessage, &m)
	return m, err
}

// Link is one turn's worth of a conversation: the messages it added, after the
// conversation up to Prev. A conversation is named by its last link, so a
// workflow passes one hash however long the conversation is.
//
// A Base link starts a conversation afresh (ADR 0020): its messages (a
// summary) stand for all before it, which Conversation does not read. Its
// Prev still names them, so they are kept and can be looked at.
type Link struct {
	Prev     string   `json:"prev,omitempty"`
	Messages []string `json:"messages"`
	Base     bool     `json:"base,omitempty"`
}

// Compacted adds a Base link holding summary after the conversation that
// ends at prev, and returns its hash.
func (s *Store) Compacted(ctx context.Context, prev, summary string) (string, error) {
	return s.PutJSON(ctx, KindHistory, Link{Prev: prev, Messages: []string{summary}, Base: true})
}

// Extend appends messages to the conversation that ends at prev.
func (s *Store) Extend(ctx context.Context, prev string, messages []string) (string, error) {
	return s.PutJSON(ctx, KindHistory, Link{Prev: prev, Messages: messages})
}

// Conversation is the hashes of the messages of the conversation that ends at
// head, oldest first.
func (s *Store) Conversation(ctx context.Context, head string) ([]string, error) {
	var links [][]string
	for h := head; h != ""; {
		var l Link
		if err := s.GetJSON(ctx, h, KindHistory, &l); err != nil {
			return nil, err
		}
		links = append(links, l.Messages)
		if l.Base {
			break // what is before it is in its summary
		}
		h = l.Prev
	}
	var out []string
	for i := len(links) - 1; i >= 0; i-- {
		out = append(out, links[i]...)
	}
	return out, nil
}

// Messages loads the messages named by hashes, in order.
func (s *Store) Messages(ctx context.Context, hashes []string) ([]msg.Message, error) {
	out := make([]msg.Message, 0, len(hashes))
	for _, h := range hashes {
		m, err := s.Message(ctx, h)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}
