package peggy

import (
	"github.com/erain/glue"
	toolscoding "github.com/erain/glue/tools/coding"
)

// CodingToolOptions configures Peggy's use of Glue's reusable coding
// tool bundle. These options are runtime-only; settings.json still owns
// the persisted workspace and safety policy.
type CodingToolOptions struct {
	// Executor runs shell_exec commands. Nil uses glue.LocalExecutor.
	Executor glue.Executor

	// Env is the exact child environment used for shell_exec.
	Env []string
}

// CodingToolOption mutates CodingToolOptions.
type CodingToolOption func(*CodingToolOptions)

// WithCodingExecutor injects a shell_exec executor for Peggy's coding
// tools. This is the product seam for future VM or container-backed
// execution.
func WithCodingExecutor(executor glue.Executor) CodingToolOption {
	return func(opts *CodingToolOptions) {
		opts.Executor = executor
	}
}

// WithCodingEnv sets the exact child process environment for shell_exec.
func WithCodingEnv(env []string) CodingToolOption {
	return func(opts *CodingToolOptions) {
		opts.Env = append([]string(nil), env...)
	}
}

// CodingTools builds Peggy's local coding tool set for the configured
// workspace. It returns no tools when settings.Enabled is false.
func CodingTools(settings CodingSettings, options ...CodingToolOption) ([]glue.Tool, CodingSettings, error) {
	var runtime CodingToolOptions
	for _, option := range options {
		if option != nil {
			option(&runtime)
		}
	}

	tools, resolved, err := toolscoding.Tools(toolscoding.Options{
		Enabled:         settings.Enabled,
		WorkDir:         settings.WorkDir,
		AllowedBinaries: settings.AllowedBinaries,
		AllowOverwrite:  settings.AllowOverwrite,
		Executor:        runtime.Executor,
		Env:             runtime.Env,
	})
	if err != nil {
		return nil, settings, err
	}
	if settings.Enabled {
		settings.WorkDir = resolved.WorkDir
		settings.AllowedBinaries = resolved.AllowedBinaries
	}
	return tools, settings, nil
}
