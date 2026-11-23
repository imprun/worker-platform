package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/imprun/worker-platform/internal/apppackage"
)

func main() {
	address := flag.String("listen", ":80", "HTTP listen address")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *address); err != nil {
		slog.Error("actor runtime", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, address string) error {
	ref, err := apppackage.ParseReference(os.Getenv(apppackage.ReferenceEnv))
	if err != nil {
		return err
	}
	transport := &http.Transport{
		DisableCompression: true,
		TLSClientConfig:    &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if pem := os.Getenv(apppackage.RegistryCAEnv); pem != "" {
		pool := x509.NewCertPool()
		if err := apppackage.AppendRegistryCA(pool, pem); err != nil {
			return err
		}
		transport.TLSClientConfig.RootCAs = pool
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if err := os.MkdirAll(os.TempDir(), 0700); err != nil {
		return err
	}
	startup, cancel := context.WithTimeout(ctx, 27*time.Second)
	defer cancel()
	app, err := apppackage.Install(startup, client, ref)
	if err != nil {
		return err
	}
	defer os.RemoveAll(app.Dir)
	runner := appRunner{app}
	if err := runner.command(startup, "--check").Run(); err != nil {
		return fmt.Errorf("app startup check: %w", err)
	}
	cancel()

	server := &http.Server{
		Addr:              address,
		Handler:           runner.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      100 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			return errors.Join(err, server.Close())
		}
		return nil
	}
}
