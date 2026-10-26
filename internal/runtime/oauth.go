package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// A plugin's OAuth (ADR 0025, 3): kairocode plugin connect runs MCP's
// authorization (the authorization code flow with PKCE, the client
// registered dynamically, or the manifest's), the browser coming back to
// a port of 127.0.0.1; what it gets is kept in the key store as one
// value. The daemon puts its access token in each request, refreshes it
// before it runs out and keeps the new one; a refresh refused makes the
// plugin's authentication expired.

// oauthKey is the name a plugin's OAuth grant is kept under.
const oauthKey = "oauth"

// oauthGrant is what a plugin's OAuth connection keeps.
type oauthGrant struct {
	ClientID     string           `json:"client_id"`
	ClientSecret string           `json:"client_secret,omitempty"`
	AuthStyle    oauth2.AuthStyle `json:"auth_style,omitempty"`
	TokenURL     string           `json:"token_url"`
	Scopes       []string         `json:"scopes,omitempty"`
	Token        *oauth2.Token    `json:"token"`
}

// PluginOAuth connects dir's plugin name by OAuth: open shows the user the
// authorization page (a browser), which comes back here. The grant is
// kept in the key store.
func PluginOAuth(ctx context.Context, dir, name string, open func(url string) error) error {
	servers, _ := pluginServers(dir)
	var s *extServer
	for i := range servers {
		if servers[i].Plugin == name {
			s = &servers[i]
			break
		}
	}
	if s == nil || s.URL == "" || s.Auth != "oauth" {
		return fmt.Errorf("%s is not a plugin of a remote MCP server that signs in with OAuth", name)
	}
	endpoint, err := value(*s, s.URL)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	// The browser comes back here.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)
	// What comes back: only the answer of the state asked with is taken
	// (another local caller cannot stop the sign-in).
	var want atomic.Value // the state asked with
	want.Store("")
	back := make(chan url.Values, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if st := want.Load().(string); st == "" || q.Get("state") != st {
			http.Error(w, "KairoCode: this is not the answer of the sign-in under way.", http.StatusBadRequest)
			return
		}
		select {
		case back <- q:
		default: // answered already
		}
		fmt.Fprintln(w, "KairoCode: received. Go back to the terminal; you can close this tab.")
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	cfg := &auth.AuthorizationCodeHandlerConfig{
		RedirectURL:         redirect,
		RequestRefreshToken: true,
		AuthorizationCodeFetcher: func(ctx context.Context, a *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			u, err := url.Parse(a.URL)
			if err != nil {
				return nil, err
			}
			// Only a web page is opened: the server says where, and a
			// project's server is the project's to say.
			if !(u.Scheme == "https" || u.Scheme == "http" && isLoopback(u.Hostname())) {
				return nil, fmt.Errorf("the authorization page is not a web page of https: %s", u.Scheme)
			}
			want.Store(u.Query().Get("state"))
			if err := open(a.URL); err != nil {
				return nil, err
			}
			select {
			case q := <-back:
				if e := q.Get("error"); e != "" {
					return nil, fmt.Errorf("the authorization was not given: %q %q", e, q.Get("error_description"))
				}
				return &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}
	var grant oauthGrant
	cfg.NewTokenSource = func(ctx context.Context, c *oauth2.Config, t *oauth2.Token) (oauth2.TokenSource, error) {
		grant = oauthGrant{ClientID: c.ClientID, ClientSecret: c.ClientSecret, AuthStyle: c.Endpoint.AuthStyle,
			TokenURL: c.Endpoint.TokenURL, Scopes: c.Scopes, Token: t}
		return c.TokenSource(ctx, t), nil
	}
	if s.ClientID != "" {
		cfg.PreregisteredClient = &oauthex.ClientCredentials{ClientID: s.ClientID}
	} else {
		cfg.DynamicClientRegistrationConfig = &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			RedirectURIs: []string{redirect}, TokenEndpointAuthMethod: "none", ClientName: "KairoCode",
			GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"}}}
	}
	h, err := auth.NewAuthorizationCodeHandler(cfg)
	if err != nil {
		return err
	}
	// Reaching the server asks for the authorization, and gets the tokens.
	t := &mcp.StreamableClientTransport{Endpoint: endpoint, OAuthHandler: h,
		HTTPClient: serverClient(headers{s: *s, connecting: true})}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "kairocode", Version: "0"}, nil).Connect(ctx, t, nil)
	if err != nil {
		return fmt.Errorf("connecting %s: %s", name, serverErr(*s, err))
	}
	cs.Close()
	if grant.Token == nil {
		return fmt.Errorf("%s's server did not ask for an authorization: it needs none", name)
	}
	b, err := json.Marshal(grant)
	if err != nil {
		return err
	}
	return Secrets().Set(pluginSecretKey(s.PluginDir, s.Project, s.Plugin, oauthKey), string(b))
}

// errNotSent: a request to a plugin's server was not sent (its token
// could not be refreshed, the token endpoint not reached): nothing ran.
var errNotSent = errors.New("not sent")

// refreshClient reaches a token endpoint: never for long, as a server's
// session waits for it.
var refreshClient = &http.Client{Timeout: 30 * time.Second}

// pluginTokens are a plugin's OAuth access tokens: one per grant kept
// (extServers.tokensOf), as a refresh token used once may be good no more.
type pluginTokens struct {
	key   string
	name  string // the plugin's
	mu    sync.Mutex
	grant oauthGrant
	// refused is told when a refresh is refused; logf when the new tokens
	// could not be kept.
	refused func()
	logf    func(format string, args ...any)
}

// readGrant reads the grant kept under key.
func readGrant(key string) (oauthGrant, bool, error) {
	v, ok, err := Secrets().Get(key)
	if err != nil || !ok {
		return oauthGrant{}, false, err
	}
	var g oauthGrant
	if json.Unmarshal([]byte(v), &g) != nil || g.Token == nil {
		return oauthGrant{}, false, nil
	}
	return g, true, nil
}

// token is the access token, refreshed if it runs out soon. A refresh the
// authorization server refuses is errRefused; one that does not reach it,
// errNotSent: either way the request is not sent.
func (p *pluginTokens) token(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.grant.Token.Valid() {
		return p.grant.Token.AccessToken, nil
	}
	// Another holder (another daemon) may have refreshed it: the kept one
	// is newer then, and the refresh token here used up.
	kept, ok, err := readGrant(p.key)
	if err != nil {
		return "", fmt.Errorf("%w: the key store could not be read", errNotSent)
	}
	if !ok {
		return "", fmt.Errorf("%w: kairocode plugin connect", errNotConnected) // disconnected
	}
	p.grant = kept
	if p.grant.Token.Valid() {
		return p.grant.Token.AccessToken, nil
	}
	if p.grant.Token.RefreshToken == "" {
		p.refused()
		return "", fmt.Errorf("%w: the token ran out, and there is no refresh token", errRefused)
	}
	c := &oauth2.Config{ClientID: p.grant.ClientID, ClientSecret: p.grant.ClientSecret, Scopes: p.grant.Scopes,
		Endpoint: oauth2.Endpoint{TokenURL: p.grant.TokenURL, AuthStyle: p.grant.AuthStyle}}
	rctx := context.WithValue(ctx, oauth2.HTTPClient, refreshClient)
	used := p.grant.Token.RefreshToken
	t, err := c.TokenSource(rctx, &oauth2.Token{RefreshToken: used}).Token()
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && (re.ErrorCode == "invalid_grant" || re.ErrorCode == "invalid_client" || re.ErrorCode == "unauthorized_client") {
			p.refused()
			return "", fmt.Errorf("%w: the authorization server refused to refresh the token (%s)", errRefused, re.ErrorCode)
		}
		return "", fmt.Errorf("%w: the token could not be refreshed: the token endpoint did not answer as it should", errNotSent)
	}
	p.grant.Token = t
	// Kept only over the grant it was refreshed from: not over one
	// connected since, nor after a disconnect.
	if now, ok, err := readGrant(p.key); err == nil && ok && now.Token.RefreshToken == used {
		if b, err := json.Marshal(p.grant); err == nil {
			if err := Secrets().Set(p.key, string(b)); err != nil {
				p.logf("plugin tokens: the refreshed tokens could not be kept: %s", err.Error())
			}
		}
	}
	return t.AccessToken, nil
}

// isLoopback says host is this machine.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
