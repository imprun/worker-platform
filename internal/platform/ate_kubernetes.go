package platform

import (
	"context"
	"encoding/json"
	"fmt"
)

type Condition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	ObservedGeneration int64  `json:"observedGeneration"`
}

func conditionTrue(conditions []Condition, name string, generation int64) bool {
	for _, condition := range conditions {
		if condition.Type == name && condition.Status == "True" && condition.ObservedGeneration == generation {
			return true
		}
	}
	return false
}

type RouteStatus struct {
	Parents []RouteParentStatus `json:"parents"`
}

type RouteParentStatus struct {
	ParentRef  RouteParentRef `json:"parentRef"`
	Conditions []Condition    `json:"conditions"`
}

type RouteParentRef struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	SectionName string `json:"sectionName"`
	Group       string `json:"group"`
	Kind        string `json:"kind"`
}

func (r Route) acceptedBy(gateway GatewayRef) bool {
	if r.Status == nil {
		return false
	}
	for _, parent := range r.Status.Parents {
		ref := parent.ParentRef
		if ref.Namespace == "" {
			ref.Namespace = r.Metadata.Namespace
		}
		if ref.Name != gateway.Name || ref.Namespace != gateway.Namespace || ref.SectionName != gateway.SectionName ||
			(ref.Group != "" && ref.Group != GatewayAPI) || (ref.Kind != "" && ref.Kind != "Gateway") {
			continue
		}
		if conditionTrue(parent.Conditions, "Accepted", r.Metadata.Generation) &&
			conditionTrue(parent.Conditions, "ResolvedRefs", r.Metadata.Generation) {
			return true
		}
	}
	return false
}

func (k Kubernetes) CheckWorkerPool(ctx context.Context, profile WorkerProfile) error {
	p := profile.Spec
	data, err := k.command(ctx, p.WorkerPool.Namespace, nil, "get", "workerpools.ate.dev", p.WorkerPool.Name, "-o", "json")
	if err != nil {
		return fmt.Errorf("read WorkerPool: %w", err)
	}
	return checkWorkerPool(data, profile)
}

func checkWorkerPool(data []byte, profile WorkerProfile) error {
	p := profile.Spec
	var pool struct {
		Metadata struct {
			ObjectName
			UID               string            `json:"uid"`
			Labels            map[string]string `json:"labels"`
			DeletionTimestamp *string           `json:"deletionTimestamp"`
		} `json:"metadata"`
		Spec struct {
			SandboxClasses []struct {
				Name string `json:"name"`
			} `json:"sandboxClasses"`
			Template struct {
				NodeSelector map[string]string `json:"nodeSelector"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(data, &pool); err != nil {
		return fmt.Errorf("decode WorkerPool: %w", err)
	}
	if pool.Metadata.ObjectName != p.WorkerPool || pool.Metadata.UID == "" || pool.Metadata.DeletionTimestamp != nil ||
		pool.Metadata.Labels[PoolIDLabel] != p.PoolID {
		return fmt.Errorf("WorkerPool %s/%s does not match the profile's pool identity", p.WorkerPool.Namespace, p.WorkerPool.Name)
	}
	if len(pool.Spec.SandboxClasses) != 1 || pool.Spec.SandboxClasses[0].Name != p.SandboxClass ||
		pool.Spec.Template.NodeSelector["kubernetes.io/arch"] != p.Architecture {
		return fmt.Errorf("WorkerPool %s/%s does not pin %s on %s", p.WorkerPool.Namespace, p.WorkerPool.Name, p.SandboxClass, p.Architecture)
	}
	return nil
}

func (k Kubernetes) GatewayProgrammed(ctx context.Context, ref GatewayRef) (bool, error) {
	data, err := k.command(ctx, ref.Namespace, nil, "get", "gateways.gateway.networking.k8s.io", ref.Name, "-o", "json")
	if err != nil {
		return false, fmt.Errorf("read Gateway: %w", err)
	}
	return gatewayProgrammed(data, ref)
}

func gatewayProgrammed(data []byte, ref GatewayRef) (bool, error) {
	var gateway struct {
		Metadata RouteMetadata `json:"metadata"`
		Status   struct {
			Conditions []Condition `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(data, &gateway); err != nil {
		return false, fmt.Errorf("decode Gateway: %w", err)
	}
	if gateway.Metadata.ObjectName != ref.ObjectName || gateway.Metadata.Generation < 1 {
		return false, fmt.Errorf("unexpected Gateway identity for %s/%s", ref.Namespace, ref.Name)
	}
	return conditionTrue(gateway.Status.Conditions, "Programmed", gateway.Metadata.Generation), nil
}
