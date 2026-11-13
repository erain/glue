// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/tools/coding"
)

// DefaultSetupTimeout bounds each setup command when the spec sets none.
const DefaultSetupTimeout = 10 * time.Minute

// setupEnd is the payload of the setup_end event.
type setupEnd struct {
	OK      bool           `json:"ok"`
	Results []VerifyResult `json:"results"`
	// Changed lists files that setup left modified or untracked; finalize
	// commits them unless .gitignore covers them.
	Changed []string `json:"changed,omitempty"`
}

// runSetup runs the spec's setup commands once per job, before the model is
// called. It returns a blocked goal result when a command fails. A job that
// completed setup before a restart does not run it again.
func (m *Manager) runSetup(ctx context.Context, id string, job Job, dir, baseSHA string) (*glue.GoalResult, error) {
	if len(job.Spec.Setup) == 0 {
		return nil, nil
	}
	if prev, ok := m.lastSetup(ctx, id); ok && prev.OK {
		return nil, nil
	}
	resolved, err := coding.ResolveOptions(m.codingOptions(id, job.Spec, dir, baseSHA))
	if err != nil {
		return nil, err
	}
	timeout := job.Spec.SetupTimeout
	if timeout <= 0 {
		timeout = DefaultSetupTimeout
	}
	if err := m.record(id, "setup_start", map[string]any{"commands": job.Spec.Setup}, "setup", nil, ""); err != nil {
		return nil, err
	}
	end := setupEnd{OK: true}
	for _, argv := range job.Spec.Setup {
		r := runVerifyCommand(ctx, dir, resolved.Env, timeout, argv)
		r.OutputTail = sanitizeToken(r.OutputTail)
		end.Results = append(end.Results, r)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if r.ExitCode != 0 {
			end.OK = false
			break
		}
	}
	if out, err := workspaceGit(ctx, dir, nil, "status", "--porcelain"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if len(line) > 3 {
				end.Changed = append(end.Changed, strings.TrimSpace(line[3:]))
			}
		}
	}
	if err := m.record(id, "setup_end", end, "setup", nil, ""); err != nil {
		return nil, err
	}
	if end.OK {
		return nil, nil
	}
	last := end.Results[len(end.Results)-1]
	summary := fmt.Sprintf("setup failed: %s exited %d", ShellJoin(last.Argv), last.ExitCode)
	return &glue.GoalResult{Status: glue.GoalBlocked, Objective: job.Spec.Objective, Summary: summary}, nil
}

// lastSetup returns the latest setup_end payload of a job.
func (m *Manager) lastSetup(ctx context.Context, id string) (setupEnd, bool) {
	events, err := m.Events(ctx, id, 0)
	if err != nil {
		return setupEnd{}, false
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != "setup_end" {
			continue
		}
		raw, _ := json.Marshal(events[i].Payload)
		var end setupEnd
		if json.Unmarshal(raw, &end) == nil {
			return end, true
		}
	}
	return setupEnd{}, false
}

// setupResults returns the commands the job's setup ran, for its result.
func (m *Manager) setupResults(id string) []VerifyResult {
	if end, ok := m.lastSetup(context.Background(), id); ok {
		return end.Results
	}
	return nil
}

// ShellJoin renders argv for display, single-quoting arguments a shell would split.
func ShellJoin(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t\n'\"$;&|<>*?()`\\") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		out[i] = a
	}
	return strings.Join(out, " ")
}
