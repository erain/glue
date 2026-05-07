package fs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/erain/glue"
)

// DefaultReadMaxBytes is the default cap on a single read_file call.
const DefaultReadMaxBytes = 80 * 1024

// ReadFileOptions configures ReadFileTool.
type ReadFileOptions struct {
	// WorkDir is the root the tool reads under. Paths supplied by the
	// model are resolved relative to WorkDir and rejected if they
	// escape it.
	WorkDir string

	// Blocklist refuses paths matching these glob patterns. Pass
	// fs.Default().Merge(extras...) to layer extras on top of the
	// shipped defaults.
	Blocklist Blocklist

	// MaxBytes caps the returned content. Zero falls back to
	// DefaultReadMaxBytes.
	MaxBytes int
}

type readFileArgs struct {
	Path     string `json:"path"`
	MaxBytes int    `json:"max_bytes"`
}

// ReadFileTool returns a glue.Tool named "read_file" that reads UTF-8
// text from opts.WorkDir, refuses sensitive paths, and truncates output
// to the requested size. Errors are returned as error ToolResults so
// the model can recover, not as Go errors that crash the loop.
func ReadFileTool(opts ReadFileOptions) glue.Tool {
	max := opts.MaxBytes
	if max <= 0 {
		max = DefaultReadMaxBytes
	}
	workDir := opts.WorkDir
	blocklist := opts.Blocklist

	return glue.NewTool[readFileArgs](
		glue.ToolSpec{
			Name:        "read_file",
			Description: "Read a UTF-8 text file from the working directory. Returns the file content, truncated if larger than max_bytes. Use this to inspect files mentioned in the diff when surrounding context is needed. Refuses to open secret-shaped files (.env, id_rsa, *.pem, credentials.json, etc.).",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path":      { "type": "string", "description": "Path relative to the working directory. '..' traversal is rejected." },
    "max_bytes": { "type": "integer", "description": "Cap on returned bytes. Default 81920." }
  },
  "required": ["path"]
}`),
		},
		func(_ context.Context, a readFileArgs) (glue.ToolResult, error) {
			if blocked, pat := blocklist.Match(a.Path); blocked {
				return glue.ErrorResult(fmt.Errorf("path %q is blocked by sensitive-file pattern %q; do not retry", a.Path, pat)), nil
			}
			resolved, err := SafeJoin(workDir, a.Path)
			if err != nil {
				return glue.ErrorResult(err), nil
			}
			limit := a.MaxBytes
			if limit <= 0 {
				limit = max
			}
			f, err := os.Open(resolved)
			if err != nil {
				return glue.ErrorResult(err), nil
			}
			defer f.Close()
			data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
			if err != nil {
				return glue.ErrorResult(err), nil
			}
			return glue.TextResult(Truncate(string(data), limit)), nil
		},
	)
}
