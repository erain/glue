package peggy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/erain/glue"
	toolsfs "github.com/erain/glue/tools/fs"
	toolsgit "github.com/erain/glue/tools/git"
	toolsshell "github.com/erain/glue/tools/shell"
)

// CodingTools builds Peggy's local coding tool set for the configured
// workspace. It returns no tools when settings.Enabled is false.
func CodingTools(settings CodingSettings) ([]glue.Tool, CodingSettings, error) {
	if !settings.Enabled {
		return nil, settings, nil
	}

	workDir := strings.TrimSpace(settings.WorkDir)
	if workDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, settings, fmt.Errorf("peggy: coding workdir: %w", err)
		}
		workDir = cwd
	}
	expanded, err := expandPath(workDir)
	if err != nil {
		return nil, settings, err
	}
	absWorkDir, err := filepath.Abs(expanded)
	if err != nil {
		return nil, settings, fmt.Errorf("peggy: coding workdir: %w", err)
	}
	info, err := os.Stat(absWorkDir)
	if err != nil {
		return nil, settings, fmt.Errorf("peggy: coding workdir: %w", err)
	}
	if !info.IsDir() {
		return nil, settings, fmt.Errorf("peggy: coding workdir %q is not a directory", absWorkDir)
	}

	allowed := normalizedAllowedBinaries(settings.AllowedBinaries)
	if len(allowed) == 0 {
		allowed = append([]string(nil), DefaultCodingAllowedBinaries...)
	}
	settings.WorkDir = absWorkDir
	settings.AllowedBinaries = allowed

	blocklist := toolsfs.Default()
	write, err := toolsfs.FileWrite(toolsfs.FileWriteOptions{
		WorkDir:        absWorkDir,
		AllowOverwrite: settings.AllowOverwrite,
		Blocklist:      blocklist,
	})
	if err != nil {
		return nil, settings, err
	}
	shell, err := toolsshell.Exec(toolsshell.ExecOptions{
		WorkDir:         absWorkDir,
		AllowedBinaries: allowed,
	})
	if err != nil {
		return nil, settings, err
	}

	return []glue.Tool{
		toolsfs.ReadFileTool(toolsfs.ReadFileOptions{WorkDir: absWorkDir, Blocklist: blocklist}),
		write,
		shell,
		toolsgit.DiffBranchTool(toolsgit.DiffBranchOptions{WorkDir: absWorkDir}),
		toolsgit.LogBranchTool(toolsgit.LogBranchOptions{WorkDir: absWorkDir}),
	}, settings, nil
}

func normalizedAllowedBinaries(in []string) []string {
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
