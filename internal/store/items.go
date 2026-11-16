package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
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
	Made    bool   // its session was made: never made again
	Started time.Time
	Ended   string // "" while kept; "taken" or "dropped"
}

const itemCols = `id, of, line, def, request, name, repo, dir, branch, base, start, made, started, ended`

func scanItem(sc interface{ Scan(...any) error }) (ItemRow, error) {
	var r ItemRow
	var at int64
	err := sc.Scan(&r.ID, &r.Of, &r.Line, &r.Def, &r.Request, &r.Name, &r.Repo, &r.Dir, &r.Branch, &r.Base, &r.Start, &r.Made, &at, &r.Ended)
	r.Started = time.UnixMilli(at)
	return r, err
}

// AddItem keeps r as an item, unless one is kept under its id already:
// the one kept, either way (a start made again finds the first).
func (s *Store) AddItem(ctx context.Context, r ItemRow) (ItemRow, error) {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO kc_item (`+itemCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		r.ID, r.Of, r.Line, r.Def, r.Request, r.Name, r.Repo, r.Dir, r.Branch, r.Base, r.Start, r.Made, r.Started.UnixMilli(), r.Ended); err != nil {
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

// SetItemMade notes that item id's session was made.
func (s *Store) SetItemMade(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE kc_item SET made = 1 WHERE id = ?`, id)
	return err
}

// EndItem notes item id taken or dropped.
func (s *Store) EndItem(ctx context.Context, id, how string) error {
	var at int64 // kept, or being ended: not ended yet
	if how != "" && !strings.HasPrefix(how, "ending:") {
		at = time.Now().UnixMilli()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE kc_item SET ended = ?, ended_at = ? WHERE id = ?`, how, at, id)
	return err
}

// SetItemResult keeps what item id came to (ADR 0045), as JSON.
func (s *Store) SetItemResult(ctx context.Context, id string, result []byte) error {
	_, err := s.db.ExecContext(ctx, `UPDATE kc_item SET result = ? WHERE id = ?`, result, id)
	return err
}

// ItemResult is what item id came to, nil when none was kept.
func (s *Store) ItemResult(ctx context.Context, id string) ([]byte, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT result FROM kc_item WHERE id = ?`, id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

// ItemsOf is the items conversation sent, the first first.
func (s *Store) ItemsOf(ctx context.Context, conversation string) ([]ItemRow, error) {
	return s.items(ctx, `of = ?`, conversation)
}

// ItemsIn is the items of repository repo not taken or dropped, those
// being ended ("ending:...") among them.
func (s *Store) ItemsIn(ctx context.Context, repo string) ([]ItemRow, error) {
	return s.items(ctx, `repo = ? AND (ended = '' OR ended LIKE 'ending:%')`, repo)
}

// ItemsKept is every item not taken or dropped, those being ended among
// them.
func (s *Store) ItemsKept(ctx context.Context) ([]ItemRow, error) {
	return s.items(ctx, `ended = '' OR ended LIKE 'ending:%'`)
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

// LineRestarted is when a person last started line of repo again (zero:
// never).
func (s *Store) LineRestarted(ctx context.Context, repo, line string) (time.Time, error) {
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT restarted FROM kc_line WHERE repo = ? AND line = ?`, repo, line).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	return time.UnixMilli(at), err
}

// RestartLine notes that a person started line of repo again, at.
func (s *Store) RestartLine(ctx context.Context, repo, line string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO kc_line (repo, line, restarted) VALUES (?, ?, ?)
		ON CONFLICT (repo, line) DO UPDATE SET restarted = excluded.restarted`, repo, line, at.UnixMilli())
	return err
}

// ItemsRepos is the repositories with items not taken or dropped.
func (s *Store) ItemsRepos(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT repo FROM kc_item WHERE ended = '' ORDER BY repo`)
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

// ItemsOfLine is the items of line in repo, the first first, ended or not.
func (s *Store) ItemsOfLine(ctx context.Context, repo, line string) ([]ItemRow, error) {
	return s.items(ctx, `repo = ? AND line = ?`, repo, line)
}
