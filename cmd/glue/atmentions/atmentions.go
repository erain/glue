// Package atmentions implements `@<path>` expansion for the glue CLI:
// before the prompt is sent to the provider, every recognized @-mention
// is read off disk and appended to the prompt under a fenced header.
//
// The same code is used by `glue run --prompt`, piped stdin, and the
// interactive TUI's submit handler, so the user can write
// `tell me about @util.go and @main.go` and the model sees the actual
// file contents.
//
// Safety:
//   - Paths are resolved relative to a workspace root and refused if
//     they escape it ("../etc/passwd", "/absolute"...).
//   - Paths that match a sensitive-file Blocklist (`.env`, `id_rsa`,
//     etc.) are refused even if they exist.
//   - Files exceeding MaxBytes are refused so a runaway 100 MB log
//     doesn't blow the model's context budget.
package atmentions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	toolsfs "github.com/erain/glue/tools/fs"
)

// DefaultMaxBytes is the per-file size ceiling.
const DefaultMaxBytes = 256 * 1024

// Options configures Expand.
type Options struct {
	// WorkDir is the workspace root. @paths are resolved relative to it.
	// Empty means current process directory.
	WorkDir string

	// Blocklist refuses sensitive paths (`.env`, `id_rsa`, etc.). Nil
	// uses tools/fs.Default().
	Blocklist toolsfs.Blocklist

	// MaxBytes caps per-file content. Zero uses DefaultMaxBytes.
	MaxBytes int
}

// Result reports what Expand did.
type Result struct {
	// Prompt is the rewritten prompt: the original verbatim, with file
	// contents appended under per-file headers.
	Prompt string
	// Included are the workspace-relative paths whose contents were
	// inlined.
	Included []string
	// Skipped are @-mentions that did not turn into content, paired with
	// the reason (file missing, blocked, oversize, etc.). Expand does
	// NOT fail on skipped paths; the caller surfaces them however it likes
	// (the TUI prints a system message; the one-shot path prints to stderr).
	Skipped []Skip
}

// Skip describes a single @-mention that was not inlined.
type Skip struct {
	Mention string // the literal "@<path>" the user typed
	Reason  string // human-readable explanation
}

// Expand walks a user-supplied prompt, locates @<path> tokens, reads
// the workspace-rooted files, and returns the rewritten prompt with the
// contents appended.
//
// Behavior matches what a human reviewer would expect:
//   - "@util.go" → relative to WorkDir.
//   - "@\"path with space\"" → quoted form supports spaces.
//   - "@@literal" → escape: a leading "@@" in a word is reduced to "@"
//     and not treated as a mention. Useful for prose like "use @@param".
//   - An @-mention that points at a directory is refused (no recursive
//     inlining; ask `list_dir` or `find_files`).
func Expand(prompt string, opts Options) (Result, error) {
	res := Result{Prompt: prompt}
	if !strings.Contains(prompt, "@") {
		return res, nil
	}

	mentions := scanMentions(prompt)
	if len(mentions) == 0 {
		return res, nil
	}

	workDir, err := resolveWorkDir(opts.WorkDir)
	if err != nil {
		return res, err
	}
	blocklist := opts.Blocklist
	if blocklist == nil {
		blocklist = toolsfs.Default()
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}

	var sections []string
	seen := map[string]struct{}{}
	for _, raw := range mentions {
		path := raw
		// De-duplicate identical mentions; one section per file.
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}

		content, skip := readMention(workDir, path, blocklist, maxBytes)
		if skip != "" {
			res.Skipped = append(res.Skipped, Skip{Mention: "@" + path, Reason: skip})
			continue
		}
		res.Included = append(res.Included, path)
		sections = append(sections, fmt.Sprintf("--- @%s ---\n%s", path, content))
	}
	sort.Strings(res.Included)

	if len(sections) > 0 {
		res.Prompt = strings.TrimRight(prompt, "\n") + "\n\n" + strings.Join(sections, "\n\n")
	}
	return res, nil
}

// scanMentions walks the prompt and returns the unquoted paths of every
// recognized @-mention, in order of first appearance.
func scanMentions(prompt string) []string {
	var out []string
	i := 0
	for i < len(prompt) {
		c := prompt[i]
		if c != '@' {
			i++
			continue
		}
		// Escape: "@@" reduces to a literal "@" — skip.
		if i+1 < len(prompt) && prompt[i+1] == '@' {
			i += 2
			continue
		}
		// Must be at start-of-string or follow whitespace/punctuation —
		// avoids matching emails like alice@example.com.
		if i > 0 && !isMentionBoundary(prompt[i-1]) {
			i++
			continue
		}
		// Quoted form: @"path with space".
		if i+1 < len(prompt) && prompt[i+1] == '"' {
			end := strings.IndexByte(prompt[i+2:], '"')
			if end < 0 {
				i++
				continue
			}
			out = append(out, prompt[i+2:i+2+end])
			i = i + 2 + end + 1
			continue
		}
		// Bare form: @path — terminate on whitespace or comma/parens/etc.
		end := i + 1
		for end < len(prompt) && !isMentionStop(prompt[end]) {
			end++
		}
		if end > i+1 {
			out = append(out, prompt[i+1:end])
		}
		i = end
	}
	return out
}

func isMentionBoundary(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '(', '[', '{', ',', ';', ':', '"', '\'':
		return true
	}
	return false
}

func isMentionStop(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', ',', ';', ')', ']', '}', '"', '\'':
		return true
	}
	return false
}

// resolveWorkDir picks the workspace root for resolution.
func resolveWorkDir(work string) (string, error) {
	work = strings.TrimSpace(work)
	if work == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("atmentions: workdir: %w", err)
		}
		work = cwd
	}
	abs, err := filepath.Abs(work)
	if err != nil {
		return "", fmt.Errorf("atmentions: workdir: %w", err)
	}
	return abs, nil
}

// readMention reads one file relative to workDir and returns its
// content. Returns content="" and a non-empty skip reason on any
// problem.
func readMention(workDir, rel string, blocklist toolsfs.Blocklist, maxBytes int) (string, string) {
	if rel == "" {
		return "", "empty path"
	}
	if blocked, pat := blocklist.Match(rel); blocked {
		return "", "blocked by sensitive-file pattern " + pat
	}
	resolved, err := toolsfs.SafeJoin(workDir, rel)
	if err != nil {
		return "", err.Error()
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "file not found"
		}
		return "", err.Error()
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", "refusing to follow symlink"
	}
	if info.IsDir() {
		return "", "is a directory (use list_dir or find_files)"
	}
	if info.Size() > int64(maxBytes) {
		return "", fmt.Sprintf("file is %d bytes, exceeds %d-byte cap", info.Size(), maxBytes)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", err.Error()
	}
	return string(data), ""
}
