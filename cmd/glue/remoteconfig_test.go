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

	"github.com/erain/glue/cmd/glue/tui"
	"github.com/erain/glue/internal/axbackend"
	"github.com/erain/glue/jobs"
)

type configFakeAPI struct {
	jobAPI
	spec *jobs.Spec
}

func (configFakeAPI) Close() error                                          { return nil }
func (configFakeAPI) List(context.Context, jobs.Filter) ([]jobs.Job, error) { return nil, nil }
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

func TestSplitCommand(t *testing.T) {
	for in, want := range map[string][]string{
		"go test ./...":                        {"go", "test", "./..."},
		"sh -c 'echo hi >&2; exit 7'":          {"sh", "-c", "echo hi >&2; exit 7"},
		`grep -q "two words" README.md`:        {"grep", "-q", "two words", "README.md"},
		`echo "a \"quoted\" word" back\ slash`: {"echo", `a "quoted" word`, "back slash"},
		"  npm   ci  ":                         {"npm", "ci"},
		`echo ''`:                              {"echo", ""},
	} {
		got, err := splitCommand(in)
		if err != nil || strings.Join(got, "|") != strings.Join(want, "|") || len(got) != len(want) {
			t.Fatalf("splitCommand(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := splitCommand("sh -c 'unterminated"); err == nil {
		t.Fatal("unterminated quote accepted")
	}
}

func TestUserConfigPrecedence(t *testing.T) {
	clearJobTestEnv(t)
	const sec = "0123456789abcdef0123456789abcdef"
	const uYAML = "ax: {context: user-ctx, kubeconfig: /user/kc, atespace: user-ns}\npublic_url: https://glue.example.com\nnotify: [https://ntfy.sh/glue-" + sec + "]\ndeliver: none\nprovider: gemini\nmodel: gemini-3.1-pro-preview\nthinking: high\n"
	repo, xdg, orig, origKube := pushedCheckout(t), t.TempDir(), newAXJobAPI, resolveKubeTarget
	t.Cleanup(func() { newAXJobAPI, resolveKubeTarget = orig, origKube })
	t.Setenv("XDG_CONFIG_HOME", xdg)
	cfgFile, remoteFile := filepath.Join(xdg, "glue", "config.yaml"), filepath.Join(repo, ".glue", "remote.yaml")
	must(0, os.MkdirAll(filepath.Dir(cfgFile), 0o700))
	must(0, os.MkdirAll(filepath.Dir(remoteFile), 0o755))
	var opts axbackend.Options
	var spec jobs.Spec
	newAXJobAPI = func(_ context.Context, o axbackend.Options) (jobAPI, error) {
		opts = o
		return configFakeAPI{spec: &spec}, nil
	}
	resolveKubeTarget = func(kc, kctx string) (string, string, error) { return kc, kctx, nil }
	envs := map[string]string{"GLUE_DELIVER": "branch", "GLUE_MODEL": "gemini-3-flash-preview", "GLUE_THINKING": "low", "GLUE_NOTIFY_URL": "https://hooks.env.example/sec12345", "GLUE_AX_ATESPACE": "env-ns", "GLUE_AX_CONTEXT": "env-ctx"}
	for _, tc := range []struct {
		name, user, repo, want string
		env                    map[string]string
		flags                  []string
	}{
		{"defaults", "", "", "|||default||", nil, nil},
		{"unknown_key", "provider: gemini\nbad_field: 1\n", "", "ERR", nil, nil},
		{"repo>user", uYAML, "deliver: pr\n", "draft_pr|gemini-3.1-pro-preview|high|user-ns|user-ctx|https://ntfy.sh/glue-" + sec, nil, nil},
		{"env>repo", uYAML, "deliver: pr\n", "branch|gemini-3-flash-preview|low|env-ns||https://hooks.env.example/sec12345", envs, nil},
		{"flag>env", uYAML, "deliver: pr\n", "none|gemini-3.1-pro-preview|medium|flag-ns|flag-ctx|https://hooks.flag.example/sec99999", envs, []string{"--deliver", "none", "--model", "gemini-3.1-pro-preview", "--thinking", "medium", "--notify", "https://hooks.flag.example/sec99999", "--atespace", "flag-ns", "--context", "flag-ctx"}},
		{"user", uYAML, "", "none|gemini-3.1-pro-preview|high|user-ns|user-ctx|https://ntfy.sh/glue-" + sec, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			must(0, os.WriteFile(cfgFile, []byte(tc.user), 0o600))
			must(0, os.WriteFile(remoteFile, []byte(tc.repo), 0o644))
			var out, errBuf bytes.Buffer
			err := jobCommand(context.Background(), append([]string{"submit", "--ax", "--repo", repo, "Add Shout"}, tc.flags...), strings.NewReader(""), &out, &errBuf, nil)
			got := strings.Join([]string{spec.Delivery, spec.Model, spec.Thinking, opts.Connect.Atespace, opts.Connect.KubeContext, strings.Join(opts.Credentials.NotifyURLs, ",")}, "|")
			checkAX(t, (tc.want == "ERR" && err != nil && strings.Contains(err.Error(), "line 2") && strings.Contains(err.Error(), "bad_field")) || (err == nil && got == tc.want && !strings.Contains(errBuf.String(), sec)), "%s: got=%q err=%v", tc.name, got, err)
		})
	}
	_, err := submitRemoteFromTUI(context.Background(), repo, tui.RemoteRequest{Objective: "TUI check"}, nil)
	var doc bytes.Buffer
	_ = jobCommand(context.Background(), []string{"doctor", "--repo", "https://github.com/erain/glue-sandbox"}, strings.NewReader(""), &doc, &doc, nil)
	checkAX(t, err == nil && spec.Delivery == "none" && spec.Model == "gemini-3.1-pro-preview" && slices.Contains(opts.EgressHosts, "ntfy.sh") && strings.Contains(doc.String(), "https://ntfy.sh/glue-0123…") && !strings.Contains(doc.String(), sec), "tui/doctor: %+v %v\n%s", spec, err, doc.String())
}

func TestUserConfigPermissions0600(t *testing.T) {
	clearJobTestEnv(t)
	dir := filepath.Join(t.TempDir(), "glue")
	t.Setenv("XDG_CONFIG_HOME", filepath.Dir(dir))
	none, _, err := loadUserConfig()
	must(0, os.MkdirAll(dir, 0o755))
	must(0, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("public_url: https://glue.example.com\nprovider: gemini\n"), 0o644))
	cfg, _, err2 := loadUserConfig()
	checkAX(t, err == nil && none == nil && err2 == nil && cfg != nil, "load: %v / %v", err, err2)
	cfg.Notify = []string{"https://ntfy.sh/glue-0123456789abcdef0123456789abcdef"}
	p := must(saveUserConfig(cfg))
	rel, _, rerr := loadUserConfig()
	checkAX(t, rerr == nil && must(os.Stat(dir)).Mode().Perm() == 0o700 && must(os.Stat(p)).Mode().Perm() == 0o600 && rel.PublicURL == "https://glue.example.com" && rel.Provider == "gemini" && len(rel.Notify) == 1, "rerr=%v", rerr)
}
