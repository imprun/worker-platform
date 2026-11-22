package main

import (
	"context"
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
}

func (o *jobOptions) register() {
	flag.StringVar(&o.address, "job-listen", "", "address of the namespace Job HTTP API")
	flag.StringVar(&o.dsnFile, "job-dsn-file", "", "file containing the Job PostgreSQL DSN")
	flag.StringVar(&o.tokenFile, "job-token-file", "", "file containing the namespace Job bearer token")
}

func (o jobOptions) run(ctx context.Context, namespace string,
	reconcile func(context.Context, func(context.Context, string, string, platform.JobRevision) error) error,
	resolve jobs.ResolvePublication, runtime jobs.Runtime, invoke jobs.InvokeAttempt) error {
	if o.address == "" && o.dsnFile == "" && o.tokenFile == "" {
		return reconcile(ctx, nil)
	}
	if o.address == "" || o.dsnFile == "" || o.tokenFile == "" || namespace == "" {
		return errors.New("Job API requires -job-listen, -job-dsn-file, -job-token-file and a namespace")
	}
	dsn, err := os.ReadFile(o.dsnFile)
	if err != nil || strings.TrimSpace(string(dsn)) == "" {
		return errors.New("read nonempty Job PostgreSQL DSN file")
	}
	secret, err := os.ReadFile(o.tokenFile)
	token := strings.TrimSpace(string(secret))
	if err != nil || len(token) < 32 || strings.ContainsAny(token, " \t\r\n") {
		return errors.New("read Job bearer token of at least 32 characters")
	}
	init, cancel := context.WithTimeout(ctx, 15*time.Second)
	store, err := jobs.Open(init, strings.TrimSpace(string(dsn)), namespace)
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
		Handler:           jobs.Handler(store, resolve, token),
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
