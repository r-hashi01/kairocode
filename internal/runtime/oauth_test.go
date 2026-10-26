package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// oauthServer is an MCP server behind OAuth, its own authorization server:
// clients register themselves, codes are given at once, access tokens
// last a second (so each use refreshes), refreshes are refused once
// refuse is set.
type oauthServer struct {
	*httptest.Server
	refuse atomic.Bool
	down   atomic.Bool // the token endpoint answers 503
	mu     sync.Mutex
	valid  map[string]bool
	n      int
}

func newOAuthServer(t *testing.T) *oauthServer {
	t.Helper()
	o := &oauthServer{valid: map[string]bool{}}
	s := mcp.NewServer(&mcp.Implementation{Name: "calendar", Version: "0"}, nil)
	s.AddTool(&mcp.Tool{Name: "events", Description: "events", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "a meeting at noon"}}}, nil
		})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	mux := http.NewServeMux()
	o.Server = httptest.NewServer(mux)
	t.Cleanup(o.Close)
	base := o.URL
	writeJSON := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		ok := o.valid[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		o.mu.Unlock()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/.well-known/oauth-protected-resource"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"resource": base + "/mcp", "authorization_servers": []string{base}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"issuer": base, "authorization_endpoint": base + "/authorize",
			"token_endpoint": base + "/token", "registration_endpoint": base + "/register",
			"response_types_supported": []string{"code"}, "code_challenge_methods_supported": []string{"S256"},
			"grant_types_supported": []string{"authorization_code", "refresh_token"}, "token_endpoint_auth_methods_supported": []string{"none"}})
	})
	mux.HandleFunc("POST /register", func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		m["client_id"] = "kc-client"
		writeJSON(w, 201, m)
	})
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("code_challenge") == "" {
			http.Error(w, "no PKCE", 400)
			return
		}
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=kc-code&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if o.down.Load() {
			writeJSON(w, 503, map[string]string{"error": "temporarily_unavailable"})
			return
		}
		switch {
		case r.Form.Get("grant_type") == "authorization_code" && r.Form.Get("code") == "kc-code" && r.Form.Get("code_verifier") != "":
		case r.Form.Get("grant_type") == "refresh_token" && !o.refuse.Load() && strings.HasPrefix(r.Form.Get("refresh_token"), "kc-refresh-"):
		default:
			writeJSON(w, 400, map[string]string{"error": "invalid_grant"})
			return
		}
		o.mu.Lock()
		o.n++
		at := fmt.Sprintf("kc-access-%d", o.n)
		o.valid[at] = true
		o.mu.Unlock()
		writeJSON(w, 200, map[string]any{"access_token": at, "token_type": "Bearer", "refresh_token": fmt.Sprintf("kc-refresh-%d", o.n), "expires_in": 1})
	})
	return o
}

// A plugin signs in by OAuth: the authorization code flow, with its client
// registered dynamically, gets tokens kept in the key store; the daemon
// refreshes them as they run out, keeping the new ones; a refresh refused
// makes it expired. No token is in the records (ADR 0025, invariant 6).
func TestPluginOAuth(t *testing.T) {
	t.Setenv("KAIROCODE_SECRETS", "file")
	userSettings(t, jsonOf(t, map[string]any{"permission": map[string]string{"cal_*": "allow"}}))
	o := newOAuthServer(t)
	writePlugin(t, filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "kairocode", "plugins"), "cal", map[string]any{
		"name":  "cal",
		"mcp":   map[string]any{"type": "remote", "url": o.URL + "/mcp"},
		"auth":  map[string]any{"type": "oauth"},
		"tools": map[string]any{"events": map[string]string{"effect": "read"}},
	})
	db := filepath.Join(t.TempDir(), "k.db")
	r := open(t, db, &provider.Fake{HoldAt: -1, Script: []provider.FakeReply{
		{Tools: []provider.FakeTool{{Name: "cal_events", Input: json.RawMessage(`{}`)}}},
		{Text: "done"},
	}})
	ctx := context.Background()
	ws := repo(t)
	list, _ := r.PluginsIn(ctx, ws)
	if p := pluginOfList(t, list, "cal"); !strings.HasPrefix(p.State, "not connected") || p.Auth != "oauth" {
		t.Fatalf("before connecting: %+v", p)
	}

	browser := func(u string) error { // the user says yes
		resp, err := http.Get(u)
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}
	if err := PluginOAuth(ctx, ws, "cal", browser); err != nil {
		t.Fatal(err)
	}
	kept := func() oauthGrant {
		v, _, _ := Secrets().Get("cal/oauth")
		var g oauthGrant
		json.Unmarshal([]byte(v), &g)
		return g
	}
	if g := kept(); g.ClientID != "kc-client" || g.Token == nil || g.Token.RefreshToken == "" {
		t.Fatalf("kept: %+v", g)
	}
	r.PluginConnected("cal")

	s, _ := r.CreateSession(ctx, "oauth", workflows.SessionInput{Dir: ws})
	turn(t, r, s, "what is on today")
	if conv := strings.Join(conversationOf(t, r, s, true), "\n"); !strings.Contains(conv, "a meeting at noon") {
		t.Fatalf("no result in\n%s", conv)
	}
	g := kept()
	if g.Token.AccessToken == "kc-access-1" {
		t.Fatal("the refreshed token was not kept")
	}

	// The token endpoint down for a while: not reached, not expired.
	o.down.Store(true)
	r.PluginConnected("cal")
	list, _ = r.PluginsIn(ctx, ws)
	if p := pluginOfList(t, list, "cal"); strings.HasPrefix(p.State, "authentication expired") || p.State == "connected" {
		t.Fatalf("with the token endpoint down: %+v", p)
	}
	o.down.Store(false)

	o.refuse.Store(true)
	r.PluginConnected("cal")
	list, _ = r.PluginsIn(ctx, ws)
	if p := pluginOfList(t, list, "cal"); !strings.HasPrefix(p.State, "authentication expired") {
		t.Fatalf("with the refresh refused: %+v", p)
	}
	r.Close()
	for _, f := range []string{db, db + "-wal"} {
		if b, err := os.ReadFile(f); err == nil && (bytes.Contains(b, []byte("kc-access-")) || bytes.Contains(b, []byte("kc-refresh-"))) {
			t.Fatalf("%s holds a token", f)
		}
	}
}
