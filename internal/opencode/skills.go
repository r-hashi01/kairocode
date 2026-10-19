package opencode

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Skill is a skill: a SKILL.md, named in its frontmatter, and the
// directory around it.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Path is the SKILL.md.
	Path string `json:"path"`
}

// SkillDirs are where skills are looked for, in the order they apply (a
// later skill of a name wins), as opencode looks: the user's
// ~/.claude/skills and ~/.agents/skills; .claude/skills and .agents/skills
// from the repository's root down to dir; then skills/ and skill/ in each
// configuration directory (Dirs).
func SkillDirs(dir string) []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".claude", "skills"), filepath.Join(home, ".agents", "skills"))
	}
	for _, d := range project(dir) {
		out = append(out, filepath.Join(d, ".claude", "skills"), filepath.Join(d, ".agents", "skills"))
	}
	for _, c := range Dirs(dir) {
		out = append(out, filepath.Join(c, "skill"), filepath.Join(c, "skills"))
	}
	return out
}

// Skills reads every SKILL.md under dirs (at any depth). One whose
// frontmatter has no name is not a skill, as in opencode, and is skipped.
func Skills(dirs []string) (map[string]Skill, error) {
	out := map[string]Skill{}
	for _, root := range dirs {
		err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
			if err != nil {
				if path == root && os.IsNotExist(err) {
					return fs.SkipDir
				}
				return err
			}
			if e.IsDir() || e.Name() != "SKILL.md" {
				return nil
			}
			v, _, err := file{path: path}.read()
			if err != nil {
				return nil // not a skill opencode would read either
			}
			name := v.Get("name").String()
			if name == "" {
				return nil
			}
			out[name] = Skill{Name: name, Description: v.Get("description").String(), Path: path}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	return out, nil
}

// SkillList is the list of skills for the system prompt, as opencode
// writes it: those with a description, by name.
func SkillList(skills map[string]Skill) string {
	var b strings.Builder
	b.WriteString("Skills provide specialized instructions and workflows for specific tasks.\n")
	b.WriteString("Use the skill tool to load a skill when a task matches its description.\n")
	names := slices.Sorted(func(yield func(string) bool) {
		for n, s := range skills {
			if s.Description != "" && !yield(n) {
				return
			}
		}
	})
	if len(names) == 0 {
		b.WriteString("No skills are currently available.\n")
		return b.String()
	}
	b.WriteString("<available_skills>\n")
	for _, n := range names {
		s := skills[n]
		fmt.Fprintf(&b, "<skill><name>%s</name><description>%s</description><location>%s</location></skill>\n", s.Name, s.Description, s.Path)
	}
	b.WriteString("</available_skills>\n")
	return b.String()
}

// maxSkillFiles is how many of a skill's other files are listed.
const maxSkillFiles = 10

// Content is what the skill tool gives the model for a skill: its body,
// where it lives, and up to ten of its other files.
func (s Skill) Content() (string, error) {
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return "", err
	}
	_, body, err := Split(b)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(s.Path)
	var files []string
	err = filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if len(files) >= maxSkillFiles {
			return fs.SkipAll
		}
		if !e.Type().IsRegular() || path == s.Path {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return "", err
	}
	var out strings.Builder
	fmt.Fprintf(&out, "<skill_content name=%q>\n# Skill: %s\n\n%s\n\n", s.Name, s.Name, strings.TrimSpace(body))
	fmt.Fprintf(&out, "Base directory for this skill: %s\nRelative paths in this skill are relative to this base directory.\n", dir)
	out.WriteString("<skill_files>\n")
	for _, f := range files {
		fmt.Fprintf(&out, "<file>%s</file>\n", f)
	}
	out.WriteString("</skill_files>\n</skill_content>")
	return out.String(), nil
}
