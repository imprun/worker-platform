package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/imprun/worker-platform/internal/ateclient"
	"github.com/imprun/worker-platform/internal/platform"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type ateOptions struct {
	ateclient.Options
}

func (o *ateOptions) register(flags *flag.FlagSet) {
	o.Register(flags)
}

func (o ateOptions) deploy(ctx context.Context, out io.Writer, worker platform.Worker, profile platform.WorkerProfile, kubeContext string) error {
	client, err := o.Open()
	if err != nil {
		return err
	}
	defer client.Close()
	deployment := platform.ATEDeployment{
		Control: client.Control, Cluster: platform.Kubernetes{Context: kubeContext}, Ready: client.Ready,
	}
	result, err := deployment.Deploy(ctx, worker, profile)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}

func renderATE(out io.Writer, worker platform.Worker, profile platform.WorkerProfile, selection string) error {
	plan, err := platform.BuildATEPlan(worker, profile)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if selection == "route" {
		return encoder.Encode(plan.Route)
	}
	values := []struct {
		name    string
		message proto.Message
	}{{"template", plan.Template}, {"actor", plan.Actor}, {"policy", plan.Policy}}
	for _, value := range values {
		if selection != "all" && selection != value.name {
			continue
		}
		data, err := (protojson.MarshalOptions{Indent: "  "}).Marshal(value.message)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, string(data)); err != nil || selection == value.name {
			return err
		}
		if _, err := fmt.Fprintln(out, "---"); err != nil {
			return err
		}
	}
	if selection == "all" {
		return encoder.Encode(plan.Route)
	}
	return fmt.Errorf("unknown ATE output %q (expected all, template, actor, policy, or route)", selection)
}
