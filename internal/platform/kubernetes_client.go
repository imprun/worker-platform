package platform

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

var (
	routesResource   = schema.GroupVersionResource{Group: GatewayAPI, Version: "v1", Resource: "httproutes"}
	gatewaysResource = schema.GroupVersionResource{Group: GatewayAPI, Version: "v1", Resource: "gateways"}
	poolsResource    = schema.GroupVersionResource{Group: "ate.dev", Version: "v1", Resource: "workerpools"}
)

// KubernetesClient uses the same pool, ownership and publication checks as the CLI.
type KubernetesClient struct {
	Client dynamic.Interface
}

func (k KubernetesClient) Read(ctx context.Context, ref ObjectName) (*Route, error) {
	data, err := k.read(ctx, routesResource, ref)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeRoute(data, ref)
}

func (k KubernetesClient) Publish(ctx context.Context, want Route, before *Route) error {
	ref := want.Metadata.ObjectName
	routes := k.Client.Resource(routesResource).Namespace(ref.Namespace)
	if before == nil {
		data, err := json.Marshal(want)
		if err != nil {
			return err
		}
		var route unstructured.Unstructured
		if err := route.UnmarshalJSON(data); err != nil {
			return err
		}
		_, err = routes.Create(ctx, &route, metav1.CreateOptions{})
		return err
	}
	current, err := k.Read(ctx, ref)
	if err != nil {
		return err
	}
	patch, err := publishRoutePatch(want, before, current)
	if err != nil || len(patch) == 0 {
		return err
	}
	_, err = routes.Patch(ctx, ref.Name, types.JSONPatchType, patch, metav1.PatchOptions{})
	return err
}

func (k KubernetesClient) CheckWorkerPool(ctx context.Context, profile WorkerProfile) error {
	data, err := k.read(ctx, poolsResource, profile.Spec.WorkerPool)
	if err != nil {
		return fmt.Errorf("read WorkerPool: %w", err)
	}
	return checkWorkerPool(data, profile)
}

func (k KubernetesClient) GatewayProgrammed(ctx context.Context, ref GatewayRef) (bool, error) {
	data, err := k.read(ctx, gatewaysResource, ref.ObjectName)
	if err != nil {
		return false, fmt.Errorf("read Gateway: %w", err)
	}
	return gatewayProgrammed(data, ref)
}

func (k KubernetesClient) read(ctx context.Context, resource schema.GroupVersionResource, ref ObjectName) ([]byte, error) {
	value, err := k.Client.Resource(resource).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return value.MarshalJSON()
}
