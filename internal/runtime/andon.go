package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/config"
	"github.com/r-hashi01/kairocode/internal/lines"
	"github.com/r-hashi01/kairocode/internal/store"
)

// A line that keeps failing stops itself (ADR 0044, 3): when its last
// lines.stopAfter items in a repository ended stopped or failed, one after
// another, it takes no new item until a person starts it again.

// defaultStopAfter is how many items stopped in a row stop a line, when
// lines.stopAfter does not say.
const defaultStopAfter = 3

// kindLineTold is what the store keeps of a line's stop told to a
// conversation: told once.
const kindLineTold = "line-stop-told"

// stopAfter is lines.stopAfter, the user's and the project's: the smaller.
func stopAfter(repo string) int {
	n := 0
	layers, _ := config.Layers(repo)
	for _, l := range layers {
		var cfg struct {
			Lines struct {
				StopAfter int `json:"stopAfter"`
			} `json:"lines"`
		}
		if l.Decode(&cfg) == nil && cfg.Lines.StopAfter > 0 && (n == 0 || cfg.Lines.StopAfter < n) {
			n = cfg.Lines.StopAfter
		}
	}
	if n == 0 {
		n = defaultStopAfter
	}
	return n
}

// lineStopped says whether line of repo stopped itself, and why: its last
// stopAfter items that ended (since a person last started it again) all
// stopped or failed. An item done or taken starts the count again; one
// dropped, or not ended, is not counted.
func (r *Runtime) lineStopped(ctx context.Context, repo, line string) (bool, string, error) {
	restarted, err := r.Store.LineRestarted(ctx, repo, line)
	if err != nil {
		return false, "", err
	}
	rows, err := r.Store.ItemsOfLine(ctx, repo, line)
	if err != nil {
		return false, "", err
	}
	n := stopAfter(repo)
	var failed []string
	for k := len(rows) - 1; k >= 0 && len(failed) < n; k-- {
		row := rows[k]
		if !row.Started.After(restarted) {
			break
		}
		it := r.itemState(ctx, row)
		switch it.State {
		case api.ItemStopped, api.ItemFailed:
			where := it.State
			if it.Station != "" {
				where += " at " + it.Station
			}
			failed = append(failed, row.Name+" "+where)
		case api.ItemDone, api.ItemTaken:
			return false, "", nil
		}
	}
	if len(failed) < n {
		return false, "", nil
	}
	return true, fmt.Sprintf("stopped itself: its last %d items did not pass (%s)", n, strings.Join(failed, "; ")), nil
}

// RestartLine starts line of dir's repository again, after it stopped
// itself: a person's doing (/line restart, kairocode line -restart).
func (r *Runtime) RestartLine(ctx context.Context, dir, line string) error {
	main, _, err := repoOf(ctx, dir)
	if err != nil {
		return ErrItemsNoGit
	}
	return r.Store.RestartLine(ctx, main, line, time.Now())
}

// Lines are the lines of dir's project (at its repository's top), and how
// each stands: stopped or not, its items in progress and queued.
func (r *Runtime) Lines(ctx context.Context, dir string) ([]api.LineInfo, error) {
	main, top, err := repoOf(ctx, dir)
	if err != nil {
		return nil, ErrItemsNoGit
	}
	all, err := lines.List(top)
	if err != nil {
		return nil, err
	}
	out := make([]api.LineInfo, 0, len(all))
	for _, e := range all {
		info := api.LineInfo{Name: e.Name, Stations: []string{}}
		if e.Err != nil {
			info.Error = e.Err.Error()
			out = append(out, info)
			continue
		}
		info.Description, info.Wip = e.Line.Description, e.Line.Wip
		for _, s := range e.Line.Stations {
			info.Stations = append(info.Stations, s.Name)
		}
		if info.Stopped, info.Why, err = r.lineStopped(ctx, main, e.Name); err != nil {
			return nil, err
		}
		rows, err := r.Store.ItemsOfLine(ctx, main, e.Name)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if row.Ended != "" {
				continue
			}
			switch r.itemState(ctx, row).State {
			case api.ItemQueued:
				info.Queued++
			case api.ItemRunning, api.ItemWaiting:
				info.InProgress++
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// tellLineStopped tells the conversation of item row, which ended its line
// stopped or failed, if the line now stopped itself: once for each stop.
func (r *Runtime) tellLineStopped(ctx context.Context, row store.ItemRow) error {
	if row.Line == "" {
		return nil
	}
	stopped, why, err := r.lineStopped(ctx, row.Repo, row.Line)
	if err != nil || !stopped {
		return err
	}
	restarted, err := r.Store.LineRestarted(ctx, row.Repo, row.Line)
	if err != nil {
		return err
	}
	key := []byte(fmt.Sprintf("%s\x00%s\x00%d\x00%s", row.Repo, row.Line, restarted.UnixMilli(), row.Of))
	marker := store.Hash(kindLineTold, key)
	if _, _, err := r.Store.Get(ctx, marker); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	text := fmt.Sprintf("[KairoCode] The line %s %s. It takes no new item until the person starts it again: /line restart %s, or kairocode line -restart %s.",
		row.Line, why, row.Line, row.Line)
	if _, err := r.Send(ctx, row.Of, text); err != nil {
		return fmt.Errorf("telling %s that the line %s stopped: %w", row.Of, row.Line, err)
	}
	_, err = r.Store.Put(ctx, kindLineTold, key)
	return err
}
