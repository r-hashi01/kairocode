package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/r-hashi01/kairocode/internal/sandbox"
)

// Timing (ADR 0009, after opencode): the errors are taken once the server
// has been quiet for Settle after publishing them for the file; nothing is
// added if they do not come within Wait. A server not used for Idle stops.
var (
	Settle = 150 * time.Millisecond
	Wait   = 5 * time.Second
	Idle   = 10 * time.Minute
)

// Diagnostic is one of a file's problems, as the server put it.
type Diagnostic struct {
	Range struct {
		Start struct {
			Line      int `json:"line"`
			Character int `json:"character"`
		} `json:"start"`
	} `json:"range"`
	Severity int    `json:"severity"` // 1: error
	Message  string `json:"message"`
}

// Manager keeps the language servers of a process: one per server and
// root, started when first needed.
type Manager struct {
	ctx    context.Context
	cancel context.CancelFunc
	log    *slog.Logger

	mu      sync.Mutex
	servers map[string]*server
}

// NewManager makes a manager; Close stops its servers.
func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{ctx: ctx, cancel: cancel, log: log, servers: map[string]*server{}}
}

// Close stops every server.
func (m *Manager) Close() {
	m.cancel()
	m.mu.Lock()
	servers := m.servers
	m.servers = map[string]*server{}
	m.mu.Unlock()
	for _, s := range servers {
		s.stop()
	}
}

// Errors are the errors the server for file (an absolute path in the
// working directory dir) publishes for content, as text to add to a tool's
// result: "" if there are none, or no server, or none in time.
func (m *Manager) Errors(ctx context.Context, servers []Server, dir, file string, content []byte) string {
	cfg, root, ok := For(servers, dir, file)
	if !ok {
		return ""
	}
	diags, err := m.diagnose(ctx, cfg, dir, root, file, content)
	if err != nil {
		m.log.Warn(fmt.Sprintf("lsp %s: %v", cfg.ID, err))
		return ""
	}
	return Format(file, diags)
}

// MaxErrors is how many of a file's errors are shown.
const MaxErrors = 20

// Format puts a file's errors as opencode does; "" when it has none.
func Format(file string, diags []Diagnostic) string {
	var lines []string
	for _, d := range diags {
		if d.Severity == 1 {
			lines = append(lines, fmt.Sprintf("ERROR [%d:%d] %s", d.Range.Start.Line+1, d.Range.Start.Character+1, d.Message))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	more := ""
	if len(lines) > MaxErrors {
		more = fmt.Sprintf("\n... and %d more", len(lines)-MaxErrors)
		lines = lines[:MaxErrors]
	}
	return "LSP errors detected in this file, please fix:\n<diagnostics file=\"" + file + "\">\n" +
		strings.Join(lines, "\n") + more + "\n</diagnostics>"
}

func (m *Manager) diagnose(ctx context.Context, cfg Server, dir, root, file string, content []byte) ([]Diagnostic, error) {
	ctx, cancel := context.WithTimeout(ctx, Wait)
	defer cancel()
	s, err := m.server(cfg, dir, root)
	if err != nil {
		return nil, err
	}
	select {
	case <-s.ready:
	case <-ctx.Done():
		return nil, fmt.Errorf("not started within %v", Wait)
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.diagnose(ctx, file, content)
}

// server is the running server for cfg in root, started if need be.
func (m *Manager) server(cfg Server, dir, root string) (*server, error) {
	if m.ctx.Err() != nil {
		return nil, errors.New("closed")
	}
	// One per server, its settings and the folders it works in: two
	// projects that set a server apart do not share it.
	conf, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	key := string(conf) + "\x00" + dir + "\x00" + root
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.servers[key]; s != nil && !s.dead() {
		s.used()
		return s, nil
	}
	if len(cfg.Command) == 0 {
		return nil, errors.New("no command")
	}
	path, err := exec.LookPath(cfg.Command[0])
	if err != nil {
		return nil, fmt.Errorf("%s is not installed: %w", cfg.Command[0], err)
	}
	s := &server{cfg: cfg, root: root, ready: make(chan struct{}), pending: map[int]chan response{},
		diags: map[string][]Diagnostic{}, pubs: map[string]int{}, published: make(chan struct{}), docs: map[string]int{}, log: m.log}
	s.ctx, s.cancel = context.WithCancel(m.ctx)
	s.idle = time.AfterFunc(Idle, func() {
		m.mu.Lock()
		if m.servers[key] == s {
			delete(m.servers, key)
		}
		m.mu.Unlock()
		s.stop()
	})
	m.servers[key] = s
	go s.start(dir, path)
	return s, nil
}

type response struct {
	Result json.RawMessage
	Error  *struct {
		Message string `json:"message"`
	}
}

// server is one language server process.
type server struct {
	cfg    Server
	root   string
	ctx    context.Context
	cancel context.CancelFunc
	log    *slog.Logger
	idle   *time.Timer

	ready chan struct{} // closed once initialized, or failed (err)
	err   error

	wmu sync.Mutex // writes to the server
	in  io.WriteCloser
	cmd *exec.Cmd

	mu        sync.Mutex
	docs      map[string]int // open documents: their version
	nextID    int
	pending   map[int]chan response
	diags     map[string][]Diagnostic
	pubs      map[string]int // publishes for a document, counted
	published chan struct{}  // closed and replaced on each publish
	exited    bool
}

func (s *server) used() { s.idle.Reset(Idle) }

func (s *server) dead() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exited || s.ctx.Err() != nil
}

func (s *server) stop() {
	s.idle.Stop()
	s.cancel()
}

// start runs the server in the sandbox and initializes it. It may write
// only the working and temporary directories and its own caches, and
// reach no network (ADR 0009).
func (s *server) start(dir, path string) {
	fail := func(err error) {
		s.err = err
		close(s.ready)
		s.cancel()
	}
	var writable []string
	if cache, err := os.UserCacheDir(); err == nil {
		for _, c := range s.cfg.Caches {
			d := filepath.Join(cache, c)
			if os.MkdirAll(d, 0o755) == nil {
				writable = append(writable, d)
			}
		}
	}
	argv := append([]string{path}, s.cfg.Command[1:]...)
	cmd, err := sandbox.Command(s.ctx, sandbox.Policy{Mode: sandbox.WorkspaceWrite, Root: dir, Writable: writable}, s.root, argv...)
	if err != nil {
		fail(err)
		return
	}
	for k, v := range s.cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	in, err := cmd.StdinPipe()
	if err != nil {
		fail(err)
		return
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		fail(err)
		return
	}
	if err := cmd.Start(); err != nil {
		fail(err)
		return
	}
	s.in, s.cmd = in, cmd
	go func() {
		// Wait once the output is read to its end (os/exec's rule).
		s.read(bufio.NewReader(out))
		_ = cmd.Wait()
		s.mu.Lock()
		s.exited = true
		for id, ch := range s.pending {
			close(ch)
			delete(s.pending, id)
		}
		s.mu.Unlock()
		s.cancel()
	}()
	rootURI := uri(s.root)
	init := map[string]any{
		"processId": os.Getpid(),
		"rootUri":   rootURI,
		"workspaceFolders": []map[string]string{
			{"uri": rootURI, "name": filepath.Base(s.root)},
		},
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"synchronization":    map[string]any{"didSave": false},
				"publishDiagnostics": map[string]any{"versionSupport": true},
			},
			"workspace": map[string]any{"configuration": true, "workspaceFolders": true},
			"window":    map[string]any{"workDoneProgress": true},
		},
	}
	if len(s.cfg.Initialization) > 0 {
		init["initializationOptions"] = s.cfg.Initialization
	}
	if _, err := s.call(s.ctx, "initialize", init); err != nil {
		fail(fmt.Errorf("initialize: %w", err))
		return
	}
	if err := s.notify(s.ctx, "initialized", map[string]any{}); err != nil {
		fail(err)
		return
	}
	close(s.ready)
}

// diagnose tells the server of file's content and waits for its errors.
func (s *server) diagnose(ctx context.Context, file string, content []byte) ([]Diagnostic, error) {
	u := uri(file)
	s.mu.Lock()
	before := s.pubs[u]
	v, open := s.docs[u]
	v++
	s.docs[u] = v
	s.mu.Unlock()
	var err error
	if !open {
		err = s.notify(ctx, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{
			"uri": u, "languageId": language(file), "version": v, "text": string(content)}})
	} else {
		err = s.notify(ctx, "textDocument/didChange", map[string]any{
			"textDocument":   map[string]any{"uri": u, "version": v},
			"contentChanges": []map[string]any{{"text": string(content)}}})
	}
	if err != nil {
		return nil, err
	}
	// Wait for a publish for the file, then for Settle without another.
	var settle <-chan time.Time
	for {
		s.mu.Lock()
		next, n, diags, exited := s.published, s.pubs[u], s.diags[u], s.exited
		s.mu.Unlock()
		if exited {
			return nil, errors.New("the server exited")
		}
		if n > before {
			before = n
			settle = time.After(Settle)
		}
		select {
		case <-next:
		case <-settle:
			return diags, nil
		case <-ctx.Done():
			return nil, fmt.Errorf("no diagnostics within %v", Wait)
		case <-s.ctx.Done():
			return nil, errors.New("the server stopped")
		}
	}
}

// read takes the server's messages: answers, diagnostics, and requests,
// which it answers so the server goes on.
func (s *server) read(r *bufio.Reader) {
	for {
		body, err := readMessage(r)
		if err != nil {
			return
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &m) != nil {
			continue
		}
		switch {
		case m.Method == "textDocument/publishDiagnostics":
			var p struct {
				URI         string       `json:"uri"`
				Version     *int         `json:"version"`
				Diagnostics []Diagnostic `json:"diagnostics"`
			}
			if json.Unmarshal(m.Params, &p) != nil {
				continue
			}
			s.mu.Lock()
			if p.Version != nil && *p.Version < s.docs[p.URI] {
				s.mu.Unlock()
				continue // of a content since replaced
			}
			s.diags[p.URI] = p.Diagnostics
			s.pubs[p.URI]++
			close(s.published)
			s.published = make(chan struct{})
			s.mu.Unlock()
		case m.Method != "" && len(m.ID) > 0:
			// Answered apart: the reader never waits on the server's input.
			go s.answer(m.ID, m.Method, m.Params)
		case m.Method == "" && len(m.ID) > 0:
			id, err := strconv.Atoi(string(m.ID))
			if err != nil {
				continue
			}
			s.mu.Lock()
			ch := s.pending[id]
			delete(s.pending, id)
			s.mu.Unlock()
			if ch != nil {
				ch <- response{Result: m.Result, Error: m.Error}
			}
		}
	}
}

// answer answers a request of the server: its settings are none.
func (s *server) answer(id json.RawMessage, method string, params json.RawMessage) {
	var result any
	if method == "workspace/configuration" {
		var p struct {
			Items []json.RawMessage `json:"items"`
		}
		_ = json.Unmarshal(params, &p)
		result = make([]any, len(p.Items))
	} else if method == "workspace/workspaceFolders" {
		result = []map[string]string{{"uri": uri(s.root), "name": filepath.Base(s.root)}}
	}
	_ = s.write(s.ctx, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *server) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ch := make(chan response, 1)
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	s.pending[id] = ch
	s.mu.Unlock()
	if err := s.write(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return nil, errors.New("the server exited")
		}
		if r.Error != nil {
			return nil, errors.New(r.Error.Message)
		}
		return r.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *server) notify(ctx context.Context, method string, params any) error {
	return s.write(ctx, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// write sends a message, by ctx's deadline. A server that does not take it
// in time no longer reads its input: it is stopped, which ends the write,
// and started again when next needed.
func (s *server) write(ctx context.Context, m any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		s.wmu.Lock()
		defer s.wmu.Unlock()
		_, err := fmt.Fprintf(s.in, "Content-Length: %d\r\n\r\n%s", len(b), b)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		s.stop()
		return fmt.Errorf("the server does not read: %w", ctx.Err())
	}
}

// readMessage reads one message of the base protocol: headers, a blank
// line, and Content-Length bytes.
func readMessage(r *bufio.Reader) ([]byte, error) {
	n := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
			if n, err = strconv.Atoi(strings.TrimSpace(v)); err != nil {
				return nil, err
			}
		}
	}
	if n < 0 {
		return nil, errors.New("no Content-Length")
	}
	body := make([]byte, n)
	_, err := io.ReadFull(r, body)
	return body, err
}

// WriteMessage writes one message of the base protocol (the fake server
// of the tests).
func WriteMessage(w io.Writer, m any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(b), b)
	return err
}

// ReadMessage reads one (the fake server of the tests).
func ReadMessage(r *bufio.Reader) ([]byte, error) { return readMessage(r) }

func uri(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}
