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
	"github.com/r-hashi01/kairocode/internal/opencode"
	"github.com/r-hashi01/kairocode/internal/runtime"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// Client talks to a data directory's daemon.
type Client struct {
	info Info
	http *http.Client
	// Stale is set when the daemon is of another build than the client and
	// could not be replaced (other clients use it): what to tell the user.
	Stale string
}

// Start says how a client starts the daemon when none runs: the command
// (argv) and its environment. The command must run Serve with Ready set to
// its standard output.
type Start struct {
	Argv []string
	Env  []string
	// Version is the client's build. A daemon of another build is stopped
	// and started again from Argv, if no other client uses it.
	Version string
}

// Connect reaches the daemon of data directory dir, starting it with start
// if none runs. Clients that find none at once start one: the first takes
// start.lock, and the others wait on the lock and find the daemon it
// started.
func Connect(ctx context.Context, dir string, start Start) (*Client, error) {
	if c, err := dial(ctx, dir); err == nil {
		if start.Version == "" || c.info.Version == start.Version {
			return c, nil
		}
		// Only a daemon that runs no step and that no other client uses is
		// replaced: a stop would make this client wait for the steps under
		// way (a Claude Code run can take minutes), and cut one that runs
		// longer than the drain. The daemon checks it as it stops (?idle),
		// and says 409 if it is busy.
		err := c.do(ctx, "POST", "/daemon/stop?idle", nil, nil)
		var se *statusError
		if errors.As(err, &se) && se.code == http.StatusConflict {
			c.Stale = fmt.Sprintf("the daemon (pid %d) is of another build of kairocode and is in use; it goes on. "+
				"Run `kairocode daemon stop` when it is idle to start a new one", c.info.PID)
			return c, nil
		}
		c.Close()
		if err != nil {
			return nil, fmt.Errorf("stopping the daemon of another build: %w", err)
		}
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

// Stop stops the daemon of dir, if one runs, and returns once it has let go
// of the data directory. A step running then is cut, as in a crash: a real
// one waits in the next daemon for a person (invariant 4). It reports
// whether a daemon was running.
func Stop(ctx context.Context, dir string) (bool, error) {
	c, err := dial(ctx, dir)
	if err != nil {
		return false, nil
	}
	defer c.Close()
	return true, c.stop(ctx)
}

// stop asks the daemon to stop. It answers once it has let go of the data
// directory: its lock is released, and a new daemon can start.
func (c *Client) stop(ctx context.Context) error {
	err := c.do(ctx, "POST", "/daemon/stop", nil, nil)
	c.Close()
	return err
}

// Status is the daemon's build, process and number of clients.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var st Status
	err := c.do(ctx, "GET", "/daemon/status", nil, &st)
	return st, err
}

// statusError is a reply other than success.
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string {
	if e.msg == "" {
		return http.StatusText(e.code)
	}
	return e.msg
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
		msg := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return &statusError{code: resp.StatusCode, msg: msg}
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

// CreateSession starts a session and returns its id. in.Dir must be
// absolute: the daemon runs elsewhere.
func (c *Client) CreateSession(ctx context.Context, in workflows.SessionInput) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := c.do(ctx, "POST", "/sessions", in, &out)
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

// Approve answers one of them. Only the first answer goes through: a later
// one returns runtime.ErrAnswered.
func (c *Client) Approve(ctx context.Context, key string, ok bool, reason string) error {
	err := c.do(ctx, "POST", "/approvals", map[string]any{"key": key, "ok": ok, "reason": reason}, nil)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusConflict {
		return runtime.ErrAnswered
	}
	return err
}

// Answer answers a flow's question with one of its options. Only the
// first answer goes through: a later one returns runtime.ErrAnswered.
func (c *Client) Answer(ctx context.Context, key, option string) error {
	err := c.do(ctx, "POST", "/questions", map[string]any{"key": key, "answer": option}, nil)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusConflict {
		return runtime.ErrAnswered
	}
	return err
}

// Events follows the session's events (GET /sessions/{id}/events): first
// what waits for a person, then what happens. The channel closes when ctx
// ends or the stream does (the daemon stopped, or this client fell
// behind); call it again to go on.
func (c *Client) Events(ctx context.Context, session string) (<-chan runtime.Event, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://kairocode"+sessionPath(session)+"/events", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.info.Token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return nil, &statusError{code: resp.StatusCode, msg: strings.TrimSpace(string(b))}
	}
	out := make(chan runtime.Event)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		r := bufio.NewReader(resp.Body)
		var data []byte
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSuffix(line, "\n")
			if d, ok := strings.CutPrefix(line, "data: "); ok {
				data = append(data, d...)
				continue
			}
			if line != "" || len(data) == 0 {
				continue
			}
			var ev runtime.Event
			err = json.Unmarshal(data, &ev)
			data = data[:0]
			if err != nil {
				return
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// History is the session's conversation as of its last finished turn.
func (c *Client) History(ctx context.Context, session string) ([]msg.Message, error) {
	var out []msg.Message
	err := c.do(ctx, "GET", sessionPath(session)+"/history", nil, &out)
	return out, err
}

// Usage is what the session's model calls recorded they used.
func (c *Client) Usage(ctx context.Context, session string) (runtime.SessionUsage, error) {
	var out runtime.SessionUsage
	err := c.do(ctx, "GET", sessionPath(session)+"/usage", nil, &out)
	return out, err
}

// Undo starts a turn that puts the working files back as they were before
// the session's last turn in effect; Redo puts back what an undo took away.
// They return the turn's number.
// Cancel stops the session's last turn (ADR 0011). It returns the turn's
// number, or runtime.ErrNoTurnToCancel when it has ended.
func (c *Client) Cancel(ctx context.Context, session string) (int, error) {
	var out struct {
		Turn int `json:"turn"`
	}
	err := c.do(ctx, "POST", sessionPath(session)+"/cancel", nil, &out)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusConflict {
		return 0, runtime.ErrNoTurnToCancel
	}
	return out.Turn, err
}

func (c *Client) Undo(ctx context.Context, session string) (int, error) {
	return c.undo(ctx, session, "undo")
}

func (c *Client) Redo(ctx context.Context, session string) (int, error) {
	return c.undo(ctx, session, "redo")
}

func (c *Client) undo(ctx context.Context, session, which string) (int, error) {
	var out struct {
		Turn int `json:"turn"`
	}
	err := c.do(ctx, "POST", sessionPath(session)+"/"+which, nil, &out)
	return out.Turn, err
}

// Commands are the commands the session can run.
func (c *Client) Commands(ctx context.Context, session string) ([]opencode.Command, error) {
	var out []opencode.Command
	err := c.do(ctx, "GET", sessionPath(session)+"/commands", nil, &out)
	return out, err
}

// Flows lists the flows of the session's project (ADR 0003).
func (c *Client) Flows(ctx context.Context, session string) ([]runtime.FlowInfo, error) {
	var out []runtime.FlowInfo
	err := c.do(ctx, "GET", sessionPath(session)+"/flows", nil, &out)
	return out, err
}

// RunFlow runs the flow name with args (/flow) and returns the turn's
// number.
func (c *Client) RunFlow(ctx context.Context, session, name, args string) (int, error) {
	var out struct {
		Turn int `json:"turn"`
	}
	err := c.do(ctx, "POST", sessionPath(session)+"/flows", map[string]string{"name": name, "arguments": args}, &out)
	return out.Turn, err
}

// RunCommand runs one with args and returns the turn's number.
func (c *Client) RunCommand(ctx context.Context, session, name, args string) (int, error) {
	var out struct {
		Turn int `json:"turn"`
	}
	err := c.do(ctx, "POST", sessionPath(session)+"/commands", map[string]string{"name": name, "arguments": args}, &out)
	return out.Turn, err
}
