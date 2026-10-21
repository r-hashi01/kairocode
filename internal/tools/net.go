package tools

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// What fetch and http share (ADR 0013): requests leave from KairoCode's
// process, never to a private address unless the user allows it, with the
// user's credentials for a host added here and kept out of every record.

// Net is the user's settings for fetch and http ("http" in their
// kairocode.json; a project's are not read).
type Net struct {
	// AllowPrivate lets a request reach loopback, private and link-local
	// addresses.
	AllowPrivate bool
	// Auth is a header to send to a host (by its name, lower case), its
	// value read from an environment variable when the request is made.
	Auth map[string]HostAuth

	rootCAs *x509.CertPool // tests: their TLS server's
}

// HostAuth: Header is Prefix and the value of the environment variable Env.
type HostAuth struct {
	Header string `json:"header"`
	Env    string `json:"env"`
	Prefix string `json:"prefix,omitempty"`
}

const (
	netTimeout = 30 * time.Second
	netBodyMax = 5 << 20 // read of a response
	netShowMax = 100_000 // shown to the model
	netHops    = 5       // redirects fetch follows
)

var errPrivate = errors.New("the address is private (loopback, private or link-local): not reached unless the user allows it (\"http\": {\"allowPrivate\": true})")

// notPublic are ranges beside those net.IP names that do not lead to the
// public internet, or lead back to a private address: "this network",
// shared (carrier-grade NAT, Tailscale, a cloud's metadata), benchmarking,
// reserved, and IPv6 forms that carry an IPv4 address (IPv4-compatible,
// NAT64, 6to4).
var notPublic = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4",
		"::/96", "64:ff9b::/96", "64:ff9b:1::/48", "2002::/16"} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// privateIP: what a request may not reach unless allowed.
func privateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	a = a.Unmap()
	for _, p := range notPublic {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// client makes requests as n says. The address is checked when it is
// dialled, after the name is resolved, so a name cannot lead to a private
// address. No proxy is used (a proxy would be dialled instead).
func (n Net) client(redirect func(*http.Request, []*http.Request) error) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); ip != nil && !n.AllowPrivate && privateIP(ip) {
			return errPrivate
		}
		return nil
	}}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:             nil,
			DialContext:       dialer.DialContext,
			TLSClientConfig:   &tls.Config{RootCAs: n.rootCAs},
			DisableKeepAlives: true, // a request is never sent again on a reused connection
		},
		CheckRedirect: redirect,
	}
}

// request makes the request for u: http or https only, with the model's
// headers and the user's for its host. It returns the secret values added,
// to keep out of what is shown.
func (n Net) request(ctx context.Context, method, u string, headers map[string]string, body io.Reader) (*http.Request, []string, error) {
	parsed, err := url.Parse(u)
	if err != nil {
		return nil, nil, err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, nil, fmt.Errorf("only http and https URLs are fetched, not %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, nil, errors.New("the URL has no host")
	}
	// A rule for a path (api.github.com/repos/*) is not left by "..".
	for _, seg := range strings.Split(parsed.Path, "/") {
		if seg == ".." || seg == "." {
			return nil, nil, errors.New(`the URL's path has a "." or ".." segment: give it without one`)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, parsed.String(), body)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	var secrets []string
	if a, ok := n.Auth[strings.ToLower(parsed.Hostname())]; ok {
		if parsed.Scheme != "https" {
			return nil, nil, fmt.Errorf("the user's credentials for %s are sent over https only", parsed.Hostname())
		}
		v := os.Getenv(a.Env)
		if v == "" {
			return nil, nil, fmt.Errorf("the user's credentials for %s are not set (the environment variable %s is empty)", parsed.Hostname(), a.Env)
		}
		req.Header.Set(a.Header, a.Prefix+v)
		secrets = append(secrets, v)
	}
	return req, secrets, nil
}

// redact keeps secrets out of s.
func redact(s string, secrets []string) string {
	for _, v := range secrets {
		if len(v) >= 4 {
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
	}
	return s
}

// readBody reads at most netBodyMax of a response, saying if there was more.
func readBody(r io.Reader) ([]byte, bool, error) {
	b, err := io.ReadAll(io.LimitReader(r, netBodyMax+1))
	if len(b) > netBodyMax {
		return b[:netBodyMax], true, err
	}
	return b, false, err
}

// textual says whether a content type is text to show.
func textual(contentType string) bool {
	if contentType == "" {
		return true
	}
	t, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return strings.HasPrefix(t, "text/") || strings.HasSuffix(t, "json") || strings.HasSuffix(t, "xml") ||
		t == "application/javascript" || t == "application/x-www-form-urlencoded" || strings.HasSuffix(t, "+json") || strings.HasSuffix(t, "+xml")
}

// shown cuts s to what is shown, saying so.
func shown(s string, cut bool) string {
	if len(s) > netShowMax {
		s, cut = s[:netShowMax], true
	}
	if cut {
		s += "\n[cut: the rest is not shown]"
	}
	return s
}

var (
	reDrop    = regexp.MustCompile(`(?is)<!--.*?-->|<(script|style|noscript|template|svg|head)\b.*?</(script|style|noscript|template|svg|head)\s*>`)
	reHeading = regexp.MustCompile(`(?is)<h([1-6])\b[^>]*>(.*?)</h[1-6]\s*>`)
	reLink    = regexp.MustCompile(`(?is)<a\b[^>]*?href\s*=\s*["']([^"']*)["'][^>]*>(.*?)</a\s*>`)
	reItem    = regexp.MustCompile(`(?i)<li\b[^>]*>`)
	reBreak   = regexp.MustCompile(`(?i)<(br|p|div|tr|section|article|header|footer|ul|ol|table|blockquote|pre)\b[^>]*>|</(p|div|tr|section|article|header|footer|ul|ol|table|blockquote|pre|li)\s*>`)
	reTag     = regexp.MustCompile(`(?s)<[^>]*>`)
	reSpaces  = regexp.MustCompile(`[ \t\r\f\v]+`)
	reBlank   = regexp.MustCompile(`\n{3,}`)
)

// htmlText is an HTML page as text, or as Markdown: its headings, links and
// list items marked.
func htmlText(page string, markdown bool) string {
	s := reDrop.ReplaceAllString(page, "")
	s = reHeading.ReplaceAllStringFunc(s, func(m string) string {
		g := reHeading.FindStringSubmatch(m)
		if markdown {
			return "\n\n" + strings.Repeat("#", int(g[1][0]-'0')) + " " + g[2] + "\n\n"
		}
		return "\n\n" + g[2] + "\n\n"
	})
	s = reLink.ReplaceAllStringFunc(s, func(m string) string {
		g := reLink.FindStringSubmatch(m)
		if markdown && g[1] != "" && !strings.HasPrefix(g[1], "#") && !strings.HasPrefix(strings.ToLower(g[1]), "javascript:") {
			return "[" + g[2] + "](" + g[1] + ")"
		}
		return g[2]
	})
	if markdown {
		s = reItem.ReplaceAllString(s, "\n- ")
	} else {
		s = reItem.ReplaceAllString(s, "\n")
	}
	s = reBreak.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(reSpaces.ReplaceAllString(l, " "))
	}
	s = reBlank.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return strings.TrimSpace(s)
}
