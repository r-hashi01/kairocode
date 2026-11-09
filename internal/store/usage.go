package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/r-hashi01/kairocode/internal/msg"
)

// UsageRow is one model call in the ledger of usage (ADR 0039).
type UsageRow struct {
	Run     string
	Attempt int32
	Session string
	Engine  string
	Model   string
	At      time.Time
	Usage   msg.Usage
	// Cost is what the engine reported the call cost (Claude Code), in US
	// dollars; nil if it reported none.
	Cost *float64
}

// PutUsage adds a call to the ledger; added again (the same attempt), it
// changes nothing.
func (s *Store) PutUsage(ctx context.Context, r UsageRow) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO kc_usage (run, attempt, session, engine, model, at,
		input, output, cache_read, cache_write, cost) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (run, attempt) DO NOTHING`,
		r.Run, r.Attempt, r.Session, r.Engine, r.Model, r.At.UnixNano(),
		r.Usage.Input, r.Usage.Output, r.Usage.CacheRead, r.Usage.CacheWrite, r.Cost)
	if err != nil {
		return fmt.Errorf("store: put usage: %w", err)
	}
	return nil
}

// UsageRows are the ledger's calls made at since or later, or of session
// (whenever made), the oldest first.
func (s *Store) UsageRows(ctx context.Context, since time.Time, session string) ([]UsageRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT run, attempt, session, engine, model, at,
		input, output, cache_read, cache_write, cost FROM kc_usage WHERE at >= ? OR session = ?
		ORDER BY at, run, attempt`, since.UnixNano(), session)
	if err != nil {
		return nil, fmt.Errorf("store: usage: %w", err)
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		var at int64
		var cost sql.NullFloat64
		if err := rows.Scan(&r.Run, &r.Attempt, &r.Session, &r.Engine, &r.Model, &at,
			&r.Usage.Input, &r.Usage.Output, &r.Usage.CacheRead, &r.Usage.CacheWrite, &cost); err != nil {
			return nil, fmt.Errorf("store: usage: %w", err)
		}
		r.At = time.Unix(0, at)
		if cost.Valid {
			r.Cost = &cost.Float64
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
