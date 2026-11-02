package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/api"
)

// flows lists the flows the session's project has (ADR 0003).
func (s *Server) flows(w http.ResponseWriter, r *http.Request) {
	fs, err := s.rt.Flows(r.Context(), sessionOf(r))
	if errors.Is(err, kairo.ErrUnknownRun) {
		fail(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, fs)
}

// runFlow runs one: {"name": "...", "arguments": "..."}. A flow that is
// not there ends its turn saying so.
func (s *Server) runFlow(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" {
		fail(w, http.StatusBadRequest, errors.New(`want {"name": "...", "arguments": "..."}`))
		return
	}
	n, err := s.rt.RunFlow(r.Context(), sessionOf(r), in.Name, in.Arguments)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"turn": n})
}

// lastFlow is the session's latest flow, with its calls' states; 404 when
// it ran none.
func (s *Server) lastFlow(w http.ResponseWriter, r *http.Request) {
	v, err := s.rt.LastFlow(r.Context(), sessionOf(r))
	if errors.Is(err, api.ErrNoFlow) || errors.Is(err, kairo.ErrUnknownRun) {
		fail(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// flowRuns are the flows' runs (ADR 0029) of ?dir= and ?flow= (either
// may be left out), the latest first, at most ?n=; ?only=tries or runs.
func (s *Server) flowRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	n, _ := strconv.Atoi(q.Get("n"))
	runs, err := s.rt.FlowRuns(r.Context(), q.Get("dir"), q.Get("flow"), q.Get("only"), n)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if runs == nil {
		runs = []api.FlowRun{}
	}
	writeJSON(w, http.StatusOK, runs)
}

// flowRun is a flow's run (?id=), with its chart and steps.
func (s *Server) flowRun(w http.ResponseWriter, r *http.Request) {
	f, err := s.rt.FlowRunByID(r.Context(), r.URL.Query().Get("id"))
	if errors.Is(err, api.ErrNoFlowRun) {
		fail(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// flowStats are the flows the sessions in ?dir= ran, and the project's
// flows that none ran.
func (s *Server) flowStats(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("dir")
	if dir == "" {
		fail(w, http.StatusBadRequest, errors.New("want ?dir="))
		return
	}
	st, err := s.rt.FlowStats(r.Context(), dir)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// newFlow has the model write a flow from words: {"request": "..."}.
func (s *Server) newFlow(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Request string `json:"request"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, errors.New(`want {"request": "..."}`))
		return
	}
	n, err := s.rt.NewFlow(r.Context(), sessionOf(r), in.Request)
	if errors.Is(err, api.ErrNoFlowRequest) {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"turn": n})
}
