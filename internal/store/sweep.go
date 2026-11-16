package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ContentKinds are the kinds of kc_message the sweep may remove (ADR
// 0045): what conversations, turns and tools wrote. The other kinds are
// markers (a decision kept, a notice told, a review made): small, and
// what they mark must stay known, so they are never swept.
var ContentKinds = []string{KindMessage, KindHistory, KindSystem, KindTools, KindContent, KindPermissions, KindImage}

// Unswept is the hashes of the messages of the content kinds last written
// before before: what the sweep removes unless something reaches them.
func (s *Store) Unswept(ctx context.Context, before time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT hash FROM kc_message WHERE at < ? AND kind IN (?, ?, ?, ?, ?, ?, ?)`,
		before.UnixMilli(), ContentKinds[0], ContentKinds[1], ContentKinds[2], ContentKinds[3], ContentKinds[4], ContentKinds[5], ContentKinds[6])
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// SweepMessage removes message hash if it was still last written before
// before (written again meanwhile, it stays): whether it went, and how
// many bytes.
func (s *Store) SweepMessage(ctx context.Context, hash string, before time.Time) (int64, bool, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `DELETE FROM kc_message WHERE hash = ? AND at < ? RETURNING length(body)`,
		hash, before.UnixMilli()).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return n, err == nil, err
}

// SweepUsage removes the usage ledger's rows of calls before before, and
// says how many went.
func (s *Store) SweepUsage(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM kc_usage WHERE at < ?`, before.UnixNano()) // at is in nanoseconds (PutUsage)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ItemsEndedBefore is the items that ended before before.
func (s *Store) ItemsEndedBefore(ctx context.Context, before time.Time) ([]ItemRow, error) {
	return s.items(ctx, `ended_at > 0 AND ended_at < ?`, before.UnixMilli())
}

// DeleteItem removes item id's row.
func (s *Store) DeleteItem(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM kc_item WHERE id = ?`, id)
	return err
}

// SessionRow is a row of a table about a session or a run of it: which
// table, its key, and the run it is of.
type SessionRow struct {
	Table, Key, Run string
}

// sessionTables are the tables of rows about a session or its runs, with
// the column that keys a row and the one naming its run.
var sessionTables = []struct{ table, key, run string }{
	{"kc_mcp_call", "run", "turn"},
	{"kc_mcp_grant", "turn || char(0) || ask", "turn"},
	{"kc_kept", "run", "session"},
	{"kc_switch", "session", "session"},
	{"kc_claude_proc", "session", "session"},
}

// SessionRows is the rows of the tables about sessions.
func (s *Store) SessionRows(ctx context.Context) ([]SessionRow, error) {
	var out []SessionRow
	for _, t := range sessionTables {
		rows, err := s.db.QueryContext(ctx, `SELECT `+t.key+`, `+t.run+` FROM `+t.table)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			r := SessionRow{Table: t.table}
			if err := rows.Scan(&r.Key, &r.Run); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DeleteSessionRow removes one of them.
func (s *Store) DeleteSessionRow(ctx context.Context, r SessionRow) error {
	for _, t := range sessionTables {
		if t.table == r.Table {
			_, err := s.db.ExecContext(ctx, `DELETE FROM `+t.table+` WHERE `+t.key+` = ?`, r.Key)
			return err
		}
	}
	return errors.New("store: no table " + r.Table)
}

// RootTexts is what the kc_ tables kept apart from kairo's records say,
// for the sweep to find the hashes in: flow runs, items (their lines and
// results) and switches not yet sent.
func (s *Store) RootTexts(ctx context.Context, each func([]byte) error) error {
	for _, q := range []string{
		`SELECT body FROM kc_flow_run`,
		`SELECT def FROM kc_item`,
		`SELECT result FROM kc_item WHERE result IS NOT NULL`,
		`SELECT body FROM kc_switch`,
	} {
		rows, err := s.db.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		for rows.Next() {
			var b []byte
			if err := rows.Scan(&b); err != nil {
				rows.Close()
				return err
			}
			if err := each(b); err != nil {
				rows.Close()
				return err
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

// Tables is the names of the kc_ tables.
func (s *Store) Tables(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'kc\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Swept is when sweep name last ran; zero if never.
func (s *Store) Swept(ctx context.Context, name string) (time.Time, error) {
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT at FROM kc_sweep WHERE name = ?`, name).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	return time.UnixMilli(at), err
}

// SetSwept records that sweep name ran at at.
func (s *Store) SetSwept(ctx context.Context, name string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO kc_sweep (name, at) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET at = excluded.at`,
		name, at.UnixMilli())
	return err
}
