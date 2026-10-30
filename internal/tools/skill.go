package tools

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/r-hashi01/kairocode/internal/opencode"
)

// SkillTool loads a skill (opencode's skill tool): the instructions of a
// SKILL.md the system prompt lists, where it lives and its other files.
var SkillTool = Def{
	Name: "skill",
	Description: "Load a skill: specialized instructions and workflows for a task. Call it with the name of a skill " +
		"listed in <available_skills> when a task matches its description, then follow what it says.",
	Schema: json.RawMessage(`{"properties":{
		"name":{"type":"string","description":"The skill's name, as listed"}},
		"required":["name"]}`),
	Effect: Read,
	Run:    runSkill,
}

func runSkill(_ context.Context, env Env, input json.RawMessage) Result {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return failed("invalid input: %v", err)
	}
	skills, err := opencode.Skills(opencode.SkillDirs(env.Dir))
	if err != nil {
		return failed("reading the skills: %v", err)
	}
	s, ok := skills[in.Name]
	if !ok {
		names := slices.Sorted(func(yield func(string) bool) {
			for n := range skills {
				if !yield(n) {
					return
				}
			}
		})
		return failed("no skill %q; the skills are: %s", in.Name, strings.Join(names, ", "))
	}
	content, err := s.Content()
	if err != nil {
		return failed("reading the skill %s: %v", in.Name, err)
	}
	return Result{Output: content}
}
