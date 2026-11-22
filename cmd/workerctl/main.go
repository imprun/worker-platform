package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/imprun/worker-platform/internal/platform"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "workerctl:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out, diagnostics io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(out, "Usage: workerctl render -f worker.yaml -profile profile.yaml [-output all|template|actor|policy|route]")
		fmt.Fprintln(out, "       workerctl deploy -f worker.yaml -profile profile.yaml -kube-context name [ATE and Router TLS options]")
		return nil
	}
	command := args[0]
	if command != "render" && command != "deploy" {
		return fmt.Errorf("unknown command %q", command)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	file := flags.String("f", "", "HTTPWorker manifest file (use - for stdin)")
	profile := flags.String("profile", "", "WorkerProfile manifest file")
	var output, kubeContext string
	var timeout time.Duration
	var options ateOptions
	if command == "render" {
		flags.StringVar(&output, "output", "all", "object to render (all, template, actor, policy, route)")
	} else {
		flags.StringVar(&kubeContext, "kube-context", "", "Kubernetes context for the deployment")
		flags.DurationVar(&timeout, "timeout", 2*time.Minute, "deployment deadline")
		options.register(flags)
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *file == "" || *profile == "" || flags.NArg() != 0 {
		return errors.New("provide an HTTPWorker with -f and its WorkerProfile with -profile")
	}
	if command == "deploy" {
		if kubeContext == "" {
			return errors.New("deployment requires an explicit -kube-context")
		}
		if timeout <= 0 {
			return errors.New("timeout must be positive")
		}
	}
	input := os.Stdin
	if *file != "-" {
		var err error
		input, err = os.Open(*file)
		if err != nil {
			return fmt.Errorf("open HTTPWorker: %w", err)
		}
		defer input.Close()
	}
	worker, err := platform.ReadWorker(input)
	if err != nil {
		return err
	}
	profileFile, err := os.Open(*profile)
	if err != nil {
		return fmt.Errorf("open WorkerProfile: %w", err)
	}
	defer profileFile.Close()
	p, err := platform.ReadWorkerProfile(profileFile)
	if err != nil {
		return err
	}
	if command == "render" {
		return renderATE(out, worker, p, output)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return options.deploy(ctx, out, worker, p, kubeContext)
}
