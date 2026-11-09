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
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/flowchart"
	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/opencode"
	"github.com/r-hashi01/kairocode/internal/server"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// Client talks to a data directory's daemon. When it cannot reach it (the
// daemon handed over to a new build, or stopped), it connects again, as
// Connect does (ADR 0019).
type Client struct {
	mu   sync.Mutex
	info Info
	http *http.Client
	// dir and start: how to connect again (none: it does not).
	dir   string
	start *Start
	// Stale is what to tell the user when the daemon cannot be of the
	// client's build: the client is older than the build installed, or
	// (an older daemon) others use it.
	Stale string
}

// conn is how to reach the daemon now.
func (c *Client) conn() (Info, *http.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info, c.http
}

// reconnect connects again, as Connect does: to the daemon server.json
// names now, or one it starts.
func (c *Client) reconnect(ctx context.Context) error {
	if c.start == nil {
		return errors.New("no way to connect again")
	}
	n, err := Connect(ctx, c.dir, *c.start)
	if err != nil {
		return err
	}
	c.mu.Lock()
	old := c.http
	c.info, c.http = n.info, n.http
	if n.Stale != "" {
		c.Stale = n.Stale
	}
	c.mu.Unlock()
	old.CloseIdleConnections()
	return nil
}

// unreached says the request never reached the daemon: no connection was
// made, or it was refused for an old token (server.json has a new one).
// Only such a request is sent again: one that may have reached it is not
// (it may have taken effect, invariant 4).
func unreached(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	var se *statusError
	return errors.As(err, &se) && se.code == http.StatusUnauthorized
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
	c, err := connect(ctx, dir, start)
	if err == nil {
		c.dir, c.start = dir, &start
	}
	return c, err
}

func connect(ctx context.Context, dir string, start Start) (*Client, error) {
	if c, err := dial(ctx, dir); err == nil {
		if start.Version == "" || c.info.Version == start.Version {
			return c, nil
		}
		// Another build: the daemon looks at the build installed now, and
		// hands over to it at once if no step runs, or once none does
		// (ADR 0019).
		var u Update
		err := c.do(ctx, "POST", "/daemon/update", nil, &u)
		var se *statusError
		switch {
		case errors.As(err, &se) && (se.code == http.StatusNotFound || se.code == http.StatusMethodNotAllowed):
			if kept, err := c.replaceOlder(ctx); err != nil || kept != nil {
				return kept, err
			}
		case err != nil:
			c.Close()
			return nil, fmt.Errorf("asking the daemon of another build to update: %w", err)
		case u.Replaced:
			c.Close() // the new build runs (or a client starts it below)
		case u.Installed != "" && u.Installed != start.Version && u.Installed == c.info.Version:
			c.Stale = "this kairocode is not the build installed, which the daemon runs; start the installed one to use it"
			return c, nil
		default:
			return c, nil // it hands over once no step runs; this client connects again then
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

// replaceOlder is ADR 0008's replacement, for a daemon of a build before
// ADR 0019: it is stopped only if no step runs and no other client uses
// it. It returns the client if the daemon is kept, nil if it stopped.
func (c *Client) replaceOlder(ctx context.Context) (*Client, error) {
	err := c.do(ctx, "POST", "/daemon/stop?idle", nil, nil)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusConflict {
		c.Stale = fmt.Sprintf("the daemon (pid %d) is of an older build of kairocode and is in use; it goes on. "+
			"Run `kairocode daemon stop` when it is idle to start a new one", c.info.PID)
		return c, nil
	}
	c.Close()
	if err != nil {
		return nil, fmt.Errorf("stopping the daemon of another build: %w", err)
	}
	return nil, nil
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
	// is: the error it stands for, by the code the server gave it.
	is error
}

// Unwrap is the error the server's code stands for: errors.Is holds as on
// the daemon's side.
func (e *statusError) Unwrap() error { return e.is }

// responseError is an error response: its message, and the error its code
// stands for (server.ErrorOf).
func responseError(status int, b []byte) error {
	var e struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	msg := strings.TrimSpace(string(b))
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		msg = e.Error
	}
	is := server.ErrorOf(e.Code)
	if is == nil && status == http.StatusGone { // a daemon from before codes (ADR 0021)
		is = api.ErrSessionFailed
	}
	return &statusError{code: status, msg: msg, is: is}
}

func (e *statusError) Error() string {
	if e.msg == "" {
		return http.StatusText(e.code)
	}
	return e.msg
}

// PID is the daemon's process.
func (c *Client) PID() int {
	info, _ := c.conn()
	return info.PID
}

func (c *Client) Close() {
	_, h := c.conn()
	h.CloseIdleConnections()
}

// do sends a request with in as its JSON body and decodes the reply into
// out. One that never reached the daemon is sent again once connected
// again.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var b []byte
	if in != nil {
		var err error
		if b, err = json.Marshal(in); err != nil {
			return err
		}
	}
	err := c.doOnce(ctx, method, path, b, in != nil, out)
	if unreached(err) && c.start != nil {
		if rerr := c.reconnect(ctx); rerr != nil {
			return fmt.Errorf("%w (connecting again: %v)", err, rerr)
		}
		err = c.doOnce(ctx, method, path, b, in != nil, out)
	}
	return err
}

func (c *Client) doOnce(ctx context.Context, method, path string, b []byte, hasBody bool, out any) error {
	var body io.Reader
	if hasBody {
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://kairocode"+path, body)
	if err != nil {
		return err
	}
	info, h := c.conn()
	req.Header.Set("Authorization", "Bearer "+info.Token)
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return responseError(resp.StatusCode, b)
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
	return c.SendImages(ctx, session, text, nil)
}

// SendImages is Send with images (ADR 0026).
func (c *Client) SendImages(ctx context.Context, session, text string, images []api.Image) (int, error) {
	var out struct {
		Turn int `json:"turn"`
	}
	type image struct {
		MediaType string `json:"media_type"`
		Data      []byte `json:"data"`
	}
	in := struct {
		Text   string  `json:"text"`
		Images []image `json:"images,omitempty"`
	}{Text: text}
	for _, im := range images {
		in.Images = append(in.Images, image{im.MediaType, im.Data})
	}
	err := c.do(ctx, "POST", sessionPath(session)+"/messages", in, &out)
	return out.Turn, err
}

// ImageData is the bytes of the image of a message stored as ref.
func (c *Client) ImageData(ctx context.Context, ref string) ([]byte, error) {
	var out struct {
		Data []byte `json:"data"`
	}
	err := c.do(ctx, "GET", "/images/"+url.PathEscape(strings.TrimPrefix(ref, "sha256:")), nil, &out)
	return out.Data, err
}

// Turn is a finished turn.
type Turn struct {
	Reply   string    `json:"reply"`
	Steps   int       `json:"steps"`
	Stopped string    `json:"stopped"`
	Usage   msg.Usage `json:"usage"`
}

// Providers are the providers a session can switch to (ADR 0014).
func (c *Client) Providers(ctx context.Context) ([]api.ProviderInfo, error) {
	var out []api.ProviderInfo
	err := c.do(ctx, "GET", "/providers", nil, &out)
	return out, err
}

// InstallProvider installs agent id of ACP's registry (ADR 0015).
func (c *Client) InstallProvider(ctx context.Context, id, version string) (api.ProviderInfo, error) {
	var out api.ProviderInfo
	err := c.do(ctx, "POST", "/providers/"+url.PathEscape(id)+"/install?version="+url.QueryEscape(version), nil, &out)
	return out, err
}

// SetMode sets the session's mode from its next turn on (ADR 0016).
func (c *Client) SetMode(ctx context.Context, session, mode string) (api.Settings, error) {
	var out api.Settings
	err := c.do(ctx, "POST", sessionPath(session)+"/settings", map[string]string{"mode": mode}, &out)
	return out, err
}

// Efforts are provider id's efforts to choose from.
func (c *Client) Efforts(ctx context.Context, id string) ([]string, error) {
	var out []string
	err := c.do(ctx, "GET", "/providers/"+url.PathEscape(id)+"/efforts", nil, &out)
	return out, err
}

// LoginMethods are the ways to sign in to provider id.
func (c *Client) LoginMethods(ctx context.Context, id string) ([]api.AuthMethod, error) {
	var out []api.AuthMethod
	err := c.do(ctx, "GET", "/providers/"+url.PathEscape(id)+"/login", nil, &out)
	return out, err
}

// Login signs in to provider id with its agent's method; done instead
// tells the daemon the user ran a terminal method.
func (c *Client) Login(ctx context.Context, id, method string, done bool) error {
	return c.do(ctx, "POST", "/providers/"+url.PathEscape(id)+"/login", map[string]any{"method": method, "done": done}, nil)
}

// Models are provider id's models to choose from.
func (c *Client) Models(ctx context.Context, id string) ([]string, error) {
	var out []string
	err := c.do(ctx, "GET", "/providers/"+url.PathEscape(id)+"/models", nil, &out)
	return out, err
}

// Settings are the provider and model the session's next turn runs with.
func (c *Client) Settings(ctx context.Context, session string) (api.Settings, error) {
	var out api.Settings
	err := c.do(ctx, "GET", sessionPath(session)+"/settings", nil, &out)
	return out, err
}

// Switch switches the session's provider or model ("": keep it) from its
// next turn on, and returns the settings it then has.
func (c *Client) Switch(ctx context.Context, session, provider, model, effort string) (api.Settings, error) {
	var out api.Settings
	err := c.do(ctx, "POST", sessionPath(session)+"/settings", map[string]string{"provider": provider, "model": model, "effort": effort}, &out)
	return out, err
}

// ErrNoTurn: the session has no turn (or is not there).
var ErrNoTurn = errors.New("the session has no turn")

// LastTurn is the number of the session's latest turn.
func (c *Client) LastTurn(ctx context.Context, session string) (int, error) {
	var out struct {
		Turn int `json:"turn"`
	}
	err := c.do(ctx, "GET", sessionPath(session)+"/turns", nil, &out)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusNotFound {
		return 0, ErrNoTurn
	}
	return out.Turn, err
}

// WaitTurn waits for turn n of the session to finish.
func (c *Client) WaitTurn(ctx context.Context, session string, n int) (Turn, error) {
	for {
		var out Turn
		err := c.do(ctx, "GET", fmt.Sprintf("%s/turns/%d", sessionPath(session), n), nil, &out)
		if err == nil || ctx.Err() != nil || !cutOff(err) {
			return out, err
		}
		// The daemon went while this waited (it handed over to a new build,
		// ADR 0019): waiting only reads, so it is waited for again, of the
		// daemon there is now.
		if rerr := c.reconnect(ctx); rerr != nil {
			return out, fmt.Errorf("%w (connecting again: %v)", err, rerr)
		}
	}
}

// cutOff says whether err is a wait cut by the daemon going, not an
// answer: it says so (503), or the connection ended under the request.
func cutOff(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.code == http.StatusServiceUnavailable
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED)
}

// CloseSession ends the session after the turns it has been sent.
func (c *Client) CloseSession(ctx context.Context, session string) error {
	return c.do(ctx, "POST", sessionPath(session)+"/close", nil, nil)
}

// Pending lists the session's calls that wait for a person.
func (c *Client) Pending(ctx context.Context, session string) ([]api.Pending, error) {
	var out []api.Pending
	err := c.do(ctx, "GET", sessionPath(session)+"/approvals", nil, &out)
	return out, err
}

// Approve answers one of them. Only the first answer goes through: a later
// one returns api.ErrAnswered.
func (c *Client) Approve(ctx context.Context, key string, ok bool, reason string) error {
	err := c.do(ctx, "POST", "/approvals", map[string]any{"key": key, "ok": ok, "reason": reason}, nil)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusConflict {
		return api.ErrAnswered
	}
	return err
}

// TryFlow tries the flow an approval would write first (ADR 0017).
func (c *Client) TryFlow(ctx context.Context, key, args string) error {
	err := c.do(ctx, "POST", "/approvals/try", map[string]any{"key": key, "args": args}, nil)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusConflict {
		return api.ErrAnswered
	}
	return err
}

// Answer answers a flow's question with one of its options. Only the
// first answer goes through: a later one returns api.ErrAnswered.
func (c *Client) Answer(ctx context.Context, key, option string) error {
	err := c.do(ctx, "POST", "/questions", map[string]any{"key": key, "answer": option}, nil)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusConflict {
		return api.ErrAnswered
	}
	return err
}

// Events follows the session's events (GET /sessions/{id}/events): first
// what waits for a person, then what happens. The channel closes when ctx
// ends or the stream does (the daemon stopped, or this client fell
// behind); call it again to go on.
func (c *Client) Events(ctx context.Context, session string) (<-chan api.Event, error) {
	resp, err := c.events(ctx, session)
	if unreached(err) && c.start != nil {
		if rerr := c.reconnect(ctx); rerr != nil {
			return nil, fmt.Errorf("%w (connecting again: %v)", err, rerr)
		}
		resp, err = c.events(ctx, session)
	}
	if err != nil {
		return nil, err
	}
	out := make(chan api.Event)
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
			var ev api.Event
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

// events opens the session's stream.
func (c *Client) events(ctx context.Context, session string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://kairocode"+sessionPath(session)+"/events", nil)
	if err != nil {
		return nil, err
	}
	info, h := c.conn()
	req.Header.Set("Authorization", "Bearer "+info.Token)
	resp, err := h.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return nil, &statusError{code: resp.StatusCode, msg: strings.TrimSpace(string(b))}
	}
	return resp, nil
}

// History is the session's conversation as of its last finished turn, then
// what the turns under way were asked: as a client shows it again.
func (c *Client) History(ctx context.Context, session string) ([]msg.Message, error) {
	var out []msg.Message
	err := c.do(ctx, "GET", sessionPath(session)+"/history?live=1", nil, &out) // as shown again: with the turns under way
	return out, err
}

// Usage is what the session's model calls recorded they used.
func (c *Client) Usage(ctx context.Context, session string) (api.SessionUsage, error) {
	var out api.SessionUsage
	err := c.do(ctx, "GET", sessionPath(session)+"/usage", nil, &out)
	return out, err
}

// Undo starts a turn that puts the working files back as they were before
// the session's last turn in effect; Redo puts back what an undo took away.
// They return the turn's number.
// Cancel stops the session's last turn (ADR 0011). It returns the turn's
// number, or api.ErrNoTurnToCancel when it has ended.
func (c *Client) Cancel(ctx context.Context, session string) (int, error) {
	var out struct {
		Turn int `json:"turn"`
	}
	err := c.do(ctx, "POST", sessionPath(session)+"/cancel", nil, &out)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusConflict {
		return 0, api.ErrNoTurnToCancel
	}
	return out.Turn, err
}

func (c *Client) Undo(ctx context.Context, session string) (int, error) {
	return c.undo(ctx, session, "undo")
}

// Fork starts a session from turn n of this one (n < 0: its last finished
// turn), with files put back as they were after it if asked (ADR 0021). It
// returns the new session and the turn it was forked from; ErrNoForkPoint
// when there is no such turn.
func (c *Client) Fork(ctx context.Context, session string, n int, files bool) (string, int, error) {
	in := map[string]any{"files": files}
	if n >= 0 {
		in["turn"] = n
	}
	var out struct {
		ID   string `json:"id"`
		Turn int    `json:"turn"`
	}
	err := c.do(ctx, "POST", sessionPath(session)+"/fork", in, &out)
	return out.ID, out.Turn, err // ErrNoForkPoint, ErrForkWhileRunning, ErrNoSnapshot by their codes
}

// Compact starts a turn that only compacts the session's conversation
// (ADR 0020), and returns its number.
func (c *Client) Compact(ctx context.Context, session string) (int, error) {
	return c.undo(ctx, session, "compact")
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

// Log is the session's record (ADR 0023).
func (c *Client) Log(ctx context.Context, session string) (api.SessionLog, error) {
	var out api.SessionLog
	err := c.do(ctx, "GET", sessionPath(session)+"/log", nil, &out)
	return out, err
}

// MCP lists the session's MCP servers and custom tools (ADR 0022).
func (c *Client) MCP(ctx context.Context, session string) ([]api.MCPInfo, error) {
	var out []api.MCPInfo
	err := c.do(ctx, "GET", sessionPath(session)+"/mcp", nil, &out)
	return out, err
}

// Sessions lists the sessions of dir ("" all), the latest used first, at
// most limit (0: all).
func (c *Client) Sessions(ctx context.Context, dir string, limit int) ([]api.SessionInfo, error) {
	var out []api.SessionInfo
	err := c.do(ctx, "GET", "/sessions?dir="+url.QueryEscape(dir)+"&limit="+strconv.Itoa(limit), nil, &out)
	return out, err
}

// RepoSessions lists the sessions of dir's repository, across its
// worktrees (ADR 0037).
func (c *Client) RepoSessions(ctx context.Context, dir string) ([]api.SessionInfo, error) {
	var out []api.SessionInfo
	err := c.do(ctx, "GET", "/sessions?repo="+url.QueryEscape(dir), nil, &out)
	return out, err
}

// MakeWorktree makes a worktree of dir's repository for a session (ADR
// 0037); name "" takes the first free.
func (c *Client) MakeWorktree(ctx context.Context, dir, name string) (api.Worktree, error) {
	var out api.Worktree
	err := c.do(ctx, "POST", "/worktrees", map[string]any{"dir": dir, "name": name}, &out)
	return out, err
}

// RemoveWorktree removes a worktree made for sessions; force drops what
// is not committed.
func (c *Client) RemoveWorktree(ctx context.Context, dir string, force bool) (api.WorktreeRemoved, error) {
	var out api.WorktreeRemoved
	err := c.do(ctx, "POST", "/worktrees/remove", map[string]any{"dir": dir, "force": force}, &out)
	return out, err
}

// Plugins lists the session's plugins and their state (ADR 0025).
func (c *Client) Plugins(ctx context.Context, session string) ([]api.PluginInfo, error) {
	var out []api.PluginInfo
	err := c.do(ctx, "GET", sessionPath(session)+"/plugins", nil, &out)
	return out, err
}

// PluginsIn lists dir's plugins and their state.
func (c *Client) PluginsIn(ctx context.Context, dir string) ([]api.PluginInfo, error) {
	var out []api.PluginInfo
	err := c.do(ctx, "GET", "/plugins?dir="+url.QueryEscape(dir), nil, &out)
	return out, err
}

// PluginConnected tells the daemon a plugin's credentials changed.
func (c *Client) PluginConnected(ctx context.Context, name string) error {
	return c.do(ctx, "POST", "/plugins/"+url.PathEscape(name)+"/connected", nil, nil)
}

// JobDone tells the daemon that a command in the background ended (its
// runner, ADR 0028).
func (c *Client) JobDone(ctx context.Context, id string) error {
	return c.do(ctx, "POST", "/jobs/"+url.PathEscape(id)+"/done", nil, nil)
}

// Skills are the session's skills, and the SKILL.md files not taken.
func (c *Client) Skills(ctx context.Context, session string) (api.SkillsInfo, error) {
	var out api.SkillsInfo
	err := c.do(ctx, "GET", sessionPath(session)+"/skills", nil, &out)
	return out, err
}

// Flows lists the flows of the session's project (ADR 0003).
func (c *Client) Flows(ctx context.Context, session string) ([]api.FlowInfo, error) {
	var out []api.FlowInfo
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

// LastFlow is the session's latest flow, with its calls' states.
func (c *Client) LastFlow(ctx context.Context, session string) (*flowchart.View, error) {
	var v flowchart.View
	err := c.do(ctx, "GET", sessionPath(session)+"/flow", nil, &v)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusNotFound {
		return nil, api.ErrNoFlow
	}
	return &v, err
}

// FlowRuns are the flows' runs of dir and flow ("" any), the latest
// first, at most n (0: all), only "tries" or "runs" or both ("") (ADR
// 0029).
func (c *Client) FlowRuns(ctx context.Context, dir, flow, only string, n int) ([]api.FlowRun, error) {
	var out []api.FlowRun
	q := url.Values{"dir": {dir}, "flow": {flow}, "only": {only}, "n": {strconv.Itoa(n)}}
	err := c.do(ctx, "GET", "/flows/runs?"+q.Encode(), nil, &out)
	return out, err
}

// FlowRun is a flow's run, with its chart and steps.
func (c *Client) FlowRun(ctx context.Context, id string) (api.FlowRun, error) {
	var f api.FlowRun
	err := c.do(ctx, "GET", "/flows/run?id="+url.QueryEscape(id), nil, &f)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusNotFound {
		return f, api.ErrNoFlowRun
	}
	return f, err
}

// FlowStats are the flows the sessions in dir ran, and the project's
// flows that none ran.
func (c *Client) FlowStats(ctx context.Context, dir string) ([]api.FlowStat, error) {
	var out []api.FlowStat
	err := c.do(ctx, "GET", "/flows/stats?dir="+url.QueryEscape(dir), nil, &out)
	return out, err
}

// NewFlow has the model write a flow from request (/flow new) and returns
// the turn's number.
func (c *Client) NewFlow(ctx context.Context, session, request string) (int, error) {
	var out struct {
		Turn int `json:"turn"`
	}
	err := c.do(ctx, "POST", sessionPath(session)+"/flows/new", map[string]string{"request": request}, &out)
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
