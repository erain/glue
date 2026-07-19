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

// Role is a named instruction profile with an optional model override.
// Roles are loaded from `<WorkDir>/roles/*.md` (with `name:`,
// `description:`, `model:` frontmatter) or supplied via [AgentOptions.Roles].
type Role struct {
	Name         string
	Description  string
	Instructions string
	Model        string
}

// ProjectContext is the WorkDir-loaded state injected by [LoadContext].
// AgentsMD is appended to the system prompt; Skills are exposed via
// [Session.Skill]; Roles are looked up by name during prompt configuration.
type ProjectContext struct {
	AgentsMD string
	Skills   map[string]Skill
	Roles    map[string]Role
}

// LoadContext loads the AGENTS.md chain (non-fatal if missing) and
// skills under `<workDir>/.agents/skills/<name>/SKILL.md`. An empty
// workDir returns an empty context.
//
// AGENTS.md is a chain, not a single file: every AGENTS.md from the
// repository root (the nearest ancestor containing `.git`, workDir
// included) down to workDir is concatenated, root first and nearest
// last — so in a monorepo a subdirectory can add local instructions on
// top of the repo-wide ones, and later (nearer) files read naturally
// as refinements. Outside a repository only `<workDir>/AGENTS.md` is
// consulted, matching the old single-file behavior.
func LoadContext(workDir string) (ProjectContext, error) {
	var ctx ProjectContext
	if strings.TrimSpace(workDir) == "" {
		return ctx, nil
	}

	agentsMD, err := loadAgentsMDChain(workDir)
	if err != nil {
		return ProjectContext{}, err
	}
	ctx.AgentsMD = agentsMD

	skills, err := loadSkills(filepath.Join(workDir, ".agents", "skills"))
	if err != nil {
		return ProjectContext{}, err
	}
	ctx.Skills = skills

	roles, err := loadRoles(filepath.Join(workDir, "roles"))
	if err != nil {
		return ProjectContext{}, err
	}
	ctx.Roles = roles
	return ctx, nil
}

// loadAgentsMDChain concatenates every AGENTS.md from the repo root
// down to workDir (root first). Missing files are skipped; read errors
// other than not-exist propagate.
func loadAgentsMDChain(workDir string) (string, error) {
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return "", err
	}

	// The chain's top is the nearest ancestor (workDir included) that
	// contains .git; without one the chain is workDir alone.
	top := abs
	for dir := abs; ; {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			top = dir
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	var chain []string
	for dir := abs; ; {
		chain = append([]string{dir}, chain...)
		if dir == top {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	var parts []string
	for _, dir := range chain {
		data, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if text := strings.TrimSpace(string(data)); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

func loadRoles(rolesDir string) (map[string]Role, error) {
	entries, err := os.ReadDir(rolesDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	roles := map[string]Role{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".md") {
			continue
		}
		path := filepath.Join(rolesDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		defaultName := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		parsed, err := parseMarkdownWithFrontmatter(string(data), defaultName)
		if err != nil {
			return nil, fmt.Errorf("glue: role %q frontmatter: %w", entry.Name(), err)
		}
		roles[parsed.Name] = Role{
			Name:         parsed.Name,
			Description:  parsed.Description,
			Instructions: parsed.Body,
			Model:        parsed.Model,
		}
	}
	if len(roles) == 0 {
		return nil, nil
	}
	return roles, nil
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

func cloneRoles(roles map[string]Role) map[string]Role {
	if len(roles) == 0 {
		return nil
	}
	out := make(map[string]Role, len(roles))
	for name, role := range roles {
		out[name] = role
	}
	return out
}

func appendRoleToSystemPrompt(systemPrompt string, role Role) string {
	if strings.TrimSpace(role.Instructions) == "" {
		return systemPrompt
	}
	block := "## Role: " + role.Name + "\n" + strings.TrimSpace(role.Instructions)
	if strings.TrimSpace(systemPrompt) == "" {
		return block
	}
	return strings.TrimSpace(systemPrompt) + "\n\n" + block
}
