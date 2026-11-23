package main

import (
	"context"
	"errors"
	"flag"
	"time"

	"github.com/imprun/worker-platform/internal/controller"
	"github.com/imprun/worker-platform/internal/jobs"
	"golang.org/x/sync/errgroup"
)

type jobOptions struct {
	dsnFile, siteKeyFile string
}

func (o *jobOptions) register() {
	flag.StringVar(&o.dsnFile, "job-dsn-file", "", "file containing the Job PostgreSQL DSN")
	flag.StringVar(&o.siteKeyFile, "job-site-key-file", "", "base64 file containing a 32-byte site session encryption key")
}

func (o jobOptions) run(ctx context.Context, namespace string, reconcile func(context.Context, controller.JobPublications) error,
	runtime jobs.Runtime, invoke jobs.InvokeAttempt) error {
	if o.dsnFile == "" {
		if o.siteKeyFile != "" {
			return errors.New("-job-site-key-file requires -job-dsn-file")
		}
		return reconcile(ctx, nil)
	}
	if namespace == "" {
		return errors.New("Job controller namespace is required")
	}
	init, cancel := context.WithTimeout(ctx, 15*time.Second)
	store, err := jobs.OpenFiles(init, namespace, o.dsnFile, o.siteKeyFile)
	cancel()
	if err != nil {
		return err
	}
	defer store.Close()
	engine := jobs.Engine{Store: store, Runtime: runtime, Invoke: invoke}
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error { return reconcile(groupCtx, store) })
	group.Go(func() error { return engine.Run(groupCtx) })
	return group.Wait()
}
