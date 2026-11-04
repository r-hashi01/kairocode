package api

import ()

// Sign-in states of a provider (ADR 0015, addendum).
const (
	AuthSignedIn = "signed-in"
	AuthNeeded   = "needed"
	AuthUnknown  = "unknown"
)

// AuthMethod is a way to sign in to a provider: a command the user runs in
// the terminal (Command), or one the agent carries out itself through
// ACP's authenticate (Agent).
type AuthMethod struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Command     []string `json:"command,omitempty"`
	Agent       bool     `json:"agent,omitempty"`
}

// AuthInfo is a provider's sign-in: its state and the ways to sign in.
type AuthInfo struct {
	State   string       `json:"state"`
	Methods []AuthMethod `json:"methods,omitempty"`
}

// ProviderInfo is a provider a session can switch to (ADR 0014, 0015): who
// runs the model loop, whether it can here, and, for an agent of ACP's
// registry not installed yet, what installing it fetches.
type ProviderInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"` // "api", "cli" or "acp"
	Description string `json:"description,omitempty"`
	// Available: it can run here now. Else Installable says the user can
	// install it (Version from Source); else Reason says why not.
	Available   bool   `json:"available"`
	Installable bool   `json:"installable,omitempty"`
	Version     string `json:"version,omitempty"`
	Source      string `json:"source,omitempty"`
	Reason      string `json:"reason,omitempty"`
	// Auth is its sign-in, for one that runs here.
	Auth *AuthInfo `json:"auth,omitempty"`
}
