// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/erain/glue/daemon"
	"github.com/erain/glue/gateway"
	"github.com/erain/glue/internal/axbackend"
	"github.com/erain/glue/internal/jobclient"
	"github.com/erain/glue/internal/redact"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr, nil); err != nil {
		fmt.Fprintf(os.Stderr, "glue-gateway: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stderr io.Writer, onReady func(addr string)) error {
	fs := flag.NewFlagSet("glue-gateway", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfgPath string
	var insecureDev bool
	fs.StringVar(&cfgPath, "config", "", "path to gateway YAML configuration file")
	fs.BoolVar(&insecureDev, "insecure-dev", false, "allow dev-token auth mode when public_url is https://")
	stateDir, listenAddr, tunnelToken, quick, tunnel := fs.String("state-dir", "", "gateway state directory"), fs.String("listen", "", "TCP listen address"), fs.String("tunnel-token", "", "Cloudflare Tunnel token"), fs.Bool("quick", false, "start with zero YAML, #token magic link, and QR code"), fs.Bool("tunnel", false, "expose gateway over cloudflared HTTPS tunnel")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := gateway.Config{}, error(nil)
	if *quick {
		cfg, err = buildQuickConfig(*stateDir, *listenAddr)
	} else if strings.TrimSpace(cfgPath) == "" {
		return errors.New("--config is required")
	} else if raw, rerr := os.ReadFile(cfgPath); rerr != nil {
		return fmt.Errorf("gateway: read config %s: %w", cfgPath, rerr)
	} else {
		cfg, err = gateway.DecodeConfig(raw)
	}
	if err != nil {
		return err
	}
	cfg.Listen, cfg.StateDir = cmp.Or(strings.TrimSpace(*listenAddr), cfg.Listen), cmp.Or(strings.TrimSpace(*stateDir), cfg.StateDir)
	cfg.TrustForwardedProto, cfg.Auth.InsecureDev = cfg.TrustForwardedProto || *tunnel, cfg.Auth.InsecureDev || insecureDev || *quick || *tunnel
	if cfg, err = cfg.Validate(); err != nil {
		return err
	}
	srv, err := gateway.NewServer(cfg)
	if err != nil && *quick && len(cfg.Backends) > 1 {
		cfg.Backends, cfg.DefaultBackend = cfg.Backends[1:], "local"
		srv, err = gateway.NewServer(cfg)
	}
	if err != nil {
		return err
	}
	defer srv.Close()

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	_, stopTunnel, err := axbackend.Expose(ctx, ln.Addr(), cfg.Auth.Token, *tunnel, *tunnelToken, *quick || *tunnel, true, stderr)
	if err != nil {
		return errors.Join(err, ln.Close())
	}
	defer stopTunnel()
	httpSrv := gateway.NewHTTPServer(cfg.Listen, srv)
	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	fmt.Fprintf(stderr, "glue-gateway listening on http://%s\n", ln.Addr().String())
	if onReady != nil {
		onReady(ln.Addr().String())
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		return <-errCh
	}
}

func buildQuickConfig(stateDir, listenAddr string) (gateway.Config, error) {
	stateDir = cmp.Or(strings.TrimSpace(stateDir), filepath.Join(jobclient.ConfigDir(), "gateway"))
	tokPath := filepath.Join(stateDir, "token")
	rawTok, _ := os.ReadFile(tokPath)
	tok := strings.TrimSpace(string(rawTok))
	if tok == "" {
		tok, _ = daemon.RandomToken()
		if err := errors.Join(os.MkdirAll(stateDir, 0o700), os.WriteFile(tokPath, []byte(tok+"\n"), 0o600)); err != nil {
			return gateway.Config{}, err
		}
	}
	_ = os.Chmod(tokPath, 0o600)
	redact.Register(tok)
	lcfg, _ := jobclient.ResolveConfig(jobclient.Config{})
	backs, def := []gateway.BackendConfig{{Name: "local", Type: "local", URL: cmp.Or(lcfg.ServerURL, "http://127.0.0.1:8787"), Token: cmp.Or(lcfg.Token, tok)}}, "local"
	if kc, kctx, kerr := axbackend.ResolveKubeTarget("", ""); kerr == nil || cmp.Or(strings.TrimSpace(os.Getenv("GLUE_AX_SERVER")), strings.TrimSpace(os.Getenv("AX_SERVER_ADDR"))) != "" {
		backs, def = append([]gateway.BackendConfig{{Name: "ax", Type: "ax", KubeConfig: kc, KubeContext: kctx, Atespace: "default"}}, backs...), "ax"
	}
	return gateway.Config{Listen: cmp.Or(strings.TrimSpace(listenAddr), "127.0.0.1:0"), StateDir: stateDir, Auth: gateway.AuthConfig{Mode: "dev-token", Token: tok, InsecureDev: true}, Backends: backs, DefaultBackend: def}, nil
}
