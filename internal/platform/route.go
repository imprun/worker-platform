package platform

import "encoding/json"

const (
	OwnerAnnotation = "workerplatform.imprun.dev/owner"
	GatewayAPI      = "gateway.networking.k8s.io"
)

type Route struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Metadata   RouteMetadata   `json:"metadata"`
	Spec       json.RawMessage `json:"spec"`
	Status     *RouteStatus    `json:"status,omitempty"`
}

type RouteMetadata struct {
	ObjectName
	Annotations map[string]string `json:"annotations,omitempty"`
	UID         string            `json:"uid,omitempty"`
	Generation  int64             `json:"generation,omitempty"`
}

func buildRoute(w Worker, owner, target string) (Route, error) {
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
					"set": []map[string]string{{"name": "ate-target-actor", "value": target}},
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
		return Route{}, err
	}
	return Route{
		APIVersion: GatewayAPI + "/v1", Kind: "HTTPRoute",
		Metadata: RouteMetadata{ObjectName: w.Metadata, Annotations: map[string]string{OwnerAnnotation: owner}},
		Spec:     raw,
	}, nil
}
