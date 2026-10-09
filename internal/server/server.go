// Package server is KairoCode's HTTP API (design 8.1): sessions, messages,
// turns and approvals. The daemon serves it (ADR 0008); the event stream
// (SSE) and undo come later.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/r-hashi01/kairocode/internal/runtime"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

type Server struct {
	rt *runtime.Runtime
	// Token, when set, is required as "Authorization: Bearer <token>".
	token string
	mux   *http.ServeMux
	newID func() string
}

func New(rt *runtime.Runtime, token string, newID func() string) *Server {
	s := &Server{rt: rt, token: token, mux: http.NewServeMux(), newID: newID}
	s.mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	s.mux.HandleFunc("POST /sessions", s.createSession)
	s.mux.HandleFunc("POST /sessions/{id}/messages", s.sendMessage)
	s.mux.HandleFunc("GET /sessions/{id}/turns/{n}", s.getTurn)
	s.mux.HandleFunc("POST /sessions/{id}/close", s.closeSession)
	s.mux.HandleFunc("GET /sessions/{id}/approvals", s.listApprovals)
	s.mux.HandleFunc("POST /approvals", s.approve)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.token != "" {
		want := "Bearer " + s.token
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Dir   string `json:"dir"`
		Agent string `json:"agent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Dir == "" {
		fail(w, http.StatusBadRequest, errors.New(`want {"dir": "...", "agent": "..."}`))
		return
	}
	id, err := s.rt.CreateSession(r.Context(), s.newID(), workflows.SessionInput{Dir: in.Dir, Agent: in.Agent})
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// Session ids contain a slash ("s/<ID>"); the path carries the part after it.
func sessionOf(r *http.Request) string { return runtime.SessionID(r.PathValue("id")) }

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Text == "" {
		fail(w, http.StatusBadRequest, errors.New(`want {"text": "..."}`))
		return
	}
	n, err := s.rt.Send(r.Context(), sessionOf(r), in.Text)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"turn": n})
}

func (s *Server) closeSession(w http.ResponseWriter, r *http.Request) {
	if err := s.rt.CloseSession(r.Context(), sessionOf(r)); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// getTurn waits for the turn to finish (the request's context bounds the
// wait) and returns its reply.
func (s *Server) getTurn(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 0 {
		fail(w, http.StatusBadRequest, errors.New("turn must be a number"))
		return
	}
	out, err := s.rt.WaitTurn(r.Context(), sessionOf(r), n)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	text, err := s.rt.Reply(r.Context(), out.Reply)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"reply": text, "steps": out.Steps, "usage": out.Usage, "stopped": out.Stopped,
	})
}

func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request) {
	p, err := s.rt.Pending(r.Context(), sessionOf(r))
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if p == nil {
		p = []runtime.Pending{}
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
		fail(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
