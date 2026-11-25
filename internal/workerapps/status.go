package workerapps

import (
	"context"
	"errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
)

var ErrDeploymentNotFound = errors.New("이 프로젝트의 워커 앱 개발 배포를 찾을 수 없습니다")

type DevelopmentStatus struct {
	AppID           string            `json:"appId"`
	ProjectID       string            `json:"projectId"`
	Environment     string            `json:"environment"`
	Operation       HTTPOperation     `json:"operation"`
	ResourceVersion string            `json:"resourceVersion"`
	Active          *ActiveRevision   `json:"active,omitempty"`
	LatestAttempt   DeploymentAttempt `json:"latestAttempt"`
}

type HTTPOperation struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

type ActiveRevision struct {
	Generation int64  `json:"generation"`
	Revision   string `json:"revision"`
	AppPackage string `json:"appPackage,omitempty"`
}

type DeploymentAttempt struct {
	Generation int64  `json:"generation"`
	AppPackage string `json:"appPackage,omitempty"`
	State      string `json:"state"`
	Reason     string `json:"reason,omitempty"`
}

// ReadDevelopment는 서버가 접근 권한과 앱 등록을 확인한 뒤 호출하는 조회 경로다.
func (target DevelopmentTarget) ReadDevelopment(ctx context.Context, client dynamic.Interface, orgID, projectID, appID string) (DevelopmentStatus, error) {
	name, owner, err := deploymentIdentity(orgID, projectID, appID)
	if err != nil {
		return DevelopmentStatus{}, err
	}
	workers := client.Resource(workerResource).Namespace(target.Namespace)
	worker, err := workers.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) || err == nil && !ownsDeployment(worker, owner) {
		return DevelopmentStatus{}, ErrDeploymentNotFound
	}
	if err != nil {
		return DevelopmentStatus{}, err
	}

	data, _, err := unstructured.NestedMap(worker.Object, "status")
	if err != nil {
		return DevelopmentStatus{}, err
	}
	var status struct {
		ObservedGeneration int64              `json:"observedGeneration"`
		ObservedAppPackage string             `json:"observedAppPackage"`
		Active             *ActiveRevision    `json:"active"`
		Conditions         []metav1.Condition `json:"conditions"`
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(data, &status); err != nil {
		return DevelopmentStatus{}, err
	}

	pkg, _, err := unstructured.NestedString(worker.Object, "spec", "appPackage")
	if err != nil {
		return DevelopmentStatus{}, err
	}
	attempt := DeploymentAttempt{Generation: worker.GetGeneration(), AppPackage: pkg, State: "queued"}
	condition := meta.FindStatusCondition(status.Conditions, "Ready")
	if status.ObservedGeneration == attempt.Generation && condition != nil && condition.ObservedGeneration == attempt.Generation {
		attempt.AppPackage = status.ObservedAppPackage
		if attempt.AppPackage == "" {
			attempt.AppPackage = pkg
		}
		attempt.Reason = condition.Reason
		switch {
		case condition.Status == metav1.ConditionTrue:
			attempt.State = "ready"
		case condition.Status == metav1.ConditionUnknown || condition.Reason == "Preparing":
			attempt.State = "preparing"
		default:
			attempt.State = "failed"
		}
	}
	return DevelopmentStatus{
		AppID: appID, ProjectID: projectID, Environment: "development",
		Operation:       HTTPOperation{Method: "POST", Path: "/invoke"},
		ResourceVersion: worker.GetResourceVersion(), Active: status.Active, LatestAttempt: attempt,
	}, nil
}
