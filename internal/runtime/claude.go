package runtime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// continuePrompt is what a turn sends when Claude Code already has its prompt
// and is resumed after the run was cut short.
const continuePrompt = "Continue where you left off."

// claudeSessionID is the Claude Code session of a KairoCode session: a UUID
// made from its id, so it is the same every time (AGENTS.md, invariant 8).
func claudeSessionID(session string) string {
	h := sha256.Sum256([]byte("kairocode/claude-session:" + session))
	h[6] = h[6]&0x0f | 0x40 // version 4
	h[8] = h[8]&0x3f | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// claudeEnv is the environment of the claude process: this process's, less
// what marks a Claude Code session it would run nested in, and less API keys
// and other secrets, so it uses the user's own Claude Code login (ADR 0007,
// 6). CLAUDE_CODE_OAUTH_TOKEN is that login (claude setup-token) and stays.
func claudeEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(name)
		switch {
		case name == "CLAUDE_CODE_OAUTH_TOKEN":
		case name == "CLAUDECODE", name == "CLAUDE_CODE_ENTRYPOINT", name == "CLAUDE_CODE_SSE_PORT":
			continue
		case strings.Contains(upper, "KEY"), strings.Contains(upper, "TOKEN"),
			strings.Contains(upper, "SECRET"), strings.Contains(upper, "PASSWORD"):
			continue
		}
		env = append(env, kv)
	}
	return env
}

// claudeToolTimeout is how long Claude Code waits for a call of
// KairoCode's tools: as long as one may run (bash_wait waits up to a day,
// ADR 0028), and a minute more. Its own default is a minute, after which
// it gave up on the call and the model never had its result.
const claudeToolTimeout = tools.JobMaxTimeout + time.Minute

// claudeTurnEnv is the environment of a turn's claude process: claudeEnv,
// and how long it waits for KairoCode's tools (MCP_TOOL_TIMEOUT, unless the
// user set it longer).
func claudeTurnEnv() []string {
	env := claudeEnv()
	want := claudeToolTimeout.Milliseconds()
	if v, err := strconv.ParseInt(os.Getenv("MCP_TOOL_TIMEOUT"), 10, 64); err == nil && v >= want {
		return env
	}
	return append(env, fmt.Sprintf("MCP_TOOL_TIMEOUT=%d", want))
}

// claudeTranscript is where Claude Code keeps session sid, or "" if it has no
// such session.
func claudeTranscript(sid string) string {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".claude")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "projects", "*", sid+".jsonl"))
	if len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// turnMark ends a turn's prompt, so the turn can tell from Claude Code's
// transcript whether its prompt is there already.
func turnMark(turn string) string { return "[kairocode turn " + turn + "]" }

func fileContains(path, s string) bool {
	b, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(b), s)
}

// stopStaleClaude stops the claude process a run of this session left
// behind, if it is still running, before the session is resumed in another.
func (r *Runtime) stopStaleClaude(ctx context.Context, sid string) error {
	return r.stopStale(ctx, sid, sid)
}

// stopStale stops the process kept under key, if it is still running and
// its command line has match.
func (r *Runtime) stopStale(ctx context.Context, key, match string) error {
	sid := key
	pid, err := r.Store.ClaudeProc(ctx, sid)
	if err != nil || pid == 0 {
		return err
	}
	// Only a process that is still a claude of this session: the pid may be
	// another process's by now.
	out, err := exec.CommandContext(ctx, "ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err == nil && strings.Contains(string(out), match) {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	return r.Store.SetClaudeProc(ctx, sid, 0)
}

// streamLine is the part of Claude Code's stream-json output a turn reads.
type streamLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Event   struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	Result          string  `json:"result"`
	IsError         bool    `json:"is_error"`
	NumTurns        int     `json:"num_turns"`
	Usage           struct {
		Input      int64 `json:"input_tokens"`
		Output     int64 `json:"output_tokens"`
		CacheRead  int64 `json:"cache_read_input_tokens"`
		CacheWrite int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// claudeImageInput is the user's message as Claude Code's stream-json
// input, prompt and then the images, when it has images; nil when not.
func (r *Runtime) claudeImageInput(ctx context.Context, user msg.Message, prompt string) ([]byte, error) {
	if !slices.ContainsFunc(user.Parts, func(p msg.Part) bool { return p.Type == msg.Image }) {
		return nil, nil
	}
	ms, err := r.withImages(ctx, []msg.Message{user})
	if err != nil {
		return nil, err
	}
	content := []any{map[string]any{"type": "text", "text": prompt}}
	for _, p := range ms[0].Parts {
		if p.Type == msg.Image {
			content = append(content, map[string]any{"type": "image", "source": map[string]any{
				"type": "base64", "media_type": p.MediaType, "data": base64.StdEncoding.EncodeToString(p.Data)}})
		}
	}
	line, err := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}})
	if err != nil {
		return nil, err
	}
	return append(line, '\n'), nil
}

// claudeTurn runs a turn in the user's Claude Code (ADR 0007): built-in tools
// off, KairoCode's tools over MCP, the user's settings, hooks and plugins not
// loaded. Done again after the process died, it resumes the same session.
func (r *Runtime) claudeTurn(t *kairo.TaskContext, in workflows.ClaudeTurnInput) (workflows.ClaudeTurnOutput, error) {
	var system string
	if err := r.Store.GetJSON(t, in.System, store.KindSystem, &system); err != nil {
		return workflows.ClaudeTurnOutput{}, err
	}
	user, err := r.Store.Message(t, in.Message)
	if err != nil {
		return workflows.ClaudeTurnOutput{}, err
	}
	sid := claudeSessionID(in.Session)
	if in.Epoch > 0 {
		// A Claude Code session of its own in each epoch after a switch
		// (ADR 0014); the first keeps the id it always had.
		sid = claudeSessionID(fmt.Sprintf("%s#%d", in.Session, in.Epoch))
	}
	if err := r.stopStaleClaude(t, sid); err != nil {
		return workflows.ClaudeTurnOutput{}, kairo.Retryable(err)
	}
	// Claude Code's own record says what to do: start the session or resume
	// it; send the prompt, or, if it has the prompt already (this turn was
	// cut short), ask it to go on.
	transcript := claudeTranscript(sid)
	mark := turnMark(in.Turn)
	prompt := user.Text() + "\n\n" + mark
	if transcript == "" && in.History != "" {
		// A session that starts after a switch: the conversation so far.
		history, err := r.carried(t, in.History)
		if err != nil {
			return workflows.ClaudeTurnOutput{}, err
		}
		prompt = "<conversation so far, with another model>\n" + history + "</conversation so far>\n\n" + prompt
	}
	// The user's images go with the prompt as content (ADR 0026): by
	// stream-json on stdin, the input that takes them.
	var input []byte
	if transcript != "" && fileContains(transcript, mark) {
		// Claude Code has the prompt: tell it the user's answers, or just
		// to go on after being cut short.
		prompt = continuePrompt
		if in.Note != "" {
			prompt = in.Note
		}
	} else if input, err = r.claudeImageInput(t, user, prompt); err != nil {
		return workflows.ClaudeTurnOutput{}, err
	}
	url, mt, closeMCP, err := r.mcp.open(t, in.Turn, in.Dir, in.Tools, in.Policy, in.Hooks, in.Session, in.Ext)
	if err != nil {
		return workflows.ClaudeTurnOutput{}, err
	}
	defer closeMCP()
	mcpConfig, err := json.Marshal(map[string]any{"mcpServers": map[string]any{
		MCPServerName: map[string]any{"type": "http", "url": url, "timeout": claudeToolTimeout.Milliseconds()},
	}})
	if err != nil {
		return workflows.ClaudeTurnOutput{}, err
	}
	args := []string{"-p", prompt}
	if input != nil {
		args = []string{"-p", "--input-format", "stream-json"}
	}
	if transcript != "" {
		args = append(args, "--resume", sid)
	} else {
		args = append(args, "--session-id", sid)
	}
	allowed := make([]string, len(in.Tools))
	for i, name := range in.Tools {
		allowed[i] = "mcp__" + MCPServerName + "__" + name
	}
	args = append(args,
		"--tools", "",
		"--setting-sources", "",
		"--disable-slash-commands",
		"--strict-mcp-config", "--mcp-config", string(mcpConfig),
		"--allowedTools", strings.Join(allowed, ","),
		"--system-prompt", system,
		"--output-format", "stream-json", "--verbose", "--include-partial-messages",
	)
	if in.Model != "" {
		args = append(args, "--model", in.Model)
	}
	if in.Effort != "" {
		args = append(args, "--effort="+in.Effort)
	}
	cmd := exec.CommandContext(t, r.cfg.ClaudePath, args...)
	cmd.Dir = in.Dir
	cmd.Env = claudeTurnEnv()
	cmd.Stdin = nil // /dev/null: claude -p otherwise waits for input
	if input != nil {
		cmd.Stdin = bytes.NewReader(input) // one message; the end of it ends the input
	}
	cmd.SysProcAttr = claudeProcAttr()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return workflows.ClaudeTurnOutput{}, err
	}
	if err := cmd.Start(); err != nil {
		return workflows.ClaudeTurnOutput{}, fmt.Errorf("starting %s: %w", r.cfg.ClaudePath, err)
	}
	if err := r.Store.SetClaudeProc(t, sid, cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return workflows.ClaudeTurnOutput{}, kairo.Retryable(err)
	}
	defer r.Store.SetClaudeProc(context.WithoutCancel(t), sid, 0)
	var result *streamLine
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		var l streamLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		switch {
		case l.Type == "stream_event" && l.Event.Type == "content_block_delta" && l.Event.Delta.Type == "text_delta" && l.ParentToolUseID == nil:
			t.Emit([]byte(l.Event.Delta.Text))
		case l.Type == "result":
			result = &l
		}
	}
	waitErr := cmd.Wait()
	if t.Err() != nil {
		return workflows.ClaudeTurnOutput{}, t.Err()
	}
	if result == nil || waitErr != nil {
		// The run was cut short: doing the turn again resumes it.
		return workflows.ClaudeTurnOutput{}, kairo.Retryable(fmt.Errorf("claude: %v: %s", errors.Join(waitErr, sc.Err()), tail(stderr.String(), 200)))
	}
	// The run ended with a result: every tool result it was handed reached it.
	if err := r.mcp.ended(t, mt); err != nil {
		return workflows.ClaudeTurnOutput{}, kairo.Retryable(err)
	}
	text := result.Result
	switch {
	case result.IsError && (strings.Contains(text, "/login") || strings.Contains(text, "Invalid API key")):
		r.authSeen(workflows.EngineClaudeCode, AuthNeeded, nil)
	case !result.IsError:
		r.authSeen(workflows.EngineClaudeCode, AuthSignedIn, nil)
	}
	if result.IsError {
		text = "[Claude Code ended the turn with an error: " + result.Subtype + "]\n" + text
	}
	h, err := r.Store.PutMessage(t, msg.Message{Role: msg.Assistant, Parts: []msg.Part{{Type: msg.Text, Text: text}}})
	if err != nil {
		return workflows.ClaudeTurnOutput{}, kairo.Retryable(err)
	}
	return workflows.ClaudeTurnOutput{
		Asks:    mt.takeAsks(),
		Message: h,
		Turns:   result.NumTurns,
		Usage: msg.Usage{Input: result.Usage.Input, Output: result.Usage.Output,
			CacheRead: result.Usage.CacheRead, CacheWrite: result.Usage.CacheWrite},
	}, nil
}

func tail(s string, n int) string {
	if len(s) > n {
		s = "…" + s[len(s)-n:]
	}
	// An image's base64 (the stream-json input echoed back) is not kept
	// in an error (ADR 0026).
	return longRun.ReplaceAllString(s, "[…]")
}

// longRun is a long run of base64.
var longRun = regexp.MustCompile(`[A-Za-z0-9+/=]{64,}`)

// claudeDecide asks Claude Code for a flow's decision (decide.go),
// fitting schema: one run, no tools, nothing kept of it. claude starts
// here, with the turns' (ADR 0007).
func (r *Runtime) claudeDecide(t *kairo.TaskContext, dir, prompt, model string, schema json.RawMessage) (workflows.DecideOutput, error) {
	args := []string{"-p", prompt,
		"--output-format", "json", "--json-schema", string(schema),
		"--tools", "", "--setting-sources", "", "--disable-slash-commands", "--strict-mcp-config",
		"--no-session-persistence", "--system-prompt", decideSystem,
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	cmd := exec.CommandContext(t, r.cfg.ClaudePath, args...)
	cmd.Dir = dir
	cmd.Env = claudeEnv()
	cmd.Stdin = nil
	cmd.SysProcAttr = claudeProcAttr()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	b, runErr := cmd.Output()
	if t.Err() != nil {
		return workflows.DecideOutput{}, t.Err()
	}
	var res struct {
		IsError          bool            `json:"is_error"`
		Subtype          string          `json:"subtype"`
		StructuredOutput json.RawMessage `json:"structured_output"`
		Usage            struct {
			Input      int64 `json:"input_tokens"`
			Output     int64 `json:"output_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	// A result is read whatever claude exited with: one that is an error
	// (it would not decide) exits non-zero too, and is an outcome.
	var typ struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(b, &res) != nil || json.Unmarshal(b, &typ) != nil || typ.Type != "result" {
		// No result: nothing was decided, and asking again is safe.
		return workflows.DecideOutput{}, kairo.Retryable(fmt.Errorf("claude: %v: %s", runErr, tail(stderr.String(), 200)))
	}
	out := workflows.DecideOutput{Usage: msg.Usage{Input: res.Usage.Input, Output: res.Usage.Output,
		CacheRead: res.Usage.CacheRead, CacheWrite: res.Usage.CacheWrite}}
	var v struct {
		Value json.RawMessage `json:"value"`
	}
	if res.IsError || json.Unmarshal(res.StructuredOutput, &v) != nil || len(v.Value) == 0 {
		out.Error = "Claude Code made no decision"
		if res.Subtype != "" && res.Subtype != "success" {
			out.Error += " (" + res.Subtype + ")"
		}
		return out, nil
	}
	out.OK, out.Value = true, v.Value
	return out, nil
}
