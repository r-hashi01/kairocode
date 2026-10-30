package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// FetchTool reads a URL with GET (ADR 0013). It only reads, so a resumed
// turn may fetch again. It follows redirects within the same host; one to
// another host is handed to the model, whose call for it is checked like
// any other.
var FetchTool = Def{
	Name: "fetch",
	Description: "Fetch a URL with GET and return its content: an HTML page as Markdown (format \"markdown\", the default), " +
		"as plain text (\"text\"), or as it is (\"html\"). Large content is cut. To send data, use the http tool.",
	Schema: json.RawMessage(`{"properties":{
		"url":{"type":"string","description":"The http or https URL"},
		"format":{"type":"string","enum":["markdown","text","html"],"description":"How to return an HTML page (default: markdown)"},
		"headers":{"type":"object","additionalProperties":{"type":"string"},"description":"Request headers (Accept, say)"}},
		"required":["url"]}`),
	Effect: Read,
	Run:    runFetch,
}

func runFetch(ctx context.Context, env Env, input json.RawMessage) Result {
	var in struct {
		URL     string            `json:"url"`
		Format  string            `json:"format"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return failed("invalid input: %v", err)
	}
	switch in.Format {
	case "":
		in.Format = "markdown"
	case "markdown", "text", "html":
	default:
		return failed("format is markdown, text or html, not %q", in.Format)
	}
	ctx, cancel := context.WithTimeout(ctx, netTimeout)
	defer cancel()
	req, secrets, err := env.Net.request(ctx, http.MethodGet, in.URL, in.Headers, nil)
	if err != nil {
		return failed("%v", err)
	}
	first := req.URL
	client := env.Net.client(func(next *http.Request, via []*http.Request) error {
		if len(via) > netHops || next.URL.Host != first.Host || (first.Scheme == "https" && next.URL.Scheme != "https") {
			return http.ErrUseLastResponse
		}
		return nil
	})
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, errPrivate) {
			return failed("%s: %v", in.URL, errPrivate)
		}
		return failed("%s", redact(err.Error(), secrets))
	}
	defer resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode >= 300 && resp.StatusCode < 400 && loc != "" {
		to := loc
		if u, err := resp.Request.URL.Parse(loc); err == nil {
			to = u.String()
		}
		return Result{Output: redact(fmt.Sprintf("HTTP %d: redirected to %s, not followed (another host, or more than %d redirects). Fetch it to go on.",
			resp.StatusCode, to, netHops), secrets)}
	}
	body, cut, err := readBody(resp.Body)
	if err != nil {
		return failed("%s: reading the response: %s", in.URL, redact(err.Error(), secrets))
	}
	ct := resp.Header.Get("Content-Type")
	var text string
	switch {
	case !textual(ct):
		text = fmt.Sprintf("[%s content, %d bytes: not shown]", ct, len(body))
	case strings.Contains(strings.ToLower(ct), "html") && in.Format != "html":
		text = htmlText(string(body), in.Format == "markdown")
	default:
		text = string(body)
	}
	out := fmt.Sprintf("HTTP %d %s\n\n%s", resp.StatusCode, resp.Request.URL, shown(text, cut))
	return Result{Output: redact(out, secrets), IsError: resp.StatusCode >= 400}
}
