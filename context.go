package glue

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Skill is a Markdown-defined reusable prompt loaded from
// `<WorkDir>/.agents/skills/<name>/SKILL.md` or supplied directly via
// [AgentOptions.Skills].
type Skill struct {
	Name         string
	Description  string
	Instructions string
}

// ProjectContext is the subset of [AgentOptions.WorkDir] state currently
// loaded by [LoadContext]. AGENTS.md is appended to the system prompt and
// Skills are exposed via [Session.Skill]. Roles are added in a follow-up
// issue.
type ProjectContext struct {
	AgentsMD string
	Skills   map[string]Skill
}

// LoadContext loads AGENTS.md (non-fatal if missing) and skills under
// `<workDir>/.agents/skills/<name>/SKILL.md`. An empty workDir returns an
// empty context.
func LoadContext(workDir string) (ProjectContext, error) {
	var ctx ProjectContext
	if strings.TrimSpace(workDir) == "" {
		return ctx, nil
	}

	if data, err := os.ReadFile(filepath.Join(workDir, "AGENTS.md")); err == nil {
		ctx.AgentsMD = strings.TrimSpace(string(data))
	} else if !errors.Is(err, os.ErrNotExist) {
		return ProjectContext{}, err
	}

	skills, err := loadSkills(filepath.Join(workDir, ".agents", "skills"))
	if err != nil {
		return ProjectContext{}, err
	}
	ctx.Skills = skills
	return ctx, nil
}

func loadSkills(skillsDir string) (map[string]Skill, error) {
	entries, err := os.ReadDir(skillsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	skills := map[string]Skill{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(skillsDir, entry.Name(), "SKILL.md")
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		parsed, err := parseMarkdownWithFrontmatter(string(data), entry.Name())
		if err != nil {
			return nil, fmt.Errorf("glue: skill %q frontmatter: %w", entry.Name(), err)
		}
		skills[parsed.Name] = Skill{
			Name:         parsed.Name,
			Description:  parsed.Description,
			Instructions: parsed.Body,
		}
	}
	if len(skills) == 0 {
		return nil, nil
	}
	return skills, nil
}

type parsedMarkdown struct {
	Name        string
	Description string
	Model       string
	Body        string
}

func parseMarkdownWithFrontmatter(content string, defaultName string) (parsedMarkdown, error) {
	parsed := parsedMarkdown{Name: defaultName, Body: strings.TrimSpace(content)}
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "---\n") && !strings.HasPrefix(trimmed, "---\r\n") {
		return parsed, nil
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(trimmed, "---\n"), "---\r\n")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return parsed, fmt.Errorf("frontmatter is unterminated (expected closing '---')")
	}

	frontmatter := rest[:end]
	body := strings.TrimPrefix(rest[end:], "\n---")
	body = strings.TrimPrefix(body, "\n")
	parsed.Body = strings.TrimSpace(body)
	for _, line := range strings.Split(frontmatter, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "name":
			if v := strings.TrimSpace(value); v != "" {
				parsed.Name = v
			}
		case "description":
			parsed.Description = strings.TrimSpace(value)
		case "model":
			parsed.Model = strings.TrimSpace(value)
		}
	}
	return parsed, nil
}

func composeSystemPrompt(base string, agentsMD string, skills map[string]Skill) string {
	parts := make([]string, 0, 3)
	if strings.TrimSpace(base) != "" {
		parts = append(parts, strings.TrimSpace(base))
	}
	if strings.TrimSpace(agentsMD) != "" {
		parts = append(parts, strings.TrimSpace(agentsMD))
	}
	if len(skills) > 0 {
		names := make([]string, 0, len(skills))
		for name := range skills {
			names = append(names, name)
		}
		sort.Strings(names)
		var b strings.Builder
		b.WriteString("## Available Skills")
		for _, name := range names {
			skill := skills[name]
			b.WriteString("\n- ")
			b.WriteString(skill.Name)
			if skill.Description != "" {
				b.WriteString(" — ")
				b.WriteString(skill.Description)
			}
		}
		parts = append(parts, b.String())
	}
	return strings.Join(parts, "\n\n")
}

func buildSkillPrompt(skill Skill, args any) (string, error) {
	var b strings.Builder
	b.WriteString(skill.Instructions)
	if args != nil {
		data, err := json.MarshalIndent(args, "", "  ")
		if err != nil {
			return "", fmt.Errorf("glue: encode skill args: %w", err)
		}
		b.WriteString("\n\nArguments:\n")
		b.Write(data)
	}
	return b.String(), nil
}

func cloneSkills(skills map[string]Skill) map[string]Skill {
	if len(skills) == 0 {
		return nil
	}
	out := make(map[string]Skill, len(skills))
	for name, skill := range skills {
		out[name] = skill
	}
	return out
}
