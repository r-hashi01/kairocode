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

	"github.com/r-hashi01/kairocode/internal/msg"
)

// Kinds of content.
const (
	KindMessage = "message"
	KindHistory = "history" // a link of the conversation: {prev, messages}
	KindSystem  = "system"
	KindTools   = "tools"
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
		// MCP calls whose result was handed back to Claude Code (ADR 0007).
		`CREATE TABLE IF NOT EXISTS kc_mcp_delivered (run TEXT PRIMARY KEY)`,
	} {
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
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
type Link struct {
	Prev     string   `json:"prev,omitempty"`
	Messages []string `json:"messages"`
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

// ClaudeProc is the pid of the claude process last started for session, or 0.
func (s *Store) ClaudeProc(ctx context.Context, session string) (int, error) {
	var pid int
	err := s.db.QueryRowContext(ctx, `SELECT pid FROM kc_claude_proc WHERE session = ?`, session).Scan(&pid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return pid, err
}

// SetClaudeProc records pid as session's claude process (0: none).
func (s *Store) SetClaudeProc(ctx context.Context, session string, pid int) error {
	if pid == 0 {
		_, err := s.db.ExecContext(ctx, `DELETE FROM kc_claude_proc WHERE session = ?`, session)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO kc_claude_proc (session, pid) VALUES (?, ?) ON CONFLICT (session) DO UPDATE SET pid = excluded.pid`,
		session, pid)
	return err
}

func (s *Store) MarkDelivered(ctx context.Context, run string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO kc_mcp_delivered (run) VALUES (?) ON CONFLICT (run) DO NOTHING`, run)
	return err
}

func (s *Store) Delivered(ctx context.Context, run string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM kc_mcp_delivered WHERE run = ?`, run).Scan(&n)
	return n > 0, err
}
