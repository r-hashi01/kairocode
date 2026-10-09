package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/runtime"
)

// Client talks to a data directory's daemon.
type Client struct {
	info Info
	http *http.Client
}

// Start says how a client starts the daemon when none runs: the command
// (argv) and its environment. The command must run Serve with Ready set to
// its standard output.
type Start struct {
	Argv []string
	Env  []string
}

// Connect reaches the daemon of data directory dir, starting it with start
// if none runs. Clients that find none at once start one: the first takes
// start.lock, and the others wait on the lock and find the daemon it
// started.
func Connect(ctx context.Context, dir string, start Start) (*Client, error) {
	if c, err := dial(ctx, dir); err == nil {
		return c, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lock, err := flockCtx(ctx, filepath.Join(dir, startLock))
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if c, err := dial(ctx, dir); err == nil {
		return c, nil
	}
	if err := spawn(ctx, dir, start); err != nil {
		return nil, err
	}
	return dial(ctx, dir)
}

// dial reaches the daemon server.json names, if it answers.
func dial(ctx context.Context, dir string) (*Client, error) {
	info, err := ReadInfo(dir)
	if err != nil {
		return nil, err
	}
	c := &Client{info: info, http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", info.Socket)
		},
	}}}
	if err := c.do(ctx, "GET", "/health", nil, nil); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// waitReady reads the daemon's standard output until it says it is ready,
// or ends (it could not start: another daemon runs, or it failed; its log
// says why). When ctx ends first, it closes stdout, which ends the read.
func waitReady(ctx context.Context, stdout io.ReadCloser, dir string) error {
	got := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if strings.TrimSpace(line) == "ready" {
			got <- nil
			return
		}
		got <- fmt.Errorf("the daemon did not start (see %s): %v", filepath.Join(dir, logFile), err)
	}()
	select {
	case err := <-got:
		return err
	case <-ctx.Done():
		stdout.Close()
		<-got
		return ctx.Err()
	}
}

// PID is the daemon's process.
func (c *Client) PID() int { return c.info.PID }

func (c *Client) Close() { c.http.CloseIdleConnections() }

// do sends a request with in as its JSON body and decodes the reply into out.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://kairocode"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.info.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		b, _ := io.ReadAll(resp.Body)
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// path is the API's name of a session (its id without "s/").
func sessionPath(session string) string {
	return "/sessions/" + url.PathEscape(strings.TrimPrefix(session, "s/"))
}

// CreateSession starts a session in dir and returns its id.
func (c *Client) CreateSession(ctx context.Context, dir, agent string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := c.do(ctx, "POST", "/sessions", map[string]string{"dir": dir, "agent": agent}, &out)
	return out.ID, err
}

// Send gives the session a message and returns the turn that answers it.
func (c *Client) Send(ctx context.Context, session, text string) (int, error) {
	var out struct {
		Turn int `json:"turn"`
	}
	err := c.do(ctx, "POST", sessionPath(session)+"/messages", map[string]string{"text": text}, &out)
	return out.Turn, err
}

// Turn is a finished turn.
type Turn struct {
	Reply   string    `json:"reply"`
	Steps   int       `json:"steps"`
	Stopped string    `json:"stopped"`
	Usage   msg.Usage `json:"usage"`
}

// WaitTurn waits for turn n of the session to finish.
func (c *Client) WaitTurn(ctx context.Context, session string, n int) (Turn, error) {
	var out Turn
	err := c.do(ctx, "GET", fmt.Sprintf("%s/turns/%d", sessionPath(session), n), nil, &out)
	return out, err
}

// CloseSession ends the session after the turns it has been sent.
func (c *Client) CloseSession(ctx context.Context, session string) error {
	return c.do(ctx, "POST", sessionPath(session)+"/close", nil, nil)
}

// Pending lists the session's calls that wait for a person.
func (c *Client) Pending(ctx context.Context, session string) ([]runtime.Pending, error) {
	var out []runtime.Pending
	err := c.do(ctx, "GET", sessionPath(session)+"/approvals", nil, &out)
	return out, err
}

// Approve answers one of them.
func (c *Client) Approve(ctx context.Context, key string, ok bool, reason string) error {
	return c.do(ctx, "POST", "/approvals", map[string]any{"key": key, "ok": ok, "reason": reason}, nil)
}
