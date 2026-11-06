package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// sessionOf is the session a run is in ("s/<id>"), if it is in one.
func sessionOf(run string) string {
	if !strings.HasPrefix(run, "s/") {
		return ""
	}
	parts := strings.SplitN(run, "/", 3)
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

// ownerSession is the session a run is for: the one it is in, or, for a
// run under a notify run (n/<id>/..., ADR 0034), the session it tells of.
func ownerSession(run string) string {
	if id, ok := strings.CutPrefix(run, "n/"); ok {
		id, _, _ = strings.Cut(id, "/")
		if id == "" {
			return ""
		}
		return SessionID(id)
	}
	return sessionOf(run)
}

// inNotify reports whether run is a notify run or under one (ADR 0034).
func inNotify(run string) bool { return strings.HasPrefix(run, "n/") }

// notifyID is the kairo id of the notify run of the request key in session
// (ADR 0034; AGENTS.md, invariant 8): made from them alone, so starting it
// again finds the same run.
func notifyID(session, key string) string {
	h := sha256.Sum256([]byte(key))
	return "n/" + strings.TrimPrefix(session, "s/") + "/" + hex.EncodeToString(h[:])[:24]
}

// turnOf is the turn a run is in, the session's child.
func turnOf(run string) string {
	parts := strings.SplitN(run, "/", 4)
	if len(parts) < 3 {
		return ""
	}
	return strings.Join(parts[:3], "/")
}

// inMCPCall reports whether run is a step of an MCP call's run
// (<session>/<turn>/mcp/...): the MCP call's own run tells of it.
func inMCPCall(run string) bool {
	parts := strings.SplitN(run, "/", 5)
	return len(parts) == 5 && parts[3] == "mcp"
}

// SessionID is the kairo id of session id (AGENTS.md, invariant 8).
func SessionID(id string) string { return "s/" + id }

// claudeSessionID is the Claude Code session of a KairoCode session: a UUID
// made from its id, so it is the same every time (AGENTS.md, invariant 8).
func claudeSessionID(session string) string {
	h := sha256.Sum256([]byte("kairocode/claude-session:" + session))
	h[6] = h[6]&0x0f | 0x40 // version 4
	h[8] = h[8]&0x3f | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// engineSessionName is the name KairoCode gives the session of engine for
// the turns of session in epoch (AGENTS.md, invariant 8).
func engineSessionName(engine, session string, epoch int) string {
	h := sha256.Sum256([]byte("kairocode/" + engine + "-session:" + session + ":" + strconv.Itoa(epoch)))
	return "kairocode-" + hex.EncodeToString(h[:12])
}
