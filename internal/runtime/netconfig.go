package runtime

import (
	"fmt"
	"strings"

	"github.com/r-hashi01/kairocode/internal/config"
	"github.com/r-hashi01/kairocode/internal/tools"
)

// userNet is the user's settings for fetch and http (ADR 0013), read when
// the tool runs:
//
//	{"http": {"allowPrivate": false, "auth": {"api.github.com": {"header": "Authorization", "env": "GITHUB_TOKEN", "prefix": "Bearer "}}}}
//
// Only the user's kairocode.json: a project's cannot have the user's
// credentials sent to a host it names, nor open private addresses.
func userNet() (tools.Net, error) {
	l, ok, err := config.User()
	if err != nil || !ok {
		return tools.Net{}, err
	}
	var cfg struct {
		HTTP struct {
			AllowPrivate bool                      `json:"allowPrivate"`
			Auth         map[string]tools.HostAuth `json:"auth"`
		} `json:"http"`
	}
	if err := l.Decode(&cfg); err != nil {
		return tools.Net{}, err
	}
	n := tools.Net{AllowPrivate: cfg.HTTP.AllowPrivate, Auth: map[string]tools.HostAuth{}}
	for host, a := range cfg.HTTP.Auth {
		if a.Header == "" || a.Env == "" {
			return tools.Net{}, fmt.Errorf("%s: http.auth.%s: header and env are needed", l.Path, host)
		}
		n.Auth[strings.ToLower(host)] = a
	}
	return n, nil
}
