package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/r-hashi01/kairocode/internal/api"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/opencode"
	"github.com/r-hashi01/kairocode/internal/permission"
	"github.com/r-hashi01/kairocode/internal/sandbox"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// sessionDir is the working directory a session started with.
func (r *Runtime) sessionDir(ctx context.Context, session string) (string, error) {
	info, err := r.K.Get(ctx, session)
	if err != nil {
		return "", err
	}
	var in workflows.SessionInput
	if err := json.Unmarshal(input(info), &in); err != nil {
		return "", err
	}
	return in.Dir, nil
}

// Commands are the commands a session can run: opencode's commands/*.md for
// its working directory (ADR 0002), and its skills as /name where no
// command has the name, by name.
func (r *Runtime) Commands(ctx context.Context, session string) ([]opencode.Command, error) {
	dir, err := r.sessionDir(ctx, session)
	if err != nil {
		return nil, err
	}
	cs, err := opencode.Commands(opencode.DefinitionDirs(dir))
	if err != nil {
		return nil, err
	}
	skills, err := opencode.Skills(opencode.SkillDirs(dir))
	if err != nil {
		return nil, err
	}
	for name, s := range skills {
		if _, taken := cs[name]; !taken {
			cs[name] = opencode.Command{Name: name, Description: s.Description, Skill: true}
		}
	}
	if _, taken := cs[initCommand.Name]; !taken {
		cs[initCommand.Name] = initCommand
	}
	out := make([]opencode.Command, 0, len(cs))
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for n := range cs {
			if !yield(n) {
				return
			}
		}
	}) {
		out = append(out, cs[name])
	}
	return out, nil
}

var ErrNoCommand = api.ErrNoCommand

// RunCommand runs command name with args in session: its template is
// expanded as opencode expands it (its !`commands` run in the sandbox,
// read-only and offline) and sent as the user's message, to the command's
// agent and model if it names them. It returns the turn's number. What was
// expanded is recorded as the message: a resumed turn does not expand it
// again.
func (r *Runtime) RunCommand(ctx context.Context, session, name, args string) (int, error) {
	dir, err := r.sessionDir(ctx, session)
	if err != nil {
		return 0, err
	}
	cs, err := opencode.Commands(opencode.DefinitionDirs(dir))
	if err != nil {
		return 0, err
	}
	c, ok := cs[name]
	if !ok {
		if name == initCommand.Name {
			skills, err := opencode.Skills(opencode.SkillDirs(dir))
			if err != nil {
				return 0, err
			}
			if _, skill := skills[name]; !skill {
				return r.sendText(ctx, session, initText(args), "", "")
			}
		}
		return r.runSkill(ctx, session, dir, name, args)
	}
	rules, err := loadRules(dir, nil)
	if err != nil {
		return 0, err
	}
	may := func(tool, subject string) bool {
		return permission.Decide(rules, tool, subject).Action != permission.Deny
	}
	run := func(cmd string) (string, error) { return shellOutput(ctx, dir, cmd) }
	if c.NoShell { // Claude Code's, its allowed-tools not allowing Bash
		run = func(cmd string) (string, error) {
			return "(not run: the command's allowed-tools do not allow Bash)", nil
		}
	}
	text, err := opencode.Expand(c, args, dir, run, may)
	if err != nil {
		return 0, fmt.Errorf("/%s: %w", name, err)
	}
	return r.sendText(ctx, session, text, c.Agent, c.Model)
}

// sendText sends text as the user's message, to agent and model if named.
func (r *Runtime) sendText(ctx context.Context, session, text, agent, model string) (int, error) {
	h, err := r.Store.PutMessage(ctx, msg.Message{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: text}}})
	if err != nil {
		return 0, err
	}
	return r.signal(ctx, session, workflows.SessionEvent{Kind: workflows.EventMessage, Message: h, Agent: agent, Model: model})
}

// initCommand is KairoCode's own /init (M8): it has the model read the
// repository and write a draft AGENTS.md. A project's command or skill of
// the name is run in its place.
var initCommand = opencode.Command{Name: "init", Hint: "[what to look at most]",
	Description: "Read the repository and write a draft AGENTS.md for coding agents (or improve the one there)"}

// initText is /init's message to the model; args, what the user adds.
func initText(args string) string {
	text := `Read this repository and write AGENTS.md at its root: the instructions a coding agent (you, another one, or a person new to it) needs to work here well. It is a draft for the user to read and change.

First look, without guessing:
- the README and other docs; the build and dependency files (go.mod, package.json, pyproject.toml, Cargo.toml, Makefile, …); the CI configuration; the layout of the source;
- instructions written for other agents, to fold in what holds: AGENTS.md, CLAUDE.md, .cursor/rules or .cursorrules, .github/copilot-instructions.md, .windsurfrules.

Then write, briefly, and only what you found:
- what the project is, in a sentence or two;
- the commands to build, test (all, and a single test), lint and format, as they are run here;
- the layout: the main directories and what is in each, and the boundaries between them that are not obvious;
- the conventions the code follows that a newcomer would get wrong;
- what to watch out for (generated files, slow or flaky tests, setup it needs).

Leave out what anyone can see from a file listing, general advice for good code, and anything you did not find in the repository. If AGENTS.md is there already, improve it: keep what is right and the user's own wording, fix what is out of date, and add what is missing. When you are done, say in a few lines what you wrote and what you were unsure of.`
	if args = strings.TrimSpace(args); args != "" {
		text += "\n\nThe user adds: " + args
	}
	return text
}

// runSkill runs skill name as /name: a turn whose message is the skill's
// instructions (as the skill tool gives them) and what the user asked of
// it. What is sent is recorded as the message: a resumed turn does not
// read the skill again.
func (r *Runtime) runSkill(ctx context.Context, session, dir, name, args string) (int, error) {
	skills, err := opencode.Skills(opencode.SkillDirs(dir))
	if err != nil {
		return 0, err
	}
	s, ok := skills[name]
	if !ok {
		return 0, fmt.Errorf("%w: /%s", ErrNoCommand, name)
	}
	content, err := s.Content()
	if err != nil {
		return 0, fmt.Errorf("/%s: %w", name, err)
	}
	text := "The user runs the skill " + name + ". Follow it.\n\n" + content
	if args = strings.TrimSpace(args); args != "" {
		text += "\n\nWhat the user asks of it: " + args
	}
	h, err := r.Store.PutMessage(ctx, msg.Message{Role: msg.User, Parts: []msg.Part{{Type: msg.Text, Text: text}}})
	if err != nil {
		return 0, err
	}
	return r.signal(ctx, session, workflows.SessionEvent{Kind: workflows.EventMessage, Message: h})
}

type SkillsInfo = api.SkillsInfo

// Skills are the skills of the session's working directory, and what of
// them is not taken.
func (r *Runtime) Skills(ctx context.Context, session string) (SkillsInfo, error) {
	dir, err := r.sessionDir(ctx, session)
	if err != nil {
		return SkillsInfo{}, err
	}
	skills, problems, err := opencode.CheckedSkills(opencode.SkillDirs(dir))
	if err != nil {
		return SkillsInfo{}, err
	}
	out := SkillsInfo{Skills: []opencode.Skill{}, Problems: problems}
	for _, name := range slices.Sorted(maps.Keys(skills)) {
		out.Skills = append(out.Skills, skills[name])
	}
	return out, nil
}

// shellLimit is how long a command's !`command` may run.
const shellLimit = 30 * time.Second

// shellOutput is what cmd prints, run by sh in dir's sandbox, read-only
// and offline, for up to shellLimit. As in opencode, a command that fails
// gives what it printed.
func shellOutput(ctx context.Context, dir, cmd string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, shellLimit)
	defer cancel()
	c, err := sandbox.Command(ctx, sandbox.Policy{Mode: sandbox.ReadOnly, Root: dir}, "", "sh", "-c", cmd)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	c.Stdout = &out
	if err := c.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("!`%s` ran longer than %v", cmd, shellLimit)
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return "", err
		}
	}
	return strings.TrimRight(out.String(), "\n"), nil
}
