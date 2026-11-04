package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// headerServer is a remote MCP server whose tool "get" has its owner
// argument marked to be sent as a header too (x-mcp-header, MCP
// 2026-07-28), as GitHub's has: a call without it is refused. ran counts
// the calls that ran; seen is the last Mcp-Param-owner it was sent.
func headerServer(t *testing.T, marked *atomic.Bool) (url string, ran *atomic.Int32, seen *atomic.Value) {
	t.Helper()
	ran, seen = &atomic.Int32{}, &atomic.Value{}
	s := mcp.NewServer(&mcp.Implementation{Name: "github", Version: "0"}, nil)
	add := func() {
		owner := map[string]any{"type": "string"}
		if marked.Load() {
			owner["x-mcp-header"] = "owner"
		}
		s.AddTool(&mcp.Tool{Name: "get", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"owner": owner}}},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				ran.Add(1)
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
			})
	}
	add()
	// Stateless, as GitHub's: MCP 2026-07-28 is spoken (the SDK speaks it
	// only so), and the headers are checked.
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { add(); return s }, &mcp.StreamableHTTPOptions{Stateless: true})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get("Mcp-Param-owner"); v != "" {
			seen.Store(v)
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, ran, seen
}

func connectTo(t *testing.T, ctx context.Context, url string) *mcp.ClientSession {
	t.Helper()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "kairocode", Version: "0"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// A server that wants an argument as a header too has it, on a session
// that had not listed its tools (one started after the turn listed them:
// another daemon's, or one stopped while idle); and, its tool changed,
// the call refused for the header is made once more after listing again.
// The tool runs once each time.
func TestMCPHeaderParams(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var marked atomic.Bool
	marked.Store(true)
	url, ran, seen := headerServer(t, &marked)
	args := map[string]any{"owner": "r-hashi01"}

	// As it was: called on a session that did not list, refused.
	if _, err := connectTo(t, ctx, url).CallTool(ctx, &mcp.CallToolParams{Name: "get", Arguments: args}); err == nil || !headerMismatch(err) {
		t.Fatalf("not refused as GitHub refuses: %v", err)
	}
	if ran.Load() != 0 {
		t.Fatal("a refused call ran")
	}

	e := &extServers{conns: map[string]*extConn{}}
	cs := connectTo(t, ctx, url)
	e.conns["k"] = &extConn{cs: cs}
	res, err := e.callTool(ctx, cs, "get", args)
	if err != nil || res.IsError {
		t.Fatalf("called: %v %+v", err, res)
	}
	if ran.Load() != 1 || seen.Load() != "r-hashi01" {
		t.Fatalf("ran %d, header %v", ran.Load(), seen.Load())
	}

	// Listed when the tool had no mark, then marked: refused, listed again,
	// called again.
	marked.Store(false)
	cs = connectTo(t, ctx, url)
	e.conns["k"] = &extConn{cs: cs}
	if _, err := e.callTool(ctx, cs, "get", args); err != nil {
		t.Fatal(err)
	}
	marked.Store(true)
	if res, err := e.callTool(ctx, cs, "get", args); err != nil || res.IsError {
		t.Fatalf("after the tool changed: %v %+v", err, res)
	}
	if ran.Load() != 3 {
		t.Fatalf("ran %d times, want 3", ran.Load())
	}
}
