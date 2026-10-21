package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"sort"
	"strings"
)

// HTTPTool sends a POST (ADR 0013). It acts on the world: a request that
// may have reached the server, its answer not had, is not sent again
// unasked (invariant 4) but stops for a person to say whether it took
// effect. It follows no redirect. A POST that only reads (a GraphQL query)
// is a POST all the same.
var HTTPTool = Def{
	Name: "http",
	Description: "Send a POST request (an API call: a comment, a CI run, a GraphQL query) and return the response's status, " +
		"main headers and body. It is not sent again by itself: if it may have reached the server without an answer, " +
		"a person is asked whether it took effect. To read a page, use fetch.",
	Schema: json.RawMessage(`{"properties":{
		"url":{"type":"string","description":"The http or https URL"},
		"headers":{"type":"object","additionalProperties":{"type":"string"},"description":"Request headers (Content-Type, Idempotency-Key, say)"},
		"body":{"type":"string","description":"The request body"}},
		"required":["url"]}`),
	Effect: Real,
	Run:    runHTTP,
}

// httpShownHeaders are the response headers shown.
var httpShownHeaders = []string{"Content-Type", "Location", "Retry-After", "Link"}

func runHTTP(ctx context.Context, env Env, input json.RawMessage) Result {
	var in struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return failed("invalid input: %v", err)
	}
	ctx, cancel := context.WithTimeout(ctx, netTimeout)
	defer cancel()
	req, secrets, err := env.Net.request(ctx, http.MethodPost, in.URL, in.Headers, strings.NewReader(in.Body))
	if err != nil {
		return failed("%v", err)
	}
	// Once a connection is made, the request may reach the server.
	var connected bool
	req = req.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected = true },
	}))
	client := env.Net.client(func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse })
	resp, err := client.Do(req)
	if err != nil {
		msg := redact(err.Error(), secrets)
		if errors.Is(err, errPrivate) {
			return failed("%s: %v", in.URL, errPrivate)
		}
		if connected {
			return Result{Output: fmt.Sprintf("the POST to %s may have reached the server, and no answer came: %s", in.URL, msg), Unknown: true}
		}
		return failed("the POST to %s was not sent: %s", in.URL, msg)
	}
	defer resp.Body.Close()
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP %d\n", resp.StatusCode)
	names := append([]string(nil), httpShownHeaders...)
	sort.Strings(names)
	for _, h := range names {
		if v := resp.Header.Get(h); v != "" {
			fmt.Fprintf(&b, "%s: %s\n", h, v)
		}
	}
	body, cut, err := readBody(resp.Body)
	b.WriteString("\n")
	switch {
	case err != nil:
		// The server answered: the request was received. Only the body is
		// missing.
		fmt.Fprintf(&b, "%s\n[the rest of the body was lost: %v]", body, err)
	case !textual(resp.Header.Get("Content-Type")):
		fmt.Fprintf(&b, "[%s content, %d bytes: not shown]", resp.Header.Get("Content-Type"), len(body))
	default:
		b.WriteString(shown(string(body), cut))
	}
	return Result{Output: redact(b.String(), secrets), IsError: resp.StatusCode >= 400}
}
