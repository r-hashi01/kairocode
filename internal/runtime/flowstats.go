package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/r-hashi01/kairocode/internal/api"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/workflows"
)

const (
	FromCommand = api.FromCommand
	FromHook    = api.FromHook
	FromAgent   = api.FromAgent
)

type FlowStat = api.FlowStat

// FlowStats are the flows the sessions in dir ran, and the project's flows
// that none ran, by name. Only what the records keep is counted: the runs
// of trees that ended longer ago than KeepFinished are gone.
func (r *Runtime) FlowStats(ctx context.Context, dir string) ([]FlowStat, error) {
	dir = sameDir(dir)
	stats := map[string]*FlowStat{}
	stat := func(name string) *FlowStat {
		if stats[name] == nil {
			stats[name] = &FlowStat{Name: name, From: map[string]int{}}
		}
		return stats[name]
	}
	defined, err := flowsIn(dir)
	if err != nil {
		return nil, err
	}
	for _, f := range defined {
		stat(f.Name).Defined = true
	}
	sessions, err := r.K.List(ctx, kairo.Filter{Workflow: workflows.Session})
	if err != nil {
		return nil, err
	}
	for _, s := range sessions {
		var in workflows.SessionInput
		if json.Unmarshal(input(s), &in) != nil || sameDir(in.Dir) != dir {
			continue
		}
		turns, err := r.K.Children(ctx, s.RunID)
		if err != nil {
			return nil, err
		}
		for _, t := range turns {
			if t.Workflow != workflows.Turn {
				continue
			}
			var ti workflows.TurnInput
			_ = json.Unmarshal(input(t), &ti)
			kids, err := r.K.Children(ctx, t.RunID)
			if err != nil {
				return nil, err
			}
			for _, k := range kids {
				if k.Workflow != workflows.Flow {
					continue
				}
				var fi workflows.FlowInput
				if json.Unmarshal(input(k), &fi) != nil || fi.Call.Name == "" {
					continue
				}
				st := stat(fi.Call.Name)
				st.Runs++
				switch {
				case fi.Hook != "":
					st.From[FromHook]++
				case ti.Flow != nil:
					st.From[FromCommand]++
				default:
					st.From[FromAgent]++
				}
				if k.Created.After(st.Last) {
					st.Last = k.Created
				}
				if !k.Finished() {
					st.Running++
					continue
				}
				// A finished run's result is there: Await does not wait.
				out, err := kairo.Await[workflows.FlowOutput](ctx, r.K, k.RunID)
				switch {
				case ctx.Err() != nil:
					return nil, ctx.Err()
				case errors.Is(err, kairo.ErrResultLost):
					// How it ended is no longer kept: counted as run only.
				case err == nil && out.OK && !out.Broken:
					st.OK++
				default:
					st.Failed++
				}
			}
		}
	}
	out := make([]FlowStat, 0, len(stats))
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for n := range stats {
			if !yield(n) {
				return
			}
		}
	}) {
		out = append(out, *stats[name])
	}
	return out, nil
}

// FlowFrom puts how a flow ran in words: "command 2, hook 1".
func FlowFrom(from map[string]int) string {
	var parts []string
	for _, k := range []string{FromCommand, FromHook, FromAgent} {
		if from[k] > 0 {
			parts = append(parts, k+" "+strconv.Itoa(from[k]))
		}
	}
	return strings.Join(parts, ", ")
}

// sameDir is dir as two names of one folder both become: absolute, with
// its links resolved where they can be.
func sameDir(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return dir
}
