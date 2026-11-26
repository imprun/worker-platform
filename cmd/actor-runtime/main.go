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

// 호출 증거에는 런타임이 검증한 OCI digest를 기록한다. 앱의 JSON 응답은 그대로 둔다.
func withPackageDigest(handler http.Handler, digest string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/invoke" {
			w.Header().Set("X-Imprun-Worker-Package-Digest", digest)
		}
		handler.ServeHTTP(w, r)
	})
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
		Handler:           withPackageDigest(runner.routes(), ref.Digest),
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
