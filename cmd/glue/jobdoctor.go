// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/erain/glue/internal/axbackend"
	"github.com/erain/glue/internal/jobclient"
	"github.com/erain/glue/jobs"
)

// doctorCheck is one line of glue job doctor output.
type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

// providerKeyEnv names the environment variable each provider's API key comes from.
var providerKeyEnv = map[string]string{"gemini": "GEMINI_API_KEY", "openrouter": "OPENROUTER_API_KEY", "deepseek": "DEEPSEEK_API_KEY", "nvidia": "NVIDIA_API_KEY", "codex": "OPENAI_API_KEY"}

// githubToken returns the GitHub token remote jobs use and where it came from.
func githubToken(ctx context.Context) (token, source string) {
	for _, k := range []string{"GLUE_GITHUB_TOKEN", "GITHUB_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v, "env " + k
		}
	}
	if v := resolveGitHubCLIToken(ctx); v != "" {
		return v, "gh auth token"
	}
	return "", ""
}

// jobDoctor checks everything a remote job needs and prints one line per
// check, with a fix for each failure. It returns an error when a check fails.
func jobDoctor(ctx context.Context, t jobTargetOpts, repo, base, provider, model, image string, asJSON bool, stdout io.Writer, httpClient httpDoer) error {
	var checks []doctorCheck
	add := func(c doctorCheck) { checks = append(checks, c) }

	api, directAX, err := resolveJobAPI(ctx, t, httpClient)
	switch {
	case err != nil && directAX:
		add(doctorCheck{Name: "target", OK: true, Detail: "AX " + axTargetText(t)})
		add(doctorCheck{Name: "ax connection", Detail: oneLine(err.Error()), Fix: "check that the cluster is up and kubectl works for this context (scripts/ax-dev/status.sh for the dev cluster), or pass --kubeconfig/--context"})
	case err != nil:
		add(doctorCheck{Name: "target", Detail: oneLine(err.Error()), Fix: "for AX: source scripts/ax-dev/env.sh or pass --context <kube-context>; for a server: pass --server and --token, or run glue job login"})
	default:
		defer api.Close()
		if directAX {
			add(doctorCheck{Name: "target", OK: true, Detail: "AX " + axTargetText(t)})
			add(doctorCheck{Name: "ax connection", OK: true, Detail: "tunnels up, router credentials minted"})
		} else {
			cfg, _ := jobclient.ResolveConfig(t.conn)
			add(doctorCheck{Name: "target", OK: true, Detail: "server " + cfg.ServerURL + " (" + serverSource(t.conn) + ")"})
		}
		lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		list, lerr := api.List(lctx, jobs.Filter{Limit: 1000})
		cancel()
		name := "server"
		if directAX {
			name = "ax-server"
		}
		if lerr != nil {
			first, _, _ := strings.Cut(lerr.Error(), "\n")
			add(doctorCheck{Name: name, Detail: oneLine(first), Fix: "start the server (glue serve --coding for a local daemon) or pass --server; for AX pass --context or source scripts/ax-dev/env.sh"})
		} else {
			add(doctorCheck{Name: name, OK: true, Detail: fmt.Sprintf("reachable, %d job(s)", len(list))})
		}
	}
	if !directAX {
		return writeDoctor(stdout, checks, asJSON)
	}

	img, src := strings.TrimSpace(image), "--image"
	if img == "" {
		img, src = axbackend.DefaultImageSource()
	}
	if img != "" {
		add(doctorCheck{Name: "task image", OK: true, Detail: img + " (" + src + ")"})
	} else {
		add(doctorCheck{Name: "task image", Detail: "no task image configured", Fix: "pass --image <ref>, set GLUE_AX_IMAGE, or build one with scripts/ax-dev/build-task-image.sh"})
	}

	rs, rerr := resolveAXRepoSpec(ctx, cmp.Or(strings.TrimSpace(repo), "."), base)
	if rerr != nil {
		add(doctorCheck{Name: "repo", Detail: oneLine(rerr.Error()), Fix: "run from a git checkout with an origin remote, or pass --repo <https-url>"})
	} else {
		add(doctorCheck{Name: "repo", OK: true, Detail: rs.URL + " @ " + cmp.Or(rs.BaseRef, "main")})
	}

	tok, src := githubToken(ctx)
	switch {
	case tok == "":
		add(doctorCheck{Name: "github token", Detail: "no GitHub token found", Fix: "set GLUE_GITHUB_TOKEN or GITHUB_TOKEN, or run gh auth login"})
	case rerr != nil:
		add(doctorCheck{Name: "github token", OK: true, Detail: "from " + src + " (push access not checked: no repo)"})
	default:
		add(githubPushCheck(ctx, httpClient, rs.URL, tok, src))
	}

	prov, _, serr := resolveSelection(true, provider, model)
	if serr != nil {
		add(doctorCheck{Name: "provider key", Detail: oneLine(serr.Error()), Fix: "pick a provider from glue models"})
	} else if env := providerKeyEnv[prov]; env != "" && strings.TrimSpace(os.Getenv(env)) == "" {
		add(doctorCheck{Name: "provider key", Detail: fmt.Sprintf("%s is not set for provider %s", env, prov), Fix: fmt.Sprintf("export %s, load it with --env <file>, or choose another provider with --provider", env)})
	} else {
		add(doctorCheck{Name: "provider key", OK: true, Detail: fmt.Sprintf("%s from env %s", prov, env)})
	}
	return writeDoctor(stdout, checks, asJSON)
}

func axTargetText(t jobTargetOpts) string {
	kc, kctx, _ := resolveKubeTarget(t.kubeconfig, t.kubectx)
	return fmt.Sprintf("context %s, kubeconfig %s, atespace %s", cmp.Or(kctx, "(default)"), cmp.Or(kc, "(default)"), cmp.Or(strings.TrimSpace(t.atespace), "default"))
}

func serverSource(c jobclient.Config) string {
	switch {
	case strings.TrimSpace(c.ServerURL) != "":
		return "--server"
	case strings.TrimSpace(os.Getenv("GLUE_SERVER")) != "":
		return "env GLUE_SERVER"
	default:
		return "local daemon metadata " + c.MetadataPath
	}
}

// githubPushCheck asks the GitHub API whether the token can push to repoURL.
func githubPushCheck(ctx context.Context, httpClient httpDoer, repoURL, tok, src string) doctorCheck {
	c := doctorCheck{Name: "github token"}
	u, err := url.Parse(repoURL)
	parts := strings.Split(strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/"), "/")
	if err != nil || u.Hostname() != "github.com" || len(parts) != 2 {
		c.OK, c.Detail = true, "from "+src+" (push access not checked: not a github.com repo)"
		return c
	}
	api := cmp.Or(strings.TrimRight(strings.TrimSpace(os.Getenv("GLUE_GITHUB_API_URL")), "/"), "https://api.github.com")
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(rctx, http.MethodGet, api+"/repos/"+parts[0]+"/"+parts[1], nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := cmp.Or[httpDoer](httpClient, http.DefaultClient).Do(req)
	if err != nil {
		c.Detail, c.Fix = "from "+src+"; GitHub API unreachable: "+oneLine(err.Error()), "check your network, then run glue job doctor again"
		return c
	}
	defer resp.Body.Close()
	var body struct {
		Permissions struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body)
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		c.Detail, c.Fix = "token from "+src+" was rejected by GitHub", "refresh it (gh auth refresh, or a new token)"
	case resp.StatusCode != http.StatusOK:
		c.Detail, c.Fix = fmt.Sprintf("token from %s cannot see %s/%s (HTTP %d)", src, parts[0], parts[1], resp.StatusCode), "use a token with access to this repo"
	case !body.Permissions.Push:
		c.Detail, c.Fix = fmt.Sprintf("token from %s has no push access to %s/%s", src, parts[0], parts[1]), "use a token with push access, or submit with --deliver none"
	default:
		c.OK, c.Detail = true, fmt.Sprintf("from %s, can push to %s/%s", src, parts[0], parts[1])
	}
	return c
}

func writeDoctor(w io.Writer, checks []doctorCheck, asJSON bool) error {
	failed := 0
	for _, c := range checks {
		if !c.OK {
			failed++
		}
	}
	if asJSON {
		data, _ := json.MarshalIndent(map[string]any{"ok": failed == 0, "checks": checks}, "", "  ")
		fmt.Fprintf(w, "%s\n", data)
	} else {
		for _, c := range checks {
			mark := "ok  "
			if !c.OK {
				mark = "FAIL"
			}
			fmt.Fprintf(w, "%s  %-14s %s\n", mark, c.Name, c.Detail)
			if c.Fix != "" {
				fmt.Fprintf(w, "      %-14s fix: %s\n", "", c.Fix)
			}
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	return nil
}

// hintDoer adds a pointer to glue job doctor when the job server cannot be reached.
type hintDoer struct {
	inner          httpDoer
	server, source string
}

func (h hintDoer) Do(req *http.Request) (*http.Response, error) {
	resp, err := cmp.Or[httpDoer](h.inner, http.DefaultClient).Do(req)
	if err != nil && isDialError(err) {
		return nil, fmt.Errorf("cannot reach the job server %s (from %s): %w\nStart it with glue serve, pass --server, or for AX pass --context or source scripts/ax-dev/env.sh. Run glue job doctor to see what glue resolved", h.server, h.source, err)
	}
	return resp, err
}

func isDialError(err error) bool {
	var op *net.OpError
	return errors.Is(err, syscall.ECONNREFUSED) || (errors.As(err, &op) && op.Op == "dial")
}
