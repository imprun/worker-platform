package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/imprun/worker-platform/internal/jobs"
	"github.com/imprun/worker-platform/internal/platform"
	"golang.org/x/sync/errgroup"
)

type jobOptions struct {
	address, dsnFile, tokenFile string
	siteKeyFile, authTokenFile  string
}

func (o *jobOptions) register() {
	flag.StringVar(&o.address, "job-listen", "", "address of the namespace Job HTTP API")
	flag.StringVar(&o.dsnFile, "job-dsn-file", "", "file containing the Job PostgreSQL DSN")
	flag.StringVar(&o.tokenFile, "job-token-file", "", "file containing the namespace Job bearer token")
	flag.StringVar(&o.siteKeyFile, "job-site-key-file", "", "base64 file containing a 32-byte site session encryption key")
	flag.StringVar(&o.authTokenFile, "job-reauth-token-file", "", "file containing the private site authentication provider bearer token")
}

func (o jobOptions) run(ctx context.Context, namespace string,
	reconcile func(context.Context, func(context.Context, string, string, platform.JobRevision) error) error,
	resolve jobs.ResolvePublication, runtime jobs.Runtime, invoke jobs.InvokeAttempt) error {
	if o.address == "" && o.dsnFile == "" && o.tokenFile == "" && o.siteKeyFile == "" && o.authTokenFile == "" {
		return reconcile(ctx, nil)
	}
	if o.address == "" || o.dsnFile == "" || o.tokenFile == "" || namespace == "" {
		return errors.New("Job API requires -job-listen, -job-dsn-file, -job-token-file and a namespace")
	}
	dsn, err := os.ReadFile(o.dsnFile)
	if err != nil || strings.TrimSpace(string(dsn)) == "" {
		return errors.New("read nonempty Job PostgreSQL DSN file")
	}
	token, err := readBearer(o.tokenFile)
	if err != nil {
		return errors.New("read Job bearer token of at least 32 characters")
	}
	siteKey, providerToken, err := o.siteSessionConfig()
	if err != nil {
		return err
	}
	if providerToken != "" && token == providerToken {
		return errors.New("Job and reauthentication provider tokens must be distinct")
	}
	init, cancel := context.WithTimeout(ctx, 15*time.Second)
	store, err := jobs.Open(init, strings.TrimSpace(string(dsn)), namespace, siteKey)
	cancel()
	if err != nil {
		return err
	}
	defer store.Close()
	listener, err := net.Listen("tcp", o.address)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{
		Handler:           jobs.Handler(store, resolve, token, providerToken),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	engine := jobs.Engine{Store: store, Runtime: runtime, Invoke: invoke}
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error { return reconcile(groupCtx, store.RecordRevision) })
	group.Go(func() error { return engine.Run(groupCtx) })
	group.Go(func() error {
		slog.Info("serving Job API", "address", listener.Addr(), "namespace", namespace)
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	})
	group.Go(func() error {
		<-groupCtx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(groupCtx), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			server.Close()
			return err
		}
		return nil
	})
	return group.Wait()
}

func (o jobOptions) siteSessionConfig() ([]byte, string, error) {
	if o.siteKeyFile == "" && o.authTokenFile == "" {
		return nil, "", nil
	}
	encoded, err := os.ReadFile(o.siteKeyFile)
	if err != nil {
		return nil, "", errors.New("site sessions require -job-site-key-file and -job-reauth-token-file")
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil || len(key) != 32 {
		return nil, "", errors.New("site session key must encode 32 bytes in base64")
	}
	token, err := readBearer(o.authTokenFile)
	if err != nil {
		return nil, "", errors.New("read reauthentication provider bearer token of at least 32 characters")
	}
	return key, token, nil
}

func readBearer(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if len(token) < 32 || strings.ContainsAny(token, " \t\r\n") {
		return "", errors.New("invalid bearer token")
	}
	return token, nil
}
