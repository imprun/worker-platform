package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"reflect"
)

const routeResource = "httproutes.gateway.networking.k8s.io"

type Kubernetes struct {
	Context string
}

func (k Kubernetes) command(ctx context.Context, ns string, input []byte, args ...string) ([]byte, error) {
	flags := []string{"--namespace", ns}
	if k.Context != "" {
		flags = append(flags, "--context", k.Context)
	}
	cmd := exec.CommandContext(ctx, "kubectl", append(flags, args...)...)
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("kubectl %s: %w %s", args[0], err, bytes.TrimSpace(stderr.Bytes()))
	}
	return stdout, nil
}

func (k Kubernetes) Read(ctx context.Context, ref ObjectName) (*Route, error) {
	data, err := k.command(ctx, ref.Namespace, nil, "get", routeResource, ref.Name, "--ignore-not-found", "-o", "json")
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return nil, err
	}
	var route Route
	if err := json.Unmarshal(data, &route); err != nil {
		return nil, fmt.Errorf("decode HTTPRoute: %w", err)
	}
	if route.Metadata.ObjectName != ref || route.Metadata.UID == "" || route.Metadata.Generation < 1 {
		return nil, fmt.Errorf("unexpected HTTPRoute identity for %s/%s", ref.Namespace, ref.Name)
	}
	return &route, nil
}

func (k Kubernetes) Publish(ctx context.Context, want Route, before *Route) error {
	ref := want.Metadata.ObjectName
	if before == nil {
		data, err := json.Marshal(want)
		if err != nil {
			return err
		}
		_, err = k.command(ctx, ref.Namespace, data, "create", "-f", "-")
		return err
	}
	owner := want.Metadata.Annotations[OwnerAnnotation]
	current, err := k.Read(ctx, ref)
	if err != nil {
		return err
	}
	if current == nil || before.Metadata.UID != current.Metadata.UID || before.Metadata.Generation != current.Metadata.Generation ||
		current.Metadata.Annotations[OwnerAnnotation] != owner || before.Metadata.Annotations[OwnerAnnotation] != owner {
		return fmt.Errorf("HTTPRoute %s/%s changed while the release was preparing", ref.Namespace, ref.Name)
	}
	patch, err := routePatch(want, current)
	if err != nil || len(patch) == 0 {
		return err
	}
	_, err = k.command(ctx, ref.Namespace, nil, "patch", routeResource, ref.Name, "--type=json", "--patch", string(patch))
	return err
}

func routePatch(want Route, current *Route) ([]byte, error) {
	var left, right map[string]any
	if err := json.Unmarshal(want.Spec, &left); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(current.Spec, &right); err != nil {
		return nil, err
	}
	if reflect.DeepEqual(left, right) {
		return nil, nil
	}
	type operation struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value any    `json:"value"`
	}
	return json.Marshal([]operation{
		{"test", "/metadata/uid", current.Metadata.UID},
		{"test", "/metadata/generation", current.Metadata.Generation},
		{"test", "/metadata/annotations/workerplatform.imprun.dev~1owner", want.Metadata.Annotations[OwnerAnnotation]},
		{"replace", "/spec", want.Spec},
	})
}
