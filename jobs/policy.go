// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/erain/glue"
	"github.com/erain/glue/tools/coding"
	toolsfs "github.com/erain/glue/tools/fs"
)

// Decision is the outcome of evaluating a permission policy.
type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionDeny  Decision = "deny"
	DecisionAsk   Decision = "ask"
)

// PolicyRule matches a permission request and returns a Decision.
type PolicyRule struct {
	Name       string   `json:"name,omitempty"`
	Tool       string   `json:"tool,omitempty"`
	Action     string   `json:"action,omitempty"`
	Target     string   `json:"target,omitempty"`
	ArgvPrefix []string `json:"argv_prefix,omitempty"`
	Decision   Decision `json:"decision"`
}

// Policy evaluates ordered rules and falls back to Default.
type Policy struct {
	Name    string       `json:"name,omitempty"`
	Rules   []PolicyRule `json:"rules,omitempty"`
	Default Decision     `json:"default"`
}

// Evaluation records a policy Decision and the rule that produced it.
type Evaluation struct {
	Decision Decision `json:"decision"`
	Rule     string   `json:"rule"`
}

var deniedExecPrefixes = [][]string{
	{"git", "push"}, {"git", "remote"}, {"git", "config", "--global"},
	{"curl"}, {"wget"}, {"ssh"}, {"scp"},
}

// BuiltInPolicy returns a named built-in policy profile ("ask-all", "workspace-auto", or "yolo").
func BuiltInPolicy(name string, allowedBinaries ...string) (Policy, error) {
	switch strings.TrimSpace(name) {
	case "", "workspace-auto":
		return WorkspaceAutoPolicy(allowedBinaries...), nil
	case "ask-all":
		return Policy{Name: "ask-all", Default: DecisionAsk}, nil
	case "yolo":
		return Policy{Name: "yolo", Default: DecisionAllow}, nil
	default:
		return Policy{}, fmt.Errorf("unknown policy profile %q", name)
	}
}

// WorkspaceAutoPolicy builds the workspace-auto profile for the given allowlisted binaries.
func WorkspaceAutoPolicy(allowedBinaries ...string) Policy {
	bins := coding.NormalizeAllowedBinaries(allowedBinaries)
	if len(bins) == 0 {
		bins = coding.DefaultAllowedBinaries
	}
	rules := []PolicyRule{
		{Name: "workspace-write", Tool: "write_file", Action: "write_file", Target: "*", Decision: DecisionAllow},
		{Name: "workspace-edit", Tool: "edit_file", Action: "edit_file", Target: "*", Decision: DecisionAllow},
	}
	for _, prefix := range deniedExecPrefixes {
		rules = append(rules, PolicyRule{Name: "ask-" + strings.Join(prefix, "-"), Action: "exec", ArgvPrefix: slices.Clone(prefix), Decision: DecisionAsk})
	}
	for _, bin := range bins {
		rules = append(rules, PolicyRule{Name: "allow-exec-" + bin, Action: "exec", Target: "*", ArgvPrefix: []string{bin}, Decision: DecisionAllow})
	}
	return Policy{Name: "workspace-auto", Rules: rules, Default: DecisionAsk}
}

// LoadPolicyFile reads and validates a JSON policy file.
func LoadPolicyFile(filePath string) (Policy, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return Policy{}, err
	}
	return ParsePolicy(data)
}

// ParsePolicy decodes and validates a JSON policy document.
func ParsePolicy(data []byte) (Policy, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return Policy{}, errors.New("policy: empty policy document")
	}
	var p Policy
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("policy: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return Policy{}, errors.New("policy: trailing data after JSON object")
	}
	return p, p.Validate()
}

// Validate checks that Default, every Rule Decision, target globs, and argv prefixes are valid.
func (p Policy) Validate() error {
	if !validDecision(p.Default) {
		return fmt.Errorf("policy: invalid default decision %q (must be allow, deny, or ask)", p.Default)
	}
	for i, r := range p.Rules {
		if !validDecision(r.Decision) {
			return fmt.Errorf("policy: rule %d: invalid decision %q (must be allow, deny, or ask)", i, r.Decision)
		}
		if r.Target != "" {
			if _, err := path.Match(filepath.ToSlash(r.Target), ""); err != nil {
				return fmt.Errorf("policy: rule %d: invalid target glob %q: %w", i, r.Target, err)
			}
		}
		if slices.ContainsFunc(r.ArgvPrefix, func(s string) bool { return strings.TrimSpace(s) == "" }) {
			return fmt.Errorf("policy: rule %d: argv_prefix must not contain empty tokens", i)
		}
	}
	return nil
}

func validDecision(d Decision) bool {
	return d == DecisionAllow || d == DecisionDeny || d == DecisionAsk
}

// Evaluate returns the first matching rule's Decision, or Default when no rule matches.
func (p Policy) Evaluate(workDir string, req glue.PermissionRequest) Evaluation {
	tool, action := strings.TrimSpace(req.Tool), strings.TrimSpace(req.Action)
	if action == "" {
		action = tool
		if tool == "shell_exec" {
			action = "exec"
		}
	}
	if tool == "" {
		tool = action
		if action == "exec" {
			tool = "shell_exec"
		}
	}
	var args struct {
		Path string   `json:"path"`
		Argv []string `json:"argv"`
		Dir  string   `json:"dir"`
	}
	if len(req.Args) > 0 {
		_ = json.Unmarshal(req.Args, &args)
	}
	argv := args.Argv
	if len(argv) == 0 && (action == "exec" || tool == "shell_exec" || tool == "exec") {
		argv = strings.Fields(req.Target)
	}
	rawPath := strings.TrimSpace(args.Path)
	if rawPath == "" && action != "exec" {
		rawPath = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(req.Target), " (overwrite)"), " (replace_all)")
	}
	for i, r := range p.Rules {
		if r.Tool != "" && r.Tool != "*" && r.Tool != tool && !((r.Tool == "exec" || r.Tool == "shell_exec") && action == "exec") {
			continue
		}
		if r.Action != "" && r.Action != "*" && r.Action != action {
			continue
		}
		if len(r.ArgvPrefix) > 0 && (len(argv) < len(r.ArgvPrefix) || !slices.Equal(argv[:len(r.ArgvPrefix)], r.ArgvPrefix)) {
			continue
		}
		if action == "exec" {
			if strings.TrimSpace(args.Dir) != "" {
				absBase, absDir, ok := confinedPath(workDir, args.Dir)
				if (!ok && (r.Decision == DecisionAllow || r.Target != "")) || (r.Target != "" && r.Target != "*" && r.Target != "**" && (!ok || !matchTargetGlob(r.Target, absBase, absDir))) {
					continue
				}
			} else if r.Target != "" && r.Target != "*" && r.Target != "**" && !matchSlashGlob(filepath.ToSlash(r.Target), filepath.ToSlash(req.Target)) {
				continue
			}
		} else {
			absBase, absTarget, ok := confinedPath(workDir, rawPath)
			if (r.Target != "" && (!ok || !matchTargetGlob(r.Target, absBase, absTarget))) || (r.Target == "" && r.Decision == DecisionAllow && workDir != "" && (action == "write_file" || action == "edit_file") && !ok) {
				continue
			}
		}
		if r.Name != "" {
			return Evaluation{Decision: r.Decision, Rule: r.Name}
		}
		return Evaluation{Decision: r.Decision, Rule: fmt.Sprintf("rule[%d]", i)}
	}
	if p.Default == "" {
		return Evaluation{Decision: DecisionAsk, Rule: "default"}
	}
	return Evaluation{Decision: p.Default, Rule: "default"}
}

func confinedPath(workDir, target string) (string, string, bool) {
	workDir, target = strings.TrimSpace(workDir), strings.TrimSpace(target)
	if workDir == "" || target == "" {
		return "", "", false
	}
	rawBase, err := filepath.Abs(workDir)
	if err != nil {
		return "", "", false
	}
	absBase := rawBase
	if eval, err := filepath.EvalSymlinks(rawBase); err == nil {
		absBase = eval
	}
	rel := target
	if filepath.IsAbs(target) {
		clean := filepath.Clean(target)
		if r, err := filepath.Rel(absBase, clean); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		} else if r, err := filepath.Rel(rawBase, clean); err == nil {
			rel = r
		} else {
			return "", "", false
		}
	}
	joined, err := toolsfs.SafeJoin(absBase, rel)
	if err != nil {
		return "", "", false
	}
	for cur := joined; ; {
		if _, err := os.Lstat(cur); err == nil {
			eval, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", "", false
			}
			relEval, err := filepath.Rel(absBase, eval)
			if err != nil || relEval == ".." || strings.HasPrefix(relEval, ".."+string(filepath.Separator)) {
				return "", "", false
			}
			return absBase, joined, true
		} else if !os.IsNotExist(err) {
			return "", "", false
		}
		if next := filepath.Dir(cur); next != cur {
			cur = next
		} else {
			return "", "", false
		}
	}
}

func matchTargetGlob(pattern, absBase, absTarget string) bool {
	pat := filepath.ToSlash(filepath.Clean(pattern))
	if pat == "*" || pat == "**" {
		return true
	}
	rel, err := filepath.Rel(absBase, absTarget)
	if err != nil {
		return false
	}
	if filepath.IsAbs(pattern) {
		return matchSlashGlob(pat, filepath.ToSlash(absTarget))
	}
	return matchSlashGlob(pat, filepath.ToSlash(rel)) || matchSlashGlob(filepath.ToSlash(filepath.Join(absBase, pattern)), filepath.ToSlash(absTarget))
}

func matchSlashGlob(pat, name string) bool {
	ok, err := path.Match(pat, name)
	return err == nil && ok
}
