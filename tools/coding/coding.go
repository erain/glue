package coding

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/erain/glue"
	toolsfs "github.com/erain/glue/tools/fs"
	toolsgit "github.com/erain/glue/tools/git"
	toolsshell "github.com/erain/glue/tools/shell"
)

// DefaultAllowedBinaries is the conservative shell_exec allowlist used
// when Options.AllowedBinaries is empty.
var (
	DefaultAllowedBinaries = []string{"go", "git", "make", "node", "npm", "python", "python3"}
	DefaultShellEnvPass    = strings.Split(DefaultShellEnvPassCSV, ",")
)

// DefaultShellTimeout and DefaultShellEnvPassCSV configure coding shell defaults.
const (
	DefaultShellTimeout    = 10 * time.Minute
	DefaultShellEnvPassCSV = "PATH,HOME,USER,LANG,TERM,TMPDIR,GOPATH,GOMODCACHE,GOCACHE,GOTOOLCHAIN,GOFLAGS,GOPROXY,GONOSUMDB,GOPRIVATE,CGO_ENABLED"
)

// Options configures the reusable coding-agent tool bundle.
type Options struct {
	// Enabled gates the whole bundle. Disabled returns no tools and does
	// not validate the rest of the options.
	Enabled bool

	// WorkDir is the workspace root for every tool. Empty uses the
	// process working directory. Leading ~ and $HOME are expanded.
	WorkDir string

	// AllowedBinaries is the shell_exec basename allowlist ("*" allows any
	// binary basename). Empty falls back to DefaultAllowedBinaries.
	AllowedBinaries []string

	// AllowOverwrite is the host-level write_file overwrite policy.
	AllowOverwrite bool

	// Executor runs shell_exec commands. Nil uses glue.LocalExecutor.
	Executor glue.Executor

	// Env holds explicit KEY=VALUE entries for shell_exec (or the exact
	// child environment when EnvPass, SecretAllow, and DefaultHome are unset).
	Env, EnvPass, SecretAllow []string
	DefaultHome, SpoolDir     string

	// Blocklist refuses secret-shaped paths for read_file and write_file.
	// Nil uses tools/fs.Default().
	Blocklist toolsfs.Blocklist

	// ReadMaxBytes caps read_file output. Zero uses the fs package default.
	ReadMaxBytes int

	// WriteMaxBytes caps write_file content. Zero uses the fs package default.
	WriteMaxBytes int

	// ShellTimeout caps shell_exec calls. Zero uses the shell package default.
	ShellTimeout time.Duration

	// ShellMaxOutputBytes caps shell_exec stdout and stderr independently.
	// Zero uses the glue executor default.
	ShellMaxOutputBytes int

	// GitDefaultBase is the default base ref for git_diff_branch and
	// git_log_branch. Empty uses the git package default.
	GitDefaultBase string

	// GitDiffMaxBytes caps git_diff_branch output. Zero uses the git
	// package default.
	GitDiffMaxBytes int

	// GitLogLimit caps git_log_branch commit count. Zero uses the git
	// package default.
	GitLogLimit int

	// GitTimeout caps each git invocation. Zero uses the git package default.
	GitTimeout time.Duration
}

// Tools builds the standard local coding tool bundle:
// read_file, write_file, edit_file, list_dir, find_files, grep,
// shell_exec, git_diff_branch, and git_log_branch.
//
// The returned Options contain the resolved absolute WorkDir, normalized
// AllowedBinaries, copied Env, and effective Blocklist.
func Tools(opts Options) ([]glue.Tool, Options, error) {
	if !opts.Enabled {
		return nil, opts, nil
	}

	resolved, err := ResolveOptions(opts)
	if err != nil {
		return nil, opts, err
	}

	// One shared read-ledger per bundle: read_file records mtimes,
	// edit_file refuses stale edits, and both mutators refresh entries
	// after their own writes.
	ledger := toolsfs.NewReadLedger()

	write, err := toolsfs.FileWrite(toolsfs.FileWriteOptions{
		WorkDir:        resolved.WorkDir,
		AllowOverwrite: resolved.AllowOverwrite,
		MaxBytes:       resolved.WriteMaxBytes,
		Blocklist:      resolved.Blocklist,
		Ledger:         ledger,
	})
	if err != nil {
		return nil, opts, err
	}
	edit, err := toolsfs.FileEdit(toolsfs.EditFileOptions{
		WorkDir:   resolved.WorkDir,
		MaxBytes:  resolved.WriteMaxBytes,
		Blocklist: resolved.Blocklist,
		Ledger:    ledger,
	})
	if err != nil {
		return nil, opts, err
	}
	navOpts := toolsfs.NavOptions{WorkDir: resolved.WorkDir, Blocklist: resolved.Blocklist, RespectGitignore: true}
	listDir, err := toolsfs.ListDirTool(navOpts)
	if err != nil {
		return nil, opts, err
	}
	find, err := toolsfs.FindTool(navOpts)
	if err != nil {
		return nil, opts, err
	}
	grep, err := toolsfs.GrepTool(navOpts)
	if err != nil {
		return nil, opts, err
	}
	shell, err := toolsshell.Exec(toolsshell.ExecOptions{
		Executor:        resolved.Executor,
		WorkDir:         resolved.WorkDir,
		Env:             resolved.Env,
		AllowedBinaries: resolved.AllowedBinaries,
		Timeout:         resolved.ShellTimeout,
		MaxOutputBytes:  resolved.ShellMaxOutputBytes,
		SpoolDir:        resolved.SpoolDir,
	})
	if err != nil {
		return nil, opts, err
	}

	return []glue.Tool{
		toolsfs.ReadFileTool(toolsfs.ReadFileOptions{
			WorkDir:   resolved.WorkDir,
			Blocklist: resolved.Blocklist,
			MaxBytes:  resolved.ReadMaxBytes,
			Ledger:    ledger,
		}),
		write,
		edit,
		listDir,
		find,
		grep,
		shell,
		toolsgit.DiffBranchTool(toolsgit.DiffBranchOptions{
			WorkDir:     resolved.WorkDir,
			DefaultBase: resolved.GitDefaultBase,
			MaxBytes:    resolved.GitDiffMaxBytes,
			Timeout:     resolved.GitTimeout,
		}),
		toolsgit.LogBranchTool(toolsgit.LogBranchOptions{
			WorkDir:      resolved.WorkDir,
			DefaultBase:  resolved.GitDefaultBase,
			DefaultLimit: resolved.GitLogLimit,
			Timeout:      resolved.GitTimeout,
		}),
	}, resolved, nil
}

// ResolveOptions validates and fills defaults for enabled coding tools.
func ResolveOptions(opts Options) (Options, error) {
	if !opts.Enabled {
		return opts, nil
	}

	workDir := strings.TrimSpace(opts.WorkDir)
	if workDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return Options{}, fmt.Errorf("coding: workdir: %w", err)
		}
		workDir = cwd
	}
	expanded, err := ExpandPath(workDir)
	if err != nil {
		return Options{}, err
	}
	absWorkDir, err := filepath.Abs(expanded)
	if err != nil {
		return Options{}, fmt.Errorf("coding: workdir: %w", err)
	}
	info, err := os.Stat(absWorkDir)
	if err != nil {
		return Options{}, fmt.Errorf("coding: workdir: %w", err)
	}
	if !info.IsDir() {
		return Options{}, fmt.Errorf("coding: workdir %q is not a directory", absWorkDir)
	}

	allowed := NormalizeAllowedBinaries(opts.AllowedBinaries)
	if len(allowed) == 0 {
		allowed = append([]string(nil), DefaultAllowedBinaries...)
	}

	blocklist := opts.Blocklist
	if blocklist == nil {
		blocklist = toolsfs.Default()
	}

	if opts.ShellTimeout < 0 {
		return Options{}, fmt.Errorf("coding: shell timeout must be non-negative")
	}
	resolved := opts
	resolved.WorkDir, resolved.AllowedBinaries, resolved.Blocklist = absWorkDir, allowed, blocklist
	if resolved.ShellTimeout == 0 {
		resolved.ShellTimeout = DefaultShellTimeout
	}
	resolved.SpoolDir = os.TempDir()
	if strings.TrimSpace(opts.SpoolDir) != "" {
		if resolved.SpoolDir, err = ensureDir(opts.SpoolDir); err != nil {
			return Options{}, fmt.Errorf("coding: spool dir: %w", err)
		}
	}
	resolved.Env = append([]string(nil), opts.Env...)
	if opts.EnvPass != nil || opts.Env == nil || len(opts.SecretAllow) > 0 || strings.TrimSpace(opts.DefaultHome) != "" {
		if resolved.Env, err = BuildShellEnv(opts.EnvPass, opts.Env, opts.SecretAllow, opts.DefaultHome); err != nil {
			return Options{}, err
		}
	}
	return resolved, nil
}

func ensureDir(raw string) (string, error) {
	expanded, err := ExpandPath(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", err
	}
	return abs, os.MkdirAll(abs, 0o700)
}

// IsSecretEnvName reports whether an environment variable name matches
// *_API_KEY, *_TOKEN, *SECRET*, or *PASSWORD* (case-insensitive).
func IsSecretEnvName(name string) bool {
	u := strings.ToUpper(strings.TrimSpace(name))
	return strings.HasSuffix(u, "_API_KEY") || strings.HasSuffix(u, "_TOKEN") || strings.Contains(u, "SECRET") || strings.Contains(u, "PASSWORD")
}

// ParseCSVList splits comma-separated items, trims whitespace, and drops empties.
func ParseCSVList(raw string) []string {
	return NormalizeAllowedBinaries(strings.Split(raw, ","))
}

// ValidateShellEnv checks that every entry has a non-empty KEY=VALUE shape.
func ValidateShellEnv(entries []string) error {
	for _, entry := range entries {
		if key, _, ok := strings.Cut(entry, "="); !ok || strings.TrimSpace(key) == "" || strings.ContainsAny(strings.TrimSpace(key), " \t") {
			return fmt.Errorf("coding: invalid shell env %q (expected KEY=VALUE)", entry)
		}
	}
	return nil
}

// BuildShellEnv constructs the child environment from pass-through variable
// names, explicit KEY=VALUE pairs, allowed secret names, and an optional
// fallback HOME directory (created when used).
func BuildShellEnv(envPass, explicitEnv, secretAllow []string, defaultHome string) ([]string, error) {
	if err := ValidateShellEnv(explicitEnv); err != nil {
		return nil, err
	}
	if envPass == nil {
		envPass = DefaultShellEnvPass
	}
	allowSecret, extraPass, vals, keys := map[string]struct{}{}, []string(nil), map[string]string{}, []string(nil)
	for _, name := range ParseCSVList(strings.Join(secretAllow, ",")) {
		allowSecret[strings.ToUpper(name)], extraPass = struct{}{}, append(extraPass, name)
	}
	set := func(k, v string) {
		if _, exists := vals[k]; !exists {
			keys = append(keys, k)
		}
		vals[k] = v
	}
	for _, key := range ParseCSVList(strings.Join(append(append([]string(nil), envPass...), extraPass...), ",")) {
		if _, ok := allowSecret[strings.ToUpper(key)]; !IsSecretEnvName(key) || ok {
			if val, ok := os.LookupEnv(key); ok {
				set(key, val)
			}
		}
	}
	for _, entry := range explicitEnv {
		key, val, _ := strings.Cut(entry, "=")
		set(strings.TrimSpace(key), val)
	}
	if strings.TrimSpace(vals["HOME"]) == "" && strings.TrimSpace(defaultHome) != "" {
		absHome, err := ensureDir(defaultHome)
		if err != nil {
			return nil, fmt.Errorf("coding: default home: %w", err)
		}
		set("HOME", absHome)
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+vals[k])
	}
	return out, nil
}

// NormalizeAllowedBinaries trims, drops empties, and deduplicates a shell
// binary allowlist while preserving first-seen order.
func NormalizeAllowedBinaries(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, raw := range in {
		bin := strings.TrimSpace(raw)
		if bin == "" {
			continue
		}
		if _, ok := seen[bin]; ok {
			continue
		}
		seen[bin] = struct{}{}
		out = append(out, bin)
	}
	return out
}

// ExpandPath resolves leading "~" plus "$HOME" and "${HOME}" placeholders.
// Other environment variables are deliberately left untouched.
func ExpandPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("coding: resolve ~: %w", err)
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	path = strings.ReplaceAll(path, "${HOME}", os.Getenv("HOME"))
	path = strings.ReplaceAll(path, "$HOME", os.Getenv("HOME"))
	return path, nil
}
