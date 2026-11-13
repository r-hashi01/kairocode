package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/tools"
)

// items lists the work items the session sent down lines (ADR 0043).
func (s *Server) items(w http.ResponseWriter, r *http.Request) {
	// Those merged are taken, and cleaned up, as a person lists them; one
	// that cannot be is logged by the runtime, and listed as it is.
	_ = s.rt.TakeItems(r.Context(), sessionOf(r))
	items, err := s.rt.Items(r.Context(), sessionOf(r))
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if items == nil {
		items = []api.Item{}
	}
	writeJSON(w, http.StatusOK, items)
}

// startItem starts an item down the standard line the person named
// (/line): {"line", "request", "name", "call"}, call naming the request so
// the same one sent again starts no second item.
func (s *Server) startItem(w http.ResponseWriter, r *http.Request) {
	var in struct {
		tools.ItemSpec
		Call string `json:"call"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Line == "" || in.Request == "" || in.Call == "" {
		fail(w, http.StatusBadRequest, errors.New(`want {"line": "...", "request": "...", "call": "..."}`))
		return
	}
	it, err := s.rt.StartNamedItem(r.Context(), sessionOf(r), in.Call, in.ItemSpec)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, api.ErrItemsCap) {
			code = http.StatusConflict
		}
		fail(w, code, err)
		return
	}
	writeJSON(w, http.StatusAccepted, it)
}

// dropItem drops an item: {"item", "force"}.
func (s *Server) dropItem(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Item  string `json:"item"`
		Force bool   `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Item == "" {
		fail(w, http.StatusBadRequest, errors.New(`want {"item": "...", "force": false}`))
		return
	}
	if err := s.rt.DropItem(r.Context(), sessionOf(r), in.Item, in.Force); err != nil {
		code := http.StatusBadRequest
		switch {
		case errors.Is(err, api.ErrNotAnItem):
			code = http.StatusNotFound
		case errors.Is(err, api.ErrWorktreeChanged) || errors.Is(err, api.ErrWorktreeBusy):
			code = http.StatusConflict
		}
		fail(w, code, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// keepLine writes the line an item ran as a line of the project (/line
// keep): {"item", "name"} → {"path"}.
func (s *Server) keepLine(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Item string `json:"item"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Item == "" || in.Name == "" {
		fail(w, http.StatusBadRequest, errors.New(`want {"item": "...", "name": "..."}`))
		return
	}
	path, err := s.rt.KeepLine(r.Context(), sessionOf(r), in.Item, in.Name)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, api.ErrNotAnItem) {
			code = http.StatusNotFound
		}
		fail(w, code, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": path})
}
