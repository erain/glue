// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package ax

import (
	"bufio"
	"cmp"
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
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erain/glue/internal/eventlog"
	"github.com/erain/glue/internal/jobclient"
)

type connectCapture struct{ Method, Host, TargetActor, ClientURI string }

type fakeConnectRouter struct {
	Addr, CAFile, BundleFile string
	FirstConnIdleTimeout     time.Duration
	mu                       sync.Mutex
	connects                 []connectCapture
	conns                    atomic.Int32
}

func startFakeConnectRouter(t *testing.T, upstream http.Handler) *fakeConnectRouter {
	t.Helper()
	ts := httptest.NewServer(upstream)
	t.Cleanup(ts.Close)
	upAddr := ts.Listener.Addr().String()
	caFile, bundleFile, serverTLS := generateTestMTLS(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	r := &fakeConnectRouter{Addr: ln.Addr().String(), CAFile: caFile, BundleFile: bundleFile}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go r.handleConn(conn.(*tls.Conn), upAddr)
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
	idleTimeout := r.FirstConnIdleTimeout
	r.mu.Unlock()
	upConn, err := net.Dial("tcp", upAddr)
	if err != nil {
		return
	}
	defer upConn.Close()
	_, _ = io.WriteString(tlsConn, "HTTP/1.1 200 Connection Established\r\n\r\n")
	connNum := r.conns.Add(1)
	go func() { _, _ = io.Copy(upConn, br); _ = upConn.Close() }()
	if connNum == 1 && idleTimeout > 0 {
		for buf := make([]byte, 4096); ; {
			_ = upConn.SetReadDeadline(time.Now().Add(idleTimeout))
			n, rerr := upConn.Read(buf)
			if n > 0 {
				_, _ = tlsConn.Write(buf[:n])
			}
			if rerr != nil {
				return
			}
		}
	}
	_, _ = io.Copy(tlsConn, upConn)
}
func (r *fakeConnectRouter) Captures() []connectCapture {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]connectCapture(nil), r.connects...)
}
func generateTestMTLS(t *testing.T) (caFile, bundleFile string, serverTLS *tls.Config) {
	t.Helper()
	now := time.Now().Add(-time.Minute)
	spiffeURI, _ := url.Parse("spiffe://cluster.local/ns/glue-system/sa/glue-gateway")
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: DefaultRouterServerName},
		DNSNames: []string{DefaultRouterServerName}, URIs: []*url.URL{spiffeURI},
		NotBefore: now, NotAfter: now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	tlsCert, _ := tls.X509KeyPair(certPEM, keyPEM)
	dir := t.TempDir()
	caFile, bundleFile = filepath.Join(dir, "ca.pem"), filepath.Join(dir, "bundle.pem")
	_ = os.WriteFile(caFile, certPEM, 0o600)
	_ = os.WriteFile(bundleFile, append(keyPEM, certPEM...), 0o600)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	return caFile, bundleFile, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{tlsCert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
}
func TestRouterTransportSetsTargetActor(t *testing.T) {
	var gotAuth, gotPath string
	router := startFakeConnectRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"job":{"id":"job-123456789abc","status":"running"}}`)
	}))
	rt, err := NewRouterTransport(RouterOptions{RouterAddr: router.Addr, Atespace: "team-a", Task: "glue-job-123456789abc", CertFile: router.BundleFile, CAFile: router.CAFile})
	if err != nil {
		t.Fatalf("NewRouterTransport: %v", err)
	}
	cli := &jobclient.Client{BaseURL: "http://glue-job-123456789abc:8080", Token: "task-secret", HTTP: &http.Client{Transport: rt}}
	job, err := cli.Start(t.Context(), "job-123456789abc")
	if err != nil || job.ID != "job-123456789abc" || gotAuth != "Bearer task-secret" || gotPath != "/v1/jobs/job-123456789abc/start" {
		t.Fatalf("Start: job=%+v err=%v auth=%q path=%q", job, err, gotAuth, gotPath)
	}
	caps := router.Captures()
	if len(caps) != 1 || caps[0].Method != http.MethodConnect || caps[0].Host != "glue-job-123456789abc:8080" ||
		caps[0].TargetActor != "team-a/glue-job-123456789abc" || caps[0].ClientURI != "spiffe://cluster.local/ns/glue-system/sa/glue-gateway" {
		t.Fatalf("unexpected CONNECT captures: %+v", caps)
	}
}
func TestRouterIdleCutReconnects(t *testing.T) {
	var lastEventIDs []string
	var mu sync.Mutex
	router := startFakeConnectRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		afterStr := cmp.Or(r.Header.Get("Last-Event-ID"), r.URL.Query().Get("after"), "0")
		mu.Lock()
		lastEventIDs = append(lastEventIDs, afterStr)
		mu.Unlock()
		after, _ := strconv.ParseInt(afterStr, 10, 64)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for seq := int64(1); seq <= 4; seq++ {
			if seq <= after {
				continue
			}
			b, _ := json.Marshal(eventlog.Envelope{Version: 2, ID: fmt.Sprintf("job-idle:%d", seq), RunID: "job-idle", Seq: seq, Type: "plan"})
			_, _ = fmt.Fprintf(w, ": keepalive\n\nid: %d\nevent: plan\ndata: %s\n\n", seq, b)
			flusher.Flush()
			if after == 0 && seq == 2 {
				<-r.Context().Done()
				return
			}
		}
	}))
	router.FirstConnIdleTimeout = 20 * time.Millisecond
	rt, err := NewRouterTransport(RouterOptions{RouterAddr: router.Addr, Atespace: "default", Task: "glue-job-idle", CertFile: router.BundleFile, CAFile: router.CAFile})
	if err != nil {
		t.Fatalf("NewRouterTransport: %v", err)
	}
	cli := &jobclient.Client{BaseURL: "http://glue-job-idle:8080", Token: "t", HTTP: &http.Client{Transport: rt}}
	var seqs []int64
	var lastSeq int64
	for lastSeq < 4 {
		_ = cli.StreamEvents(t.Context(), "job-idle", lastSeq, func(ev eventlog.Envelope) error {
			if ev.Seq <= lastSeq {
				t.Fatalf("duplicate or out-of-order seq=%d after=%d", ev.Seq, lastSeq)
			}
			seqs, lastSeq = append(seqs, ev.Seq), ev.Seq
			return nil
		})
	}
	mu.Lock()
	gotAfter := append([]string(nil), lastEventIDs...)
	mu.Unlock()
	if fmt.Sprint(seqs) != "[1 2 3 4]" || fmt.Sprint(gotAfter) != "[0 2]" || len(router.Captures()) != 2 {
		t.Fatalf("seqs=%v lastEventIDs=%v connects=%d", seqs, gotAfter, len(router.Captures()))
	}
}
