// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/erain/glue/gateway"
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
	fs.StringVar(&cfgPath, "config", "", "path to gateway YAML configuration file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(cfgPath) == "" {
		return errors.New("--config is required")
	}
	cfg, err := gateway.LoadConfig(cfgPath)
	if err != nil {
		return err
	}
	srv, err := gateway.NewServer(cfg)
	if err != nil {
		return err
	}
	defer srv.Close()

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}
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
