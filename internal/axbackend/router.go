// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

// Package axbackend provides internal AX gRPC dialing and mTLS HTTP CONNECT routing.
package axbackend

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
	"strconv"
	"strings"

	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	DefaultAXNamespace   = "ax-system"
	DefaultAXServerAddr  = "ax-server.ax-system:8080"
	DefaultRouterAddr    = "atenet-router.ate-system:8444"
	DefaultRouterSNI     = "atenet-router.ate-system.svc"
	DefaultControlAddr   = "api.ate-system.svc.cluster.local:443"
	DefaultControlSNI    = "api.ate-system.svc"
	DefaultPodCertBundle = "/run/podidentity.podcert.ate.dev/credential-bundle.pem"
	DefaultServiceDNSCA  = "/run/servicedns-ca/trust-bundle.pem"
	DefaultControlToken  = "/var/run/secrets/ateapi/token"
	TargetActorHeader    = "ate-target-actor"
	DefaultTaskPort      = 8080
)

// RouterOptions configures mTLS HTTP CONNECT tunneling to AX tasks.
type RouterOptions struct {
	RouterAddr, ServerName, Atespace, Task string
	Port                                   int
	TLSConfig                              *tls.Config
	GetTLSConfig                           func(context.Context) (*tls.Config, error)
	DialContext                            func(ctx context.Context, network, addr string) (net.Conn, error)
}

// RouterTransport tunnels HTTP/SSE requests over mTLS HTTP CONNECT to AX tasks.
type RouterTransport struct {
	opts    RouterOptions
	baseTLS *tls.Config
	tr      *http.Transport
}

// NewRouterTransport constructs a RouterTransport for the given RouterOptions.
func NewRouterTransport(opts RouterOptions) (*RouterTransport, error) {
	opts.RouterAddr = trimScheme(cmp.Or(strings.TrimSpace(opts.RouterAddr), DefaultRouterAddr))
	opts.ServerName = cmp.Or(strings.TrimSpace(opts.ServerName), DefaultRouterSNI)
	opts.Atespace = cmp.Or(strings.TrimSpace(opts.Atespace), "default")
	opts.Port = cmp.Or(opts.Port, DefaultTaskPort)
	base := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: opts.ServerName}
	if opts.TLSConfig != nil {
		base = opts.TLSConfig.Clone()
		base.ServerName = cmp.Or(base.ServerName, opts.ServerName)
	}
	rt := &RouterTransport{opts: opts, baseTLS: base}
	rt.tr = &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return rt.dialTunnel(ctx, addr)
		},
	}
	return rt, nil
}

func trimScheme(s string) string {
	return strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "https://"), "http://")
}

// RoundTrip executes req over an mTLS HTTP CONNECT tunnel to the target AX task.
func (rt *RouterTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	task, port := strings.TrimSpace(rt.opts.Task), rt.opts.Port
	if task == "" && req.URL != nil {
		task = req.URL.Hostname()
		if p, err := strconv.Atoi(req.URL.Port()); err == nil && p > 0 {
			port = p
		}
	}
	if task == "" {
		return nil, fmt.Errorf("axbackend: missing target task in request %q", req.URL)
	}
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = net.JoinHostPort(task, strconv.Itoa(port))
	clone.Host = clone.URL.Host
	return rt.tr.RoundTrip(clone)
}

func (rt *RouterTransport) dialTunnel(ctx context.Context, dest string) (net.Conn, error) {
	task, _, _ := net.SplitHostPort(dest)
	if task == "" {
		task = cmp.Or(rt.opts.Task, dest)
	}
	tlsCfg := rt.baseTLS.Clone()
	if rt.opts.GetTLSConfig != nil {
		dyn, err := rt.opts.GetTLSConfig(ctx)
		if err != nil {
			return nil, err
		}
		tlsCfg = dyn.Clone()
	}
	tlsCfg.ServerName = cmp.Or(tlsCfg.ServerName, rt.opts.ServerName)
	dial := rt.opts.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	rawConn, err := dial(ctx, "tcp", rt.opts.RouterAddr)
	if err != nil {
		return nil, fmt.Errorf("axbackend: dial router %s: %w", rt.opts.RouterAddr, err)
	}
	tlsConn := tls.Client(rawConn, tlsCfg)
	ok := false
	defer func() {
		if !ok {
			_ = tlsConn.Close()
		}
	}()
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("axbackend: router TLS handshake with %s: %w", rt.opts.RouterAddr, err)
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = tlsConn.Close() })
	defer stopCancel()
	connectReq := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Host: dest},
		Host:   dest,
		Header: http.Header{TargetActorHeader: []string{rt.opts.Atespace + "/" + task}},
	}
	if err := connectReq.Write(tlsConn); err != nil {
		return nil, fmt.Errorf("axbackend: write CONNECT %s: %w", dest, err)
	}
	br := bufio.NewReader(tlsConn)
	resp, err := http.ReadResponse(br, connectReq)
	if err != nil {
		return nil, fmt.Errorf("axbackend: read CONNECT %s response: %w", dest, err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("axbackend: CONNECT %s rejected (%s): %s", dest, resp.Status, strings.TrimSpace(string(body)))
	}
	if !stopCancel() {
		return nil, ctx.Err()
	}
	ok = true
	return &tunnelConn{Conn: tlsConn, br: br}, nil
}

type tunnelConn struct {
	net.Conn
	br *bufio.Reader
}

func (c *tunnelConn) Read(p []byte) (int, error) { return c.br.Read(p) }

// DialAX connects to the ax.v1alpha1.AX gRPC service (plaintext h2c by default).
func DialAX(target string, opts ...grpc.DialOption) (v1alpha1.AXClient, *grpc.ClientConn, error) {
	if len(opts) == 0 {
		opts = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}
	conn, err := grpc.NewClient(trimScheme(cmp.Or(strings.TrimSpace(target), DefaultAXServerAddr)), opts...)
	return v1alpha1.NewAXClient(conn), conn, err
}

type bearerFunc func(context.Context) (string, error)

func (fn bearerFunc) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	tok, err := fn(ctx)
	return map[string]string{"authorization": "Bearer " + strings.TrimSpace(tok)}, err
}

func (bearerFunc) RequireTransportSecurity() bool { return false }

func dialControl(target string, rootCAs *x509.CertPool, getToken func(context.Context) (string, error), extraOpts ...grpc.DialOption) (ateapipb.ControlClient, *grpc.ClientConn, error) {
	target = trimScheme(cmp.Or(strings.TrimSpace(target), DefaultControlAddr))
	if len(extraOpts) == 0 {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: DefaultControlSNI, RootCAs: rootCAs}
		extraOpts = append(extraOpts, grpc.WithTransportCredentials(credentials.NewTLS(cfg)), grpc.WithAuthority(DefaultControlSNI))
		if getToken != nil {
			extraOpts = append(extraOpts, grpc.WithPerRPCCredentials(bearerFunc(getToken)))
		}
	}
	conn, err := grpc.NewClient(target, extraOpts...)
	return ateapipb.NewControlClient(conn), conn, err
}
