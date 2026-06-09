package fs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/erain/glue"
)

// DefaultReadMaxBytes is the default cap on a single read_file call.
const DefaultReadMaxBytes = 80 * 1024

// DefaultReadMaxLines is the default line cap on a single read_file
// call. Whichever of the byte and line caps hits first wins.
const DefaultReadMaxLines = 2000

// maxReadFileBytes refuses pathologically large files outright; the
// model should reach for shell tools (grep, sed) instead of paging
// through them.
const maxReadFileBytes = 20 << 20

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
	Offset   int    `json:"offset"`
	MaxLines int    `json:"max_lines"`
	MaxBytes int    `json:"max_bytes"`
}

// ReadFileTool returns a glue.Tool named "read_file" that reads UTF-8
// text from opts.WorkDir, refuses sensitive paths, and pages through
// large files by line offset. Truncated reads embed the exact next
// call to make ("Use offset=N to continue"), so the model never has to
// guess how to fetch the rest. Errors are returned as error
// ToolResults so the model can recover, not as Go errors that crash
// the loop.
func ReadFileTool(opts ReadFileOptions) glue.Tool {
	max := opts.MaxBytes
	if max <= 0 {
		max = DefaultReadMaxBytes
	}
	workDir := opts.WorkDir
	blocklist := opts.Blocklist

	return glue.NewTool[readFileArgs](
		glue.ToolSpec{
			Name:          "read_file",
			Description:   fmt.Sprintf("Read a UTF-8 text file from the working directory. Returns at most max_lines lines (default %d) and max_bytes bytes (default %d), whichever cap hits first; truncated reads say how to continue with offset. Refuses to open secret-shaped files (.env, id_rsa, *.pem, credentials.json, etc.).", DefaultReadMaxLines, DefaultReadMaxBytes),
			PromptSnippet: "Read a file (line-offset paging for large files)",
			PromptGuidelines: []string{
				"Use read_file to examine files instead of shell cat/sed.",
			},
			Parameters: json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "path":      { "type": "string", "description": "Path relative to the working directory. '..' traversal is rejected." },
    "offset":    { "type": "integer", "description": "1-based line number to start reading from. Default 1." },
    "max_lines": { "type": "integer", "description": "Cap on returned lines. Default %d." },
    "max_bytes": { "type": "integer", "description": "Cap on returned bytes. Default %d." }
  },
  "required": ["path"]
}`, DefaultReadMaxLines, DefaultReadMaxBytes)),
		},
		func(_ context.Context, a readFileArgs) (glue.ToolResult, error) {
			if blocked, pat := blocklist.Match(a.Path); blocked {
				return glue.ErrorResult(fmt.Errorf("path %q is blocked by sensitive-file pattern %q; do not retry", a.Path, pat)), nil
			}
			resolved, err := SafeJoin(workDir, a.Path)
			if err != nil {
				return glue.ErrorResult(err), nil
			}
			byteLimit := a.MaxBytes
			if byteLimit <= 0 {
				byteLimit = max
			}
			lineLimit := a.MaxLines
			if lineLimit <= 0 {
				lineLimit = DefaultReadMaxLines
			}
			offset := a.Offset
			if offset <= 0 {
				offset = 1
			}
			info, err := os.Stat(resolved)
			if err != nil {
				return glue.ErrorResult(err), nil
			}
			if info.Size() > maxReadFileBytes {
				return glue.ErrorResult(fmt.Errorf("file is %d bytes, exceeds the %d-byte read_file limit; use shell_exec with grep/sed/head to extract what you need", info.Size(), maxReadFileBytes)), nil
			}
			data, err := os.ReadFile(resolved)
			if err != nil {
				return glue.ErrorResult(err), nil
			}
			return glue.TextResult(pageLines(string(data), offset, lineLimit, byteLimit)), nil
		},
	)
}

// pageLines returns the requested line window of content, bounded by
// both lineLimit and byteLimit, with a trailing note that names the
// total line count and the offset to continue from when anything was
// left out.
func pageLines(content string, offset, lineLimit, byteLimit int) string {
	lines := strings.Split(content, "\n")
	total := len(lines)
	if total > 0 && lines[total-1] == "" {
		total-- // a trailing newline does not start a new line
	}
	if total == 0 {
		return ""
	}
	if offset > total {
		return fmt.Sprintf("[Offset %d is beyond the end of the file (%d lines total).]", offset, total)
	}

	end := offset - 1 + lineLimit
	if end > total {
		end = total
	}
	window := lines[offset-1 : end]

	// Apply the byte cap, never splitting a line unless the very first
	// line alone exceeds the cap.
	kept := 0
	used := 0
	for _, line := range window {
		need := len(line)
		if kept > 0 {
			need++ // the joining newline
		}
		if used+need > byteLimit {
			break
		}
		used += need
		kept++
	}
	if kept == 0 {
		first := window[0]
		if len(first) > byteLimit {
			return fmt.Sprintf("%s\n[Line %d is %d bytes, exceeds the %d-byte cap; shown truncated. Use shell_exec with sed/cut for the rest.]", first[:byteLimit], offset, len(first), byteLimit)
		}
		kept = 1
	}
	body := strings.Join(window[:kept], "\n")
	lastShown := offset - 1 + kept

	if offset == 1 && lastShown == total {
		return body
	}
	return fmt.Sprintf("%s\n\n[Showing lines %d-%d of %d. Use offset=%d to continue.]", body, offset, lastShown, total, lastShown+1)
}
