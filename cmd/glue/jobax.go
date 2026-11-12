// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/erain/glue/internal/axbackend"
	"github.com/erain/glue/internal/eventlog"
	"github.com/erain/glue/internal/jobclient"
	"github.com/erain/glue/jobs"
	"github.com/erain/glue/providers"
)

const errDirectAXMissingRemote = "direct AX jobs require a git remote URL; set origin or pass --repo <https-url>"

var (
	resolveKubeTarget     = axbackend.ResolveKubeTarget
	resolveGitHubCLIToken = ghAuthToken
	newAXJobAPI           = func(ctx context.Context, opts axbackend.Options) (jobAPI, error) {
		b, err := axbackend.NewBackend(ctx, opts)
		if err != nil {
			return nil, err
		}
		return &axJobAPI{b}, nil
	}
)

type remoteFlag struct {
	enabled   bool
	objective string
}

func (r *remoteFlag) IsBoolFlag() bool { return true }
func (r *remoteFlag) String() string {
	if r == nil || !r.enabled {
		return "false"
	}
	return cmp.Or(r.objective, "true")
}
func (r *remoteFlag) Set(v string) error {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "true", "1":
		r.enabled = true
	case "false", "0":
		r.enabled, r.objective = false, ""
	default:
		r.enabled, r.objective = true, v
	}
	return nil
}

type httpJobAPI struct{ *jobclient.Client }

func (httpJobAPI) Close() error { return nil }

type axJobAPI struct{ *axbackend.Backend }

func (a *axJobAPI) List(ctx context.Context, f jobs.Filter) ([]jobs.Job, error) {
	return a.ListJobs(ctx, f)
}
func (a *axJobAPI) Delete(ctx context.Context, id string) error { return a.DeleteTask(ctx, id) }
func (a *axJobAPI) StreamEvents(ctx context.Context, id string, after int64, h func(eventlog.Envelope) error) error {
	return a.Stream(ctx, id, after, h)
}
func (a *axJobAPI) Submit(ctx context.Context, spec jobs.Spec, start bool) (jobs.Job, error) {
	if spec.Budgets.MaxIterations < 0 || spec.Budgets.TokenBudget < 0 || spec.Budgets.WallClock < 0 {
		return jobs.Job{}, errors.New("job budgets must not be negative")
	}
	if !slices.Contains([]string{"", "none", "branch", "draft_pr"}, spec.Delivery) {
		return jobs.Job{}, fmt.Errorf("unknown delivery mode %q (expected none, branch, or draft_pr)", spec.Delivery)
	}
	if spec.Policy != "" {
		if _, err := jobs.BuiltInPolicy(spec.Policy); err != nil {
			return jobs.Job{}, err
		}
	}
	prov, mod, err := resolveSelection(true, spec.Provider, spec.Model)
	if err == nil {
		_, err = providers.ThinkingOptions(prov, mod, spec.Thinking)
	}
	id := strings.TrimSpace(spec.ID)
	if err == nil && id == "" {
		id, err = jobs.NewID()
	}
	if err != nil {
		return jobs.Job{}, err
	}
	spec.Backend = "ax"
	if spec.Delivery == "" {
		// Work done in a remote sandbox must reach origin, or it is lost with the task.
		spec.Delivery = "branch"
	}
	if start {
		return a.Backend.Submit(ctx, id, spec)
	}
	return a.Create(ctx, id, spec)
}

type jobTargetOpts struct {
	conn                                          jobclient.Config
	metadataExplicit, ax, remote                  bool
	backend, kubeconfig, kubectx, atespace, image string
}

func resolveJobAPI(ctx context.Context, t jobTargetOpts, httpClient httpDoer) (jobAPI, bool, error) {
	serverFlag, backend := strings.TrimSpace(t.conn.ServerURL), strings.TrimSpace(t.backend)
	kubeCfg, kubeCtx := strings.TrimSpace(t.kubeconfig), strings.TrimSpace(t.kubectx)
	openAX := func() (jobAPI, bool, error) {
		api, err := newAXJobAPI(ctx, axbackend.Options{
			Connect: axbackend.ConnectOptions{
				AXServer:   cmp.Or(strings.TrimSpace(os.Getenv("GLUE_AX_SERVER")), strings.TrimSpace(os.Getenv("AX_SERVER_ADDR"))),
				KubeConfig: kubeCfg, KubeContext: kubeCtx, Atespace: cmp.Or(strings.TrimSpace(t.atespace), "default"),
			},
			Image: strings.TrimSpace(t.image), Credentials: resolveAXCredentials(ctx),
		})
		return api, true, err
	}
	openHTTP := func() (jobAPI, bool, error) {
		c, err := jobclient.New(t.conn, httpClient)
		if err != nil {
			return nil, false, err
		}
		return httpJobAPI{c}, false, nil
	}
	if (t.ax || t.remote) && serverFlag == "" && backend != "local" {
		return openAX()
	}
	if serverFlag != "" || strings.TrimSpace(os.Getenv("GLUE_SERVER")) != "" || backend == "local" || t.metadataExplicit {
		return openHTTP()
	}
	if backend == "ax" || kubeCfg != "" || kubeCtx != "" || envSet("AX_SERVER_ADDR", "GLUE_AX_SERVER", "GLUE_AX_KUBECONFIG", "GLUE_AX_CONTEXT") {
		return openAX()
	}
	cfg, resolveErr := jobclient.ResolveConfig(t.conn)
	if resolveErr == nil {
		return httpJobAPI{&jobclient.Client{BaseURL: cfg.ServerURL, Token: cfg.Token, HTTP: cmp.Or[jobclient.HTTPDoer](httpClient, http.DefaultClient)}}, false, nil
	}
	if _, _, kerr := resolveKubeTarget(kubeCfg, kubeCtx); kerr == nil {
		return openAX()
	}
	return nil, false, resolveErr
}

func envSet(keys ...string) bool {
	return slices.ContainsFunc(keys, func(k string) bool { return strings.TrimSpace(os.Getenv(k)) != "" })
}

func resolveAXCredentials(ctx context.Context) jobs.Credentials {
	pks := map[string]string{}
	for p, env := range map[string]string{"gemini": "GEMINI_API_KEY", "openrouter": "OPENROUTER_API_KEY", "deepseek": "DEEPSEEK_API_KEY", "nvidia": "NVIDIA_API_KEY", "codex": "OPENAI_API_KEY"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			pks[p] = v
		}
	}
	ghTok := cmp.Or(strings.TrimSpace(os.Getenv("GLUE_GITHUB_TOKEN")), strings.TrimSpace(os.Getenv("GITHUB_TOKEN")))
	if ghTok == "" {
		ghTok = resolveGitHubCLIToken(ctx)
	}
	creds := jobs.Credentials{GitHubToken: ghTok}
	if len(pks) > 0 {
		creds.ProviderKeys = pks
	}
	return creds
}

func ghAuthToken(parent context.Context) string {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	return strings.TrimSpace(string(out))
}

func resolveSubmitRepo(ctx context.Context, directAX, remote, ignoreLocal bool, backend, rawRepo, baseRef, obj string) (jobs.RepoSpec, error) {
	rawRepo, baseRef = strings.TrimSpace(rawRepo), strings.TrimSpace(baseRef)
	if directAX {
		if obj == "" {
			return jobs.RepoSpec{}, errors.New("missing required --objective")
		}
		return resolveRemoteCheckout(ctx, rawRepo, baseRef, ignoreLocal)
	}
	if remote && rawRepo == "" {
		if strings.TrimSpace(backend) != "local" && obj != "" {
			rs, err := resolveRemoteCheckout(ctx, ".", baseRef, ignoreLocal)
			if err == nil {
				return rs, nil
			}
			if lse := (*localStateError)(nil); errors.As(err, &lse) {
				return jobs.RepoSpec{}, err
			}
		}
		rawRepo = "."
	}
	if rawRepo == "" || obj == "" {
		return jobs.RepoSpec{}, errors.New("missing required --repo and --objective")
	}
	if isRemoteRepoRef(rawRepo) {
		return jobs.RepoSpec{URL: rawRepo, BaseRef: baseRef}, nil
	}
	return jobs.RepoSpec{Path: rawRepo, BaseRef: baseRef}, nil
}

// resolveRemoteCheckout resolves the repo spec for a job that runs from a
// fresh clone. For a local checkout without an explicit base ref, it refuses
// to proceed when the clone would miss local work, unless ignoreLocal is set.
func resolveRemoteCheckout(ctx context.Context, raw, baseRef string, ignoreLocal bool) (jobs.RepoSpec, error) {
	rs, err := resolveAXRepoSpec(ctx, raw, baseRef)
	if err != nil || ignoreLocal || strings.TrimSpace(baseRef) != "" || isRemoteRepoRef(cmp.Or(strings.TrimSpace(raw), ".")) {
		return rs, err
	}
	if err := checkLocalState(ctx, cmp.Or(strings.TrimSpace(raw), "."), rs.BaseRef); err != nil {
		return jobs.RepoSpec{}, err
	}
	return rs, nil
}

// localStateError reports local work that a remote clone of the base ref would not contain.
type localStateError struct {
	branch, base       string
	dirtyFiles         int
	unpushed           int
	branchMissingOnRem bool
}

func (e *localStateError) Error() string {
	var b strings.Builder
	b.WriteString("the remote job would start from a fresh clone of ")
	b.WriteString(cmp.Or(e.base, "the default branch"))
	b.WriteString(" on origin and miss local work:")
	if e.branchMissingOnRem {
		fmt.Fprintf(&b, "\n  - branch %q does not exist on origin", e.branch)
	}
	if e.unpushed > 0 {
		fmt.Fprintf(&b, "\n  - %d commit(s) on HEAD are not on origin", e.unpushed)
	}
	if e.dirtyFiles > 0 {
		fmt.Fprintf(&b, "\n  - %d file(s) with uncommitted or untracked changes", e.dirtyFiles)
	}
	b.WriteString("\nTo continue, either:")
	b.WriteString("\n  - commit and push your work (git push -u origin HEAD), then submit again,")
	b.WriteString("\n  - pass --base <ref> to choose the remote ref to start from, or")
	b.WriteString("\n  - pass --ignore-local to start from the remote ref anyway.")
	return b.String()
}

// checkLocalState compares the checkout in dir with what a clone of baseRef
// from origin would contain. Remote-tracking refs are used, so no network is needed.
func checkLocalState(ctx context.Context, dir, baseRef string) error {
	e := &localStateError{base: baseRef}
	e.branch, _ = gitOutput(ctx, dir, "branch", "--show-current")
	if e.branch != "" && e.branch != baseRef {
		e.branchMissingOnRem = true
	}
	if n, err := gitOutput(ctx, dir, "rev-list", "--count", "HEAD", "--not", "--remotes=origin"); err == nil {
		fmt.Sscan(n, &e.unpushed)
	}
	if out, err := gitOutput(ctx, dir, "status", "--porcelain", "--untracked-files=all"); err == nil {
		for line := range strings.SplitSeq(out, "\n") {
			if len(line) > 3 && !strings.HasPrefix(strings.Trim(line[3:], `"`), ".glue/") {
				e.dirtyFiles++
			}
		}
	}
	if e.branchMissingOnRem || e.unpushed > 0 || e.dirtyFiles > 0 {
		return e
	}
	return nil
}

func isRemoteRepoRef(raw string) bool {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, "://") {
		return true
	}
	at, colon, slash := strings.IndexByte(raw, '@'), strings.IndexByte(raw, ':'), strings.IndexByte(raw, '/')
	return !strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, ".") && at > 0 && colon > at && (slash == -1 || colon < slash)
}

func resolveAXRepoSpec(ctx context.Context, raw, baseRef string) (jobs.RepoSpec, error) {
	raw, baseRef = cmp.Or(strings.TrimSpace(raw), "."), strings.TrimSpace(baseRef)
	if isRemoteRepoRef(raw) {
		u, err := normalizeAXRepoURL(ctx, raw)
		return jobs.RepoSpec{URL: u, BaseRef: baseRef}, err
	}
	origin, err := gitOutput(ctx, raw, "config", "--get", "remote.origin.url")
	if err != nil || origin == "" {
		return jobs.RepoSpec{}, errors.New(errDirectAXMissingRemote)
	}
	u, err := normalizeAXRepoURL(ctx, origin)
	if err == nil && baseRef == "" {
		baseRef = detectDefaultBaseRef(ctx, raw)
	}
	return jobs.RepoSpec{URL: u, BaseRef: baseRef}, err
}

func normalizeAXRepoURL(ctx context.Context, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		if !isRemoteRepoRef(raw) {
			return "", errors.New(errDirectAXMissingRemote)
		}
		hostPart, pathPart, _ := strings.Cut(raw, ":")
		raw = "ssh://" + strings.TrimSpace(hostPart) + "/" + strings.TrimLeft(strings.TrimSpace(pathPart), "/")
	}
	u, err := url.Parse(raw)
	if err != nil || !slices.Contains([]string{"ssh", "git", "https", "http"}, strings.ToLower(u.Scheme)) {
		return "", errors.New(errDirectAXMissingRemote)
	}
	if s := strings.ToLower(u.Scheme); s == "ssh" || s == "git" {
		u.Host, u.Scheme = resolveSSHHost(ctx, u.Hostname()), "https"
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	if u.Host == "" || strings.Trim(u.Path, "/") == "" {
		return "", errors.New(errDirectAXMissingRemote)
	}
	return u.String(), nil
}

func resolveSSHHost(parent context.Context, host string) string {
	if host == "" || strings.HasPrefix(host, "-") || host == "github.com" || host == "gitlab.com" {
		return host
	}
	ctx, cancel := context.WithTimeout(parent, time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "ssh", "-G", "--", host).Output()
	for line := range strings.SplitSeq(string(out), "\n") {
		if resolved, ok := strings.CutPrefix(strings.TrimSpace(line), "hostname "); ok && strings.TrimSpace(resolved) != "" {
			return strings.TrimSpace(resolved)
		}
	}
	return host
}

// detectDefaultBaseRef prefers the current branch when origin has it, then origin's default branch.
func detectDefaultBaseRef(ctx context.Context, dir string) string {
	if cur, err := gitOutput(ctx, dir, "branch", "--show-current"); err == nil && cur != "" {
		if _, err := gitOutput(ctx, dir, "show-ref", "--verify", "--quiet", "refs/remotes/origin/"+cur); err == nil {
			return cur
		}
	}
	if sym, err := gitOutput(ctx, dir, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil && len(sym) > len("origin/") && strings.HasPrefix(sym, "origin/") {
		return strings.TrimPrefix(sym, "origin/")
	}
	return ""
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}
