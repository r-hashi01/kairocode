package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ItemRow is a work item a conversation sent down a line (ADR 0043).
type ItemRow struct {
	ID      string // the item's session
	Of      string // the conversation's session
	Line    string // the standard line's name, "" for one put together on the spot
	Def     []byte // the line as it started, JSON
	Request string
	Name    string // its worktree's
	Repo    string // the main checkout of the conversation's repository
	Dir     string // its worktree, once made
	Branch  string
	Base    string // the branch its worktree started from
	Start   string // and the commit
	Started time.Time
	Ended   string // "" while kept; "taken" or "dropped"
}

const itemCols = `id, of, line, def, request, name, repo, dir, branch, base, start, started, ended`

func scanItem(sc interface{ Scan(...any) error }) (ItemRow, error) {
	var r ItemRow
	var at int64
	err := sc.Scan(&r.ID, &r.Of, &r.Line, &r.Def, &r.Request, &r.Name, &r.Repo, &r.Dir, &r.Branch, &r.Base, &r.Start, &at, &r.Ended)
	r.Started = time.UnixMilli(at)
	return r, err
}

// AddItem keeps r as an item, unless one is kept under its id already:
// the one kept, either way (a start made again finds the first).
func (s *Store) AddItem(ctx context.Context, r ItemRow) (ItemRow, error) {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO kc_item (`+itemCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		r.ID, r.Of, r.Line, r.Def, r.Request, r.Name, r.Repo, r.Dir, r.Branch, r.Base, r.Start, r.Started.UnixMilli(), r.Ended); err != nil {
		return ItemRow{}, err
	}
	return s.Item(ctx, r.ID)
}

// ErrNoItem: no item under that id.
var ErrNoItem = errors.New("no such item")

// Item is the item kept under id.
func (s *Store) Item(ctx context.Context, id string) (ItemRow, error) {
	r, err := scanItem(s.db.QueryRowContext(ctx, `SELECT `+itemCols+` FROM kc_item WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ItemRow{}, ErrNoItem
	}
	return r, err
}

// SetItemWorktree notes the worktree item id was made in, and the branch
// and commit it started from.
func (s *Store) SetItemWorktree(ctx context.Context, id, dir, branch, base, start string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE kc_item SET dir = ?, branch = ?, base = ?, start = ? WHERE id = ?`, dir, branch, base, start, id)
	return err
}

// EndItem notes item id taken or dropped.
func (s *Store) EndItem(ctx context.Context, id, how string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE kc_item SET ended = ? WHERE id = ?`, how, id)
	return err
}

// ItemsOf is the items conversation sent, the first first.
func (s *Store) ItemsOf(ctx context.Context, conversation string) ([]ItemRow, error) {
	return s.items(ctx, `of = ?`, conversation)
}

// ItemsIn is the items of repository repo not taken or dropped.
func (s *Store) ItemsIn(ctx context.Context, repo string) ([]ItemRow, error) {
	return s.items(ctx, `repo = ? AND ended = ''`, repo)
}

// ItemNames is the names the items of repository repo took, ended or not.
func (s *Store) ItemNames(ctx context.Context, repo string) (map[string]bool, error) {
	rows, err := s.items(ctx, `repo = ?`, repo)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range rows {
		out[r.Name] = true
	}
	return out, nil
}

func (s *Store) items(ctx context.Context, where string, args ...any) ([]ItemRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+itemCols+` FROM kc_item WHERE `+where+` ORDER BY started, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ItemRow
	for rows.Next() {
		r, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
