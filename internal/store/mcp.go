package store

import (
	"context"
	"database/sql"
	"errors"
)

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

// MCPTurns are the turns with an MCP call recorded: one of them may hold a
// call cut off, to be reviewed, after it finished.
func (s *Store) MCPTurns(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT turn FROM kc_mcp_call`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out[t] = true
	}
	return out, rows.Err()
}
