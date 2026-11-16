package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/r-hashi01/kairocode/internal/api"
)

func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request) {
	p, err := s.rt.Pending(r.Context(), sessionOf(r))
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	// ?items=1: its work items' too, each naming its item (ADR 0043).
	if r.URL.Query().Get("items") != "" {
		ip, err := s.rt.ItemsPending(r.Context(), sessionOf(r))
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		p = append(p, ip...)
	}
	if p == nil {
		p = []api.Pending{}
	}
	writeJSON(w, http.StatusOK, p)
}

// approve answers a request for approval (FR-11): from any client, any time
// after it was made.
func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key    string `json:"key"`
		OK     bool   `json:"ok"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Key == "" {
		fail(w, http.StatusBadRequest, errors.New(`want {"key": "...", "ok": true|false, "reason": "..."}`))
		return
	}
	if err := s.rt.Approve(r.Context(), in.Key, in.OK, in.Reason); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, api.ErrAnswered) {
			code = http.StatusConflict // another client answered first
		}
		fail(w, code, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// tryFlow tries the flow an approval would write (ADR 0017): {"key":
// "...", "args": "<the words to run it with>"}. The approval waits on.
func (s *Server) tryFlow(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key  string `json:"key"`
		Args string `json:"args"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Key == "" {
		fail(w, http.StatusBadRequest, errors.New(`want {"key": "...", "args": "..."}`))
		return
	}
	if err := s.rt.TryFlow(r.Context(), in.Key, in.Args); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, api.ErrAnswered) {
			code = http.StatusConflict
		}
		fail(w, code, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// answer answers a flow's question: {"key": "...", "answer": "<one of its
// options>"}. 409 when it was answered already.
func (s *Server) answer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key    string `json:"key"`
		Answer string `json:"answer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Key == "" {
		fail(w, http.StatusBadRequest, errors.New(`want {"key": "...", "answer": "..."}`))
		return
	}
	if err := s.rt.Answer(r.Context(), in.Key, in.Answer); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, api.ErrAnswered) {
			code = http.StatusConflict
		}
		fail(w, code, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
