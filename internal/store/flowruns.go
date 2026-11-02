package store

import (
	"context"
	"fmt"
	"time"
)

// FlowRunRow is a flow's run as it is kept (ADR 0029): its body is the
// runtime's (JSON).
type FlowRunRow struct {
	ID, Dir, Name, Status string
	Try                   bool // a try of the flow (ADR 0017), counted apart
	Started, Ended        time.Time
	Body                  []byte
}

// PutFlowRun keeps a flow's run; kept again, it is replaced.
func (s *Store) PutFlowRun(ctx context.Context, r FlowRunRow) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO kc_flow_run (id, dir, name, started, ended, status, body, try)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO UPDATE SET
		dir = excluded.dir, name = excluded.name, started = excluded.started, ended = excluded.ended,
		status = excluded.status, body = excluded.body, try = excluded.try`,
		r.ID, r.Dir, r.Name, r.Started.UnixNano(), r.Ended.UnixNano(), r.Status, r.Body, r.Try)
	if err != nil {
		return fmt.Errorf("store: put flow run: %w", err)
	}
	return nil
}

// HasFlowRun says a flow's run is kept.
func (s *Store) HasFlowRun(ctx context.Context, id string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM kc_flow_run WHERE id = ?`, id).Scan(&n)
	return n > 0, err
}

// FlowRun is a kept flow's run.
func (s *Store) FlowRun(ctx context.Context, id string) (FlowRunRow, error) {
	rows, err := s.flowRuns(ctx, `WHERE id = ?`, id)
	if err != nil {
		return FlowRunRow{}, err
	}
	if len(rows) == 0 {
		return FlowRunRow{}, ErrNotFound
	}
	return rows[0], nil
}

// FlowRuns are the kept runs of dir's flows ("" any dir), of flow name
// ("" any), the latest first, at most limit (0: all), with their bodies;
// only "tries" or "runs" of them, or both ("").
func (s *Store) FlowRuns(ctx context.Context, dir, name, only string, limit int) ([]FlowRunRow, error) {
	where, args := `WHERE 1 = 1`, []any{}
	switch only {
	case "tries":
		where += ` AND try = 1`
	case "runs":
		where += ` AND try = 0`
	}
	if dir != "" {
		where, args = where+` AND dir = ?`, append(args, dir)
	}
	if name != "" {
		where, args = where+` AND name = ?`, append(args, name)
	}
	where += ` ORDER BY started DESC, id`
	if limit > 0 {
		where, args = where+` LIMIT ?`, append(args, limit)
	}
	return s.flowRuns(ctx, where, args...)
}

// FlowRunsSince counts dir's kept runs (or tries) of flow name started
// after at.
func (s *Store) FlowRunsSince(ctx context.Context, dir, name string, try bool, at time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM kc_flow_run WHERE dir = ? AND name = ? AND try = ? AND started > ?`,
		dir, name, try, at.UnixNano()).Scan(&n)
	return n, err
}

func (s *Store) flowRuns(ctx context.Context, where string, args ...any) ([]FlowRunRow, error) {
	q, err := s.db.QueryContext(ctx, `SELECT id, dir, name, started, ended, status, body, try FROM kc_flow_run `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("store: flow runs: %w", err)
	}
	defer q.Close()
	var out []FlowRunRow
	for q.Next() {
		var r FlowRunRow
		var started, ended int64
		if err := q.Scan(&r.ID, &r.Dir, &r.Name, &started, &ended, &r.Status, &r.Body, &r.Try); err != nil {
			return nil, err
		}
		r.Started, r.Ended = time.Unix(0, started), time.Unix(0, ended)
		out = append(out, r)
	}
	return out, q.Err()
}

// PruneFlowRuns removes dir's runs (or tries: counted apart) of flow name
// past the newest keep, or started before before.
func (s *Store) PruneFlowRuns(ctx context.Context, dir, name string, try bool, keep int, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM kc_flow_run WHERE dir = ? AND name = ? AND try = ? AND (started < ? OR id NOT IN
		(SELECT id FROM kc_flow_run WHERE dir = ? AND name = ? AND try = ? ORDER BY started DESC, id LIMIT ?))`,
		dir, name, try, before.UnixNano(), dir, name, try, keep)
	if err != nil {
		return fmt.Errorf("store: prune flow runs: %w", err)
	}
	return nil
}
