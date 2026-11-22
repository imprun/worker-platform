package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	ax "github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/imprun/worker-platform/internal/platform"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
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
		fmt.Fprintln(out, "Usage: workerctl render -f worker.yaml [-output all|task|route]")
		fmt.Fprintln(out, "       workerctl deploy -f worker.yaml [-ax-address host:port] [-kube-context name]")
		return nil
	}
	command := args[0]
	if command != "render" && command != "deploy" {
		return fmt.Errorf("unknown command %q", command)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	file := flags.String("f", "", "HTTPWorker manifest file (use - for stdin)")
	var output, address, ca, serverName, kubeContext *string
	var timeout *time.Duration
	if command == "render" {
		output = flags.String("output", "all", "object to render (all, task, route)")
	} else {
		address = flags.String("ax-address", "127.0.0.1:8080", "AX gRPC address")
		ca = flags.String("ax-ca", "", "PEM trust bundle for AX TLS")
		serverName = flags.String("ax-server-name", "", "override the AX TLS server name")
		kubeContext = flags.String("kube-context", "", "kubectl context (defaults to its current context)")
		timeout = flags.Duration("timeout", 2*time.Minute, "deployment deadline")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *file == "" || flags.NArg() != 0 {
		return errors.New("provide a single manifest with -f")
	}
	input := os.Stdin
	if *file != "-" {
		var err error
		input, err = os.Open(*file)
		if err != nil {
			return err
		}
		defer input.Close()
	}
	worker, err := platform.ReadWorker(input)
	if err != nil {
		return err
	}
	if command == "render" {
		return render(out, worker, *output)
	}
	if *timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	transport, err := axTransport(*address, *ca, *serverName)
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(*address, grpc.WithTransportCredentials(transport))
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	deployment := platform.Deployment{AX: ax.NewAXClient(conn), Routes: platform.Kubernetes{Context: *kubeContext}}
	task, err := deployment.Deploy(ctx, worker)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(map[string]string{
		"task": task.GetMetadata().GetName(), "actorUID": task.GetStatus().GetActorUid(),
		"route": worker.Metadata.Namespace + "/" + worker.Metadata.Name, "status": "published",
	})
}

func render(out io.Writer, w platform.Worker, selection string) error {
	if selection != "all" && selection != "task" && selection != "route" {
		return fmt.Errorf("unknown output %q", selection)
	}
	plan, err := platform.BuildPlan(w)
	if err != nil {
		return err
	}
	if selection != "route" {
		data, err := (protojson.MarshalOptions{Indent: "  "}).Marshal(plan.Task)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, string(data)); err != nil || selection == "task" {
			return err
		}
		if _, err := fmt.Fprintln(out, "---"); err != nil {
			return err
		}
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(plan.Route)
}

func axTransport(address, ca, serverName string) (credentials.TransportCredentials, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return nil, errors.New("AX address must be host:port")
	}
	ip := net.ParseIP(host)
	if ca == "" && serverName == "" && (host == "localhost" || ip != nil && ip.IsLoopback()) {
		return insecure.NewCredentials(), nil
	}
	if ca != "" {
		return credentials.NewClientTLSFromFile(ca, serverName)
	}
	return credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}), nil
}
