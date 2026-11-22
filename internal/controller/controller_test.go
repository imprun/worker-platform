package controller

import (
	"errors"
	"os"
	"testing"

	"github.com/imprun/worker-platform/internal/platform"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic/fake"
)

func TestAppliedResourcesAndObservedGeneration(t *testing.T) {
	worker := example(t, "httpworker.yaml")
	worker.SetUID("worker-uid")
	worker.SetGeneration(1)
	profile := example(t, "workerprofile.yaml")
	profile.SetUID("profile-uid")
	profile.SetGeneration(1)
	w, err := readWorker(worker)
	if err != nil {
		t.Fatal(err)
	}
	p, err := readProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := platform.BuildATEPlan(w, p); err != nil {
		t.Fatal(err)
	}

	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), worker, profile)
	c := Controller{workers: client.Resource(workersGVR).Namespace(worker.GetNamespace()), profiles: client.Resource(profilesGVR).Namespace(worker.GetNamespace())}
	published := &platform.ATEDeploymentResult{Actor: "echo-v1", ActorUID: "first-actor"}
	if err := c.record(t.Context(), worker, profile, "Published", "ready", published); err != nil {
		t.Fatal(err)
	}
	currentProfile, err := c.profiles.Get(t.Context(), profile.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	currentProfile.SetGeneration(2)
	if currentProfile, err = c.profiles.Update(t.Context(), currentProfile, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := c.checkInputs(t.Context(), worker, profile); !errors.Is(err, errSuperseded) {
		t.Fatalf("expected the profile change to supersede this rollout, got %v", err)
	}
	if err := c.record(t.Context(), worker, currentProfile, "Preparing", "waiting", nil); err != nil {
		t.Fatal(err)
	}
	current, err := c.workers.Get(t.Context(), worker.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	status, _, err := unstructured.NestedMap(current.Object, "status")
	if err != nil {
		t.Fatal(err)
	}
	var got workerStatus
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(status, &got); err != nil {
		t.Fatal(err)
	}
	if got.Active == nil || got.Active.ActorUID != published.ActorUID || got.Active.Profile.Generation != 1 ||
		got.ObservedGeneration != 1 || got.ObservedProfile == nil || got.ObservedProfile.Generation != 2 ||
		len(got.Conditions) != 1 || got.Conditions[0].Status != metav1.ConditionFalse {
		t.Fatalf("profile rollout lost the active revision or advertised readiness too early: %+v", got)
	}

	current.SetGeneration(2)
	if _, err := c.workers.Update(t.Context(), current, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := c.record(t.Context(), worker, currentProfile, "Published", "ready", published); !errors.Is(err, errSuperseded) {
		t.Fatalf("old worker generation overwrote status: %v", err)
	}
}

func example(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	data, err := os.ReadFile("../../examples/echo/" + name)
	if err != nil {
		t.Fatal(err)
	}
	data, err = yaml.ToJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	var obj unstructured.Unstructured
	if err := obj.UnmarshalJSON(data); err != nil {
		t.Fatal(err)
	}
	return &obj
}
