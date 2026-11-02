package runtime

import (
	"errors"
	"maps"
	"net/url"
	"slices"
	"strings"
)

// shownURL is a URL as it may be shown and recorded: without what may be
// a secret in it (credentials, the query).
func shownURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(a URL that cannot be read)"
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}

// safeErr is an error's text without the URL a request error carries.
func safeErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Op + ": " + ue.Err.Error()
	}
	return err.Error()
}

// serverErr is safeErr for server s's error: a plugin's credentials, which
// an error's text may carry (a URL with one in it), are put out of it
// (invariant 6).
func serverErr(s extServer, err error) string {
	return redact(s, safeErr(err))
}

// masked is what is shown of a credential: its first and last four
// characters, to tell which one it was; nothing of a short one, which they
// would be most of.
func masked(v string) string {
	if r := []rune(v); len(r) >= 20 {
		return string(r[:4]) + "…" + string(r[len(r)-4:])
	}
	return "***"
}

// redact is text with server s's credentials put out of it.
func redact(s extServer, text string) string {
	if s.Plugin == "" {
		return text
	}
	vals := []string{s.URL}
	for _, k := range slices.Sorted(maps.Keys(s.Headers)) {
		vals = append(vals, s.Headers[k])
	}
	for _, k := range slices.Sorted(maps.Keys(s.Env)) {
		vals = append(vals, s.Env[k])
	}
	store := Secrets()
	for _, v := range vals {
		for _, m := range secretRef.FindAllStringSubmatch(v, -1) {
			if sec, ok, _ := store.Get(pluginSecretKey(s.PluginDir, s.Project, s.Plugin, m[1])); ok && sec != "" {
				for _, form := range []string{sec, url.PathEscape(sec), url.QueryEscape(sec)} {
					text = strings.ReplaceAll(text, form, masked(sec))
				}
			}
		}
	}
	return text
}
