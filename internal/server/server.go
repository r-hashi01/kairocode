// Package server is KairoCode's HTTP API (design 8.1): sessions, messages,
// turns, approvals, undo and redo, and a session's events (SSE). The daemon serves it (ADR
// 0008).
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/agents"
	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/runtime"
	"github.com/r-hashi01/kairocode/internal/store"
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
	s.mux.HandleFunc("GET /sessions", s.listSessions)
	s.mux.HandleFunc("POST /sessions/{id}/messages", s.sendMessage)
	s.mux.HandleFunc("GET /sessions/{id}/turns", s.lastTurn)
	s.mux.HandleFunc("GET /sessions/{id}/turns/{n}", s.getTurn)
	s.mux.HandleFunc("POST /sessions/{id}/close", s.closeSession)
	s.mux.HandleFunc("GET /sessions/{id}/approvals", s.listApprovals)
	s.mux.HandleFunc("POST /approvals", s.approve)
	s.mux.HandleFunc("POST /approvals/try", s.tryFlow)
	s.mux.HandleFunc("POST /questions", s.answer)
	s.mux.HandleFunc("GET /sessions/{id}/events", s.events)
	s.mux.HandleFunc("GET /sessions/{id}/history", s.history)
	s.mux.HandleFunc("GET /images/{hash}", s.image)
	s.mux.HandleFunc("GET /sessions/{id}/usage", s.usage)
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

func fail(w http.ResponseWriter, code int, err error) {
	if errors.Is(err, runtime.ErrSessionFailed) {
		code = http.StatusGone // it takes no more (ADR 0021)
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var in workflows.SessionInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Dir == "" || !filepath.IsAbs(in.Dir) {
		fail(w, http.StatusBadRequest, errors.New(`want {"dir": "<absolute path>", "agent", "engine", "model"}`))
		return
	}
	in.From, in.History = nil, "" // a fork's, which Fork sets (ADR 0021)
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
		// Images (ADR 0026), their bytes in base64.
		Images []struct {
			MediaType string `json:"media_type"`
			Data      []byte `json:"data"`
		} `json:"images"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Text == "" && len(in.Images) == 0 {
		fail(w, http.StatusBadRequest, errors.New(`want {"text": "...", "images": [{"media_type": "image/png", "data": "<base64>"}]}`))
		return
	}
	var images []runtime.Image
	for _, im := range in.Images {
		images = append(images, runtime.Image{MediaType: im.MediaType, Data: im.Data})
	}
	n, err := s.rt.SendImages(r.Context(), sessionOf(r), in.Text, images)
	if errors.Is(err, runtime.ErrBadImage) {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"turn": n})
}

// image is the bytes of an image of a message, by its hash (the part after
// "sha256:"), in base64 (ADR 0026).
func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	data, err := s.rt.ImageData(r.Context(), "sha256:"+r.PathValue("hash"))
	if errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]byte{"data": data})
}

// fork starts a session from a turn of this one (ADR 0021): {"turn": n}
// (absent: the last finished), {"files": true} to put the files back as
// they were after it. 409 when there is no such turn.
func (s *Server) fork(w http.ResponseWriter, r *http.Request) {
	in := struct {
		Turn  *int `json:"turn"`
		Files bool `json:"files"`
	}{}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			fail(w, http.StatusBadRequest, errors.New(`want {"turn": n, "files": bool}`))
			return
		}
	}
	n := -1
	if in.Turn != nil {
		n = *in.Turn
	}
	id, turn, err := s.rt.Fork(r.Context(), s.newID(), sessionOf(r), n, in.Files)
	if errors.Is(err, runtime.ErrNoForkPoint) {
		fail(w, http.StatusConflict, err)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "turn": turn})
}

// compact starts a turn that only compacts the session's conversation
// (ADR 0020).
func (s *Server) compact(w http.ResponseWriter, r *http.Request) {
	n, err := s.rt.Compact(r.Context(), sessionOf(r))
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
// providers are the providers a session can switch to (ADR 0014).
func (s *Server) providers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.rt.Providers(r.Context()))
}

// loginMethods are the ways to sign in to a provider.
func (s *Server) loginMethods(w http.ResponseWriter, r *http.Request) {
	ms, err := s.rt.LoginMethods(r.Context(), r.PathValue("id"))
	if errors.Is(err, runtime.ErrNoProvider) {
		fail(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if ms == nil {
		ms = []runtime.AuthMethod{}
	}
	writeJSON(w, http.StatusOK, ms)
}

// login signs in to a provider: {"method": id} with the agent's own
// method; {"done": true} after the user ran a terminal method.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Method string `json:"method"`
		Done   bool   `json:"done"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, errors.New(`want {"method": "..."} or {"done": true}`))
		return
	}
	if in.Done {
		s.rt.LoggedIn(r.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.rt.Login(r.Context(), r.PathValue("id"), in.Method); err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// installProvider installs an agent of ACP's registry (ADR 0015): the
// user chose to.
func (s *Server) installProvider(w http.ResponseWriter, r *http.Request) {
	p, err := s.rt.InstallProvider(r.Context(), r.PathValue("id"), r.URL.Query().Get("version"))
	if errors.Is(err, runtime.ErrNoProvider) {
		fail(w, http.StatusNotFound, err)
		return
	}
	if errors.Is(err, agents.ErrChanged) {
		fail(w, http.StatusConflict, err)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// efforts are a provider's efforts to choose from (none: it has none).
func (s *Server) efforts(w http.ResponseWriter, r *http.Request) {
	es, err := s.rt.Efforts(r.Context(), r.PathValue("id"))
	if errors.Is(err, runtime.ErrNoProvider) {
		fail(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if es == nil {
		es = []string{}
	}
	writeJSON(w, http.StatusOK, es)
}

// models are a provider's models to choose from.
func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	ms, err := s.rt.Models(r.Context(), r.PathValue("id"))
	if errors.Is(err, runtime.ErrNoProvider) {
		fail(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if ms == nil {
		ms = []string{}
	}
	writeJSON(w, http.StatusOK, ms)
}

// settings are the provider and model the session's next turn runs with.
func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	st, err := s.rt.Settings(r.Context(), sessionOf(r))
	if errors.Is(err, kairo.ErrUnknownRun) {
		fail(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// switchSettings switches them from the next turn on: {"provider": "...",
// "model": "..."}, either may be left out.
func (s *Server) switchSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Effort   string `json:"effort"`
		Mode     string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Provider == "" && in.Model == "" && in.Effort == "" && in.Mode == "" {
		fail(w, http.StatusBadRequest, errors.New(`want {"provider": "...", "model": "...", "effort": "...", "mode": "..."}`))
		return
	}
	var st runtime.Settings
	var err error
	if in.Mode != "" {
		st, err = s.rt.SetMode(r.Context(), sessionOf(r), in.Mode)
	}
	if err == nil && (in.Provider != "" || in.Model != "" || in.Effort != "") {
		st, err = s.rt.Switch(r.Context(), sessionOf(r), in.Provider, in.Model, in.Effort)
	}
	switch {
	case errors.Is(err, kairo.ErrUnknownRun), errors.Is(err, runtime.ErrNoProvider):
		fail(w, http.StatusNotFound, err)
		return
	case errors.Is(err, runtime.ErrUnavailable), errors.Is(err, runtime.ErrNotInstalled):
		fail(w, http.StatusConflict, err)
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// lastTurn is the number of the session's latest turn; 404 if it has
// none.
func (s *Server) lastTurn(w http.ResponseWriter, r *http.Request) {
	n, ok, err := s.rt.LastTurn(r.Context(), sessionOf(r))
	if errors.Is(err, kairo.ErrUnknownRun) || err == nil && !ok {
		fail(w, http.StatusNotFound, errors.New("the session has no turn"))
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"turn": n})
}

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
	if errors.Is(err, kairo.ErrStopped) || errors.Is(err, context.Canceled) {
		// The wait was cut (the daemon going, as it hands over: ADR 0019),
		// not the turn: the client waits again, there.
		fail(w, http.StatusServiceUnavailable, err)
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
		if errors.Is(err, runtime.ErrAnswered) {
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

// skills lists the session's skills, and the SKILL.md files not taken.
// sessionLog is the session's record (ADR 0023).
func (s *Server) sessionLog(w http.ResponseWriter, r *http.Request) {
	l, err := s.rt.Log(r.Context(), sessionOf(r))
	if errors.Is(err, kairo.ErrUnknownRun) {
		fail(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

// mcpList is the session's MCP servers and custom tools (ADR 0022).
func (s *Server) mcpList(w http.ResponseWriter, r *http.Request) {
	list, err := s.rt.MCP(r.Context(), sessionOf(r))
	if errors.Is(err, kairo.ErrUnknownRun) {
		fail(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// listSessions lists the sessions of ?dir= (all without), the latest used
// first, at most ?limit=.
func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.rt.Sessions(r.Context(), r.URL.Query().Get("dir"), limit)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// sessionPlugins lists the session's plugins and their state (ADR 0025).
func (s *Server) sessionPlugins(w http.ResponseWriter, r *http.Request) {
	list, err := s.rt.Plugins(r.Context(), sessionOf(r))
	if errors.Is(err, kairo.ErrUnknownRun) {
		fail(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// plugins lists the plugins of ?dir= and their state.
func (s *Server) plugins(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("dir")
	if dir == "" {
		fail(w, http.StatusBadRequest, errors.New("want ?dir="))
		return
	}
	list, err := s.rt.PluginsIn(r.Context(), dir)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// pluginConnected: a plugin's credentials changed (kairocode plugin
// connect or disconnect).
func (s *Server) pluginConnected(w http.ResponseWriter, r *http.Request) {
	s.rt.PluginConnected(r.PathValue("name"))
	w.WriteHeader(http.StatusNoContent)
}

// jobDone: a command's runner says it ended (ADR 0028).
func (s *Server) jobDone(w http.ResponseWriter, r *http.Request) {
	if err := s.rt.JobDone(r.PathValue("id")); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, runtime.ErrClosing) {
			code = http.StatusServiceUnavailable // the runner tells the next daemon
		}
		fail(w, code, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) skills(w http.ResponseWriter, r *http.Request) {
	sk, err := s.rt.Skills(r.Context(), sessionOf(r))
	if errors.Is(err, kairo.ErrUnknownRun) {
		fail(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, sk)
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
		runs = []runtime.FlowRun{}
	}
	writeJSON(w, http.StatusOK, runs)
}

// flowRun is a flow's run (?id=), with its chart and steps.
func (s *Server) flowRun(w http.ResponseWriter, r *http.Request) {
	f, err := s.rt.FlowRunByID(r.Context(), r.URL.Query().Get("id"))
	if errors.Is(err, runtime.ErrNoFlowRun) {
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
