package fs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/erain/glue"
)

// DefaultNavMaxResults caps the number of entries or matches a single
// navigation tool call returns when the call does not request otherwise.
const DefaultNavMaxResults = 200

// DefaultGrepMaxFileBytes is the per-file size ceiling for grep. Files
// larger than this are skipped so a single binary blob cannot stall a
// search.
const DefaultGrepMaxFileBytes = 512 * 1024

// NavOptions configures the read-only navigation tools (list_dir,
// find_files, grep).
type NavOptions struct {
	// WorkDir is the root the tools operate under. Required.
	WorkDir string

	// MaxResults caps returned entries/matches. Zero falls back to
	// DefaultNavMaxResults.
	MaxResults int

	// Blocklist refuses reading contents of matching paths in grep, and
	// hides them from find_files/list_dir output.
	Blocklist Blocklist

	// GrepMaxFileBytes is the per-file size ceiling for grep. Zero falls
	// back to DefaultGrepMaxFileBytes.
	GrepMaxFileBytes int
}

func (o NavOptions) maxResults() int {
	if o.MaxResults > 0 {
		return o.MaxResults
	}
	return DefaultNavMaxResults
}

func (o NavOptions) grepMaxFileBytes() int {
	if o.GrepMaxFileBytes > 0 {
		return o.GrepMaxFileBytes
	}
	return DefaultGrepMaxFileBytes
}

func navWorkDir(workDir string) (string, error) {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		return "", errors.New("fs: WorkDir is required")
	}
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return "", fmt.Errorf("fs: resolve WorkDir: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("fs: stat WorkDir: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("fs: WorkDir %q is not a directory", abs)
	}
	return abs, nil
}

// relArg returns the cleaned root for a navigation call, defaulting an
// empty path to the workspace root.
func navRoot(workDir, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		path = "."
	}
	return SafeJoin(workDir, path)
}

type listDirArgs struct {
	Path string `json:"path"`
	All  bool   `json:"all"`
}

// ListDirTool returns a read-only "list_dir" tool that lists the
// immediate entries of a directory inside opts.WorkDir.
func ListDirTool(opts NavOptions) (glue.Tool, error) {
	workDir, err := navWorkDir(opts.WorkDir)
	if err != nil {
		return glue.Tool{}, err
	}
	blocklist := opts.Blocklist
	limit := opts.maxResults()

	return glue.NewTool[listDirArgs](
		glue.ToolSpec{
			Name:        "list_dir",
			Description: "List the immediate entries of a directory inside the workspace (non-recursive). Read-only.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": { "type": "string", "description": "Directory path relative to the workspace. Defaults to the workspace root." },
    "all": { "type": "boolean", "description": "Include dotfiles. Default false.", "default": false }
  }
}`),
		},
		func(_ context.Context, args listDirArgs) (glue.ToolResult, error) {
			root, err := navRoot(workDir, args.Path)
			if err != nil {
				return glue.ErrorResult(err), nil
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				return glue.ErrorResult(err), nil
			}
			lines := make([]string, 0, len(entries))
			truncated := false
			for _, entry := range entries {
				name := entry.Name()
				if !args.All && strings.HasPrefix(name, ".") {
					continue
				}
				rel := relForDisplay(workDir, filepath.Join(root, name))
				if blocked, _ := blocklist.Match(rel); blocked {
					continue
				}
				if len(lines) >= limit {
					truncated = true
					break
				}
				switch {
				case entry.IsDir():
					lines = append(lines, name+"/")
				case entry.Type()&os.ModeSymlink != 0:
					lines = append(lines, name+"@")
				default:
					size := int64(-1)
					if info, err := entry.Info(); err == nil {
						size = info.Size()
					}
					lines = append(lines, fmt.Sprintf("%s\t%d", name, size))
				}
			}
			sort.Strings(lines)
			text := strings.Join(lines, "\n")
			if truncated {
				text += fmt.Sprintf("\n[... truncated at %d entries]", limit)
			}
			if text == "" {
				text = "(empty)"
			}
			return glue.TextResult(text), nil
		},
	), nil
}

type findFilesArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	MaxResults int    `json:"max_results"`
}

// FindTool returns a read-only "find_files" tool that recursively finds
// files whose basename matches a glob pattern under opts.WorkDir.
func FindTool(opts NavOptions) (glue.Tool, error) {
	workDir, err := navWorkDir(opts.WorkDir)
	if err != nil {
		return glue.Tool{}, err
	}
	blocklist := opts.Blocklist
	defaultLimit := opts.maxResults()

	return glue.NewTool[findFilesArgs](
		glue.ToolSpec{
			Name:        "find_files",
			Description: "Recursively find files whose name matches a glob pattern (e.g. *.go) under a workspace directory. Returns workspace-relative paths. Read-only; skips .git.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": { "type": "string", "description": "Glob matched against each file's basename, e.g. *.go or *_test.go." },
    "path": { "type": "string", "description": "Directory to search under, relative to the workspace. Defaults to the workspace root." },
    "max_results": { "type": "integer", "description": "Cap on returned paths." }
  },
  "required": ["pattern"]
}`),
		},
		func(_ context.Context, args findFilesArgs) (glue.ToolResult, error) {
			pattern := strings.TrimSpace(args.Pattern)
			if pattern == "" {
				return glue.ErrorResult(errors.New("fs: pattern is required")), nil
			}
			if _, err := filepath.Match(pattern, "probe"); err != nil {
				return glue.ErrorResult(fmt.Errorf("fs: invalid glob pattern: %w", err)), nil
			}
			root, err := navRoot(workDir, args.Path)
			if err != nil {
				return glue.ErrorResult(err), nil
			}
			limit := defaultLimit
			if args.MaxResults > 0 && args.MaxResults < limit {
				limit = args.MaxResults
			}

			matches := []string{}
			truncated := false
			walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if d.IsDir() {
					if d.Name() == ".git" {
						return filepath.SkipDir
					}
					return nil
				}
				if d.Type()&os.ModeSymlink != 0 {
					return nil
				}
				ok, _ := filepath.Match(pattern, d.Name())
				if !ok {
					return nil
				}
				rel := relForDisplay(workDir, p)
				if blocked, _ := blocklist.Match(rel); blocked {
					return nil
				}
				if len(matches) >= limit {
					truncated = true
					return filepath.SkipAll
				}
				matches = append(matches, rel)
				return nil
			})
			if walkErr != nil {
				return glue.ErrorResult(walkErr), nil
			}
			sort.Strings(matches)
			text := strings.Join(matches, "\n")
			if truncated {
				text += fmt.Sprintf("\n[... truncated at %d matches]", limit)
			}
			if text == "" {
				text = "(no matches)"
			}
			return glue.TextResult(text), nil
		},
	), nil
}

type grepArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	MaxResults int    `json:"max_results"`
}

// GrepTool returns a read-only "grep" tool that recursively searches file
// contents for a regular expression under opts.WorkDir.
func GrepTool(opts NavOptions) (glue.Tool, error) {
	workDir, err := navWorkDir(opts.WorkDir)
	if err != nil {
		return glue.Tool{}, err
	}
	blocklist := opts.Blocklist
	defaultLimit := opts.maxResults()
	maxFileBytes := opts.grepMaxFileBytes()

	return glue.NewTool[grepArgs](
		glue.ToolSpec{
			Name:        "grep",
			Description: "Recursively search file contents for a regular expression (RE2) under a workspace directory. Returns path:line:text matches. Read-only; skips .git, secret-shaped files, and files over the size ceiling.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": { "type": "string", "description": "RE2 regular expression matched against each line." },
    "path": { "type": "string", "description": "Directory to search under, relative to the workspace. Defaults to the workspace root." },
    "glob": { "type": "string", "description": "Optional glob to restrict matched files by basename, e.g. *.go." },
    "max_results": { "type": "integer", "description": "Cap on returned match lines." }
  },
  "required": ["pattern"]
}`),
		},
		func(_ context.Context, args grepArgs) (glue.ToolResult, error) {
			pattern := strings.TrimSpace(args.Pattern)
			if pattern == "" {
				return glue.ErrorResult(errors.New("fs: pattern is required")), nil
			}
			re, err := regexp.Compile(pattern)
			if err != nil {
				return glue.ErrorResult(fmt.Errorf("fs: invalid regular expression: %w", err)), nil
			}
			glob := strings.TrimSpace(args.Glob)
			if glob != "" {
				if _, err := filepath.Match(glob, "probe"); err != nil {
					return glue.ErrorResult(fmt.Errorf("fs: invalid glob: %w", err)), nil
				}
			}
			root, err := navRoot(workDir, args.Path)
			if err != nil {
				return glue.ErrorResult(err), nil
			}
			limit := defaultLimit
			if args.MaxResults > 0 && args.MaxResults < limit {
				limit = args.MaxResults
			}

			matches := []string{}
			truncated := false
			walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if d.IsDir() {
					if d.Name() == ".git" {
						return filepath.SkipDir
					}
					return nil
				}
				if d.Type()&os.ModeSymlink != 0 {
					return nil
				}
				if glob != "" {
					if ok, _ := filepath.Match(glob, d.Name()); !ok {
						return nil
					}
				}
				rel := relForDisplay(workDir, p)
				if blocked, _ := blocklist.Match(rel); blocked {
					return nil
				}
				info, err := d.Info()
				if err != nil || info.Size() > int64(maxFileBytes) {
					return nil
				}
				data, err := os.ReadFile(p)
				if err != nil {
					return nil
				}
				for i, line := range strings.Split(string(data), "\n") {
					if !re.MatchString(line) {
						continue
					}
					if len(matches) >= limit {
						truncated = true
						return filepath.SkipAll
					}
					matches = append(matches, fmt.Sprintf("%s:%d:%s", rel, i+1, Truncate(line, 500)))
				}
				return nil
			})
			if walkErr != nil {
				return glue.ErrorResult(walkErr), nil
			}
			text := strings.Join(matches, "\n")
			if truncated {
				text += fmt.Sprintf("\n[... truncated at %d matches]", limit)
			}
			if text == "" {
				text = "(no matches)"
			}
			return glue.TextResult(text), nil
		},
	), nil
}

// relForDisplay returns a slash-separated path relative to workDir for
// tool output. Falls back to the absolute path if it cannot be made
// relative.
func relForDisplay(workDir, abs string) string {
	rel, err := filepath.Rel(workDir, abs)
	if err != nil {
		return filepath.ToSlash(abs)
	}
	return filepath.ToSlash(rel)
}
