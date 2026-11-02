package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/r-hashi01/kairocode/internal/api"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/r-hashi01/kairocode/internal/jsonrpc"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

const (
	AuthSignedIn = api.AuthSignedIn
	AuthNeeded   = api.AuthNeeded
	AuthUnknown  = api.AuthUnknown
)

type AuthMethod = api.AuthMethod

type AuthInfo = api.AuthInfo

// auths keeps what is known of each provider's sign-in: from what
// happened when it ran, and the ways its agent offers.
type auths struct {
	mu   sync.Mutex
	seen map[string]AuthInfo
	// checked: a CLI's own answer (claude auth status), and when.
	checked map[string]checkedAuth
}

type checkedAuth struct {
	state string
	at    time.Time
}

// authSeen notes what running provider showed of its sign-in (state "":
// leave it), and the ways it offers (nil: leave them).
func (r *Runtime) authSeen(provider, state string, methods []AuthMethod) {
	r.auth.mu.Lock()
	defer r.auth.mu.Unlock()
	if r.auth.seen == nil {
		r.auth.seen = map[string]AuthInfo{}
	}
	a := r.auth.seen[provider]
	if state != "" {
		a.State = state
	}
	if methods != nil {
		a.Methods = methods
	}
	r.auth.seen[provider] = a
}

// Auth is provider id's sign-in as far as it is known: a CLI's own word
// for Claude Code and Codex (asked at most once a minute), what running it
// showed, else unknown.
func (r *Runtime) Auth(ctx context.Context, id string) AuthInfo {
	engine, _ := engineOf(id)
	r.auth.mu.Lock()
	a := r.auth.seen[providerOf(engine)]
	r.auth.mu.Unlock()
	if a.State == "" {
		a.State = AuthUnknown
	}
	switch {
	case engine == workflows.EngineAPI:
		if a.State == AuthUnknown && anthropicEnv() {
			a.State = AuthSignedIn
		}
		if a.Methods == nil {
			a.Methods = []AuthMethod{{ID: "api-key", Name: "API key",
				Description: "Set ANTHROPIC_API_KEY where the daemon starts (kairocode daemon stop, then start it again)"}}
			if _, err := exec.LookPath("ant"); err == nil {
				a.Methods = append(a.Methods, AuthMethod{ID: "ant", Name: "ant auth login", Command: []string{"ant", "auth", "login"}})
			}
		}
	case engine == workflows.EngineClaudeCode:
		if s := r.cliAuth(ctx, workflows.EngineClaudeCode); s != "" {
			a.State = s
		}
		a.Methods = []AuthMethod{{ID: "claude", Name: "claude auth login", Command: []string{r.cfg.ClaudePath, "auth", "login"}}}
	case registryAgent(engine) == codexAgent:
		if s := r.cliAuth(ctx, workflows.EngineCodex); s != "" && a.State == AuthUnknown {
			a.State = s
		}
	}
	return a
}

// anthropicEnv reports whether the environment names Anthropic credentials.
func anthropicEnv() bool {
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_PROFILE", "ANTHROPIC_FEDERATION_RULE_ID"} {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}

// cliAuth is a CLI's own word on its sign-in, kept for a minute ("" if it
// cannot say).
func (r *Runtime) cliAuth(ctx context.Context, engine string) string {
	r.auth.mu.Lock()
	c, ok := r.auth.checked[engine]
	r.auth.mu.Unlock()
	if ok && time.Since(c.at) < time.Minute {
		return c.state
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	state := r.cliSignedIn(ctx, engine)
	r.auth.mu.Lock()
	if r.auth.checked == nil {
		r.auth.checked = map[string]checkedAuth{}
	}
	r.auth.checked[engine] = checkedAuth{state: state, at: time.Now()}
	r.auth.mu.Unlock()
	return state
}

// errAuthRequired: the agent wants the user to sign in first.
var errAuthRequired = errors.New("sign-in needed")

// authRequired reports whether err is an agent's "sign in first": ACP's
// auth_required (-32000, which agents also use for other failures, so its
// words are looked at too).
func authRequired(err error) bool {
	if err == nil {
		return false
	}
	e := strings.ToLower(err.Error())
	var re *jsonrpc.Error
	if errors.As(err, &re) {
		e = strings.ToLower(re.Message)
	}
	for _, s := range []string{"auth_required", "authentication required", "auth required", "not authenticated", "not logged in", "please log in", "login required", "sign in"} {
		if strings.Contains(e, s) {
			return true
		}
	}
	return false
}

// acpAuthMethods are the ways an agent's initialize offers to sign in.
func acpAuthMethods(raw json.RawMessage) []AuthMethod {
	var ms []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Type        string `json:"type"`
		Meta        struct {
			Terminal *struct {
				Command string   `json:"command"`
				Args    []string `json:"args"`
			} `json:"terminal-auth"`
		} `json:"_meta"`
	}
	if json.Unmarshal(raw, &ms) != nil {
		return nil
	}
	out := []AuthMethod{}
	for _, m := range ms {
		a := AuthMethod{ID: m.ID, Name: m.Name, Description: m.Description}
		switch {
		case m.Meta.Terminal != nil:
			a.Command = append([]string{m.Meta.Terminal.Command}, m.Meta.Terminal.Args...)
		case m.Type == "terminal":
			continue // runs the agent itself in the terminal: not offered here
		default:
			a.Agent = true
		}
		out = append(out, a)
	}
	return out
}

// LoginMethods are the ways to sign in to provider id. For an agent not
// run yet, it is started to ask.
func (r *Runtime) LoginMethods(ctx context.Context, id string) ([]AuthMethod, error) {
	engine, ok := engineOf(id)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoProvider, id)
	}
	if a := r.Auth(ctx, id); len(a.Methods) > 0 || registryAgent(engine) == "" {
		return a.Methods, nil
	}
	var methods []AuthMethod
	err := r.withAgent(ctx, engine, func(ctx context.Context, conn *jsonrpc.Conn, init json.RawMessage) error {
		methods = acpAuthMethods(init)
		return nil
	})
	if err != nil {
		return nil, err
	}
	r.authSeen(providerOf(engine), "", methods)
	return methods, nil
}

// Login signs in to provider id with an agent's own method (ACP's
// authenticate; it may open a browser and wait). A terminal method is the
// client's to run.
func (r *Runtime) Login(ctx context.Context, id, method string) error {
	engine, ok := engineOf(id)
	if !ok || registryAgent(engine) == "" {
		return fmt.Errorf("%w: %s has no sign-in through its agent", ErrUnavailable, id)
	}
	err := r.withAgent(ctx, engine, func(ctx context.Context, conn *jsonrpc.Conn, _ json.RawMessage) error {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		return conn.Call(ctx, "authenticate", map[string]any{"methodId": method}, nil)
	})
	if err != nil {
		return err
	}
	r.authSeen(providerOf(engine), AuthSignedIn, nil)
	return nil
}

// LoggedIn notes that the user signed in to provider id by a command of
// their own (a terminal method): it is known again when it next runs.
func (r *Runtime) LoggedIn(id string) {
	engine, _ := engineOf(id)
	r.authSeen(providerOf(engine), AuthUnknown, nil)
	r.auth.mu.Lock()
	delete(r.auth.checked, engine)
	delete(r.auth.checked, workflows.EngineCodex)
	r.auth.mu.Unlock()
}
