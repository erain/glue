// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

// Package ax implements the glue-gateway AX execution backend and router transport.
package ax

import (
	"bufio"
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	DefaultAXServerAddr       = "ax-server.ax-system:8080"
	DefaultSubstrateAddr      = "api.ate-system.svc.cluster.local:443"
	DefaultSubstrateAuthority = "api.ate-system.svc"
	DefaultRouterAddr         = "atenet-router.ate-system:8444"
	DefaultRouterServerName   = "atenet-router.ate-system.svc"
	DefaultPodCertBundle      = "/run/podidentity.podcert.ate.dev/credential-bundle.pem"
	DefaultServiceDNSCA       = "/run/servicedns-ca/trust-bundle.pem"
	DefaultSubstrateTokenFile = "/var/run/secrets/ateapi/token"
	TargetActorHeader         = "ate-target-actor"
	DefaultTaskPort           = 8080
)

// RouterOptions configures mTLS HTTP CONNECT tunneling through atenet-router.
type RouterOptions struct {
	RouterAddr, ServerName, Atespace, Task, CertFile, KeyFile, CAFile string
	Port                                                              int
	TLSConfig                                                         *tls.Config
	DialContext                                                       func(ctx context.Context, network, addr string) (net.Conn, error)
}

// RouterTransport implements http.RoundTripper by tunneling HTTP/SSE requests
// over an mTLS HTTP CONNECT stream to atenet-router:8444 with ate-target-actor.
type RouterTransport struct {
	opts RouterOptions
	tr   *http.Transport
}

// NewRouterTransport constructs a RouterTransport for the given RouterOptions.
func NewRouterTransport(opts RouterOptions) (*RouterTransport, error) {
	opts.RouterAddr = strings.TrimPrefix(strings.TrimPrefix(cmp.Or(strings.TrimSpace(opts.RouterAddr), DefaultRouterAddr), "https://"), "http://")
	opts.ServerName = cmp.Or(strings.TrimSpace(opts.ServerName), DefaultRouterServerName)
	opts.Atespace = cmp.Or(strings.TrimSpace(opts.Atespace), "default")
	opts.Port = cmp.Or(opts.Port, DefaultTaskPort)
	tlsCfg, err := buildTLS(opts.TLSConfig, opts.ServerName, opts.CAFile, opts.CertFile, opts.KeyFile)
	if err != nil {
		return nil, err
	}
	rt := &RouterTransport{opts: opts}
	rt.tr = &http.Transport{
		DisableKeepAlives: true,
		DialContext:       func(ctx context.Context, _, addr string) (net.Conn, error) { return rt.dialTunnel(ctx, tlsCfg, addr) },
	}
	return rt, nil
}
func buildTLS(base *tls.Config, serverName, caFile, certFile, keyFile string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if base != nil {
		cfg = base.Clone()
	}
	cfg.ServerName = cmp.Or(cfg.ServerName, serverName)
	if caFile = strings.TrimSpace(caFile); caFile != "" {
		pemBytes, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("ax: read CA %s: %w", caFile, err)
		}
		if cfg.RootCAs = x509.NewCertPool(); !cfg.RootCAs.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("ax: parse CA %s: no certificates found", caFile)
		}
	}
	if certFile = strings.TrimSpace(certFile); certFile != "" {
		keyFile = cmp.Or(strings.TrimSpace(keyFile), certFile)
		if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
			return nil, fmt.Errorf("ax router: load client certificate %s: %w", certFile, err)
		}
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, err := tls.LoadX509KeyPair(certFile, keyFile)
			return &cert, err
		}
	}
	return cfg, nil
}

// RoundTrip executes req over an mTLS HTTP CONNECT tunnel to the target task.
func (rt *RouterTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	task, port := strings.TrimSpace(rt.opts.Task), rt.opts.Port
	if task == "" && req.URL != nil {
		task = req.URL.Hostname()
		if p, err := strconv.Atoi(req.URL.Port()); err == nil && p > 0 {
			port = p
		}
	}
	if task == "" {
		return nil, fmt.Errorf("ax router: missing target task in request %q", req.URL)
	}
	clone := req.Clone(req.Context())
	clone.URL.Scheme, clone.URL.Host = "http", net.JoinHostPort(task, strconv.Itoa(port))
	clone.Host = clone.URL.Host
	return rt.tr.RoundTrip(clone)
}
func (rt *RouterTransport) dialTunnel(ctx context.Context, baseTLS *tls.Config, targetAddr string) (net.Conn, error) {
	task, portStr, err := net.SplitHostPort(targetAddr)
	if err != nil || task == "" {
		task, portStr = cmp.Or(rt.opts.Task, targetAddr), strconv.Itoa(rt.opts.Port)
	}
	dial := rt.opts.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	rawConn, err := dial(ctx, "tcp", rt.opts.RouterAddr)
	if err != nil {
		return nil, fmt.Errorf("ax router: dial %s: %w", rt.opts.RouterAddr, err)
	}
	tlsConn := tls.Client(rawConn, baseTLS.Clone())
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("ax router: TLS handshake with %s: %w", rt.opts.RouterAddr, err)
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = tlsConn.Close() })
	defer stopCancel()
	dest := net.JoinHostPort(task, portStr)
	connectReq := &http.Request{
		Method: http.MethodConnect, URL: &url.URL{Host: dest}, Host: dest,
		Header: http.Header{TargetActorHeader: []string{rt.opts.Atespace + "/" + task}},
	}
	if err := connectReq.Write(tlsConn); err != nil {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("ax router: write CONNECT %s: %w", dest, err)
	}
	br := bufio.NewReader(tlsConn)
	resp, err := http.ReadResponse(br, connectReq)
	if err != nil {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("ax router: read CONNECT %s response: %w", dest, err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
		_ = tlsConn.Close()
		return nil, fmt.Errorf("ax router: CONNECT %s rejected (%s): %s", dest, resp.Status, strings.TrimSpace(string(body)))
	}
	if !stopCancel() {
		_ = tlsConn.Close()
		return nil, ctx.Err()
	}
	return &tunnelConn{Conn: tlsConn, br: br}, nil
}

type tunnelConn struct {
	net.Conn
	br *bufio.Reader
}

func (c *tunnelConn) Read(p []byte) (int, error) { return c.br.Read(p) }

// DialAX connects to the ax.v1alpha1.AX gRPC service (plaintext h2c by default).
func DialAX(target string, opts ...grpc.DialOption) (v1alpha1.AXClient, *grpc.ClientConn, error) {
	target = strings.TrimPrefix(strings.TrimPrefix(cmp.Or(strings.TrimSpace(target), DefaultAXServerAddr), "http://"), "https://")
	if len(opts) == 0 {
		opts = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}
	conn, err := grpc.NewClient(target, opts...)
	return v1alpha1.NewAXClient(conn), conn, err
}

// SubstrateOptions configures a gRPC connection to Substrate's Control API.
type SubstrateOptions struct {
	Target, Authority, CAFile, TokenFile string
	Plaintext                            bool
}
type fileBearerCreds string

func (f fileBearerCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	b, err := os.ReadFile(string(f))
	return map[string]string{"authorization": "Bearer " + strings.TrimSpace(string(b))}, err
}
func (fileBearerCreds) RequireTransportSecurity() bool { return false }

// DialSubstrate connects to Substrate's Control API for per-actor egress policies.
func DialSubstrate(opts SubstrateOptions, extraOpts ...grpc.DialOption) (ateapipb.ControlClient, *grpc.ClientConn, error) {
	target, authority := cmp.Or(strings.TrimSpace(opts.Target), DefaultSubstrateAddr), cmp.Or(strings.TrimSpace(opts.Authority), DefaultSubstrateAuthority)
	if len(extraOpts) == 0 {
		if opts.Plaintext {
			extraOpts = append(extraOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
		} else {
			tlsCfg, err := buildTLS(nil, authority, opts.CAFile, "", "")
			if err != nil {
				return nil, nil, err
			}
			extraOpts = append(extraOpts, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)), grpc.WithAuthority(authority))
		}
		if tf := strings.TrimSpace(opts.TokenFile); tf != "" {
			extraOpts = append(extraOpts, grpc.WithPerRPCCredentials(fileBearerCreds(tf)))
		}
	}
	conn, err := grpc.NewClient(target, extraOpts...)
	return ateapipb.NewControlClient(conn), conn, err
}
