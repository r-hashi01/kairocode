package store

import (
	"context"
	"database/sql"
	"errors"
)

// Event is an event from outside for a session's flows (ADR 0047).
type Event struct {
	ID, Name string
	Value    []byte
	// Run is the flow run it was given to; "" while it is kept for a wait
	// to come.
	Run string
}

// KeepEvent keeps event ev of session, given to ev.Run ("" none yet). An
// event of that id kept before is kept as it was: the first is returned,
// with false.
func (s *Store) KeepEvent(ctx context.Context, session string, ev Event) (Event, bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO kc_event (session, id, name, value, seq, run)
		VALUES (?, ?, ?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM kc_event), ?) ON CONFLICT (session, id) DO NOTHING`,
		session, ev.ID, ev.Name, ev.Value, ev.Run)
	if err != nil {
		return Event{}, false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		return ev, err == nil, err
	}
	var was Event
	err = s.db.QueryRowContext(ctx, `SELECT id, name, value, run FROM kc_event WHERE session = ? AND id = ?`, session, ev.ID).
		Scan(&was.ID, &was.Name, &was.Value, &was.Run)
	return was, false, err
}

// EventsKept are session's events of name kept for a wait to come, in the
// order they came.
func (s *Store) EventsKept(ctx context.Context, session, name string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, value FROM kc_event WHERE session = ? AND name = ? AND run = '' ORDER BY seq`,
		session, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var ev Event
		if err := rows.Scan(&ev.ID, &ev.Name, &ev.Value); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// GiveEvent notes session's event id as given to run ("" kept again for a
// wait to come).
func (s *Store) GiveEvent(ctx context.Context, session, id, run string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE kc_event SET run = ? WHERE session = ? AND id = ?`, run, session, id)
	return err
}

// SetEventWait notes run as the flow run of session that waits for its
// events of name.
func (s *Store) SetEventWait(ctx context.Context, session, name, run string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO kc_event_wait (session, name, run) VALUES (?, ?, ?)
		ON CONFLICT (session, name) DO UPDATE SET run = excluded.run`, session, name, run)
	return err
}

// EventWait is the flow run of session that waits for its events of name,
// "" if none does.
func (s *Store) EventWait(ctx context.Context, session, name string) (string, error) {
	var run string
	err := s.db.QueryRowContext(ctx, `SELECT run FROM kc_event_wait WHERE session = ? AND name = ?`, session, name).Scan(&run)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return run, err
}

// ClearEventWait forgets run as waiting for session's events of name, if
// it is the one noted.
func (s *Store) ClearEventWait(ctx context.Context, session, name, run string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM kc_event_wait WHERE session = ? AND name = ? AND run = ?`, session, name, run)
	return err
}
