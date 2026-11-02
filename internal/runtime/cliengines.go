package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/r-hashi01/kairocode/internal/store"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// What the CLI engines share (ADR 0007, 0014): the sessions KairoCode
// names, the conversation a session that starts after a switch is given,
// the models and whether a CLI is there. Codex's and Copilot's turns are
// in acpengine.go.

// ProviderAnthropic is the API engine's name as a provider (ADR 0014).
const ProviderAnthropic = "anthropic"

// maxCarried is how much of the conversation, at most, a CLI session that
// starts after a switch of engine is given (its latest part).
const maxCarried = 60_000

// carried is the conversation before a turn (head), as text, for a CLI
// session that starts without it (ADR 0014): what the user and the
// assistant said, not the tool calls.
func (r *Runtime) carried(ctx context.Context, head string) (string, error) {
	if head == "" {
		return "", nil
	}
	hashes, err := r.Store.Conversation(ctx, head)
	if err != nil {
		return "", err
	}
	ms, err := r.Store.Messages(ctx, hashes)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, m := range ms {
		text := strings.TrimSpace(m.Text())
		if text == "" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n\n", m.Role, text)
	}
	s := b.String()
	if len(s) > maxCarried {
		s = "…" + s[len(s)-maxCarried:]
	}
	return s, nil
}

// cliPrompt is a turn's prompt for a CLI: the user's message and the
// turn's mark; for a CLI session that starts, the agent's instructions
// (these CLIs take none apart) and the conversation so far before it. The
// message's images are named as files, unless imageParts (the agent reads
// images: they are given as parts).
func (r *Runtime) cliPrompt(ctx context.Context, in workflows.ClaudeTurnInput, starts, imageParts bool) (string, error) {
	user, err := r.Store.Message(ctx, in.Message)
	if err != nil {
		return "", err
	}
	images := ""
	if !imageParts {
		if images, err = r.imageNote(ctx, user); err != nil {
			return "", err
		}
	}
	prompt := user.Text() + images + "\n\n" + turnMark(in.Turn)
	if !starts {
		return prompt, nil
	}
	var system string
	if err := r.Store.GetJSON(ctx, in.System, store.KindSystem, &system); err != nil {
		return "", err
	}
	history, err := r.carried(ctx, in.History)
	if err != nil {
		return "", err
	}
	head := "<instructions>\n" + system + "\n</instructions>\n\n" +
		"Work only through the tools of the MCP server " + MCPServerName + ": they are the user's, and checked as they run.\n\n"
	if history != "" {
		head += "<conversation so far, with another model>\n" + history + "</conversation so far>\n\n"
	}
	return head + prompt, nil
}

// cliEnv is a CLI's environment: this process's, less secrets (the CLI
// uses its own sign-in) and what marks a Claude Code session it would run
// nested in.
func cliEnv(keep ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(name)
		switch {
		case slices.Contains(keep, name):
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

// cliModels are the models a CLI says it has: Codex's catalog (codex debug
// models), the Copilot CLI's list in its help on settings; Claude Code's
// are its aliases. nil if the CLI does not say.
func (r *Runtime) cliModels(ctx context.Context, engine string) []string {
	switch engine {
	case workflows.EngineClaudeCode:
		return []string{"opus", "sonnet", "haiku"}
	case workflows.EngineCodex:
		if os.Getenv(NoCLIs) == "1" {
			return nil
		}
		b, err := exec.CommandContext(ctx, "codex", "debug", "models").Output()
		if err != nil {
			return nil
		}
		var cat struct {
			Models []struct {
				Slug       string `json:"slug"`
				Visibility string `json:"visibility"`
			} `json:"models"`
		}
		if json.Unmarshal(b, &cat) != nil {
			return nil
		}
		var out []string
		for _, m := range cat.Models {
			if m.Visibility == "list" {
				out = append(out, m.Slug)
			}
		}
		return out
	case workflows.EngineCopilot:
		b, err := exec.CommandContext(ctx, r.cfg.CopilotPath, "help", "config").Output()
		if err != nil {
			return nil
		}
		// `model`: …, then one "- "name"" line per model.
		var out []string
		in := false
		for _, line := range strings.Split(string(b), "\n") {
			l := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(l, "`model`:"):
				in = true
			case in && strings.HasPrefix(l, "- \""):
				out = append(out, strings.Trim(strings.TrimPrefix(l, "- "), "\""))
			case in && l != "" && !strings.HasPrefix(l, "- "):
				in = false
			}
		}
		return out
	}
	return nil
}

// cliAvailable reports whether engine's CLI is installed.
func (r *Runtime) cliAvailable(engine string) bool {
	path := map[string]string{workflows.EngineClaudeCode: r.cfg.ClaudePath, workflows.EngineCodex: r.cfg.CodexPath,
		workflows.EngineCopilot: r.cfg.CopilotPath}[engine]
	_, err := exec.LookPath(path)
	return path != "" && err == nil
}

// cliSignedIn is a CLI's own word on whether the user is signed in:
// claude auth status, codex login status ("" if it cannot say).
func (r *Runtime) cliSignedIn(ctx context.Context, engine string) string {
	switch engine {
	case workflows.EngineClaudeCode:
		out, _ := exec.CommandContext(ctx, r.cfg.ClaudePath, "auth", "status", "--json").Output()
		var st struct {
			LoggedIn *bool `json:"loggedIn"`
		}
		if json.Unmarshal(out, &st) != nil || st.LoggedIn == nil {
			return ""
		}
		if *st.LoggedIn {
			return AuthSignedIn
		}
		return AuthNeeded
	case workflows.EngineCodex:
		if _, err := exec.LookPath("codex"); err != nil || os.Getenv(NoCLIs) == "1" {
			return ""
		}
		out, err := exec.CommandContext(ctx, "codex", "login", "status").CombinedOutput()
		switch {
		case err == nil && strings.Contains(string(out), "Logged in"):
			return AuthSignedIn
		case err != nil && ctx.Err() == nil:
			return AuthNeeded
		}
	}
	return ""
}
