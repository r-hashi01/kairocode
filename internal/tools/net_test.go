package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func netRun(t *testing.T, d Def, n Net, in any) Result {
	t.Helper()
	b, _ := json.Marshal(in)
	return d.Run(context.Background(), Env{Net: n}, b)
}

const testPage = `<html><head><title>T</title><style>p{}</style></head><body>
<h1>Title</h1><script>alert(1)</script><p>One &amp; two</p>
<ul><li>first</li><li><a href="/x">second</a></li></ul></body></html>`

// fetch returns a page as Markdown, text or HTML; other content is named,
// not shown; large content is cut.
func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, testPage)
		case "/big":
			w.Header().Set("Content-Type", "text/plain")
			io.WriteString(w, strings.Repeat("x", netShowMax+10))
		case "/image":
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte{0x89, 'P', 'N', 'G'})
		case "/accept":
			io.WriteString(w, "accept="+r.Header.Get("Accept"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	n := Net{AllowPrivate: true}
	md := netRun(t, FetchTool, n, map[string]any{"url": srv.URL + "/page"})
	for _, want := range []string{"# Title", "One & two", "- first", "- [second](/x)"} {
		if !strings.Contains(md.Output, want) || md.IsError {
			t.Errorf("markdown: no %q in\n%s", want, md.Output)
		}
	}
	if strings.Contains(md.Output, "alert") || strings.Contains(md.Output, "p{}") || strings.Contains(md.Output, "<") {
		t.Errorf("markdown kept a script, style or tag:\n%s", md.Output)
	}
	if text := netRun(t, FetchTool, n, map[string]any{"url": srv.URL + "/page", "format": "text"}).Output; strings.Contains(text, "#") || !strings.Contains(text, "second") {
		t.Errorf("text:\n%s", text)
	}
	if raw := netRun(t, FetchTool, n, map[string]any{"url": srv.URL + "/page", "format": "html"}).Output; !strings.Contains(raw, "<h1>Title</h1>") {
		t.Errorf("html:\n%s", raw)
	}
	if big := netRun(t, FetchTool, n, map[string]any{"url": srv.URL + "/big"}).Output; !strings.HasSuffix(big, "[cut: the rest is not shown]") || len(big) > netShowMax+200 {
		t.Errorf("big: %d bytes, ends %q", len(big), big[len(big)-40:])
	}
	if img := netRun(t, FetchTool, n, map[string]any{"url": srv.URL + "/image"}).Output; !strings.Contains(img, "image/png content, 4 bytes: not shown") {
		t.Errorf("image: %s", img)
	}
	if a := netRun(t, FetchTool, n, map[string]any{"url": srv.URL + "/accept", "headers": map[string]string{"Accept": "text/plain"}}).Output; !strings.Contains(a, "accept=text/plain") {
		t.Errorf("headers: %s", a)
	}
	if nf := netRun(t, FetchTool, n, map[string]any{"url": srv.URL + "/none"}); !nf.IsError || !strings.Contains(nf.Output, "HTTP 404") {
		t.Errorf("404: %+v", nf)
	}
	if bad := netRun(t, FetchTool, n, map[string]any{"url": "file:///etc/passwd"}); !bad.IsError || !strings.Contains(bad.Output, "only http and https") {
		t.Errorf("file URL: %+v", bad)
	}
}

// Private addresses are not reached unless the user allows it, for both
// tools; the POST is not sent.
func TestNetPrivateRefused(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	for _, d := range []Def{FetchTool, HTTPTool} {
		for _, u := range []string{srv.URL, strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)} {
			r := netRun(t, d, Net{}, map[string]any{"url": u})
			if !r.IsError || r.Unknown || !strings.Contains(r.Output, "private") {
				t.Errorf("%s %s: %+v", d.Name, u, r)
			}
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("the server was reached %d times", hits.Load())
	}
	for _, s := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1", "0.0.0.0", "0.1.2.3", "10.0.0.1", "172.16.0.1",
		"192.168.1.1", "169.254.169.254", "100.100.100.200", "100.64.0.1", "198.18.0.1", "240.0.0.1", "fe80::1", "fc00::1",
		"::7f00:1", "64:ff9b::7f00:1", "2002:7f00:1::1", "ff02::1"} {
		if !privateIP(net.ParseIP(s)) {
			t.Errorf("%s is taken for public", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "140.82.112.3", "2606:4700:4700::1111"} {
		if privateIP(net.ParseIP(s)) {
			t.Errorf("%s is taken for private", s)
		}
	}
}

// A "." or ".." in a URL's path is refused: a rule for a path is not left
// through one.
func TestNetDotSegments(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	for _, p := range []string{"/repos/../admin", "/repos/%2e%2e/admin", "/./x"} {
		for _, d := range []Def{FetchTool, HTTPTool} {
			if r := netRun(t, d, Net{AllowPrivate: true}, map[string]any{"url": srv.URL + p}); !r.IsError || !strings.Contains(r.Output, `".."`) {
				t.Errorf("%s %s: %+v", d.Name, p, r)
			}
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("sent %d", hits.Load())
	}
}

// fetch follows a redirect within the host; one to another host is handed
// to the model, so that its call for it is checked (ADR 0013). http follows
// none.
func TestNetRedirects(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "OTHER-BODY") }))
	defer other.Close()
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/same":
			http.Redirect(w, r, "/final", http.StatusFound)
		case "/final":
			io.WriteString(w, "final")
		case "/away":
			http.Redirect(w, r, other.URL+"/x", http.StatusFound)
		case "/post":
			posts.Add(1)
			http.Redirect(w, r, "/final", http.StatusSeeOther)
		}
	}))
	defer srv.Close()
	n := Net{AllowPrivate: true}
	if r := netRun(t, FetchTool, n, map[string]any{"url": srv.URL + "/same"}); !strings.Contains(r.Output, "final") {
		t.Errorf("same host: %s", r.Output)
	}
	if r := netRun(t, FetchTool, n, map[string]any{"url": srv.URL + "/away"}); !strings.Contains(r.Output, "redirected to "+other.URL+"/x, not followed") || strings.Contains(r.Output, "OTHER-BODY") {
		t.Errorf("another host: %s", r.Output)
	}
	r := netRun(t, HTTPTool, n, map[string]any{"url": srv.URL + "/post", "body": "x"})
	if !strings.Contains(r.Output, "HTTP 303") || !strings.Contains(r.Output, "Location: /final") || posts.Load() != 1 {
		t.Errorf("post: %s (%d)", r.Output, posts.Load())
	}
}

// http sends its POST once, with its headers and body, and shows the
// status, main headers and body.
func TestHTTPPost(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, fmt.Sprintf("%s %s %s", r.Method, r.Header.Get("Content-Type"), b))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Hidden", "no")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"id": 7}`)
	}))
	defer srv.Close()
	r := netRun(t, HTTPTool, Net{AllowPrivate: true}, map[string]any{"url": srv.URL + "/c",
		"headers": map[string]string{"Content-Type": "application/json"}, "body": `{"q": 1}`})
	if r.IsError || r.Unknown || r.Output != "HTTP 201\nContent-Type: application/json\n\n{\"id\": 7}" {
		t.Errorf("result %+v", r)
	}
	if len(got) != 1 || got[0] != `POST application/json {"q": 1}` {
		t.Errorf("server got %q", got)
	}
}

// A POST the server may have had, its answer lost, is of unknown outcome;
// one never sent is a failure (invariant 4).
func TestHTTPUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close() // received, no answer
		}
	}))
	defer srv.Close()
	n := Net{AllowPrivate: true}
	if r := netRun(t, HTTPTool, n, map[string]any{"url": srv.URL, "body": "x"}); !r.Unknown || !strings.Contains(r.Output, "may have reached the server") {
		t.Errorf("cut off: %+v", r)
	}
	// Received, and no answer in time.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		<-r.Context().Done()
	}))
	defer slow.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	b, _ := json.Marshal(map[string]any{"url": slow.URL, "body": "x"})
	if r := HTTPTool.Run(ctx, Env{Net: n}, b); !r.Unknown {
		t.Errorf("timed out: %+v", r)
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	u := closed.URL
	closed.Close()
	if r := netRun(t, HTTPTool, n, map[string]any{"url": u, "body": "x"}); r.Unknown || !r.IsError || !strings.Contains(r.Output, "was not sent") {
		t.Errorf("refused connection: %+v", r)
	}
}

// The user's credentials for a host are sent over https only, read from
// the environment when the request is made, and kept out of what is shown.
func TestNetAuth(t *testing.T) {
	const token = "kc-test-credential-5791"
	t.Setenv("KC_TEST_CREDENTIAL", token)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "you sent: "+r.Header.Get("Authorization"))
	}))
	defer srv.Close()
	pool := srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	auth := map[string]HostAuth{"127.0.0.1": {Header: "Authorization", Env: "KC_TEST_CREDENTIAL", Prefix: "Bearer "}}
	n := Net{AllowPrivate: true, Auth: auth, rootCAs: pool}
	for _, d := range []Def{FetchTool, HTTPTool} {
		r := netRun(t, d, n, map[string]any{"url": srv.URL})
		if strings.Contains(r.Output, token) || !strings.Contains(r.Output, "you sent: Bearer [redacted]") {
			t.Errorf("%s: %+v", d.Name, r)
		}
	}
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "you sent: "+r.Header.Get("Authorization"))
	}))
	defer plain.Close()
	if r := netRun(t, FetchTool, n, map[string]any{"url": plain.URL}); !r.IsError || !strings.Contains(r.Output, "over https only") {
		t.Errorf("over http: %+v", r)
	}
	t.Setenv("KC_TEST_CREDENTIAL", "")
	if r := netRun(t, FetchTool, n, map[string]any{"url": srv.URL}); !r.IsError || !strings.Contains(r.Output, "KC_TEST_CREDENTIAL is empty") {
		t.Errorf("not set: %+v", r)
	}
}
