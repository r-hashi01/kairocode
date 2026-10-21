package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/jsonrpc"
	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// The Codex and Copilot engines (ADR 0014): KairoCode is an ACP client of
// the agent (codex-acp, copilot --acp), which runs the model loop. What the
// agent asks of its client (writing a file, running a command) KairoCode
// does with its own tools, as Claude Code's MCP calls (ADR 0007): recorded,
// checked, in the sandbox, never twice. KairoCode names each agent session
// it starts, from its session and epoch, and records the agent's id under
// that name.

// acpArgv is how to start engine's agent, and whether it then has no
// tools of its own: Copilot is told KairoCode's are its only ones
// (checked with copilot 1.0.54, ADR 0014). Env is the registry's for it.
func (r *Runtime) acpArgv(engine, dir string, tools []string, effort string) ([]string, map[string]string, bool, error) {
	argv, env, err := r.agentCommand(engine)
	if err != nil {
		return nil, nil, false, err
	}
	own, err := ownHooks()
	if err != nil {
		return nil, nil, false, err
	}
	if registryAgent(engine) == codexAgent && !own {
		// Codex reads its AGENTS.md and .agents/skills by itself, and
		// KairoCode gives it the rest (ADR 0018); its own hooks are off,
		// KairoCode running hooks. Over the user's CODEX_CONFIG, if there
		// is one.
		cfg := map[string]any{}
		user := env["CODEX_CONFIG"]
		if user == "" {
			user = os.Getenv("CODEX_CONFIG")
		}
		if user != "" && json.Unmarshal([]byte(user), &cfg) != nil {
			// Not JSON (codex-acp could not read it either): KairoCode's
			// alone, so that codex's own hooks are still off.
			r.logf("CODEX_CONFIG is not a JSON object: it is not given to codex")
			cfg = map[string]any{}
		}
		features, _ := cfg["features"].(map[string]any)
		if features == nil {
			features = map[string]any{}
		}
		features["hooks"] = false
		cfg["features"] = features
		b, err := json.Marshal(cfg)
		if err != nil {
			return nil, nil, false, err
		}
		withConfig := map[string]string{"CODEX_CONFIG": string(b)}
		for k, v := range env {
			if k != "CODEX_CONFIG" {
				withConfig[k] = v
			}
		}
		return argv, withConfig, false, nil
	}
	if registryAgent(engine) != copilotAgent {
		return argv, env, false, nil
	}
	// Its own hooks: off, unless the user lets it run them (ADR 0018).
	if err := r.copilotHooks(own, env); err != nil {
		return nil, nil, false, err
	}
	if !own {
		if err := copilotRepoHooks(dir); err != nil {
			return nil, nil, false, err
		}
	}
	names := make([]string, len(tools))
	for i, n := range tools {
		names[i] = MCPServerName + "-" + n
	}
	// It reads AGENTS.md and CLAUDE.md by itself (ADR 0018).
	argv = append(argv, "--available-tools="+strings.Join(names, ","))
	if effort != "" { // Copilot's session has no setting for it: a flag
		argv = append(argv, "--reasoning-effort="+effort)
	}
	return argv, env, true, nil
}

// lockedBuffer is a strings.Builder for a process's stderr, read while the
// process may still write it.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// acpTurn runs a turn in an ACP agent: started, initialized, its session
// made or loaded, its model set, prompted; its reply gathered as it comes.
// Done again after the process died, it loads the same session and, if the
// agent has the prompt already, asks it to go on.
func (r *Runtime) acpTurn(engine string) func(*kairo.TaskContext, workflows.ClaudeTurnInput) (workflows.ClaudeTurnOutput, error) {
	return func(t *kairo.TaskContext, in workflows.ClaudeTurnInput) (workflows.ClaudeTurnOutput, error) {
		name := engineSessionName(engine, in.Session, in.Epoch)
		id, err := r.Store.EngineSession(t, name)
		if err != nil {
			return workflows.ClaudeTurnOutput{}, kairo.Retryable(err)
		}
		url, mt, closeMCP, err := r.mcp.open(in.Turn, in.Dir, in.Tools, in.Policy, in.Hooks, in.Session)
		if err != nil {
			return workflows.ClaudeTurnOutput{}, err
		}
		defer closeMCP()
		if in.Policy.Dry && !dryEngine(engine) { // it may act without asking: not in a try (ADR 0017)
			return r.finishCLITurn(t, mt, fmt.Sprintf("[%s is not run in a try of a flow: it may change things without asking]", registryAgent(engine)), 0, msg.Usage{})
		}
		argv, agentEnv, bare, err := r.acpArgv(engine, in.Dir, in.Tools, in.Effort)
		if err != nil { // not there to run: the user is told
			return r.finishCLITurn(t, mt, fmt.Sprintf("[%s cannot run: %v]", registryAgent(engine), err), 0, msg.Usage{})
		}
		// The agent a run of this session left, if it still runs: its pid
		// is kept under the session's name, and it is still that command.
		if err := r.stopStale(t, name, filepath.Base(argv[0])); err != nil {
			return workflows.ClaudeTurnOutput{}, kairo.Retryable(err)
		}

		ctx, stop := context.WithCancel(t)
		defer stop()
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = in.Dir
		cmd.Env = cliEnv()
		for k, v := range agentEnv {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		cmd.SysProcAttr = claudeProcAttr()
		// Stopped, the agent's process group goes, and the turn does not
		// wait long for what it started.
		cmd.Cancel = func() error {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			return cmd.Process.Kill()
		}
		cmd.WaitDelay = 5 * time.Second
		var stderr lockedBuffer
		cmd.Stderr = &stderr
		toAgent, err := cmd.StdinPipe()
		if err != nil {
			return workflows.ClaudeTurnOutput{}, err
		}
		fromAgent, err := cmd.StdoutPipe()
		if err != nil {
			return workflows.ClaudeTurnOutput{}, err
		}
		if err := cmd.Start(); err != nil {
			return workflows.ClaudeTurnOutput{}, fmt.Errorf("starting %s: %w", argv[0], err)
		}
		if err := r.Store.SetClaudeProc(t, name, cmd.Process.Pid); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return workflows.ClaudeTurnOutput{}, kairo.Retryable(err)
		}
		defer r.Store.SetClaudeProc(context.WithoutCancel(t), name, 0)
		// Copilot has none of its own tools here (acpArgv): a bare name is
		// ours. An agent that keeps its own must name ours with the server.
		c := &acpClient{r: r, t: t, mt: mt, dir: in.Dir, tools: in.Tools, bare: bare, turn: in.Turn, round: in.Round,
			terms: map[string]*acpTerminal{}, uses: map[string]int{}}
		conn := jsonrpc.NewConn(toAgent)
		served := make(chan error, 1)
		go func() { served <- conn.Serve(ctx, fromAgent, c.handle) }()
		// The agent is stopped, its output read to the end (no handler of
		// it still runs), then waited for.
		defer func() {
			toAgent.Close()
			stop()
			select {
			case <-served:
			case <-time.After(5 * time.Second):
				// Something outside the group holds the output open: it
				// is not read further.
				fromAgent.Close()
				<-served
			}
			_ = cmd.Wait()
		}()
		fail := func(step string, err error) (workflows.ClaudeTurnOutput, error) {
			if t.Err() != nil {
				return workflows.ClaudeTurnOutput{}, t.Err()
			}
			// Nothing of the turn is known to have happened: done again,
			// it loads the session and goes on.
			return workflows.ClaudeTurnOutput{}, kairo.Retryable(fmt.Errorf("%s: %s: %w: %s", engine, step, err, tail(stderr.String(), 300)))
		}

		var init struct {
			AuthMethods       json.RawMessage `json:"authMethods"`
			AgentCapabilities struct {
				LoadSession     bool `json:"loadSession"`
				MCPCapabilities struct {
					HTTP bool `json:"http"`
				} `json:"mcpCapabilities"`
			} `json:"agentCapabilities"`
		}
		if err := conn.Call(t, "initialize", map[string]any{
			"protocolVersion": 1,
			"clientCapabilities": map[string]any{
				"fs":       map[string]any{"readTextFile": true, "writeTextFile": true},
				"terminal": true,
			},
			"clientInfo": map[string]any{"name": "kairocode", "version": "0"},
		}, &init); err != nil {
			return fail("initialize", err)
		}
		if ms := acpAuthMethods(init.AuthMethods); len(ms) > 0 {
			r.authSeen(providerOf(engine), "", ms)
		}
		mcpServers := []any{}
		if init.AgentCapabilities.MCPCapabilities.HTTP {
			mcpServers = append(mcpServers, map[string]any{"type": "http", "name": MCPServerName, "url": url, "headers": []any{}})
		}
		// The session: loaded if the agent has it, and can; else a new one,
		// given the conversation so far.
		var opts []acpOption
		loaded := false
		if id != "" && init.AgentCapabilities.LoadSession {
			c.replaying.Store(true)
			var out struct {
				ConfigOptions []acpOption `json:"configOptions"`
			}
			err := conn.Call(t, "session/load", map[string]any{"sessionId": id, "cwd": in.Dir, "mcpServers": mcpServers}, &out)
			c.replaying.Store(false)
			if err == nil {
				loaded, opts = true, out.ConfigOptions
			}
		}
		if !loaded {
			var out struct {
				SessionID     string      `json:"sessionId"`
				ConfigOptions []acpOption `json:"configOptions"`
			}
			if err := conn.Call(t, "session/new", map[string]any{"cwd": in.Dir, "mcpServers": mcpServers}, &out); err != nil {
				if authRequired(err) { // the user is to sign in: said, not tried again
					r.authSeen(providerOf(engine), AuthNeeded, nil)
					return r.finishCLITurn(t, mt, fmt.Sprintf("[%s needs you to sign in first: /login shows how]", registryAgent(engine)), 0, msg.Usage{})
				}
				return fail("session/new", err)
			}
			id, opts = out.SessionID, out.ConfigOptions
			if err := r.Store.SetEngineSession(t, name, id); err != nil {
				return workflows.ClaudeTurnOutput{}, kairo.Retryable(err)
			}
		}
		c.session = id
		r.authSeen(providerOf(engine), AuthSignedIn, nil)
		r.keepACPOptions(t, engine, opts)
		// A model the agent's session does not have (its list changes
		// with the account) does not stop the turn: the agent's own runs
		// it, and the reply says so.
		note := ""
		if in.Model != "" {
			if err := setACPModel(t, conn, id, opts, in.Model); err != nil {
				note = fmt.Sprintf("[%s did not take the model %s (%v): its own model answered]\n", engine, in.Model, err)
			}
		}
		if in.Effort != "" && registryAgent(engine) != copilotAgent {
			err := setACPOption(t, conn, id, opts, "thought_level", "effort", in.Effort)
			switch {
			case err != nil && len(opts) == 0:
				// A loaded session may not say its settings: the usual id.
				_ = conn.Call(t, "session/set_config_option", map[string]any{"sessionId": id, "configId": "reasoning_effort", "value": in.Effort}, nil)
			case err != nil:
				note += fmt.Sprintf("[%s did not take the effort %s (%v)]\n", engine, in.Effort, err)
			}
		}
		prompt, err := r.cliPrompt(t, in, !loaded)
		if err != nil {
			return workflows.ClaudeTurnOutput{}, err
		}
		if loaded && strings.Contains(c.replayed.String(), turnMark(in.Turn)) {
			prompt = continuePrompt // the agent has the prompt: it was cut short
			if in.Note != "" {
				prompt = in.Note
			}
		}
		var res struct {
			StopReason string `json:"stopReason"`
			Usage      *struct {
				Input     int64 `json:"inputTokens"`
				Output    int64 `json:"outputTokens"`
				CacheRead int64 `json:"cachedReadTokens"`
			} `json:"usage"`
		}
		if err := conn.Call(t, "session/prompt", map[string]any{"sessionId": id,
			"prompt": []any{map[string]any{"type": "text", "text": prompt}}}, &res); err != nil {
			return fail("session/prompt", err)
		}
		var usage msg.Usage
		if res.Usage != nil {
			usage = msg.Usage{Input: res.Usage.Input, Output: res.Usage.Output, CacheRead: res.Usage.CacheRead}
		}
		c.mu.Lock()
		reply := note + c.reply.String()
		c.mu.Unlock()
		if res.StopReason != "" && res.StopReason != "end_turn" && res.StopReason != "cancelled" {
			reply += "\n[the agent stopped: " + res.StopReason + "]"
		}
		return r.finishCLITurn(t, mt, reply, 1, usage)
	}
}

// withAgent starts engine's agent in a directory of its own, initializes
// it, gives fn the connection and what initialize said, and stops it.
func (r *Runtime) withAgent(ctx context.Context, engine string, fn func(context.Context, *jsonrpc.Conn, json.RawMessage) error) error {
	argv, agentEnv, _, err := r.acpArgv(engine, os.TempDir(), nil, "") // its directory: nothing of a repository's
	if err != nil {
		return err
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = os.TempDir()
	cmd.Env = cliEnv()
	for k, v := range agentEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.SysProcAttr = claudeProcAttr()
	cmd.Cancel = func() error {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 5 * time.Second
	toAgent, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	fromAgent, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", argv[0], err)
	}
	conn := jsonrpc.NewConn(toAgent)
	served := make(chan error, 1)
	go func() {
		served <- conn.Serve(ctx, fromAgent, func(context.Context, string, json.RawMessage) (any, error) { return nil, nil })
	}()
	defer func() {
		toAgent.Close()
		stop()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
			// Something outside the group (a browser it opened) holds the
			// output open: it is not read further.
			fromAgent.Close()
			<-served
		}
		_ = cmd.Wait()
	}()
	var init struct {
		AuthMethods json.RawMessage `json:"authMethods"`
	}
	ictx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = conn.Call(ictx, "initialize", map[string]any{"protocolVersion": 1,
		"clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": true, "writeTextFile": true}, "terminal": true}}, &init)
	cancel()
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	return fn(ctx, conn, init.AuthMethods)
}

// finishCLITurn stores a CLI turn's reply and hands back what the turn
// records.
func (r *Runtime) finishCLITurn(t *kairo.TaskContext, mt *mcpTurn, text string, turns int, usage msg.Usage) (workflows.ClaudeTurnOutput, error) {
	// The turn ended with a reply: every tool result it was handed reached it.
	if err := r.mcp.acknowledge(t, mt); err != nil {
		return workflows.ClaudeTurnOutput{}, kairo.Retryable(err)
	}
	h, err := r.Store.PutMessage(t, msg.Message{Role: msg.Assistant, Parts: []msg.Part{{Type: msg.Text, Text: text}}})
	if err != nil {
		return workflows.ClaudeTurnOutput{}, kairo.Retryable(err)
	}
	return workflows.ClaudeTurnOutput{Asks: mt.takeAsks(), Message: h, Turns: turns, Usage: usage}, nil
}

// keepACPOptions keeps the models and efforts the agent's session says it
// has, for /model and /effort (ADR 0014, 追記).
func (r *Runtime) keepACPOptions(ctx context.Context, engine string, opts []acpOption) {
	var models, efforts []string
	for _, o := range opts {
		var vs []string
		for _, v := range o.Options {
			vs = append(vs, v.Value)
		}
		switch {
		case o.Category == "model" || o.ID == "model":
			models = vs
		case o.Category == "thought_level" || strings.Contains(o.ID, "effort"):
			efforts = vs
		}
	}
	if models != nil || efforts != nil {
		if err := r.Store.SetProviderOptions(ctx, providerOf(engine), models, efforts); err != nil {
			r.logf("keeping %s's options: %v", engine, err)
		}
	}
}

// setACPOption sets the agent's session setting of category (or whose id
// has name) to value.
func setACPOption(ctx context.Context, conn *jsonrpc.Conn, session string, opts []acpOption, category, name, value string) error {
	for _, o := range opts {
		if o.Category == category || strings.Contains(o.ID, name) {
			return conn.Call(ctx, "session/set_config_option", map[string]any{"sessionId": session, "configId": o.ID, "value": value}, nil)
		}
	}
	return fmt.Errorf("the agent has no setting for its %s", name)
}

// acpOption is one of an ACP session's settings.
type acpOption struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Options  []struct {
		Value string `json:"value"`
	} `json:"options"`
}

// setACPModel chooses model in the agent's session: through its model
// setting, else session/set_model (agents of ACP before settings).
func setACPModel(ctx context.Context, conn *jsonrpc.Conn, session string, opts []acpOption, model string) error {
	id := "model" // a loaded session may not say its settings
	for _, o := range opts {
		if o.Category == "model" || o.ID == "model" {
			id = o.ID
			break
		}
	}
	err := conn.Call(ctx, "session/set_config_option", map[string]any{"sessionId": session, "configId": id, "value": model}, nil)
	if err == nil {
		return nil
	}
	if conn.Call(ctx, "session/set_model", map[string]any{"sessionId": session, "modelId": model}, nil) == nil {
		return nil
	}
	return err
}

// acpClient answers what an agent asks of its client during a turn.
type acpClient struct {
	r     *Runtime
	t     *kairo.TaskContext
	mt    *mcpTurn
	dir   string
	tools []string // KairoCode's tools the agent has
	// bare: the agent has no tools of its own, so a call named bare is
	// one of KairoCode's.
	bare bool
	// turn and round: the turn's run, and its round (asks are per round).
	turn    string
	round   int
	session string

	// replaying: session/load is replaying the session; replayed, what the
	// user said in it (to find the turn's mark).
	replaying atomicBool
	mu        sync.Mutex
	replayed  strings.Builder
	reply     strings.Builder
	terms     map[string]*acpTerminal
	nterm     int
	uses      map[string]int // asks for an agent's own call, by its key
}

type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (b *atomicBool) Store(v bool) { b.mu.Lock(); b.v = v; b.mu.Unlock() }
func (b *atomicBool) Load() bool   { b.mu.Lock(); defer b.mu.Unlock(); return b.v }

// acpTerminal is a command an agent ran: done when made (KairoCode runs it
// as its bash tool), its output kept for the agent to read.
type acpTerminal struct {
	output string
	exit   int
}

func (c *acpClient) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "session/update":
		var p struct {
			Update struct {
				SessionUpdate string `json:"sessionUpdate"`
				Content       struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		}
		if json.Unmarshal(params, &p) != nil || p.Update.Content.Type != "text" {
			return nil, nil
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		switch {
		case c.replaying.Load():
			if p.Update.SessionUpdate == "user_message_chunk" {
				c.replayed.WriteString(p.Update.Content.Text)
			}
		case p.Update.SessionUpdate == "agent_message_chunk":
			c.reply.WriteString(p.Update.Content.Text)
			c.t.Emit([]byte(p.Update.Content.Text))
		}
		return nil, nil
	case "session/request_permission":
		// A call of one of KairoCode's tools is allowed here: the tool
		// checks it as it runs (permission, approval, sandbox). Anything
		// else would act outside KairoCode, and is refused.
		var p struct {
			ToolCall struct {
				ToolCallID string          `json:"toolCallId"`
				Title      string          `json:"title"`
				RawInput   json.RawMessage `json:"rawInput"`
			} `json:"toolCall"`
			Options []struct {
				OptionID string `json:"optionId"`
				Kind     string `json:"kind"`
			} `json:"options"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		want := "allow_once"
		if !c.ours(p.ToolCall.Title) {
			allowed, err := c.agentTool(ctx, p.ToolCall.ToolCallID, p.ToolCall.Title, p.ToolCall.RawInput)
			if err != nil {
				return nil, err
			}
			if !allowed {
				want = "reject_once"
			}
		}
		for _, o := range p.Options {
			if o.Kind == want {
				return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": o.OptionID}}, nil
			}
		}
		return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
	case "fs/read_text_file":
		var p struct {
			Path  string `json:"path"`
			Line  int    `json:"line"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		rel, err := c.rel(p.Path)
		if err != nil {
			return nil, err
		}
		root, err := os.OpenRoot(c.dir)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		b, err := root.ReadFile(rel)
		if err != nil {
			return nil, err
		}
		text := string(b)
		if p.Line > 0 || p.Limit > 0 {
			lines := strings.SplitAfter(text, "\n")
			from := max(p.Line-1, 0)
			to := len(lines)
			if p.Limit > 0 && from+p.Limit < to {
				to = from + p.Limit
			}
			if from > len(lines) {
				from = len(lines)
			}
			text = strings.Join(lines[from:to], "")
		}
		return map[string]any{"content": text}, nil
	case "fs/write_text_file":
		var p struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		rel, err := c.rel(p.Path)
		if err != nil {
			return nil, err
		}
		args, _ := json.Marshal(map[string]string{"path": rel, "content": p.Content})
		text, isErr, err := c.tool(ctx, "write", args)
		if err != nil {
			return nil, err
		}
		if isErr {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternal, Message: text}
		}
		return map[string]any{}, nil
	case "terminal/create":
		var p struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		command := p.Command
		for _, a := range p.Args {
			command += " " + shellQuote(a)
		}
		args, _ := json.Marshal(map[string]string{"command": command})
		text, isErr, err := c.tool(ctx, "bash", args)
		if err != nil {
			return nil, err
		}
		exit := 0
		if isErr {
			exit = 1
		}
		c.mu.Lock()
		c.nterm++
		tid := "term-" + strconv.Itoa(c.nterm)
		c.terms[tid] = &acpTerminal{output: text, exit: exit}
		c.mu.Unlock()
		return map[string]any{"terminalId": tid}, nil
	case "terminal/output", "terminal/wait_for_exit", "terminal/kill", "terminal/release":
		var p struct {
			TerminalID string `json:"terminalId"`
		}
		_ = json.Unmarshal(params, &p)
		c.mu.Lock()
		term := c.terms[p.TerminalID]
		if method == "terminal/release" {
			delete(c.terms, p.TerminalID)
		}
		c.mu.Unlock()
		if term == nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "no terminal " + p.TerminalID}
		}
		switch method {
		case "terminal/output":
			return map[string]any{"output": term.output, "truncated": false, "exitStatus": map[string]any{"exitCode": term.exit}}, nil
		case "terminal/wait_for_exit":
			return map[string]any{"exitCode": term.exit}, nil
		}
		return map[string]any{}, nil
	}
	return nil, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "unknown method " + method}
}

// agentTool decides a call of the agent's own tool (ADR 0015): it runs
// in the agent, outside KairoCode, so the user is asked. An answer given
// lets the same call (by title and input) once; else the call is refused
// for now, the question recorded, and the turn's round ends to wait for
// the answer (as an MCP call's, ADR 0007).
func (c *acpClient) agentTool(ctx context.Context, callID, title string, input json.RawMessage) (bool, error) {
	if c.mt.policy.Dry { // a try of a flow: the agent's own tools are not run (ADR 0017)
		return false, nil
	}
	key, err := callKey("agent:"+title, input)
	if err != nil {
		return false, nil
	}
	// The answer is used by this run of the agent (its MCP endpoint's
	// token is new each run), never by the agent's own id for the call: a
	// run after a crash, whose agent asks again, is asked again.
	c.mu.Lock()
	c.uses[key]++
	use := fmt.Sprintf("acp:%s:%s:%d", c.mt.proc, key, c.uses[key])
	c.mu.Unlock()
	found, ok, _, err := c.r.Store.UseGrant(ctx, c.turn, key, use)
	if err != nil {
		return false, err
	}
	if found {
		return ok, nil
	}
	subject := strings.TrimSpace(string(input))
	if len(subject) > 200 {
		subject = subject[:200] + "…"
	}
	c.mt.addAsk(workflows.AskRequest{Key: fmt.Sprintf("agent-%s-%d", key, c.round), Content: key,
		Tool: title, Subject: subject, Rule: "the agent's own tool: it runs in the agent, outside KairoCode's sandbox and record"})
	return false, nil
}

// ours reports whether a call the agent asks permission for, by its
// title, is of one of KairoCode's tools: named as the MCP server's, or
// bare by an agent that has no tools of its own.
func (c *acpClient) ours(title string) bool {
	name, ok := strings.CutPrefix(title, MCPServerName+"-")
	if !ok {
		name, ok = strings.CutPrefix(title, "mcp__"+MCPServerName+"__")
	}
	if !ok && !c.bare {
		return false // an agent's own tool may have the same name
	}
	return slices.Contains(c.tools, name)
}

// tool runs one of KairoCode's tools for the agent, as a call over MCP is
// run (ADR 0007): recorded, checked, never twice.
func (c *acpClient) tool(ctx context.Context, name string, args json.RawMessage) (string, bool, error) {
	if _, ok := c.mt.tools[name]; !ok {
		return "the agent may not use " + name + " here", true, nil
	}
	res, err := c.r.mcp.call(ctx, c.mt, name, args)
	if err != nil {
		return "", false, err
	}
	text := ""
	for _, ct := range res.Content {
		if tc, ok := ct.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	return text, res.IsError, nil
}

// rel is path, which the agent gives whole, in the working directory.
func (c *acpClient) rel(path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(c.dir, path)
	}
	rel, err := filepath.Rel(c.dir, filepath.Clean(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: path + " is outside the working directory"}
	}
	return rel, nil
}

// shellQuote quotes s for sh.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:,@+", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
