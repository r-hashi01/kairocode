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
	// KindContent is a file's content as a planned write leaves it.
	KindContent = "content"
	// KindPermissions is the permission rules as a turn started with them.
	KindPermissions = "permissions"
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
		// Per provider (ADR 0014, 追記): the model and effort last chosen,
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
		// may remove a finished run before its turn ends (kairo-gap #17).
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

// ProviderPref is what is kept of a provider: the model and effort last
// chosen, and the models and efforts its agent said it has.
type ProviderPref struct {
	Model, Effort   string
	Models, Efforts []string
}

// ProviderPref is provider's (zero if nothing is kept).
func (s *Store) ProviderPref(ctx context.Context, provider string) (ProviderPref, error) {
	var p ProviderPref
	var models, efforts string
	err := s.db.QueryRowContext(ctx, `SELECT model, effort, models, efforts FROM kc_provider_pref WHERE provider = ?`, provider).
		Scan(&p.Model, &p.Effort, &models, &efforts)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderPref{}, nil
	}
	if err != nil {
		return ProviderPref{}, err
	}
	_ = json.Unmarshal([]byte(models), &p.Models)
	_ = json.Unmarshal([]byte(efforts), &p.Efforts)
	return p, nil
}

// LastProvider is the provider last chosen, in any session ("" if none).
// It is kept in the row of provider "", in its model.
func (s *Store) LastProvider(ctx context.Context) (string, error) {
	p, err := s.ProviderPref(ctx, "")
	return p.Model, err
}

// SetLastProvider keeps provider as the one last chosen.
func (s *Store) SetLastProvider(ctx context.Context, provider string) error {
	return s.SetProviderChoice(ctx, "", provider, "")
}

// SetProviderChoice keeps the model and the effort chosen for provider
// ("" leaves one as it is).
func (s *Store) SetProviderChoice(ctx context.Context, provider, model, effort string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO kc_provider_pref (provider, model, effort) VALUES (?, ?, ?)
		ON CONFLICT (provider) DO UPDATE SET
			model = CASE WHEN excluded.model = '' THEN kc_provider_pref.model ELSE excluded.model END,
			effort = CASE WHEN excluded.effort = '' THEN kc_provider_pref.effort ELSE excluded.effort END`,
		provider, model, effort)
	return err
}

// SetProviderOptions keeps the models and efforts provider's agent said
// it has (nil leaves one as it is).
func (s *Store) SetProviderOptions(ctx context.Context, provider string, models, efforts []string) error {
	m, _ := json.Marshal(models)
	e, _ := json.Marshal(efforts)
	_, err := s.db.ExecContext(ctx, `INSERT INTO kc_provider_pref (provider, models, efforts) VALUES (?, ?, ?)
		ON CONFLICT (provider) DO UPDATE SET
			models = CASE WHEN excluded.models = 'null' THEN kc_provider_pref.models ELSE excluded.models END,
			efforts = CASE WHEN excluded.efforts = 'null' THEN kc_provider_pref.efforts ELSE excluded.efforts END`,
		provider, string(m), string(e))
	return err
}

// EngineSession is the id a CLI gave the session KairoCode calls name,
// or "" if it has none yet.
func (s *Store) EngineSession(ctx context.Context, name string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM kc_engine_session WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// SetEngineSession records that the CLI's session name is id.
func (s *Store) SetEngineSession(ctx context.Context, name, id string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO kc_engine_session (name, id) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET id = excluded.id`,
		name, id)
	return err
}

// MCPCall is one recorded MCP call.
type MCPCall struct {
	Run          string
	N            int
	Proc         string
	Acknowledged bool
}

// MCPCalls are the recorded calls of content key in turn, by occurrence.
func (s *Store) MCPCalls(ctx context.Context, turn, key string) ([]MCPCall, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT run, n, proc, acknowledged FROM kc_mcp_call WHERE turn = ? AND key = ? ORDER BY n`, turn, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MCPCall
	for rows.Next() {
		var c MCPCall
		var ack int
		if err := rows.Scan(&c.Run, &c.N, &c.Proc, &ack); err != nil {
			return nil, err
		}
		c.Acknowledged = ack != 0
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) AddMCPCall(ctx context.Context, run, turn, key string, n int, proc string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO kc_mcp_call (run, turn, key, n, proc) VALUES (?, ?, ?, ?, ?) ON CONFLICT (run) DO NOTHING`,
		run, turn, key, n, proc)
	return err
}

// AcknowledgeMCPCall records that run's result reached Claude Code.
func (s *Store) AcknowledgeMCPCall(ctx context.Context, run string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE kc_mcp_call SET acknowledged = 1 WHERE run = ?`, run)
	return err
}

// Grant records the user's answer to request ask, about calls of content
// key in turn.
func (s *Store) Grant(ctx context.Context, turn, ask, key string, ok bool, reason string) error {
	v := 0
	if ok {
		v = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO kc_mcp_grant (turn, ask, key, ok, reason) VALUES (?, ?, ?, ?, ?) ON CONFLICT (turn, ask) DO NOTHING`,
		turn, ask, key, v, reason)
	return err
}

// UseGrant gives run an answer about calls of content key in turn: the one
// it was given before, or else the oldest one no run has used. An answer
// lets one run, never two: a call made again later, after an interruption,
// is asked about again (ADR 0005, 0007).
func (s *Store) UseGrant(ctx context.Context, turn, key, run string) (found, ok bool, reason string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, false, "", err
	}
	defer tx.Rollback()
	var ask string
	var v int
	err = tx.QueryRowContext(ctx, `SELECT ask, ok, reason FROM kc_mcp_grant WHERE turn = ? AND key = ? AND used_by = ?`,
		turn, key, run).Scan(&ask, &v, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `SELECT ask, ok, reason FROM kc_mcp_grant WHERE turn = ? AND key = ? AND used_by IS NULL ORDER BY rowid LIMIT 1`,
			turn, key).Scan(&ask, &v, &reason)
		if errors.Is(err, sql.ErrNoRows) {
			return false, false, "", nil
		}
		if err != nil {
			return false, false, "", err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE kc_mcp_grant SET used_by = ? WHERE turn = ? AND ask = ?`, run, turn, ask); err != nil {
			return false, false, "", err
		}
	} else if err != nil {
		return false, false, "", err
	}
	return true, v != 0, reason, tx.Commit()
}

// MCPRuns are the runs of every MCP call recorded for turn.
func (s *Store) MCPRuns(ctx context.Context, turn string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT run FROM kc_mcp_call WHERE turn = ? ORDER BY rowid`, turn)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
