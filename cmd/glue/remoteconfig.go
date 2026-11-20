// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/erain/glue/internal/axbackend"
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
	if t != nil {
		t.kubeconfig, t.kubectx, t.atespace = resolveUserAXTarget(t.kubeconfig, t.kubectx, t.atespace)
	}
	or := func(keep bool, cur, env, def string) string {
		return map[bool]string{true: cur, false: cmp.Or(trim(os.Getenv(env)), trim(def))}[keep]
	}
	*provider = or(set["provider"], *provider, "GLUE_PROVIDER", u.Provider)
	sameProv := (trim(u.Provider) == "" && (trim(*provider) == "" || strings.EqualFold(trim(*provider), "gemini"))) || strings.EqualFold(trim(*provider), trim(u.Provider))
	*model = or(set["model"], *model, "GLUE_MODEL", map[bool]string{true: u.Model}[sameProv])
	*thinking = or(set["thinking"], *thinking, "GLUE_THINKING", map[bool]string{true: u.Thinking}[sameProv && ((!set["model"] && !envSet("GLUE_MODEL")) || strings.EqualFold(normalizeModel(trim(*model)), normalizeModel(trim(u.Model))))])
	if deliver != nil {
		*deliver, set["deliver"] = or(set["deliver"] || set["delivery"], *deliver, "GLUE_DELIVER", cmp.Or(*deliver, u.Deliver)), set["deliver"] || envSet("GLUE_DELIVER")
	}
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

const remoteGatewayNS, remoteUsage, remoteManifestsT = "glue-gateway", "Usage:\n  glue remote <setup|status|down> [flags]", "apiVersion: v1\nkind: Namespace\nmetadata: {name: glue-gateway, labels: {app.kubernetes.io/name: glue-gateway}}\n---\napiVersion: v1\nkind: ServiceAccount\nmetadata: {name: glue-gateway, namespace: glue-gateway}\n---\napiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata: {name: glue-gateway}\nrules: [{apiGroups: [certificates.k8s.io], resources: [clustertrustbundles], verbs: [get, list]}]\n---\napiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata: {name: glue-gateway}\nroleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: glue-gateway}\nsubjects: [{kind: ServiceAccount, name: glue-gateway, namespace: glue-gateway}]\n---\napiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata: {name: glue-gateway-ax, namespace: %q}\nrules: [{apiGroups: [certificates.k8s.io], resources: [podcertificaterequests], verbs: [create, get, delete]}, {apiGroups: [\"\"], resources: [secrets], resourceNames: [glue-ax-task-hmac], verbs: [get]}, {apiGroups: [\"\"], resources: [secrets], verbs: [create]}]\n---\napiVersion: rbac.authorization.k8s.io/v1\nkind: RoleBinding\nmetadata: {name: glue-gateway-ax, namespace: %q}\nroleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: glue-gateway-ax}\nsubjects: [{kind: ServiceAccount, name: glue-gateway, namespace: glue-gateway}]\n---\napiVersion: v1\nkind: PersistentVolumeClaim\nmetadata: {name: glue-gateway-state, namespace: glue-gateway}\nspec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 1Gi}}}\n---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: glue-gateway, namespace: glue-gateway}\ndata: {gateway.yaml: %s}\n---\napiVersion: v1\nkind: Secret\nmetadata: {name: glue-gateway-secrets, namespace: glue-gateway}\ntype: Opaque\nstringData: %s\n---\napiVersion: apps/v1\nkind: Deployment\nmetadata: {name: glue-gateway, namespace: glue-gateway}\nspec: {replicas: 1, strategy: {type: Recreate}, selector: {matchLabels: {app: glue-gateway}}, template: {metadata: {labels: {app: glue-gateway}, annotations: {glue.erain.dev/config-hash: %q}}, spec: {serviceAccountName: glue-gateway, securityContext: {seccompProfile: {type: RuntimeDefault}}, containers: [{name: glue-gateway, image: %q, args: [--config, /etc/glue-gateway/gateway.yaml], ports: [{name: http, containerPort: 8800}], env: [{name: GLUE_CONFIG_DIR, value: /var/lib/glue-gateway/.config}, {name: AX_SYSTEM_NAMESPACE, value: %q}], envFrom: [{secretRef: {name: glue-gateway-secrets}}], readinessProbe: {httpGet: {path: /healthz, port: 8800}, initialDelaySeconds: 2, periodSeconds: 5}, livenessProbe: {httpGet: {path: /healthz, port: 8800}, initialDelaySeconds: 5, periodSeconds: 10}, securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}, volumeMounts: [{name: state, mountPath: /var/lib/glue-gateway}, {name: config, mountPath: /etc/glue-gateway, readOnly: true}]}%s], volumes: [{name: state, persistentVolumeClaim: {claimName: glue-gateway-state}}, {name: config, configMap: {name: glue-gateway}}]}}}\n---\napiVersion: v1\nkind: Service\nmetadata: {name: glue-gateway, namespace: glue-gateway}\nspec: {selector: {app: glue-gateway}, ports: [{name: http, port: 8800, targetPort: 8800}]}\n"

var (
	remoteForward = axbackend.EnsureTunnel
	remoteExec    = func(ctx context.Context, dir string, stdin []byte, name string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir, cmd.Stdin = dir, bytes.NewReader(stdin)
		return cmd.CombinedOutput()
	}
)

type remoteGatewayYAML struct {
	Listen              string
	StateDir            string `yaml:"state_dir"`
	PublicURL           string `yaml:"public_url,omitempty"`
	TrustForwardedProto bool   `yaml:"trust_forwarded_proto,omitempty"`
	DefaultBackend      string `yaml:"default_backend"`
	Auth                struct {
		Mode, AUD     string
		TeamDomain    string   `yaml:"team_domain,omitempty"`
		AllowedEmails []string `yaml:"allowed_emails,omitempty"`
		CertsURL      string   `yaml:"certs_url,omitempty"`
		InsecureDev   bool     `yaml:"insecure_dev,omitempty"`
	}
	Backends []struct{ Name, Type, Atespace, Image string }
}

func validateGatewayYAML(raw []byte, hasDevToken bool) (remoteGatewayYAML, error) {
	var cfg remoteGatewayYAML
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("gateway.yaml: %w", err)
	}
	if td := strings.TrimRight(strings.TrimSpace(cfg.Auth.TeamDomain), "/"); td != "" {
		u, err := url.Parse(map[bool]string{true: td, false: "https://" + td}[strings.Contains(td, "://")])
		if err != nil || u.Hostname() == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && slices.Contains([]string{"localhost", "127.0.0.1", "::1"}, strings.ToLower(u.Hostname())))) {
			return cfg, fmt.Errorf("invalid --cf-team %q", cfg.Auth.TeamDomain)
		}
		cfg.Auth.TeamDomain = strings.ToLower(u.Scheme + "://" + u.Host)
	}
	authOK := (cfg.Auth.Mode == "dev-token" && hasDevToken && (!strings.HasPrefix(strings.ToLower(cfg.PublicURL), "https://") || cfg.Auth.InsecureDev)) || (cfg.Auth.Mode == "cloudflare-access" && cfg.Auth.TeamDomain != "" && strings.TrimSpace(cfg.Auth.AUD) != "")
	bad := cfg.StateDir == "" || !authOK || slices.ContainsFunc(cfg.Auth.AllowedEmails, func(s string) bool { s = strings.TrimSpace(s); return s == "" || s == "@" }) || len(cfg.Backends) == 0 || cfg.Backends[0].Type != "ax" || strings.TrimSpace(cfg.Backends[0].Image) == ""
	return cfg, map[bool]error{true: fmt.Errorf("invalid gateway configuration (mode=%q)", cfg.Auth.Mode)}[bad]
}

func remoteCommand(ctx context.Context, args []string, stdout, _ io.Writer, httpClient httpDoer) (retErr error) {
	defer func() {
		if retErr != nil {
			retErr = errors.New(redact.String(retErr.Error()))
		}
	}()
	fs := flag.NewFlagSet("glue remote", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	publicURL, cfTeam, cfAUD, cfCerts := fs.String("public-url", "", "public https:// URL"), fs.String("cf-team", "", "Cloudflare Access team domain"), fs.String("cf-aud", "", "Cloudflare Access AUD tag"), fs.String("cf-certs-url", "", "Cloudflare Access JWKS URL")
	fs.StringVar(cfTeam, "cf-team-domain", "", "alias for --cf-team")
	var allowEmails repeatedStrings
	var devTok remoteFlag
	fs.Var(&allowEmails, "allow-email", "allowed email or @domain; repeatable")
	fs.Var(&devTok, "dev-token", "enable dev-token auth")
	insecureDev, saveCfg, buildLocal, dryRun, purgePVC := fs.Bool("insecure-dev", false, "allow --dev-token with https://"), fs.Bool("save-config", true, "save --public-url to config.yaml"), fs.Bool("build-local", false, "build local gateway image"), fs.Bool("dry-run", false, "print redacted manifests"), fs.Bool("purge-pvc", false, "down: delete PVC and namespace")
	tunnelTok, gwImg, taskImg, kubeCfg, kubeCtx, atespace := fs.String("tunnel-token", "", "Cloudflare Tunnel token"), fs.String("gateway-image", "", "glue-gateway image"), fs.String("image", "", "AX task image"), fs.String("kubeconfig", "", "kubeconfig path"), fs.String("context", "", "Kubernetes context"), fs.String("atespace", "", "AX atespace")
	pos, err := parseInterspersed(fs, args)
	if len(args) == 0 || slices.Contains([]string{"-h", "-help", "--help", "help"}, args[0]) || errors.Is(err, flag.ErrHelp) {
		printFlagSetUsage(fs, stdout, remoteUsage)
		return nil
	}
	if devTok.enabled && devTok.objective == "" && len(pos) > 1 && pos[0] == "setup" {
		devTok.objective, pos = pos[1], pos[:1]
	}
	redact.Register(*tunnelTok, os.Getenv("CLOUDFLARE_TUNNEL_TOKEN"), devTok.objective, os.Getenv("GLUE_GATEWAY_TOKEN"), os.Getenv("GLUE_TOKEN"))
	kc, kctx, ats := resolveUserAXTarget(*kubeCfg, *kubeCtx, *atespace)
	axNS := cmp.Or(strings.TrimSpace(os.Getenv("AX_SYSTEM_NAMESPACE")), axbackend.DefaultAXNamespace)
	if err = cmp.Or(err, map[bool]error{true: fmt.Errorf("expected setup, status, or down\n%s", remoteUsage)}[len(pos) != 1 || !slices.Contains([]string{"setup", "status", "down"}, pos[0])]); err == nil && !*dryRun {
		kc, kctx, err = resolveKubeTarget(kc, kctx)
	}
	if err != nil {
		return err
	}
	run := func(dir string, in []byte, name string, a ...string) ([]byte, error) {
		out, e := remoteExec(ctx, dir, in, name, a...)
		return out, map[bool]error{false: fmt.Errorf("%s %s: %w (%s)", name, a[0], e, redact.String(strings.TrimSpace(string(out))))}[e == nil]
	}
	kubectl := func(stdin []byte, a ...string) ([]byte, error) {
		kf := append(append([]string(nil), map[bool][]string{true: {"--kubeconfig", kc}}[kc != ""]...), map[bool][]string{true: {"--context", kctx}}[kctx != ""]...)
		return run("", stdin, "kubectl", append(kf, a...)...)
	}
	probe := func(tok string) (string, error) {
		addr, e := remoteForward(ctx, kc, kctx, remoteGatewayNS, "glue-gateway", 8800, "/healthz")
		if e != nil {
			return "", e
		}
		get := func(path, bearer string) (int, map[string]any, error) {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
			req.Header = map[bool]http.Header{true: {"Authorization": {"Bearer " + bearer}}, false: {}}[bearer != ""]
			resp, re := cmp.Or[httpDoer](httpClient, http.DefaultClient).Do(req)
			if re != nil {
				return 0, nil, re
			}
			defer resp.Body.Close()
			var doc map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&doc)
			return resp.StatusCode, doc, nil
		}
		st1, hdoc, e1 := get("/healthz", "")
		st2, bdoc, e2 := get("/v1/backends", tok)
		def, _ := bdoc["default_backend"].(string)
		bad := e1 != nil || st1 != http.StatusOK || hdoc["ok"] != true || e2 != nil || (st2 != http.StatusOK && !(st2 == http.StatusUnauthorized && tok == ""))
		return map[bool]string{true: "ok (" + cmp.Or(def, "ax") + ")", false: "ok (ax, access-protected)"}[st2 == http.StatusOK], map[bool]error{true: cmp.Or(e1, e2, fmt.Errorf("glue-gateway probe failed (healthz=%d, backends=%d)", st1, st2))}[bad]
	}
	if pos[0] == "down" {
		_ = axbackend.StopTunnels(kctx)
		qs := [][]string{append([]string{"-n", remoteGatewayNS, "delete", "--ignore-not-found", "deployment/glue-gateway", "service/glue-gateway", "configmap/glue-gateway", "secret/glue-gateway-secrets", "serviceaccount/glue-gateway"}, map[bool][]string{true: {"pvc/glue-gateway-state"}}[*purgePVC]...), {"-n", axNS, "delete", "--ignore-not-found", "role/glue-gateway-ax", "rolebinding/glue-gateway-ax"}, {"delete", "--ignore-not-found", "clusterrole/glue-gateway", "clusterrolebinding/glue-gateway"}}
		for _, q := range append(qs, map[bool][][]string{true: {{"delete", "--ignore-not-found", "namespace/" + remoteGatewayNS}}}[*purgePVC]...) {
			_, e := kubectl(nil, q...)
			err = errors.Join(err, e)
		}
		_, werr := fmt.Fprintf(map[bool]io.Writer{true: stdout, false: io.Discard}[err == nil], "Removed glue-gateway resources (purge_pvc=%v)\n", *purgePVC)
		return errors.Join(err, werr)
	}
	if pos[0] == "status" {
		cmOut, e1 := kubectl(nil, "-n", remoteGatewayNS, "get", "configmap", "glue-gateway", "-o", `jsonpath={.data.gateway\.yaml}`)
		repOut, e2 := kubectl(nil, "-n", remoteGatewayNS, "get", "deployment", "glue-gateway", "-o", "jsonpath={.status.readyReplicas}/{.spec.replicas}")
		var cfg remoteGatewayYAML
		if _ = yaml.Unmarshal(cmOut, &cfg); errors.Join(e1, e2) != nil {
			return fmt.Errorf("glue-gateway status: %w", errors.Join(e1, e2))
		}
		rep, tok := strings.TrimSpace(string(repOut)), strings.TrimSpace(os.Getenv("GLUE_TOKEN"))
		if tok == "" && cfg.Auth.Mode == "dev-token" {
			raw, _ := kubectl(nil, "-n", remoteGatewayNS, "get", "secret", "glue-gateway-secrets", "-o", "jsonpath={.data.GLUE_GATEWAY_TOKEN}")
			dec, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
			tok = strings.TrimSpace(string(dec))
		}
		redact.Register(tok)
		bsum, err := probe(tok)
		_, werr := fmt.Fprintf(map[bool]io.Writer{true: stdout, false: io.Discard}[err == nil], "namespace: %s  ready: %s%s  auth: %s  public_url: %s  healthz: ok  backends: %s\n", remoteGatewayNS, map[bool]string{true: "0"}[strings.HasPrefix(rep, "/")], rep, cfg.Auth.Mode, cmp.Or(cfg.PublicURL, "(cluster-only)"), bsum)
		return errors.Join(err, werr)
	}
	pub, team, aud := strings.TrimRight(strings.TrimSpace(*publicURL), "/"), cmp.Or(strings.TrimSpace(*cfTeam), strings.TrimSpace(os.Getenv("GLUE_CF_ACCESS_TEAM_DOMAIN"))), cmp.Or(strings.TrimSpace(*cfAUD), strings.TrimSpace(os.Getenv("GLUE_CF_ACCESS_AUD")))
	allowEmails = append(allowEmails, strings.FieldsFunc(map[bool]string{true: os.Getenv("GLUE_ALLOWED_EMAILS")}[len(allowEmails) == 0], func(r rune) bool { return r == ',' })...)
	if (pub != "" && jobs.ValidNotifyURL(pub) != nil) || ((team != "" || aud != "") && devTok.enabled) || (!devTok.enabled && (team == "" || aud == "")) || (!devTok.enabled && !strings.HasPrefix(strings.ToLower(pub), "https://") && !strings.HasPrefix(strings.ToLower(pub), "http://localhost") && !strings.HasPrefix(strings.ToLower(pub), "http://127.0.0.1")) {
		return errors.New("pass valid --public-url (https://...), --cf-team, and --cf-aud, or pass --dev-token")
	}
	ghTok, _ := githubToken(ctx)
	var rnd [32]byte
	_, _ = rand.Read(rnd[:])
	secrets, secRed := map[string]string{"GLUE_GITHUB_TOKEN": ghTok, "GLUE_NOTIFY_URL": strings.Join(notifyURLs(nil), ","), "TUNNEL_TOKEN": cmp.Or(strings.TrimSpace(*tunnelTok), os.Getenv("CLOUDFLARE_TUNNEL_TOKEN")), "GLUE_GATEWAY_TOKEN": map[bool]string{true: cmp.Or(strings.TrimSpace(devTok.objective), strings.TrimSpace(os.Getenv("GLUE_GATEWAY_TOKEN")), hex.EncodeToString(rnd[:]))}[devTok.enabled]}, map[string]string{}
	for _, env := range providerKeyEnv {
		secrets[env] = os.Getenv(env)
	}
	for k, v := range secrets {
		delete(secrets, k)
		if v = strings.TrimSpace(v); v != "" {
			redact.Register(v)
			secrets[k], secRed[k] = v, redact.Replacement
		}
	}
	mode := map[bool]string{true: "dev-token", false: "cloudflare-access"}[devTok.enabled]
	team = map[bool]string{true: "https://" + team, false: team}[team != "" && !strings.Contains(team, "://")]
	var gwCfg remoteGatewayYAML
	gwCfg.Listen, gwCfg.PublicURL, gwCfg.TrustForwardedProto, gwCfg.StateDir, gwCfg.DefaultBackend = "0.0.0.0:8800", pub, secrets["TUNNEL_TOKEN"] != "" || strings.HasPrefix(strings.ToLower(pub), "https://"), "/var/lib/glue-gateway", "ax"
	gwCfg.Auth.Mode, gwCfg.Auth.TeamDomain, gwCfg.Auth.AUD, gwCfg.Auth.AllowedEmails, gwCfg.Auth.CertsURL, gwCfg.Auth.InsecureDev = mode, strings.ToLower(strings.TrimRight(team, "/")), aud, allowEmails, strings.TrimSpace(*cfCerts), *insecureDev
	gwCfg.Backends = append(gwCfg.Backends, struct{ Name, Type, Atespace, Image string }{"ax", "ax", cmp.Or(ats, "default"), cmp.Or(strings.TrimSpace(*taskImg), axbackend.DefaultImage(), axbackend.PublishedImage)})
	gwRaw, _ := yaml.Marshal(gwCfg)
	if _, err = validateGatewayYAML(gwRaw, secrets["GLUE_GATEWAY_TOKEN"] != ""); err != nil {
		return err
	}
	img := cmp.Or(strings.TrimSpace(*gwImg), strings.TrimSpace(os.Getenv("GLUE_GATEWAY_IMAGE")), "ghcr.io/erain/glue-gateway:main")
	if *buildLocal && !*dryRun {
		root, _ := gitOutput(ctx, ".", "rev-parse", "--show-toplevel")
		root, img = cmp.Or(root, "."), cmp.Or(strings.TrimSpace(*gwImg), "localhost:5001/glue-gateway:dev")
		if _, err = run(root, nil, "docker", "build", "--build-arg", "TARGETARCH="+runtime.GOARCH, "-t", img, "-f", filepath.Join(root, "deploy/gateway/Dockerfile"), root); err == nil {
			if _, err = run(root, nil, "docker", "push", img); err == nil {
				out, _ := run(root, nil, "docker", "inspect", "--format={{index .RepoDigests 0}}", img)
				img = cmp.Or(strings.TrimSpace(string(out)), img)
			} else if name, ok := strings.CutPrefix(kctx, "kind-"); ok && name != "" {
				_, err = run(root, nil, "kind", "load", "docker-image", "--name", name, img)
			}
		}
		if err != nil {
			return err
		}
	}
	j := func(v any) []byte { b, _ := json.Marshal(v); return b }
	sum := sha256.Sum256(append([]byte(string(gwRaw)+"\x00"+img+"\x00"), j(secrets)...))
	sidecar := map[bool]string{true: ", {name: cloudflared, image: \"cloudflare/cloudflared:2024.12.2\", args: [tunnel, --no-autoupdate, --url, \"http://127.0.0.1:8800\", run], env: [{name: TUNNEL_TOKEN, valueFrom: {secretKeyRef: {name: glue-gateway-secrets, key: TUNNEL_TOKEN}}}], securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}}"}[secrets["TUNNEL_TOKEN"] != ""]
	manifest := fmt.Appendf(nil, remoteManifestsT, axNS, axNS, j(string(gwRaw)), j(map[bool]map[string]string{true: secRed, false: secrets}[*dryRun]), hex.EncodeToString(sum[:16]), img, axNS, sidecar)
	if *dryRun {
		_, err = fmt.Fprint(stdout, redact.String(string(manifest)))
		return err
	}
	if _, err = kubectl(manifest, "apply", "-f", "-"); err == nil {
		if _, err = kubectl(nil, "-n", remoteGatewayNS, "rollout", "status", "deployment/glue-gateway", "--timeout=120s"); err == nil {
			_, err = probe(secrets["GLUE_GATEWAY_TOKEN"])
		}
	}
	if err == nil && *saveCfg && pub != "" {
		ucfg, _, uerr := loadUserConfig()
		c := *cmp.Or(ucfg, &userConfig{})
		if c.PublicURL, err = pub, uerr; err == nil {
			_, err = saveUserConfig(&c)
		}
	}
	_, werr := fmt.Fprintf(map[bool]io.Writer{true: stdout, false: io.Discard}[err == nil], "Deployed glue-gateway in namespace %s (auth=%s, url=%s, healthz=ok)\n", remoteGatewayNS, mode, redact.String(cmp.Or(pub, "svc/glue-gateway:8800")))
	return errors.Join(err, werr)
}
