// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/erain/glue/internal/jobclient"
	"github.com/erain/glue/internal/qr"
	"github.com/erain/glue/internal/redact"
	"github.com/erain/glue/jobs"
	"gopkg.in/yaml.v3"
)

// remoteConfigPath is where a repo describes how its remote jobs run.
const remoteConfigPath = ".glue/remote.yaml"

// remoteConfig is the content of .glue/remote.yaml. Every field is optional;
// command-line flags take precedence over it.
type remoteConfig struct {
	Image       string   `yaml:"image"`
	Setup       []string `yaml:"setup"`
	Verify      []string `yaml:"verify"`
	Deliver     string   `yaml:"deliver"`
	Policy      string   `yaml:"policy"`
	EgressHosts []string `yaml:"egress_hosts"`
	Resources   struct {
		CPU    string `yaml:"cpu"`
		Memory string `yaml:"memory"`
	} `yaml:"resources"`
	Budgets struct {
		WallClock     string `yaml:"wall_clock"`
		TokenBudget   int64  `yaml:"token_budget"`
		MaxIterations int    `yaml:"max_iterations"`
	} `yaml:"budgets"`
}

// loadRemoteConfig reads .glue/remote.yaml from the git checkout containing
// dir. A missing file is not an error. Unknown keys and wrong types are.
func loadRemoteConfig(dir string) (*remoteConfig, string, error) {
	root := dir
	if out, err := gitOutput(context.Background(), dir, "rev-parse", "--show-toplevel"); err == nil && out != "" {
		root = out
	}
	path := filepath.Join(root, remoteConfigPath)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, path, err
	}
	var cfg remoteConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) { // an empty file is an empty config
		return nil, path, fmt.Errorf("%s: %w", remoteConfigPath, err)
	}
	if cfg.Budgets.WallClock != "" {
		if _, err := time.ParseDuration(cfg.Budgets.WallClock); err != nil {
			return nil, path, fmt.Errorf("%s: budgets.wall_clock: %w", remoteConfigPath, err)
		}
	}
	for _, h := range cfg.EgressHosts {
		if h = strings.TrimSpace(h); h == "" || strings.ContainsAny(h, "/: ") {
			return nil, path, fmt.Errorf("%s: egress_hosts: %q is not a host name", remoteConfigPath, h)
		}
	}
	return &cfg, path, nil
}

// applyRemoteConfig copies config values into the submit options whose flags
// were not set on the command line, and describes what it applied.
func applyRemoteConfig(cfg *remoteConfig, set map[string]bool, t *jobTargetOpts, verify *repeatedStrings, deliver, policy *string, wallClock *time.Duration, budget *int64, maxIter *int) string {
	var used []string
	use := func(key string, ok bool) {
		if ok {
			used = append(used, key)
		}
	}
	if cfg.Image != "" && !set["image"] {
		t.image = cfg.Image
		use("image", true)
	}
	if len(cfg.Verify) > 0 && !set["verify"] {
		*verify = append(repeatedStrings(nil), cfg.Verify...)
		use("verify", true)
	}
	if cfg.Deliver != "" && !set["deliver"] && !set["delivery"] {
		*deliver = cfg.Deliver
		use("deliver", true)
	}
	if cfg.Policy != "" && !set["policy"] {
		*policy = cfg.Policy
		use("policy", true)
	}
	if len(cfg.EgressHosts) > 0 {
		t.egress = append(t.egress, cfg.EgressHosts...)
		use("egress_hosts", true)
	}
	if cfg.Resources.CPU != "" && !set["cpu"] {
		t.cpu = cfg.Resources.CPU
		use("resources.cpu", true)
	}
	if cfg.Resources.Memory != "" && !set["memory"] {
		t.memory = cfg.Resources.Memory
		use("resources.memory", true)
	}
	if cfg.Budgets.WallClock != "" && !set["wall-clock"] {
		*wallClock, _ = time.ParseDuration(cfg.Budgets.WallClock)
		use("budgets.wall_clock", true)
	}
	if cfg.Budgets.TokenBudget > 0 && !set["budget"] {
		*budget = cfg.Budgets.TokenBudget
		use("budgets.token_budget", true)
	}
	if cfg.Budgets.MaxIterations > 0 && !set["max-iterations"] {
		*maxIter = cfg.Budgets.MaxIterations
		use("budgets.max_iterations", true)
	}
	if len(cfg.Setup) > 0 {
		use("setup", true)
	}
	if len(used) == 0 {
		return remoteConfigPath + " (nothing applied)"
	}
	return remoteConfigPath + ": " + strings.Join(used, ", ")
}

// splitCommand splits a command line into argv the way a POSIX shell would
// for plain words: whitespace separates arguments, single quotes keep text
// literally, double quotes keep spaces and honor \" and \\, and a backslash
// outside quotes escapes the next character. It does not expand variables or
// globs; use sh -c '...' for that.
func splitCommand(line string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inArg, quote := false, rune(0)
	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteRune(c)
			}
		case quote == '"':
			switch {
			case c == '"':
				quote = 0
			case c == '\\' && i+1 < len(runes) && (runes[i+1] == '"' || runes[i+1] == '\\'):
				i++
				cur.WriteRune(runes[i])
			default:
				cur.WriteRune(c)
			}
		case c == '\'' || c == '"':
			quote, inArg = c, true
		case c == '\\' && i+1 < len(runes):
			i++
			cur.WriteRune(runes[i])
			inArg = true
		case c == ' ' || c == '\t' || c == '\n':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(c)
			inArg = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote in %q", quote, line)
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args, nil
}

// userConfig holds personal remote defaults in ~/.config/glue/config.yaml.
type userConfig struct {
	AX        struct{ Context, Kubeconfig, Atespace string } `yaml:"ax,omitempty"`
	PublicURL string                                         `yaml:"public_url,omitempty"`
	Notify    []string                                       `yaml:"notify,omitempty"`
	Deliver   string                                         `yaml:"deliver,omitempty"`
	Provider  string                                         `yaml:"provider,omitempty"`
	Model     string                                         `yaml:"model,omitempty"`
	Thinking  string                                         `yaml:"thinking,omitempty"`
}

func loadUserConfig() (*userConfig, string, error) {
	path := filepath.Join(jobclient.ConfigDir(), "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, path, map[bool]error{false: err}[errors.Is(err, os.ErrNotExist)]
	}
	var cfg userConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, path, fmt.Errorf("%s: %w", path, err)
	}
	if !slices.Contains([]string{"", "none", "branch", "pr", "draft_pr"}, strings.TrimSpace(cfg.Deliver)) || (strings.TrimSpace(cfg.PublicURL) != "" && jobs.ValidNotifyURL(cfg.PublicURL) != nil) {
		return nil, path, fmt.Errorf("%s: invalid deliver or public_url", path)
	}
	for i, u := range cfg.Notify {
		if u = strings.TrimSpace(u); jobs.ValidNotifyURL(u) != nil {
			return nil, path, fmt.Errorf("%s: invalid notify URL %s", path, redactNotify(u))
		}
		cfg.Notify[i] = u
		redact.Register(u)
	}
	return &cfg, path, nil
}

func saveUserConfig(cfg *userConfig) (string, error) {
	dir := jobclient.ConfigDir()
	path, tmp := filepath.Join(dir, "config.yaml"), filepath.Join(dir, fmt.Sprintf(".config.yaml.%d", time.Now().UnixNano()))
	data, err := yaml.Marshal(cfg)
	if err = errors.Join(err, os.MkdirAll(dir, 0o700), os.Chmod(dir, 0o700)); err == nil {
		if err = errors.Join(os.WriteFile(tmp, data, 0o600), os.Chmod(tmp, 0o600)); err == nil {
			return path, os.Rename(tmp, path)
		}
	}
	_ = os.Remove(tmp)
	return path, err
}

func resolveUserAXTarget(kubeCfg, kubeCtx, atespace string) (string, string, string) {
	u, _, _ := loadUserConfig()
	u, trim := cmp.Or(u, &userConfig{}), strings.TrimSpace
	kc := cmp.Or(trim(kubeCfg), map[bool]string{false: u.AX.Kubeconfig}[envSet("GLUE_AX_KUBECONFIG", "KUBECONFIG")])
	kctx := cmp.Or(trim(kubeCtx), map[bool]string{false: u.AX.Context}[envSet("GLUE_AX_CONTEXT", "KUBE_CONTEXT", "KUBECONTEXT")])
	return trim(kc), trim(kctx), cmp.Or(trim(atespace), trim(os.Getenv("GLUE_AX_ATESPACE")), trim(u.AX.Atespace))
}

func applyUserConfig(cfg *userConfig, set map[string]bool, t *jobTargetOpts, provider, model, thinking, deliver *string) {
	u, trim := cmp.Or(cfg, &userConfig{}), strings.TrimSpace
	t.kubeconfig, t.kubectx, t.atespace = resolveUserAXTarget(t.kubeconfig, t.kubectx, t.atespace)
	or := func(keep bool, cur, env, def string) string {
		return map[bool]string{true: cur, false: cmp.Or(trim(os.Getenv(env)), trim(def))}[keep]
	}
	*provider = or(set["provider"], *provider, "GLUE_PROVIDER", u.Provider)
	sameProv := trim(u.Provider) == "" || strings.EqualFold(trim(*provider), trim(u.Provider))
	*model = or(set["model"], *model, "GLUE_MODEL", map[bool]string{true: u.Model}[sameProv])
	*thinking = or(set["thinking"], *thinking, "GLUE_THINKING", map[bool]string{true: u.Thinking}[sameProv && ((!set["model"] && !envSet("GLUE_MODEL")) || strings.EqualFold(normalizeModel(trim(*model)), normalizeModel(trim(u.Model))))])
	*deliver, set["deliver"] = or(set["deliver"] || set["delivery"], *deliver, "GLUE_DELIVER", cmp.Or(*deliver, u.Deliver)), set["deliver"] || envSet("GLUE_DELIVER")
}

func redactNotify(urls ...string) string {
	out := make([]string, len(urls))
	for i, raw := range urls {
		out[i] = redact.Replacement
		if u, err := url.Parse(strings.TrimSpace(raw)); err == nil && u.Scheme != "" && u.Host != "" {
			seg := strings.Trim(u.Path, "/")
			out[i] = u.Scheme + "://" + u.Host + "/" + seg[:min(len(seg), max(0, len(seg)-1), 4+5*map[bool]int{true: 1}[strings.HasPrefix(seg, "glue-")])] + "…"
		}
	}
	return strings.Join(out, ", ")
}

func jobInit(ctx context.Context, t jobTargetOpts, repo, base, provider, model string, stdout io.Writer, httpClient httpDoer) error {
	_ = jobDoctor(ctx, t, repo, base, provider, model, t.image, false, stdout, httpClient)
	cfg, _, err := loadUserConfig()
	urls, generated := notifyURLs(t.notify), false
	if err == nil && len(urls) == 0 {
		var rnd [16]byte
		_, _ = rand.Read(rnd[:])
		urls, generated = []string{"https://ntfy.sh/glue-" + hex.EncodeToString(rnd[:])}, true
		redact.Register(urls[0])
	}
	if err != nil {
		return err
	}
	qrArt, err := qr.Render(urls[0])
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\nScan in the ntfy app (iOS/Android): %s\n%s", map[bool]string{true: urls[0], false: redactNotify(urls[0])}[generated], qrArt)
	const msg = "Glue phone notifications connected"
	for _, u := range urls {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		ntfy := strings.Contains(u, "://ntfy.") || strings.Contains(u, "/glue-")
		req, _ := http.NewRequestWithContext(rctx, http.MethodPost, u, strings.NewReader(map[bool]string{true: msg, false: `{"event":"init","status":"ok","text":"` + msg + `"}`}[ntfy]))
		req.Header.Set("Content-Type", map[bool]string{true: "text/plain; charset=utf-8", false: "application/json"}[ntfy])
		req.Header.Set("Title", "Glue")
		resp, err := cmp.Or[httpDoer](httpClient, http.DefaultClient).Do(req)
		cancel()
		if err == nil {
			resp.Body.Close()
			err = map[bool]error{false: fmt.Errorf("HTTP %d", resp.StatusCode)}[resp.StatusCode >= 200 && resp.StatusCode < 300]
		}
		if err != nil {
			return fmt.Errorf("notify %s: %s", redactNotify(u), strings.ReplaceAll(err.Error(), u, redactNotify(u)))
		}
	}
	c := *cmp.Or(cfg, &userConfig{})
	c.Notify = urls
	savedPath, err := saveUserConfig(&c)
	_, werr := fmt.Fprintf(map[bool]io.Writer{true: stdout, false: io.Discard}[err == nil], "verified test notification to %s\nsaved %s (0600)\n", redactNotify(urls...), savedPath)
	return errors.Join(err, werr)
}
