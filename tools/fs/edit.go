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

// EditFileOptions configures FileEdit.
type EditFileOptions struct {
	// WorkDir is the root the tool edits under. Required.
	WorkDir string

	// MaxBytes caps the size of the file being edited. Zero falls back to
	// DefaultWriteMaxBytes.
	MaxBytes int

	// Blocklist refuses paths matching these glob patterns. Pass
	// fs.Default().Merge(extras...) to layer extras on top of the
	// shipped defaults.
	Blocklist Blocklist
}

type fileEditArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// FileEdit returns a glue.Tool named "edit_file" that performs an exact
// string replacement inside a single existing file under opts.WorkDir.
// It is permission-gated via ToolSpec metadata. Unlike write_file, it is
// independent of any overwrite policy: editing an existing file is its
// purpose, and every call is permission-gated.
//
// The replacement must be unambiguous: old_string must appear exactly
// once unless replace_all is set. This mirrors the surgical-edit
// contract used by Pi-class coding agents and prevents the model from
// silently changing the wrong occurrence.
func FileEdit(opts EditFileOptions) (glue.Tool, error) {
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

	return glue.NewTool[fileEditArgs](
		glue.ToolSpec{
			Name:               "edit_file",
			Description:        "Replace an exact string in an existing UTF-8 text file inside the configured workspace. Requires permission. old_string must match exactly once unless replace_all is set. Use this for surgical edits instead of rewriting the whole file with write_file.",
			RequiresPermission: true,
			PermissionAction:   "edit_file",
			PermissionTarget:   fileEditPermissionTarget,
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": { "type": "string", "description": "Path relative to the configured workspace. The file must already exist." },
    "old_string": { "type": "string", "description": "Exact text to replace. Include enough surrounding context to match exactly once unless replace_all is set." },
    "new_string": { "type": "string", "description": "Replacement text. Must differ from old_string." },
    "replace_all": { "type": "boolean", "description": "Replace every occurrence instead of requiring a unique match.", "default": false }
  },
  "required": ["path", "old_string", "new_string"]
}`),
		},
		func(_ context.Context, args fileEditArgs) (glue.ToolResult, error) {
			result, err := editFile(absWorkDir, args, editPolicy{
				maxBytes:  max,
				blocklist: blocklist,
			})
			if err != nil {
				return glue.ErrorResult(err), nil
			}
			return result, nil
		},
	), nil
}

type editPolicy struct {
	maxBytes  int
	blocklist Blocklist
}

func editFile(workDir string, args fileEditArgs, policy editPolicy) (glue.ToolResult, error) {
	rel := strings.TrimSpace(args.Path)
	if rel == "" {
		return glue.ToolResult{}, errors.New("fs: path is required")
	}
	if args.OldString == "" {
		return glue.ToolResult{}, errors.New("fs: old_string is required and must be non-empty")
	}
	if args.OldString == args.NewString {
		return glue.ToolResult{}, errors.New("fs: old_string and new_string are identical; nothing to change")
	}
	if blocked, pat := policy.blocklist.Match(rel); blocked {
		return glue.ToolResult{}, fmt.Errorf("path %q is blocked by sensitive-file pattern %q; do not retry", rel, pat)
	}

	target, err := SafeJoin(workDir, rel)
	if err != nil {
		return glue.ToolResult{}, err
	}

	info, err := os.Lstat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return glue.ToolResult{}, fmt.Errorf("fs: target %q does not exist; use write_file to create it", rel)
		}
		return glue.ToolResult{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return glue.ToolResult{}, fmt.Errorf("fs: target %q is a symlink; refusing to edit", rel)
	}
	if info.IsDir() {
		return glue.ToolResult{}, fmt.Errorf("fs: target %q is a directory; refusing to edit", rel)
	}
	if info.Size() > int64(policy.maxBytes) {
		return glue.ToolResult{}, fmt.Errorf("fs: file %q is %d bytes, exceeds max %d", rel, info.Size(), policy.maxBytes)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		return glue.ToolResult{}, err
	}
	content := string(data)

	count := strings.Count(content, args.OldString)
	if count == 0 {
		return glue.ToolResult{}, fmt.Errorf("fs: old_string not found in %q; read the file and match its exact text", rel)
	}
	if count > 1 && !args.ReplaceAll {
		return glue.ToolResult{}, fmt.Errorf("fs: old_string matches %d times in %q; add surrounding context for a unique match or set replace_all=true", count, rel)
	}

	var updated string
	replacements := count
	if args.ReplaceAll {
		updated = strings.ReplaceAll(content, args.OldString, args.NewString)
	} else {
		updated = strings.Replace(content, args.OldString, args.NewString, 1)
		replacements = 1
	}

	updatedBytes := []byte(updated)
	if len(updatedBytes) > policy.maxBytes {
		return glue.ToolResult{}, fmt.Errorf("fs: edited content is %d bytes, exceeds max %d", len(updatedBytes), policy.maxBytes)
	}

	perm := info.Mode().Perm()
	if perm == 0 {
		perm = 0o644
	}
	if err := atomicWriteFile(filepath.Dir(target), target, updatedBytes, perm); err != nil {
		return glue.ToolResult{}, err
	}

	cleanRel := filepath.ToSlash(filepath.Clean(rel))
	plural := ""
	if replacements != 1 {
		plural = "s"
	}
	return glue.ToolResult{
		Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: fmt.Sprintf("made %d replacement%s in %s (%d bytes)", replacements, plural, cleanRel, len(updatedBytes))}},
		Metadata: map[string]any{
			"path":         cleanRel,
			"replacements": replacements,
			"bytes":        len(updatedBytes),
		},
	}, nil
}

func fileEditPermissionTarget(call glue.ToolCall) string {
	var args fileEditArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil || strings.TrimSpace(args.Path) == "" {
		return "edit_file"
	}
	target := strings.TrimSpace(args.Path)
	if args.ReplaceAll {
		return target + " (replace_all)"
	}
	return target
}
