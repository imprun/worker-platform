package controller

import (
	"context"
	"fmt"
	"reflect"

	"github.com/imprun/worker-platform/internal/platform"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

type profileRevision struct {
	Name       string    `json:"name"`
	UID        types.UID `json:"uid"`
	Generation int64     `json:"generation"`
}

type publishedRevision struct {
	Generation int64           `json:"generation"`
	Profile    profileRevision `json:"profile"`
	Revision   string          `json:"revision"`
	AppPackage string          `json:"appPackage,omitempty"`
	Atespace   string          `json:"atespace"`
	Actor      string          `json:"actor"`
	ActorUID   string          `json:"actorUID"`
	Worker     string          `json:"worker"`
	WorkerPod  string          `json:"workerPod"`
	Route      string          `json:"route"`
}

type workerStatus struct {
	ObservedGeneration int64              `json:"observedGeneration"`
	ObservedAppPackage string             `json:"observedAppPackage,omitempty"`
	ObservedProfile    *profileRevision   `json:"observedProfile,omitempty"`
	Active             *publishedRevision `json:"active,omitempty"`
	Conditions         []metav1.Condition `json:"conditions"`
}

func readWorker(obj *unstructured.Unstructured) (platform.Worker, error) {
	worker := platform.Worker{
		APIVersion: platform.WorkerVersion, Kind: "HTTPWorker",
		Metadata: platform.ObjectName{Name: obj.GetName(), Namespace: obj.GetNamespace()},
	}
	if err := decodeSpec(obj, &worker.Spec); err != nil {
		return platform.Worker{}, err
	}
	return worker, worker.Validate()
}

func readProfile(obj *unstructured.Unstructured) (platform.WorkerProfile, error) {
	profile := platform.WorkerProfile{
		APIVersion: platform.WorkerVersion, Kind: "WorkerProfile",
		Metadata: platform.ObjectName{Name: obj.GetName(), Namespace: obj.GetNamespace()},
	}
	if err := decodeSpec(obj, &profile.Spec); err != nil {
		return platform.WorkerProfile{}, err
	}
	return profile, profile.Validate()
}

func decodeSpec(obj *unstructured.Unstructured, target any) error {
	spec, found, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil || !found {
		return fmt.Errorf("%s %s requires an object spec", obj.GetKind(), obj.GetName())
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(spec, target); err != nil {
		return fmt.Errorf("decode %s %s spec: %w", obj.GetKind(), obj.GetName(), err)
	}
	return nil
}

func (c Controller) record(ctx context.Context, used, profile *unstructured.Unstructured, reason, message string, result *platform.ATEDeploymentResult) error {
	current, err := c.workers.Get(ctx, used.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return errSuperseded
	}
	if err != nil {
		return err
	}
	if current.GetDeletionTimestamp() != nil || !sameRevision(current, used) {
		return errSuperseded
	}
	before, _, err := unstructured.NestedMap(current.Object, "status")
	if err != nil {
		return err
	}
	var status workerStatus
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(before, &status); err != nil {
		return err
	}
	var observed *profileRevision
	if profile != nil {
		observed = &profileRevision{Name: profile.GetName(), UID: profile.GetUID(), Generation: profile.GetGeneration()}
	}
	appPackage, _, err := unstructured.NestedString(used.Object, "spec", "appPackage")
	if err != nil && reason != "InvalidWorker" {
		return err
	}
	status.ObservedAppPackage = appPackage
	status.ObservedGeneration = used.GetGeneration()
	status.ObservedProfile = observed
	condition := metav1.Condition{
		Type: "Ready", Status: metav1.ConditionFalse, ObservedGeneration: status.ObservedGeneration, Reason: reason, Message: message,
	}
	if result != nil {
		condition.Status = metav1.ConditionTrue
		status.Active = &publishedRevision{
			Generation: status.ObservedGeneration, Profile: *observed, Revision: result.Revision, AppPackage: appPackage,
			Atespace: result.Atespace, Actor: result.Actor, ActorUID: result.ActorUID,
			Worker: result.Worker, WorkerPod: result.WorkerPod, Route: result.Route,
		}
	}
	meta.SetStatusCondition(&status.Conditions, condition)
	after, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
	if err != nil || reflect.DeepEqual(before, after) {
		return err
	}
	current.Object["status"] = after
	_, err = c.workers.UpdateStatus(ctx, current, metav1.UpdateOptions{})
	return err
}
