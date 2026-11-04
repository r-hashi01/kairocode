package store

import (
	"context"
	"database/sql"
	"errors"
)

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

// Switch is session's switch not yet sent (nil body: none) and the count
// of its changes, to give ClearSwitch.
func (s *Store) Switch(ctx context.Context, session string) (body []byte, seq int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT body, seq FROM kc_switch WHERE session = ?`, session).Scan(&body, &seq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	}
	return body, seq, err
}

// SetSwitch keeps body as session's switch not yet sent.
func (s *Store) SetSwitch(ctx context.Context, session string, body []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO kc_switch (session, body, seq) VALUES (?, ?, 1)
		ON CONFLICT (session) DO UPDATE SET body = excluded.body, seq = kc_switch.seq + 1`, session, body)
	return err
}

// ClearSwitch removes session's switch once sent, unless it changed after
// Switch gave seq: the changed one waits for the next event.
func (s *Store) ClearSwitch(ctx context.Context, session string, seq int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM kc_switch WHERE session = ? AND seq = ?`, session, seq)
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

// KeptRun is a run a flow that went another way left running (ADR 0003,
// addendum 2026-11-04), and the session's turn it is under.
type KeptRun struct {
	Run, Top string
}

// Keep notes run, under turn top of session, as left running.
func (s *Store) Keep(ctx context.Context, session, top, run string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO kc_kept (run, session, top) VALUES (?, ?, ?) ON CONFLICT (run) DO NOTHING`,
		run, session, top)
	return err
}

// Kept are the runs noted as left running in session.
func (s *Store) Kept(ctx context.Context, session string) ([]KeptRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT run, top FROM kc_kept WHERE session = ? ORDER BY run`, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeptRun
	for rows.Next() {
		var k KeptRun
		if err := rows.Scan(&k.Run, &k.Top); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Unkeep forgets run, once it has finished.
func (s *Store) Unkeep(ctx context.Context, run string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM kc_kept WHERE run = ?`, run)
	return err
}
