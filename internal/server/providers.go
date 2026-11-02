package server

import (
	"encoding/json"
	"errors"
	"net/http"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/agents"
	"github.com/r-hashi01/kairocode/internal/api"
)

// getTurn waits for the turn to finish (the request's context bounds the
// wait) and returns its reply.
// providers are the providers a session can switch to (ADR 0014).
func (s *Server) providers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.rt.Providers(r.Context()))
}

// loginMethods are the ways to sign in to a provider.
func (s *Server) loginMethods(w http.ResponseWriter, r *http.Request) {
	ms, err := s.rt.LoginMethods(r.Context(), r.PathValue("id"))
	if errors.Is(err, api.ErrNoProvider) {
		fail(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if ms == nil {
		ms = []api.AuthMethod{}
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
	if errors.Is(err, api.ErrNoProvider) {
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
	if errors.Is(err, api.ErrNoProvider) {
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
	if errors.Is(err, api.ErrNoProvider) {
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
	var st api.Settings
	var err error
	if in.Mode != "" {
		st, err = s.rt.SetMode(r.Context(), sessionOf(r), in.Mode)
	}
	if err == nil && (in.Provider != "" || in.Model != "" || in.Effort != "") {
		st, err = s.rt.Switch(r.Context(), sessionOf(r), in.Provider, in.Model, in.Effort)
	}
	switch {
	case errors.Is(err, kairo.ErrUnknownRun), errors.Is(err, api.ErrNoProvider):
		fail(w, http.StatusNotFound, err)
		return
	case errors.Is(err, api.ErrUnavailable), errors.Is(err, api.ErrNotInstalled):
		fail(w, http.StatusConflict, err)
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}
