// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package axbackend

import (
	"bufio"
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erain/glue/internal/eventlog"
	"github.com/erain/glue/internal/jobclient"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

func TestRootPackageHasZeroAXImports(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", ".")
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps .: %v (%s)", err, out)
	}
	for _, pkg := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		for _, forbidden := range []string{"internal/axbackend", "github.com/google/ax", "go.ate.dev/substrate"} {
			if strings.Contains(pkg, forbidden) {
				t.Fatalf("root package glue must not import %q (found %q)", forbidden, pkg)
			}
		}
	}
}

type connectCapture struct{ Method, Host, TargetActor, ClientURI string }

type fakeConnectRouter struct {
	Addr      string
	ClientTLS *tls.Config
	mu        sync.Mutex
	connects  []connectCapture
}

func newTestTLS(t *testing.T, dnsName string, uris ...*url.URL) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	now := time.Now().Add(-time.Minute)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: dnsName}, DNSNames: []string{dnsName}, URIs: uris,
		NotBefore: now, NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	tlsCert, _ := tls.X509KeyPair(certPEM, keyPEM)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	return tlsCert, pool
}

func startFakeConnectRouter(t *testing.T, upstream http.Handler) *fakeConnectRouter {
	t.Helper()
	ts := httptest.NewServer(upstream)
	t.Cleanup(ts.Close)
	upAddr := ts.Listener.Addr().String()

	spiffeURI, _ := url.Parse("spiffe://cluster.local/ns/ax-system/sa/ax-server")
	tlsCert, pool := newTestTLS(t, DefaultRouterSNI, spiffeURI)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{tlsCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	})
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	r := &fakeConnectRouter{
		Addr:      ln.Addr().String(),
		ClientTLS: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, Certificates: []tls.Certificate{tlsCert}, ServerName: DefaultRouterSNI},
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go r.handleConn(c.(*tls.Conn), upAddr)
		}
	}()
	return r
}

func (r *fakeConnectRouter) handleConn(tlsConn *tls.Conn, upAddr string) {
	defer tlsConn.Close()
	if tlsConn.Handshake() != nil {
		return
	}
	var clientURI string
	if peers := tlsConn.ConnectionState().PeerCertificates; len(peers) > 0 && len(peers[0].URIs) > 0 {
		clientURI = peers[0].URIs[0].String()
	}
	br := bufio.NewReader(tlsConn)
	req, err := http.ReadRequest(br)
	if err != nil || req.Method != http.MethodConnect || req.Header.Get(TargetActorHeader) == "" {
		_, _ = io.WriteString(tlsConn, "HTTP/1.1 400 Bad Request\r\n\r\n")
		return
	}
	r.mu.Lock()
	r.connects = append(r.connects, connectCapture{req.Method, req.Host, req.Header.Get(TargetActorHeader), clientURI})
	r.mu.Unlock()
	upConn, err := net.Dial("tcp", upAddr)
	if err != nil {
		return
	}
	defer upConn.Close()
	_, _ = io.WriteString(tlsConn, "HTTP/1.1 200 Connection Established\r\n\r\n")
	go func() { _, _ = io.Copy(upConn, br); _ = upConn.Close() }()
	_, _ = io.Copy(tlsConn, upConn)
}

func TestRouterTransportSetsTargetActorAndReconnectsSSE(t *testing.T) {
	var lastEventIDs []string
	var mu sync.Mutex
	router := startFakeConnectRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/jobs/job-123/start" {
			if r.Header.Get("Authorization") != "Bearer task-secret" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"job":{"id":"job-123","status":"running"}}`)
			return
		}
		afterStr := cmp.Or(r.Header.Get("Last-Event-ID"), "0")
		mu.Lock()
		lastEventIDs = append(lastEventIDs, afterStr)
		mu.Unlock()
		after, _ := strconv.ParseInt(afterStr, 10, 64)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Connection", "close")
		flusher, _ := w.(http.Flusher)
		for seq := int64(1); seq <= 4; seq++ {
			if seq <= after {
				continue
			}
			b, _ := json.Marshal(eventlog.Envelope{Version: 2, ID: fmt.Sprintf("job-123:%d", seq), RunID: "job-123", Seq: seq, Type: "plan"})
			_, _ = fmt.Fprintf(w, "id: %d\nevent: plan\ndata: %s\n\n", seq, b)
			flusher.Flush()
			if after == 0 && seq == 2 {
				return
			}
		}
	}))

	var getTLSCalls atomic.Int32
	rt, err := NewRouterTransport(RouterOptions{
		RouterAddr: router.Addr,
		Atespace:   "team-a",
		GetTLSConfig: func(context.Context) (*tls.Config, error) {
			getTLSCalls.Add(1)
			return router.ClientTLS, nil
		},
	})
	if err != nil {
		t.Fatalf("NewRouterTransport: %v", err)
	}
	cli := &jobclient.Client{BaseURL: "http://glue-job-123:8080", Token: "task-secret", HTTP: &http.Client{Transport: rt}}
	if job, err := cli.Start(t.Context(), "job-123"); err != nil || job.ID != "job-123" {
		t.Fatalf("Start: job=%+v err=%v", job, err)
	}
	var seqs []int64
	for lastSeq := int64(0); lastSeq < 4; {
		_ = cli.StreamEvents(t.Context(), "job-123", lastSeq, func(ev eventlog.Envelope) error {
			seqs, lastSeq = append(seqs, ev.Seq), ev.Seq
			return nil
		})
	}
	router.mu.Lock()
	caps := append([]connectCapture(nil), router.connects...)
	router.mu.Unlock()
	if len(caps) != 3 || getTLSCalls.Load() != 3 || caps[0].TargetActor != "team-a/glue-job-123" ||
		caps[0].ClientURI != "spiffe://cluster.local/ns/ax-system/sa/ax-server" ||
		fmt.Sprint(seqs) != "[1 2 3 4]" || fmt.Sprint(lastEventIDs) != "[0 2]" {
		t.Fatalf("caps=%+v tlsCalls=%d seqs=%v lastEventIDs=%v", caps, getTLSCalls.Load(), seqs, lastEventIDs)
	}
}

type stubAXServer struct{ v1alpha1.UnimplementedAXServer }

func (stubAXServer) ListTasks(context.Context, *v1alpha1.ListTasksRequest) (*v1alpha1.ListTasksResponse, error) {
	return &v1alpha1.ListTasksResponse{}, nil
}

type stubControlServer struct {
	ateapipb.UnimplementedControlServer
	gotAuth atomic.Value
}

func (s *stubControlServer) GetAtespace(ctx context.Context, _ *ateapipb.GetAtespaceRequest) (*ateapipb.Atespace, error) {
	if md, ok := metadata.FromIncomingContext(ctx); ok && len(md.Get("authorization")) > 0 {
		s.gotAuth.Store(md.Get("authorization")[0])
	}
	return &ateapipb.Atespace{}, nil
}

func TestDialAXAndControl(t *testing.T) {
	axLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	axSrv := grpc.NewServer()
	v1alpha1.RegisterAXServer(axSrv, stubAXServer{})
	t.Cleanup(axSrv.Stop)
	go func() { _ = axSrv.Serve(axLn) }()

	axCli, axConn, err := DialAX("http://" + axLn.Addr().String())
	if err != nil {
		t.Fatalf("DialAX: %v", err)
	}
	t.Cleanup(func() { _ = axConn.Close() })
	if _, err := axCli.ListTasks(t.Context(), &v1alpha1.ListTasksRequest{Atespace: "default"}); err != nil {
		t.Fatalf("ListTasks: %v", err)
	}

	tlsCert, pool := newTestTLS(t, DefaultControlSNI)
	ctrlLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctrlImpl := &stubControlServer{}
	ctrlSrv := grpc.NewServer(grpc.Creds(credentials.NewServerTLSFromCert(&tlsCert)))
	ateapipb.RegisterControlServer(ctrlSrv, ctrlImpl)
	t.Cleanup(ctrlSrv.Stop)
	go func() { _ = ctrlSrv.Serve(ctrlLn) }()

	ctrlCli, ctrlConn, err := dialControl(ctrlLn.Addr().String(), pool, func(context.Context) (string, error) {
		return "minted-token", nil
	})
	if err != nil {
		t.Fatalf("dialControl: %v", err)
	}
	t.Cleanup(func() { _ = ctrlConn.Close() })
	if _, err := ctrlCli.GetAtespace(t.Context(), &ateapipb.GetAtespaceRequest{Atespace: &ateapipb.ObjectRef{Name: "default"}}); err != nil {
		t.Fatalf("GetAtespace: %v", err)
	}
	if got, _ := ctrlImpl.gotAuth.Load().(string); got != "Bearer minted-token" {
		t.Fatalf("expected Bearer minted-token, got %q", got)
	}
}
