// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/erain/glue/internal/axbackend"
	"github.com/erain/glue/jobs"
)

type configFakeAPI struct {
	jobAPI
	spec *jobs.Spec
}

func (configFakeAPI) Close() error { return nil }
func (f configFakeAPI) Submit(_ context.Context, spec jobs.Spec, _ bool) (jobs.Job, error) {
	*f.spec = spec
	return jobs.Job{ID: "job-0123456789ab", Spec: spec}, nil
}

// submitWithConfig writes .glue/remote.yaml into a pushed checkout and submits from it.
func submitWithConfig(t *testing.T, yaml string, extra ...string) (axbackend.Options, jobs.Spec, string, error) {
	t.Helper()
	clearJobTestEnv(t)
	repo := pushedCheckout(t)
	if yaml != "" {
		must(0, os.MkdirAll(filepath.Join(repo, ".glue"), 0o755))
		must(0, os.WriteFile(filepath.Join(repo, ".glue", "remote.yaml"), []byte(yaml), 0o644))
	}
	orig := newAXJobAPI
	t.Cleanup(func() { newAXJobAPI = orig })
	var opts axbackend.Options
	var spec jobs.Spec
	newAXJobAPI = func(_ context.Context, o axbackend.Options) (jobAPI, error) {
		opts = o
		return configFakeAPI{spec: &spec}, nil
	}
	var stdout, stderr bytes.Buffer
	args := append([]string{"submit", "--ax", "--repo", repo, "Add Shout"}, extra...)
	err := jobCommand(context.Background(), args, strings.NewReader(""), &stdout, &stderr, nil)
	return opts, spec, stderr.String(), err
}

const sampleConfig = `image: registry.example.com/node-task:1
verify:
  - npm ci
  - npm test
deliver: draft_pr
policy: workspace-auto
egress_hosts: [registry.npmjs.org, nodejs.org]
resources: {cpu: "2", memory: 4Gi}
budgets: {wall_clock: 30m, token_budget: 500000, max_iterations: 4}
setup: [npm ci]
`

func TestRemoteConfigPrecedence(t *testing.T) {
	opts, spec, _, err := submitWithConfig(t, sampleConfig)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Image != "registry.example.com/node-task:1" || opts.CPU != "2" || opts.Memory != "4Gi" {
		t.Fatalf("file values not applied to AX options: image=%q cpu=%q mem=%q", opts.Image, opts.CPU, opts.Memory)
	}
	if spec.Delivery != "draft_pr" || spec.Policy != "workspace-auto" || len(spec.Verify) != 2 || strings.Join(spec.Verify[1], " ") != "npm test" ||
		spec.Budgets.WallClock != 30*time.Minute || spec.Budgets.TokenBudget != 500000 || spec.Budgets.MaxIterations != 4 {
		t.Fatalf("file values not applied to the spec: %+v", spec)
	}
	opts, spec, _, err = submitWithConfig(t, sampleConfig, "--cpu", "8", "--verify", "go test ./...", "--deliver", "none", "--wall-clock", "5m", "--image", "img:flag")
	if err != nil {
		t.Fatal(err)
	}
	if opts.CPU != "8" || opts.Memory != "4Gi" || opts.Image != "img:flag" || spec.Delivery != "none" || len(spec.Verify) != 1 || spec.Budgets.WallClock != 5*time.Minute || spec.Budgets.TokenBudget != 500000 {
		t.Fatalf("flags must win over the file: cpu=%q mem=%q image=%q spec=%+v", opts.CPU, opts.Memory, opts.Image, spec)
	}
}

func TestRemoteConfigEgressMerged(t *testing.T) {
	opts, _, _, err := submitWithConfig(t, sampleConfig, "--egress-host", "pypi.example.org")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"github.com", "proxy.golang.org", "registry.npmjs.org", "nodejs.org", "pypi.example.org"} {
		if !slices.Contains(opts.EgressHosts, h) {
			t.Fatalf("egress hosts %v lack %s", opts.EgressHosts, h)
		}
	}
}

func TestRemoteConfigUnknownKey(t *testing.T) {
	_, _, _, err := submitWithConfig(t, "image: x\nverfy: [go test]\n")
	if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "verfy") {
		t.Fatalf("unknown key: %v", err)
	}
	if _, _, _, err := submitWithConfig(t, "budgets: {wall_clock: soon}\n"); err == nil || !strings.Contains(err.Error(), "wall_clock") {
		t.Fatalf("bad duration: %v", err)
	}
	if _, _, _, err := submitWithConfig(t, "egress_hosts: [\"https://evil.example/x\"]\n"); err == nil || !strings.Contains(err.Error(), "not a host name") {
		t.Fatalf("bad host: %v", err)
	}
	if _, _, _, err := submitWithConfig(t, ""); err != nil {
		t.Fatalf("no config file should be fine: %v", err)
	}
}

func TestRemoteConfigSummary(t *testing.T) {
	_, _, stderr, err := submitWithConfig(t, sampleConfig, "--quiet=false")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"egress    defaults + registry.npmjs.org, nodejs.org", "config    .glue/remote.yaml: image, verify, deliver, policy, egress_hosts, resources.cpu, resources.memory, budgets.wall_clock, budgets.token_budget, budgets.max_iterations, setup"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("summary missing %q:\n%s", want, stderr)
		}
	}
}
