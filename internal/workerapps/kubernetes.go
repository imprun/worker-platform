package workerapps

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"

	"github.com/imprun/worker-platform/internal/platform"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var workerResource = schema.GroupVersionResource{
	Group: "workerplatform.imprun.dev", Version: "v1alpha3", Resource: "httpworkers",
}

const appOwnerAnnotation = "workerplatform.imprun.dev/worker-app-owner"

var ErrDeploymentChanged = errors.New("배포 상태가 변경되었습니다. 최신 배포를 조회한 뒤 다시 시도하세요")

// SubmitDevelopment는 권한 확인을 마친 프로젝트의 등록 앱을 개발 환경에 제출한다.
// expectedVersion은 업데이트 전 조회한 HTTPWorker의 resourceVersion이다. 최초 배포에는 빈 값을 사용한다.
func (target DevelopmentTarget) SubmitDevelopment(
	ctx context.Context, client dynamic.Interface, orgID, projectID, appID, expectedVersion string,
	profile platform.WorkerProfile, choice PackageChoice,
) (*unstructured.Unstructured, error) {
	name, owner, err := deploymentIdentity(orgID, projectID, appID)
	if err != nil {
		return nil, err
	}
	worker, err := target.Deployment(name, profile, choice)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	spec, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&worker.Spec)
	if err != nil {
		return nil, err
	}
	workers := client.Resource(workerResource).Namespace(worker.Metadata.Namespace)
	current, err := workers.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) && expectedVersion == "" {
		desired := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": workerResource.GroupVersion().String(),
			"kind":       worker.Kind,
			"spec":       spec,
		}}
		desired.SetName(name)
		desired.SetNamespace(worker.Metadata.Namespace)
		desired.SetAnnotations(map[string]string{appOwnerAnnotation: owner})
		created, err := workers.Create(ctx, desired, metav1.CreateOptions{})
		if !apierrors.IsAlreadyExists(err) {
			return created, err
		}
		current, err = workers.Get(ctx, name, metav1.GetOptions{})
	}
	if apierrors.IsNotFound(err) {
		return nil, ErrDeploymentChanged
	}
	if err != nil {
		return nil, err
	}
	if !ownsDeployment(current, owner) {
		return nil, ErrDeploymentChanged
	}
	before, _, err := unstructured.NestedMap(current.Object, "spec")
	if err != nil {
		return nil, err
	}
	if reflect.DeepEqual(before, spec) {
		return current, nil
	}
	if expectedVersion == "" || current.GetResourceVersion() != expectedVersion {
		return nil, ErrDeploymentChanged
	}
	current.Object["spec"] = spec
	updated, err := workers.Update(ctx, current, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
		return nil, ErrDeploymentChanged
	}
	return updated, err
}

func deploymentIdentity(orgID, projectID, appID string) (name, owner string, err error) {
	if orgID == "" || projectID == "" || appID == "" {
		return "", "", errors.New("등록된 조직, 프로젝트와 앱이 필요합니다")
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%q/%q/%q", orgID, projectID, appID))
	return fmt.Sprintf("app-%x", sum[:18]), fmt.Sprintf("%x", sum), nil
}

func ownsDeployment(worker *unstructured.Unstructured, owner string) bool {
	return worker.GetDeletionTimestamp() == nil && worker.GetAnnotations()[appOwnerAnnotation] == owner
}
