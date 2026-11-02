package server

import (
	"encoding/json"
	"errors"
	"net/http"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/runtime"
)

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
