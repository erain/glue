package fs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/erain/glue"
)

// DefaultWriteMaxBytes caps a single write_file call when
// FileWriteOptions.MaxBytes is zero.
const DefaultWriteMaxBytes = 1024 * 1024

// FileWriteOptions configures FileWrite.
type FileWriteOptions struct {
	// WorkDir is the root the tool writes under. Required.
	WorkDir string

	// AllowOverwrite is the host-level overwrite policy. Existing files
	// are overwritten only when this is true and the tool call also sets
	// overwrite=true.
	AllowOverwrite bool

	// MaxBytes caps content size. Zero falls back to DefaultWriteMaxBytes.
	MaxBytes int

	// Blocklist refuses paths matching these glob patterns. Pass
	// fs.Default().Merge(extras...) to layer extras on top of the
	// shipped defaults.
	Blocklist Blocklist
}

type fileWriteArgs struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Overwrite bool   `json:"overwrite,omitempty"`
}

// FileWrite returns a glue.Tool named "write_file" that writes UTF-8 text
// inside opts.WorkDir using a temp-file-plus-rename flow. The tool is
// permission-gated via ToolSpec metadata.
func FileWrite(opts FileWriteOptions) (glue.Tool, error) {
	workDir := strings.TrimSpace(opts.WorkDir)
	if workDir == "" {
		return glue.Tool{}, errors.New("fs: WorkDir is required")
	}
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return glue.Tool{}, fmt.Errorf("fs: resolve WorkDir: %w", err)
	}
	info, err := os.Stat(absWorkDir)
	if err != nil {
		return glue.Tool{}, fmt.Errorf("fs: stat WorkDir: %w", err)
	}
	if !info.IsDir() {
		return glue.Tool{}, fmt.Errorf("fs: WorkDir %q is not a directory", absWorkDir)
	}
	max := opts.MaxBytes
	if max < 0 {
		return glue.Tool{}, errors.New("fs: MaxBytes must be non-negative")
	}
	if max == 0 {
		max = DefaultWriteMaxBytes
	}
	blocklist := opts.Blocklist

	return glue.NewTool[fileWriteArgs](
		glue.ToolSpec{
			Name:          "write_file",
			Description:   "Write UTF-8 text to a file inside the configured workspace. Requires permission. Refuses path escape, symlink escape, oversized content, and overwrites unless explicitly allowed.",
			PromptSnippet: "Create a new file (or overwrite when allowed)",
			PromptGuidelines: []string{
				"Prefer edit_file for changing existing files; write_file is for new files or full rewrites.",
			},
			RequiresPermission: true,
			PermissionAction:   "write_file",
			PermissionTarget:   fileWritePermissionTarget,
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": { "type": "string", "description": "Path relative to the configured workspace." },
    "content": { "type": "string", "description": "Full file content to write." },
    "overwrite": { "type": "boolean", "description": "Set true only when intentionally replacing an existing file. Host policy must also allow overwrites.", "default": false }
  },
  "required": ["path", "content"]
}`),
		},
		func(_ context.Context, args fileWriteArgs) (glue.ToolResult, error) {
			result, err := writeFile(absWorkDir, args, writePolicy{
				allowOverwrite: opts.AllowOverwrite,
				maxBytes:       max,
				blocklist:      blocklist,
			})
			if err != nil {
				return glue.ErrorResult(err), nil
			}
			return result, nil
		},
	), nil
}

type writePolicy struct {
	allowOverwrite bool
	maxBytes       int
	blocklist      Blocklist
}

func writeFile(workDir string, args fileWriteArgs, policy writePolicy) (glue.ToolResult, error) {
	rel := strings.TrimSpace(args.Path)
	if rel == "" {
		return glue.ToolResult{}, errors.New("fs: path is required")
	}
	if blocked, pat := policy.blocklist.Match(rel); blocked {
		return glue.ToolResult{}, fmt.Errorf("path %q is blocked by sensitive-file pattern %q; do not retry", rel, pat)
	}
	contentBytes := []byte(args.Content)
	if len(contentBytes) > policy.maxBytes {
		return glue.ToolResult{}, fmt.Errorf("fs: content is %d bytes, exceeds max %d", len(contentBytes), policy.maxBytes)
	}

	target, err := SafeJoin(workDir, rel)
	if err != nil {
		return glue.ToolResult{}, err
	}
	parent := filepath.Dir(target)
	if err := ensureSafeWriteParent(workDir, parent); err != nil {
		return glue.ToolResult{}, err
	}

	overwritten := false
	if info, err := os.Lstat(target); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return glue.ToolResult{}, fmt.Errorf("fs: target %q is a symlink; refusing to write", rel)
		}
		if info.IsDir() {
			return glue.ToolResult{}, fmt.Errorf("fs: target %q is a directory; refusing to write", rel)
		}
		if !policy.allowOverwrite || !args.Overwrite {
			return glue.ToolResult{}, fmt.Errorf("fs: target %q exists; set overwrite=true and enable AllowOverwrite to replace it", rel)
		}
		overwritten = true
	} else if !os.IsNotExist(err) {
		return glue.ToolResult{}, err
	}

	if err := atomicWriteFile(parent, target, contentBytes, 0o644); err != nil {
		return glue.ToolResult{}, err
	}

	cleanRel := filepath.ToSlash(filepath.Clean(rel))
	return glue.ToolResult{
		Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: fmt.Sprintf("wrote %d bytes to %s", len(contentBytes), cleanRel)}},
		Metadata: map[string]any{
			"path":        cleanRel,
			"bytes":       len(contentBytes),
			"overwritten": overwritten,
		},
	}, nil
}

func ensureSafeWriteParent(workDir, parent string) error {
	baseEval, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return fmt.Errorf("fs: resolve work directory symlinks: %w", err)
	}
	baseEval, err = filepath.Abs(baseEval)
	if err != nil {
		return err
	}

	missing := []string{}
	cur := parent
	for {
		info, err := os.Lstat(cur)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				eval, err := filepath.EvalSymlinks(cur)
				if err != nil {
					return err
				}
				if !pathWithin(baseEval, eval) {
					return fmt.Errorf("fs: parent %q escapes work directory through symlink", cur)
				}
			} else if !info.IsDir() {
				return fmt.Errorf("fs: parent %q is not a directory", cur)
			}
			eval, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return err
			}
			if !pathWithin(baseEval, eval) {
				return fmt.Errorf("fs: parent %q escapes work directory", cur)
			}
			break
		}
		if !os.IsNotExist(err) {
			return err
		}
		missing = append(missing, cur)
		next := filepath.Dir(cur)
		if next == cur {
			return fmt.Errorf("fs: no existing parent for %q", parent)
		}
		cur = next
	}

	for i := len(missing) - 1; i >= 0; i-- {
		dir := missing[i]
		if err := os.Mkdir(dir, 0o755); err != nil && !os.IsExist(err) {
			return err
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("fs: parent %q is not a safe directory", dir)
		}
		eval, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return err
		}
		if !pathWithin(baseEval, eval) {
			return fmt.Errorf("fs: parent %q escapes work directory", dir)
		}
	}
	return nil
}

func pathWithin(base, candidate string) bool {
	base, err := filepath.Abs(base)
	if err != nil {
		return false
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(base, candidate)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func atomicWriteFile(parent, target string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(parent, ".glue-write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func fileWritePermissionTarget(call glue.ToolCall) string {
	var args fileWriteArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil || strings.TrimSpace(args.Path) == "" {
		return "write_file"
	}
	target := strings.TrimSpace(args.Path)
	if args.Overwrite {
		return target + " (overwrite)"
	}
	return target
}
