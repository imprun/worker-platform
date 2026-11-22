package platform

import (
	"context"
	"fmt"
	"time"

	ax "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type RouteStore interface {
	Read(context.Context, ObjectName) (*Route, error)
	Publish(context.Context, Route, *Route) error
}

type Deployment struct {
	AX           ax.AXClient
	Routes       RouteStore
	PollInterval time.Duration
}

// Deploy snapshots ownership before waiting on AX. Publish must refuse a stale snapshot.
func (d Deployment) Deploy(ctx context.Context, w Worker) (*ax.Task, error) {
	plan, err := BuildPlan(w)
	if err != nil {
		return nil, err
	}
	before, err := d.Routes.Read(ctx, plan.Route.Metadata.ObjectName)
	if err != nil {
		return nil, fmt.Errorf("read HTTPRoute: %w", err)
	}
	if before != nil && before.Metadata.Annotations[OwnerAnnotation] != plan.Route.Metadata.Annotations[OwnerAnnotation] {
		return nil, fmt.Errorf("HTTPRoute %s/%s belongs to another owner", w.Metadata.Namespace, w.Metadata.Name)
	}
	task, err := d.awaitReady(ctx, plan.Task)
	if err != nil {
		return nil, err
	}
	if err := d.Routes.Publish(ctx, plan.Route, before); err != nil {
		return nil, fmt.Errorf("publish HTTPRoute: %w", err)
	}
	return task, nil
}

func (d Deployment) awaitReady(ctx context.Context, desired *ax.Task) (*ax.Task, error) {
	space, name := desired.Metadata.Atespace, desired.Metadata.Name
	get := &ax.GetTaskRequest{Atespace: space, Name: name}
	task, err := d.AX.GetTask(ctx, get)
	if status.Code(err) == codes.NotFound {
		task, err = d.AX.CreateTask(ctx, &ax.CreateTaskRequest{Task: desired})
		if status.Code(err) == codes.AlreadyExists {
			task, err = d.AX.GetTask(ctx, get)
		}
	}
	interval := d.PollInterval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	resumed := false
	for {
		if err != nil {
			return nil, fmt.Errorf("read or create AX Task %s/%s: %w", space, name, err)
		}
		if task.GetMetadata().GetName() != name || task.GetMetadata().GetAtespace() != space || !proto.Equal(task.GetSpec(), desired.Spec) {
			return nil, fmt.Errorf("AX Task %s/%s differs from this release", space, name)
		}
		current := task.GetStatus()
		if current.GetPhase() == "Running" && current.GetActor() == name && current.GetActorUid() != "" {
			for _, c := range current.GetConditions() {
				if c.GetType() == "Ready" && c.GetStatus() == "True" {
					return task, nil
				}
			}
		}
		if current.GetPhase() == "Failed" {
			return nil, fmt.Errorf("AX Task %s/%s failed before becoming Ready", space, name)
		}
		if current.GetPhase() == "Suspended" && !resumed {
			_, err := d.AX.ResumeTask(ctx, &ax.ResumeTaskRequest{Atespace: space, Name: name})
			if err != nil {
				return nil, fmt.Errorf("resume AX Task %s/%s: %w", space, name, err)
			}
			resumed = true
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for AX Task %s/%s in phase %q: %w", space, name, current.GetPhase(), ctx.Err())
		case <-ticker.C:
			task, err = d.AX.GetTask(ctx, get)
		}
	}
}
