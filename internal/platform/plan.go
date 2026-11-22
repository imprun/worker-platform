package platform

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	ax "github.com/google/ax/pkg/apis/v1alpha1"
)

const (
	OwnerAnnotation = "workerplatform.imprun.dev/owner"
	GatewayAPI      = "gateway.networking.k8s.io"
)

type Plan struct {
	Task  *ax.Task
	Route Route
}

type Route struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Metadata   RouteMetadata   `json:"metadata"`
	Spec       json.RawMessage `json:"spec"`
}

type RouteMetadata struct {
	ObjectName
	Annotations map[string]string `json:"annotations,omitempty"`
	UID         string            `json:"uid,omitempty"`
	Generation  int64             `json:"generation,omitempty"`
}

func BuildPlan(w Worker) (Plan, error) {
	if err := w.Validate(); err != nil {
		return Plan{}, err
	}
	owner := w.Metadata.Namespace + "/" + w.Metadata.Name
	taskSpec := &ax.TaskSpec{
		Image:     w.Spec.Image,
		Resources: &ax.ResourcesSpec{Limits: &ax.ResourceList{Cpu: w.Spec.CPU, Memory: w.Spec.Memory}},
	}
	if w.Spec.EgressGateway != "" {
		taskSpec.Gateway = &ax.GatewayRef{Name: w.Spec.EgressGateway}
	}
	identity, err := json.Marshal(struct {
		Owner string
		Space string
		Spec  *ax.TaskSpec
	}{owner, w.Spec.Atespace, taskSpec})
	if err != nil {
		return Plan{}, err
	}
	hash := sha256.Sum256(identity)
	name := fmt.Sprintf("w-%s-%x", w.Metadata.Name, hash[:8])
	task := &ax.Task{
		ApiVersion: "ax.io/v1alpha1", Kind: "Task", Spec: taskSpec,
		Metadata: &ax.ObjectMeta{Name: name, Atespace: w.Spec.Atespace},
	}
	t := w.Spec.Trigger
	spec := map[string]any{
		"parentRefs": []any{map[string]any{
			"group": GatewayAPI, "kind": "Gateway", "name": t.Gateway.Name,
			"namespace": t.Gateway.Namespace, "sectionName": t.Gateway.SectionName,
		}},
		"hostnames": []string{t.Hostname},
		"rules": []any{map[string]any{
			"matches": []any{map[string]any{"method": "POST", "path": map[string]string{"type": "Exact", "value": t.Path}}},
			"filters": []any{map[string]any{
				"type": "RequestHeaderModifier", "requestHeaderModifier": map[string]any{
					"set": []map[string]string{{"name": "ate-target-actor", "value": w.Spec.Atespace + "/" + name}},
				},
			}},
			"backendRefs": []any{map[string]any{
				"group": "", "kind": "Service", "name": t.Router.Name,
				"namespace": t.Router.Namespace, "port": t.Router.Port, "weight": 1,
			}},
		}},
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return Plan{}, err
	}
	return Plan{Task: task, Route: Route{
		APIVersion: GatewayAPI + "/v1", Kind: "HTTPRoute",
		Metadata: RouteMetadata{ObjectName: w.Metadata, Annotations: map[string]string{OwnerAnnotation: owner}},
		Spec:     raw,
	}}, nil
}
