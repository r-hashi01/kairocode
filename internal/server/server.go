// Package server is KairoCode's HTTP API (design 8.1): sessions, messages,
// turns, approvals, undo and redo, and a session's events (SSE). The daemon serves it (ADR
// 0008).
package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/runtime"
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
	s.mux.HandleFunc("GET /sessions", s.listSessions)
	s.mux.HandleFunc("POST /worktrees", s.makeWorktree)
	s.mux.HandleFunc("POST /worktrees/remove", s.removeWorktree)
	s.mux.HandleFunc("POST /sessions/{id}/messages", s.sendMessage)
	s.mux.HandleFunc("GET /sessions/{id}/turns", s.lastTurn)
	s.mux.HandleFunc("GET /sessions/{id}/turns/{n}", s.getTurn)
	s.mux.HandleFunc("POST /sessions/{id}/close", s.closeSession)
	s.mux.HandleFunc("GET /sessions/{id}/approvals", s.listApprovals)
	s.mux.HandleFunc("GET /sessions/{id}/items", s.items)
	s.mux.HandleFunc("POST /sessions/{id}/items", s.startItem)
	s.mux.HandleFunc("POST /sessions/{id}/items/drop", s.dropItem)
	s.mux.HandleFunc("POST /sessions/{id}/items/keep", s.keepLine)
	s.mux.HandleFunc("POST /approvals", s.approve)
	s.mux.HandleFunc("POST /approvals/try", s.tryFlow)
	s.mux.HandleFunc("POST /questions", s.answer)
	s.mux.HandleFunc("GET /sessions/{id}/events", s.events)
	s.mux.HandleFunc("GET /sessions/{id}/history", s.history)
	s.mux.HandleFunc("GET /images/{hash}", s.image)
	s.mux.HandleFunc("GET /sessions/{id}/usage", s.usage)
	s.mux.HandleFunc("GET /usage", s.usageNow)
	s.mux.HandleFunc("GET /sessions/{id}/commands", s.commands)
	s.mux.HandleFunc("GET /sessions/{id}/skills", s.skills)
	s.mux.HandleFunc("GET /sessions/{id}/mcp", s.mcpList)
	s.mux.HandleFunc("GET /sessions/{id}/plugins", s.sessionPlugins)
	s.mux.HandleFunc("GET /plugins", s.plugins)
	s.mux.HandleFunc("POST /plugins/{name}/connected", s.pluginConnected)
	s.mux.HandleFunc("POST /jobs/{id}/done", s.jobDone)
	s.mux.HandleFunc("GET /sessions/{id}/log", s.sessionLog)
	s.mux.HandleFunc("POST /sessions/{id}/commands", s.runCommand)
	s.mux.HandleFunc("POST /sessions/{id}/undo", s.undo(false))
	s.mux.HandleFunc("POST /sessions/{id}/redo", s.undo(true))
	s.mux.HandleFunc("POST /sessions/{id}/compact", s.compact)
	s.mux.HandleFunc("POST /sessions/{id}/fork", s.fork)
	s.mux.HandleFunc("POST /sessions/{id}/cancel", s.cancel)
	s.mux.HandleFunc("GET /sessions/{id}/flows", s.flows)
	s.mux.HandleFunc("POST /sessions/{id}/flows", s.runFlow)
	s.mux.HandleFunc("POST /sessions/{id}/flows/new", s.newFlow)
	s.mux.HandleFunc("GET /sessions/{id}/flow", s.lastFlow)
	s.mux.HandleFunc("GET /flows/stats", s.flowStats)
	s.mux.HandleFunc("GET /flows/runs", s.flowRuns)
	s.mux.HandleFunc("GET /flows/run", s.flowRun)
	s.mux.HandleFunc("GET /providers", s.providers)
	s.mux.HandleFunc("GET /providers/{id}/models", s.models)
	s.mux.HandleFunc("GET /providers/{id}/efforts", s.efforts)
	s.mux.HandleFunc("POST /providers/{id}/install", s.installProvider)
	s.mux.HandleFunc("GET /providers/{id}/login", s.loginMethods)
	s.mux.HandleFunc("POST /providers/{id}/login", s.login)
	s.mux.HandleFunc("GET /sessions/{id}/settings", s.settings)
	s.mux.HandleFunc("POST /sessions/{id}/settings", s.switchSettings)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Authorized(s.mux.ServeHTTP).ServeHTTP(w, r)
}

// Authorized serves h only to requests that carry the token.
func (s *Server) Authorized(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" {
			want := "Bearer " + s.token
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(want)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		h(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Fail writes err as the API's error response: the daemon's client is
// checked against it (TestErrorsCrossTheSocket).
func Fail(w http.ResponseWriter, code int, err error) { fail(w, code, err) }

func fail(w http.ResponseWriter, code int, err error) {
	if errors.Is(err, api.ErrSessionFailed) || errors.Is(err, api.ErrFolderGone) {
		code = http.StatusGone // it takes no more (ADR 0021, 0037)
	}
	body := map[string]string{"error": err.Error()}
	if c := ErrorCode(err); c != "" { // the client gives back the same error
		body["code"] = c
	}
	writeJSON(w, code, body)
}
