package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/imprun/worker-platform/internal/ateclient"
	"github.com/imprun/worker-platform/internal/controller"
	"github.com/imprun/worker-platform/internal/platform"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("worker-controller", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	namespace := flag.String("namespace", os.Getenv("POD_NAMESPACE"), "namespace to watch")
	var options ateclient.Options
	options.Register(flag.CommandLine)
	flag.Parse()
	config, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	config.UserAgent = "worker-platform/worker-controller"
	watchClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return err
	}
	config.Timeout = 10 * time.Second
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return err
	}
	connection, err := options.Open()
	if err != nil {
		return err
	}
	defer connection.Close()
	deployment := platform.ATEDeployment{
		Control: connection.Control,
		Cluster: platform.KubernetesClient{Client: client},
		Ready:   connection.Ready,
	}
	return controller.Run(ctx, watchClient, client, *namespace, deployment)
}
