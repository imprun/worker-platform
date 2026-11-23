package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/imprun/worker-platform/internal/jobs"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("run-api", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	namespace := flag.String("namespace", os.Getenv("POD_NAMESPACE"), "namespace of the Job API")
	address := flag.String("job-listen", ":8084", "address of the Job HTTP API")
	dsnFile := flag.String("job-dsn-file", "", "file containing the Job PostgreSQL DSN")
	tokenFile := flag.String("job-token-file", "", "file containing the namespace Job bearer token")
	siteKeyFile := flag.String("job-site-key-file", "", "base64 file containing a 32-byte site session encryption key")
	providerFile := flag.String("job-reauth-token-file", "", "file containing the private site authentication provider bearer token")
	flag.Parse()
	if *namespace == "" || *address == "" || *dsnFile == "" || *tokenFile == "" {
		return errors.New("Run API requires -namespace, -job-listen, -job-dsn-file and -job-token-file")
	}
	if (*siteKeyFile == "") != (*providerFile == "") {
		return errors.New("site sessions require -job-site-key-file and -job-reauth-token-file")
	}
	token, err := readBearer(*tokenFile)
	if err != nil {
		return err
	}
	var providerToken string
	if *providerFile != "" {
		providerToken, err = readBearer(*providerFile)
		if err != nil {
			return err
		}
		if providerToken == token {
			return errors.New("Job and reauthentication provider tokens must be distinct")
		}
	}
	init, cancel := context.WithTimeout(ctx, 15*time.Second)
	store, err := jobs.OpenFiles(init, *namespace, *dsnFile, *siteKeyFile)
	cancel()
	if err != nil {
		return err
	}
	defer store.Close()

	mux := http.NewServeMux()
	mux.Handle("/", jobs.Handler(store, token, providerToken))
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		probe, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := store.Ping(probe); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	server := &http.Server{
		Addr:              *address,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			return errors.Join(err, server.Close())
		}
		return nil
	}
}

func readBearer(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("read bearer token file")
	}
	token := strings.TrimSpace(string(data))
	if len(token) < 32 || strings.ContainsAny(token, " \t\r\n") {
		return "", errors.New("invalid bearer token")
	}
	return token, nil
}
