package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/runtime"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

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
// messages' roles and parts (not their providers' encodings); with ?live=1,
// then what the turns under way were asked (Runtime.HistoryLive).
func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	history := s.rt.History
	if r.URL.Query().Get("live") != "" { // with what the turns under way were asked
		history = s.rt.HistoryLive
	}
	ms, err := history(r.Context(), sessionOf(r))
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
