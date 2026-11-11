// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package axbackend

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/erain/glue/internal/jobclient"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"gopkg.in/yaml.v3"
)

var (
	tunnelMu          sync.Mutex
	nsProbeCache      sync.Map
	podCertBundlePath = DefaultPodCertBundle
)

// ConnectOptions configures client-side AX connectivity without exposing internal transport flags.
type ConnectOptions struct {
	AXServer, KubeConfig, KubeContext, Atespace, AXNamespace, RouterAddr string
	AXClient                                                             v1alpha1.AXClient
	ControlClient                                                        ateapipb.ControlClient
	RouterTLS                                                            *tls.Config
	RouterDialContext                                                    func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Connection holds ready-to-use AX gRPC, internal control, and router HTTP clients.
type Connection struct {
	Atespace, KubeConfig, KubeContext, RouterAddr string
	RouterTLS                                     *tls.Config
	AX                                            v1alpha1.AXClient
	Control                                       ateapipb.ControlClient
	RouterHTTP                                    *http.Client
	closers                                       []io.Closer
}

// Close closes gRPC client connections while leaving shared local port-forward tunnels warm.
func (c *Connection) Close() error {
	if c == nil {
		return nil
	}
	errs := make([]error, 0, len(c.closers))
	for _, cl := range c.closers {
		errs = append(errs, cl.Close())
	}
	c.closers = nil
	return errors.Join(errs...)
}

type tunnelInfo struct {
	Context   string
	Port, PID int
}
type cachedCreds struct {
	TrustBundlePEM, CertChainPEM, KeyPEM, Token string
	ExpiresAt                                   time.Time
}

// TunnelDir returns the 0700 directory used for local AX tunnel state and short-lived credentials.
func TunnelDir() string {
	dir := filepath.Join(jobclient.ConfigDir(), "tunnels")
	_ = os.MkdirAll(dir, 0o700)
	_ = os.Chmod(dir, 0o700)
	return dir
}
func cacheKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}
func envOr(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}
func newUUID() string {
	var u [16]byte
	_, _ = rand.Read(u[:])
	u[6], u[8] = (u[6]&0x0f)|0x40, (u[8]&0x3f)|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}
func writePrivateJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := errors.Join(os.WriteFile(tmp, data, 0o600), os.Chmod(tmp, 0o600)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
func kubectlArgv(kubeCfg, kubeCtx string, rest ...string) (args []string) {
	if kubeCfg != "" {
		args = append(args, "--kubeconfig", kubeCfg)
	}
	if kubeCtx != "" {
		args = append(args, "--context", kubeCtx)
	}
	return append(args, rest...)
}
func runKubectl(ctx context.Context, kubeCfg, kubeCtx string, stdin []byte, rest ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "kubectl", kubectlArgv(kubeCfg, kubeCtx, rest...)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%w (%s)", err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
func readKubeYAML(path string) (string, []string) {
	var cfg struct {
		Current  string              `yaml:"current-context"`
		Contexts []map[string]string `yaml:"contexts"`
	}
	if data, err := os.ReadFile(path); err != nil || yaml.Unmarshal(data, &cfg) != nil {
		return "", nil
	}
	names := make([]string, 0, len(cfg.Contexts))
	for _, c := range cfg.Contexts {
		names = append(names, strings.TrimSpace(c["name"]))
	}
	return strings.TrimSpace(cfg.Current), names
}

// ResolveKubeTarget resolves the kubeconfig file and context without mutating ~/.kube/config.
func ResolveKubeTarget(kubeConfigOpt, kubeContextOpt string) (string, string, error) {
	wantCtx := cmp.Or(strings.TrimSpace(kubeContextOpt), envOr("GLUE_AX_CONTEXT", "KUBE_CONTEXT", "KUBECONTEXT"))
	home, _ := os.UserHomeDir()
	axDevCfg := filepath.Join(cmp.Or(envOr("XDG_CACHE_HOME"), filepath.Join(home, ".cache")), "glue", "ax-dev", "kubeconfig")
	defaultCfg := filepath.Join(home, ".kube", "config")
	candidates := []string{defaultCfg, axDevCfg}
	if explicit := cmp.Or(strings.TrimSpace(kubeConfigOpt), envOr("GLUE_AX_KUBECONFIG")); explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", "", fmt.Errorf("axbackend: kubeconfig %q: %w", explicit, err)
		}
		candidates = []string{explicit}
	} else if envCfg := envOr("KUBECONFIG"); envCfg != "" {
		candidates = append(filepath.SplitList(envCfg), axDevCfg)
	} else if defCur, _ := readKubeYAML(defaultCfg); wantCtx == "" {
		if axCur, _ := readKubeYAML(axDevCfg); axCur != "" && !strings.Contains(defCur, "ax") {
			miss := defCur == ""
			if !miss {
				key := defaultCfg + "\x00" + defCur
				if v, ok := nsProbeCache.Load(key); ok {
					miss = v.(bool)
				} else {
					pCtx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
					_, nsErr := runKubectl(pCtx, defaultCfg, defCur, nil, "get", "ns", DefaultAXNamespace, "-o", "name")
					cancel()
					miss = nsErr != nil
					nsProbeCache.Store(key, miss)
				}
			}
			if miss {
				candidates = []string{axDevCfg, defaultCfg}
			}
		}
	}
	for _, f := range candidates {
		cur, names := readKubeYAML(f)
		if (wantCtx != "" && (cur == wantCtx || slices.Contains(names, wantCtx))) || (wantCtx == "" && cur != "") {
			return f, cmp.Or(wantCtx, cur), nil
		}
	}
	if _, err := os.Stat(candidates[0]); wantCtx != "" && err == nil {
		return candidates[0], wantCtx, nil
	}
	return "", "", errors.New("axbackend: no Kubernetes context found for AX connection")
}

// Connect establishes AX gRPC, internal control gRPC, and router mTLS HTTP clients.
func Connect(ctx context.Context, opts ConnectOptions) (*Connection, error) {
	opts.Atespace = cmp.Or(strings.TrimSpace(opts.Atespace), "default")
	opts.AXNamespace = cmp.Or(strings.TrimSpace(opts.AXNamespace), envOr("AX_SYSTEM_NAMESPACE"), DefaultAXNamespace)
	explicitKube := opts.KubeConfig != "" || opts.KubeContext != "" || envOr("GLUE_AX_KUBECONFIG", "GLUE_AX_CONTEXT") != ""
	inCluster := !explicitKube
	for _, p := range []string{podCertBundlePath, DefaultServiceDNSCA, DefaultControlToken} {
		if _, err := os.Stat(p); err != nil {
			inCluster = false
		}
	}
	axTarget, routerAddr := cmp.Or(strings.TrimSpace(opts.AXServer), envOr("AX_SERVER")), strings.TrimSpace(opts.RouterAddr)
	needAX, needRouter := opts.AXClient == nil && axTarget == "" && !inCluster, routerAddr == "" && opts.RouterDialContext == nil && !inCluster
	needCtrl, needCreds := opts.ControlClient == nil && !inCluster, opts.RouterTLS == nil || opts.ControlClient == nil
	var kubeCfg, kubeCtx, ctrlAddr string
	var err error
	if !inCluster && (needAX || needRouter || needCtrl || needCreds || explicitKube) {
		if kubeCfg, kubeCtx, err = ResolveKubeTarget(opts.KubeConfig, opts.KubeContext); err != nil && (needAX || needRouter || needCtrl || needCreds) {
			return nil, err
		}
	}
	if needAX {
		axTarget, err = ensureTunnel(ctx, kubeCfg, kubeCtx, opts.AXNamespace, "ax-server", 8080, "/healthz")
	}
	if err == nil && needRouter {
		routerAddr, err = ensureTunnel(ctx, kubeCfg, kubeCtx, "ate-system", "atenet-router", 8444, "")
	}
	if err == nil && needCtrl {
		ctrlAddr, err = ensureTunnel(ctx, kubeCfg, kubeCtx, "ate-system", "api", 443, "")
	}
	if err != nil {
		return nil, err
	}
	var getRouterTLS func(context.Context) (*tls.Config, error)
	var getToken func(context.Context) (string, error)
	var initTLS *tls.Config
	var ctrlRootCAs *x509.CertPool
	if needCreds {
		cs := &credSource{kubeCfg: kubeCfg, kubeCtx: kubeCtx, namespace: opts.AXNamespace, inCluster: inCluster}
		if initTLS, _, err = cs.get(ctx); err != nil {
			return nil, err
		}
		ctrlRootCAs = initTLS.RootCAs
		getToken = func(c context.Context) (string, error) { _, t, e := cs.get(c); return t, e }
		getRouterTLS = func(c context.Context) (*tls.Config, error) { cfg, _, e := cs.get(c); return cfg, e }
	}
	if opts.RouterTLS != nil {
		initTLS, getRouterTLS = opts.RouterTLS.Clone(), nil
		if ctrlRootCAs == nil {
			ctrlRootCAs = initTLS.RootCAs
		}
	}
	rt, _ := NewRouterTransport(RouterOptions{RouterAddr: routerAddr, Atespace: opts.Atespace, TLSConfig: initTLS, GetTLSConfig: getRouterTLS, DialContext: opts.RouterDialContext})
	conn := &Connection{Atespace: opts.Atespace, KubeConfig: kubeCfg, KubeContext: kubeCtx, RouterAddr: rt.opts.RouterAddr, RouterTLS: rt.baseTLS.Clone(), AX: opts.AXClient, Control: opts.ControlClient, RouterHTTP: &http.Client{Transport: rt}}
	if conn.AX == nil {
		axCli, cc, err := DialAX(axTarget)
		if err != nil {
			return nil, err
		}
		conn.AX, conn.closers = axCli, append(conn.closers, cc)
	}
	if conn.Control == nil {
		ctrlCli, cc, err := dialControl(ctrlAddr, ctrlRootCAs, getToken)
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		conn.Control, conn.closers = ctrlCli, append(conn.closers, cc)
	}
	return conn, nil
}
func tunnelHealthy(port int, httpPath string) bool {
	if port <= 0 {
		return false
	}
	if httpPath != "" {
		resp, err := (&http.Client{Timeout: 400 * time.Millisecond}).Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, httpPath))
		if err == nil {
			_ = resp.Body.Close()
		}
		return err == nil && resp.StatusCode == http.StatusOK
	}
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err == nil {
		_ = c.Close()
	}
	return err == nil
}
func isPortForwardProc(pid int) bool {
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return false
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return err != nil || bytes.Contains(cmdline, []byte("port-forward"))
}
func killPID(pid int) {
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
}

// StopTunnels terminates cached local port-forward processes for kubeContext (or all contexts when empty).
func StopTunnels(kubeContext string) error {
	tunnelMu.Lock()
	defer tunnelMu.Unlock()
	dir := TunnelDir()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if jsonPath := filepath.Join(dir, e.Name()); !e.IsDir() && strings.HasSuffix(e.Name(), "-tunnel.json") {
			var info tunnelInfo
			if data, err := os.ReadFile(jsonPath); err == nil && json.Unmarshal(data, &info) == nil && (kubeContext == "" || info.Context == kubeContext) {
				if isPortForwardProc(info.PID) {
					killPID(info.PID)
				}
				_ = os.Remove(jsonPath)
				_ = os.Remove(strings.TrimSuffix(jsonPath, ".json") + ".log")
			}
		}
	}
	return nil
}
func ensureTunnel(ctx context.Context, kubeCfg, kubeCtx, namespace, service string, remotePort int, httpHealthPath string) (string, error) {
	tunnelMu.Lock()
	defer tunnelMu.Unlock()
	base := filepath.Join(TunnelDir(), cacheKey(kubeCfg, kubeCtx, namespace, service))
	jsonPath, logPath := base+"-tunnel.json", base+"-tunnel.log"
	if data, err := os.ReadFile(jsonPath); err == nil {
		var info tunnelInfo
		if json.Unmarshal(data, &info) == nil && isPortForwardProc(info.PID) && tunnelHealthy(info.Port, httpHealthPath) {
			return fmt.Sprintf("127.0.0.1:%d", info.Port), nil
		}
		if isPortForwardProc(info.PID) {
			killPID(info.PID)
		}
		_ = os.Remove(jsonPath)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("axbackend: open tunnel log: %w", err)
	}
	_ = logFile.Chmod(0o600)
	cmd := exec.Command("kubectl", kubectlArgv(kubeCfg, kubeCtx, "-n", namespace, "port-forward", "--address=127.0.0.1", "svc/"+service, fmt.Sprintf("0:%d", remotePort))...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	startErr := cmd.Start()
	_ = logFile.Close()
	if startErr != nil {
		return "", fmt.Errorf("axbackend: start port-forward %s/%s: %w", namespace, service, startErr)
	}
	go func() { _ = cmd.Wait() }()
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); time.Sleep(40 * time.Millisecond) {
		if ctx.Err() != nil {
			killPID(cmd.Process.Pid)
			return "", ctx.Err()
		}
		logData, _ := os.ReadFile(logPath)
		localPort := 0
		if _, rest, ok := strings.Cut(string(logData), "127.0.0.1:"); ok {
			pStr, _, _ := strings.Cut(rest, " ")
			localPort, _ = strconv.Atoi(strings.TrimSpace(pStr))
		}
		if localPort > 0 && tunnelHealthy(localPort, httpHealthPath) {
			if err := writePrivateJSON(jsonPath, tunnelInfo{Context: kubeCtx, Port: localPort, PID: cmd.Process.Pid}); err != nil {
				killPID(cmd.Process.Pid)
				return "", err
			}
			return fmt.Sprintf("127.0.0.1:%d", localPort), nil
		}
		if syscall.Kill(cmd.Process.Pid, 0) != nil {
			return "", fmt.Errorf("axbackend: port-forward %s/%s exited: %s", namespace, service, strings.TrimSpace(string(logData)))
		}
	}
	killPID(cmd.Process.Pid)
	logData, _ := os.ReadFile(logPath)
	return "", fmt.Errorf("axbackend: port-forward %s/%s did not become healthy: %s", namespace, service, strings.TrimSpace(string(logData)))
}

type credSource struct {
	kubeCfg, kubeCtx, namespace, token string
	inCluster                          bool
	mu                                 sync.Mutex
	tlsCfg                             *tls.Config
	expiresAt                          time.Time
}

func (cs *credSource) get(ctx context.Context) (*tls.Config, string, error) {
	if cs.inCluster {
		ca, caErr := os.ReadFile(DefaultServiceDNSCA)
		bundle, bErr := os.ReadFile(podCertBundlePath)
		tok, tErr := os.ReadFile(DefaultControlToken)
		cfg, _, pErr := parseCredTLS(cachedCreds{TrustBundlePEM: string(ca), CertChainPEM: string(bundle), KeyPEM: string(bundle)})
		return cfg, strings.TrimSpace(string(tok)), errors.Join(caErr, bErr, tErr, pErr)
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	now := time.Now()
	if cs.tlsCfg != nil && cs.token != "" && now.Before(cs.expiresAt) {
		return cs.tlsCfg, cs.token, nil
	}
	tunnelMu.Lock()
	defer tunnelMu.Unlock()
	path := filepath.Join(TunnelDir(), cacheKey(cs.kubeCfg, cs.kubeCtx, cs.namespace)+"-creds.json")
	if data, err := os.ReadFile(path); err == nil {
		var c cachedCreds
		if json.Unmarshal(data, &c) == nil && c.Token != "" && now.Before(c.ExpiresAt) {
			if cfg, notAfter, err := parseCredTLS(c); err == nil && now.Add(time.Minute).Before(notAfter) {
				cs.tlsCfg, cs.token, cs.expiresAt = cfg, c.Token, c.ExpiresAt
				return cfg, c.Token, nil
			}
		}
	}
	ctbOut, err := runKubectl(ctx, cs.kubeCfg, cs.kubeCtx, nil, "get", "clustertrustbundles.certificates.k8s.io", "-l", "podcert.ate.dev/canarying=live", "-o", `jsonpath={range .items[?(@.spec.signerName=="servicedns.podcert.ate.dev/identity")]}{.spec.trustBundle}{"\n"}{end}`)
	if err != nil || strings.TrimSpace(string(ctbOut)) == "" {
		return nil, "", fmt.Errorf("axbackend: get servicedns ClusterTrustBundle: %w", cmp.Or(err, errors.New("empty trust bundle")))
	}
	chainPEM, keyPEM, err := mintPodCert(ctx, cs.kubeCfg, cs.kubeCtx, cs.namespace)
	if err != nil {
		return nil, "", err
	}
	tokOut, err := runKubectl(ctx, cs.kubeCfg, cs.kubeCtx, nil, "-n", cs.namespace, "create", "token", "ax-server", "--audience="+DefaultControlSNI, "--duration=3600s")
	if err != nil {
		return nil, "", fmt.Errorf("axbackend: create token in %s: %w", cs.namespace, err)
	}
	c := cachedCreds{TrustBundlePEM: string(ctbOut), CertChainPEM: chainPEM, KeyPEM: keyPEM, Token: strings.TrimSpace(string(tokOut))}
	cfg, notAfter, err := parseCredTLS(c)
	if err != nil {
		return nil, "", err
	}
	c.ExpiresAt = time.Unix(min(now.Add(25*time.Minute).Unix(), notAfter.Add(-time.Minute).Unix()), 0)
	if !now.Before(c.ExpiresAt) {
		return nil, "", fmt.Errorf("axbackend: minted certificate expires too soon (%s)", notAfter.UTC().Format(time.RFC3339))
	}
	if err := writePrivateJSON(path, c); err != nil {
		return nil, "", err
	}
	cs.tlsCfg, cs.token, cs.expiresAt = cfg, c.Token, c.ExpiresAt
	return cfg, c.Token, nil
}
func parseCredTLS(c cachedCreds) (*tls.Config, time.Time, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(c.TrustBundlePEM)) {
		return nil, time.Time{}, errors.New("axbackend: trust bundle has no certificates")
	}
	cert, err := tls.X509KeyPair([]byte(c.CertChainPEM), []byte(c.KeyPEM))
	if err != nil || cert.Leaf == nil {
		return nil, time.Time{}, fmt.Errorf("axbackend: parse client keypair: %w", err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, Certificates: []tls.Certificate{cert}, ServerName: DefaultRouterSNI}, cert.Leaf.NotAfter, nil
}
func mintPodCert(ctx context.Context, kubeCfg, kubeCtx, namespace string) (string, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return "", "", err
	}
	var sfx [4]byte
	_, _ = rand.Read(sfx[:])
	name := "glue-ax-" + hex.EncodeToString(sfx[:])
	var createErr error
	for _, apiVer := range []string{"certificates.k8s.io/v1", "certificates.k8s.io/v1beta1"} {
		body, _ := json.Marshal(map[string]any{
			"apiVersion": apiVer, "kind": "PodCertificateRequest", "metadata": map[string]any{"name": name, "namespace": namespace},
			"spec": map[string]any{
				"signerName": "podidentity.podcert.ate.dev/identity", "podName": name, "podUID": newUUID(),
				"serviceAccountName": "ax-server", "serviceAccountUID": newUUID(),
				"nodeName": "glue-cli", "nodeUID": newUUID(), "maxExpirationSeconds": 3600,
				"stubPKCS10Request": base64.StdEncoding.EncodeToString(csr),
			},
		})
		if _, createErr = runKubectl(ctx, kubeCfg, kubeCtx, body, "create", "-f", "-"); createErr == nil {
			break
		}
	}
	if createErr != nil {
		return "", "", fmt.Errorf("axbackend: create PodCertificateRequest %s/%s: %w", namespace, name, createErr)
	}
	defer func() {
		delCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = runKubectl(delCtx, kubeCfg, kubeCtx, nil, "-n", namespace, "delete", "podcertificaterequests.certificates.k8s.io", name, "--ignore-not-found")
	}()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(150 * time.Millisecond) {
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		if out, err := runKubectl(ctx, kubeCfg, kubeCtx, nil, "-n", namespace, "get", "podcertificaterequests.certificates.k8s.io", name, "-o", "jsonpath={.status.certificateChain}"); err == nil && strings.TrimSpace(string(out)) != "" {
			return string(out), keyPEM, nil
		}
	}
	return "", "", fmt.Errorf("axbackend: timed out waiting for PodCertificateRequest %s/%s", namespace, name)
}
