// Package server is KairoCode's HTTP API (design 8.1): sessions, messages,
// turns, approvals, undo and redo, and a session's events (SSE). The daemon serves it (ADR
// 0008).
package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/msg"
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
	s.mux.HandleFunc("POST /questions", s.answer)
	s.mux.HandleFunc("GET /sessions/{id}/events", s.events)
	s.mux.HandleFunc("GET /sessions/{id}/history", s.history)
	s.mux.HandleFunc("GET /sessions/{id}/usage", s.usage)
	s.mux.HandleFunc("GET /sessions/{id}/commands", s.commands)
	s.mux.HandleFunc("POST /sessions/{id}/commands", s.runCommand)
	s.mux.HandleFunc("POST /sessions/{id}/undo", s.undo(false))
	s.mux.HandleFunc("POST /sessions/{id}/redo", s.undo(true))
	s.mux.HandleFunc("POST /sessions/{id}/cancel", s.cancel)
	s.mux.HandleFunc("GET /sessions/{id}/flows", s.flows)
	s.mux.HandleFunc("POST /sessions/{id}/flows", s.runFlow)
	s.mux.HandleFunc("POST /sessions/{id}/flows/new", s.newFlow)
	s.mux.HandleFunc("GET /sessions/{id}/flow", s.lastFlow)
	s.mux.HandleFunc("GET /flows/stats", s.flowStats)
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

func fail(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var in workflows.SessionInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Dir == "" || !filepath.IsAbs(in.Dir) {
		fail(w, http.StatusBadRequest, errors.New(`want {"dir": "<absolute path>", "agent", "engine", "model"}`))
		return
	}
	id, err := s.rt.CreateSession(r.Context(), s.newID(), in)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
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

// undo starts a turn that undoes (or redoes) the session's last turn's
// changes to the working files (design 7).
func (s *Server) undo(redo bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ask := s.rt.Undo
		if redo {
			ask = s.rt.Redo
		}
		n, err := ask(r.Context(), sessionOf(r))
		if errors.Is(err, runtime.ErrNoUndo) {
			fail(w, http.StatusConflict, err)
			return
		}
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]int{"turn": n})
	}
}

// cancel stops the session's last turn (ADR 0011); 409 when it has ended.
func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	n, err := s.rt.Cancel(r.Context(), sessionOf(r))
	if errors.Is(err, runtime.ErrNoTurnToCancel) {
		fail(w, http.StatusConflict, err)
		return
	}
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
	if errors.Is(err, kairo.ErrCancelled) { // stopped by the user: an end, not a failure
		writeJSON(w, http.StatusOK, map[string]any{"stopped": runtime.StoppedCancelled})
		return
	}
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
		code := http.StatusBadRequest
		if errors.Is(err, runtime.ErrAnswered) {
			code = http.StatusConflict // another client answered first
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
		if errors.Is(err, runtime.ErrAnswered) {
			code = http.StatusConflict
		}
		fail(w, code, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// events streams the session's events as server-sent events: "id" is the
// event's seq, "event" its type, "data" the event as JSON. It first sends
// what waits for a person in the session (runtime.Events). The stream ends
// when the client goes, or when it falls too far behind: it then connects
// again.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	session := sessionOf(r)
	if _, err := s.rt.K.Get(r.Context(), session); err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, http.StatusInternalServerError, errors.New("streaming is not supported"))
		return
	}
	events, err := s.rt.Events(r.Context(), session)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	for ev := range events {
		b, err := json.Marshal(ev)
		if err != nil {
			return
		}
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, b); err != nil {
			return
		}
		flusher.Flush()
	}
}

// history is the session's conversation as of its last finished turn: the
// messages' roles and parts (not their providers' encodings).
func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	ms, err := s.rt.History(r.Context(), sessionOf(r))
	if errors.Is(err, kairo.ErrUnknownRun) {
		fail(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]msg.Message, 0, len(ms))
	for _, m := range ms {
		out = append(out, msg.Message{Role: m.Role, Parts: m.Parts})
	}
	writeJSON(w, http.StatusOK, out)
}

// usage is what the session's model calls recorded they used.
func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	u, err := s.rt.Usage(r.Context(), sessionOf(r))
	if errors.Is(err, kairo.ErrUnknownRun) {
		fail(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// commands are the commands the session can run (opencode's commands/*.md).
func (s *Server) commands(w http.ResponseWriter, r *http.Request) {
	cs, err := s.rt.Commands(r.Context(), sessionOf(r))
	if errors.Is(err, kairo.ErrUnknownRun) {
		fail(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, cs)
}

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
	if errors.Is(err, runtime.ErrNoFlow) || errors.Is(err, kairo.ErrUnknownRun) {
		fail(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
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
	if errors.Is(err, runtime.ErrNoFlowRequest) {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"turn": n})
}

// runCommand runs one: {"name": "...", "arguments": "..."}.
func (s *Server) runCommand(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" {
		fail(w, http.StatusBadRequest, errors.New(`want {"name": "...", "arguments": "..."}`))
		return
	}
	n, err := s.rt.RunCommand(r.Context(), sessionOf(r), in.Name, in.Arguments)
	if errors.Is(err, runtime.ErrNoCommand) {
		fail(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"turn": n})
}
