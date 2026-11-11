// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package axbackend

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
)

func axClusterRequired() bool {
	return os.Getenv("CI") == "true" || os.Getenv("GLUE_E2E_AX_STRICT") == "1"
}

func checkLiveAXPrecondition(kubeCfg, kubeCtx string) error {
	if _, err := exec.LookPath("kubectl"); err != nil {
		return fmt.Errorf("kubectl binary not found (%w); install kubectl and run scripts/ax-dev/up.sh", err)
	}
	cfg, kctx, err := ResolveKubeTarget(kubeCfg, kubeCtx)
	if err != nil {
		return fmt.Errorf("%w; run 'scripts/ax-dev/up.sh && source scripts/ax-dev/env.sh'", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := runKubectl(ctx, cfg, kctx, nil, "get", "ns", DefaultAXNamespace, "-o", "name"); err != nil {
		return fmt.Errorf("AX namespace %q unreachable in context %q (%w); run 'scripts/ax-dev/up.sh && source scripts/ax-dev/env.sh'", DefaultAXNamespace, kctx, err)
	}
	return nil
}

func TestLiveAXPreconditionStrict(t *testing.T) {
	t.Setenv("CI", "true")
	if !axClusterRequired() {
		t.Fatal("expected axClusterRequired() to be true when CI=true")
	}
	err := checkLiveAXPrecondition(filepath.Join(t.TempDir(), "missing-kubeconfig"), "kind-missing-ax")
	if err == nil || !strings.Contains(err.Error(), "scripts/ax-dev/up.sh") {
		t.Fatalf("expected actionable strict failure mentioning scripts/ax-dev/up.sh, got: %v", err)
	}
}

func TestFakeKubectlHelper(t *testing.T) {
	if os.Getenv("GLUE_TEST_FAKE_KUBECTL") != "1" {
		return
	}
	stateDir := os.Getenv("GLUE_TEST_FAKE_K8S_DIR")
	args := os.Args[1:]
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	f, _ := os.OpenFile(filepath.Join(stateDir, "kubectl.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	_, _ = fmt.Fprintln(f, strings.Join(args, " "))
	_ = f.Close()

	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "--context kind-e2e get ns ax-system"):
		os.Exit(1)
	case strings.Contains(joined, "get clustertrustbundles.certificates.k8s.io"):
		ca, _ := os.ReadFile(filepath.Join(stateDir, "ca.pem"))
		_, _ = os.Stdout.Write(ca)
	case strings.Contains(joined, "create -f -"):
		raw, _ := io.ReadAll(os.Stdin)
		var req struct {
			Metadata struct{ Name string }              `json:"metadata"`
			Spec     struct{ StubPKCS10Request string } `json:"spec"`
		}
		_ = json.Unmarshal(raw, &req)
		csrDER, _ := base64.StdEncoding.DecodeString(req.Spec.StubPKCS10Request)
		csr, _ := x509.ParseCertificateRequest(csrDER)
		caPEM, _ := os.ReadFile(filepath.Join(stateDir, "ca.pem"))
		caKeyPEM, _ := os.ReadFile(filepath.Join(stateDir, "ca-key.pem"))
		caBlock, _ := pem.Decode(caPEM)
		caCert, _ := x509.ParseCertificate(caBlock.Bytes)
		keyBlock, _ := pem.Decode(caKeyPEM)
		caKey, _ := x509.ParseECPrivateKey(keyBlock.Bytes)
		now := time.Now().Add(-time.Minute)
		leafDER, _ := x509.CreateCertificate(rand.Reader, &x509.Certificate{
			SerialNumber: big.NewInt(2), NotBefore: now, NotAfter: now.Add(time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}, caCert, csr.PublicKey, caKey)
		_ = os.WriteFile(filepath.Join(stateDir, "pcr-"+req.Metadata.Name+".pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), 0o600)
	case strings.Contains(joined, "get podcertificaterequests.certificates.k8s.io"):
		for i, a := range args {
			if a == "podcertificaterequests.certificates.k8s.io" && i+1 < len(args) {
				b, _ := os.ReadFile(filepath.Join(stateDir, "pcr-"+args[i+1]+".pem"))
				_, _ = os.Stdout.Write(b)
			}
		}
	case strings.Contains(joined, "delete podcertificaterequests.certificates.k8s.io"):
		for i, a := range args {
			if a == "podcertificaterequests.certificates.k8s.io" && i+1 < len(args) {
				_ = os.Remove(filepath.Join(stateDir, "pcr-"+args[i+1]+".pem"))
			}
		}
	case strings.Contains(joined, "create token ax-server"):
		_, _ = io.WriteString(os.Stdout, "minted-sa-token\n")
	case strings.Contains(joined, "port-forward"):
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			os.Exit(1)
		}
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		if strings.Contains(joined, "svc/ax-server") {
			go func() {
				_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
			}()
		} else {
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					_ = c.Close()
				}
			}()
		}
		_, _ = fmt.Fprintf(os.Stdout, "Forwarding from 127.0.0.1:%d -> 8080\n", ln.Addr().(*net.TCPAddr).Port)
		<-ch
	}
	os.Exit(0)
}

func TestResolveKubeTargetAndTunnelCredCache(t *testing.T) {
	tmp := t.TempDir()
	home, cfgDir, k8sDir, binDir := filepath.Join(tmp, "home"), filepath.Join(tmp, "cfg"), filepath.Join(tmp, "k8s"), filepath.Join(tmp, "bin")
	for _, d := range []string{filepath.Join(home, ".kube"), filepath.Join(home, ".cache", "glue", "ax-dev"), cfgDir, k8sDir, binDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	hostKubePath := filepath.Join(home, ".kube", "config")
	hostKubeBytes := []byte("apiVersion: v1\ncurrent-context: kind-e2e\ncontexts:\n- name: kind-e2e\n")
	axKubePath := filepath.Join(home, ".cache", "glue", "ax-dev", "kubeconfig")
	axKubeBytes := []byte("apiVersion: v1\ncurrent-context: kind-glue-ax\ncontexts:\n- name: kind-glue-ax\n")
	_ = os.WriteFile(hostKubePath, hostKubeBytes, 0o600)
	_ = os.WriteFile(axKubePath, axKubeBytes, 0o600)

	caCert, _ := newTestTLS(t, "ca")
	caKeyDER, _ := x509.MarshalECPrivateKey(caCert.PrivateKey.(*ecdsa.PrivateKey))
	_ = os.WriteFile(filepath.Join(k8sDir, "ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Certificate[0]}), 0o600)
	_ = os.WriteFile(filepath.Join(k8sDir, "ca-key.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: caKeyDER}), 0o600)

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	kubectlWrapper := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=^TestFakeKubectlHelper$ -- \"$@\"\n", exe)
	if err := os.WriteFile(filepath.Join(binDir, "kubectl"), []byte(kubectlWrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"HOME": home, "XDG_CACHE_HOME": filepath.Join(home, ".cache"), "GLUE_CONFIG_DIR": cfgDir,
		"KUBECONFIG": "", "KUBE_CONTEXT": "", "KUBECONTEXT": "", "GLUE_AX_CONTEXT": "", "GLUE_AX_KUBECONFIG": "",
		"AX_SERVER": "", "GLUE_TEST_FAKE_KUBECTL": "1", "GLUE_TEST_FAKE_K8S_DIR": k8sDir,
		"PATH": binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	} {
		t.Setenv(k, v)
	}
	t.Cleanup(func() { _ = StopTunnels("") })

	c1, err := Connect(t.Context(), ConnectOptions{})
	if err != nil {
		t.Fatalf("first Connect: %v", err)
	}
	t.Cleanup(func() { _ = c1.Close() })
	if c1.KubeConfig != axKubePath || c1.KubeContext != "kind-glue-ax" {
		t.Fatalf("unexpected resolved kube target: cfg=%q ctx=%q", c1.KubeConfig, c1.KubeContext)
	}
	log1, _ := os.ReadFile(filepath.Join(k8sDir, "kubectl.log"))
	if !strings.Contains(string(log1), "delete podcertificaterequests.certificates.k8s.io") {
		t.Fatalf("expected PodCertificateRequest cleanup in kubectl log:\n%s", log1)
	}

	c2, err := Connect(t.Context(), ConnectOptions{KubeConfig: axKubePath, KubeContext: "kind-glue-ax"})
	if err != nil {
		t.Fatalf("second Connect: %v", err)
	}
	t.Cleanup(func() { _ = c2.Close() })
	log2, _ := os.ReadFile(filepath.Join(k8sDir, "kubectl.log"))
	if strings.Count(string(log2), "create token") != 1 || strings.Count(string(log2), "port-forward") != 3 {
		t.Fatalf("expected 1 token mint and 3 port-forwards across warm Connect calls, got:\n%s", log2)
	}

	credsPath := filepath.Join(TunnelDir(), cacheKey(axKubePath, "kind-glue-ax", DefaultAXNamespace)+"-creds.json")
	var expired cachedCreds
	if data, err := os.ReadFile(credsPath); err != nil || json.Unmarshal(data, &expired) != nil {
		t.Fatalf("read cached creds: %v", err)
	}
	expired.ExpiresAt = time.Now().Add(-time.Minute)
	if err := writePrivateJSON(credsPath, expired); err != nil {
		t.Fatalf("expire cached creds: %v", err)
	}
	c3, err := Connect(t.Context(), ConnectOptions{KubeConfig: axKubePath, KubeContext: "kind-glue-ax"})
	if err != nil {
		t.Fatalf("post-expiry Connect: %v", err)
	}
	t.Cleanup(func() { _ = c3.Close() })
	log3, _ := os.ReadFile(filepath.Join(k8sDir, "kubectl.log"))
	if strings.Count(string(log3), "create token") != 2 || strings.Count(string(log3), "port-forward") != 3 {
		t.Fatalf("expected automatic credential refresh with warm tunnels preserved, got:\n%s", log3)
	}

	dirInfo, err := os.Stat(TunnelDir())
	if err != nil || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("tunnel dir mode=%v err=%v", dirInfo, err)
	}
	tEntries, _ := os.ReadDir(TunnelDir())
	for _, e := range tEntries {
		if fi, err := e.Info(); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("tunnel file %s mode=%v err=%v", e.Name(), fi, err)
		}
	}
	if gotHostKube, _ := os.ReadFile(hostKubePath); string(gotHostKube) != string(hostKubeBytes) {
		t.Fatal("host ~/.kube/config was mutated")
	}
	if err := StopTunnels("kind-glue-ax"); err != nil {
		t.Fatalf("StopTunnels: %v", err)
	}
}

func TestLiveAXConnect(t *testing.T) {
	if os.Getenv("GLUE_E2E_AX") != "1" {
		t.Skip("skipping live AX test; set GLUE_E2E_AX=1 to run")
	}
	if err := checkLiveAXPrecondition("", ""); err != nil {
		if axClusterRequired() {
			t.Fatalf("live AX cluster is required when CI=true and GLUE_E2E_AX=1: %v", err)
		}
		t.Skipf("live AX cluster unavailable (%v)", err)
	}
	t.Setenv("GLUE_CONFIG_DIR", t.TempDir())
	t.Setenv("AX_SERVER", "")
	t.Cleanup(func() { _ = StopTunnels("") })

	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()

	t0 := time.Now()
	conn, err := Connect(ctx, ConnectOptions{})
	coldDur := time.Since(t0)
	if err != nil {
		t.Fatalf("cold Connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if _, err := conn.AX.ListTasks(ctx, &v1alpha1.ListTasksRequest{Atespace: conn.Atespace}); err != nil {
		t.Fatalf("AX.ListTasks: %v", err)
	}
	if _, err := conn.Control.GetAtespace(ctx, &ateapipb.GetAtespaceRequest{Atespace: &ateapipb.ObjectRef{Name: conn.Atespace}}); err != nil {
		t.Fatalf("Control.GetAtespace: %v", err)
	}
	tlsConn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", conn.RouterAddr, conn.RouterTLS)
	if err != nil {
		t.Fatalf("mTLS handshake with atenet-router (%s): %v", conn.RouterAddr, err)
	}
	_ = tlsConn.Close()

	t1 := time.Now()
	warmConn, err := Connect(ctx, ConnectOptions{})
	warmDur := time.Since(t1)
	if err != nil {
		t.Fatalf("warm Connect: %v", err)
	}
	t.Cleanup(func() { _ = warmConn.Close() })
	if warmDur > 50*time.Millisecond {
		t.Fatalf("expected warm Connect < 50ms, got %s", warmDur)
	}
	if _, err := warmConn.AX.ListTasks(ctx, &v1alpha1.ListTasksRequest{Atespace: warmConn.Atespace}); err != nil {
		t.Fatalf("warm AX.ListTasks: %v", err)
	}
	t.Logf("live AX Connect timings against %s: cold=%s warm=%s", conn.KubeContext, coldDur, warmDur)
}
