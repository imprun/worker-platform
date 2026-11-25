package workerapps

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/imprun/worker-platform/internal/platform"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	kubetesting "k8s.io/client-go/testing"
)

func TestDevelopmentDeployment(t *testing.T) {
	const repository = "packages.example.test/workers/echo"
	digest := "sha256:" + strings.Repeat("2", 64)
	fixture, err := os.ReadFile("../../examples/echo/packaged.yaml")
	if err != nil {
		t.Fatal(err)
	}
	profileYAML, workerYAML, ok := strings.Cut(string(fixture), "\n---\n")
	if !ok {
		t.Fatal("프로필과 워커 배포 명세가 필요합니다")
	}
	profile, err := platform.ReadWorkerProfile(strings.NewReader(internalVersion(profileYAML)))
	if err != nil {
		t.Fatal(err)
	}
	example, err := platform.ReadWorker(strings.NewReader(internalVersion(workerYAML)))
	if err != nil {
		t.Fatal(err)
	}
	target := DevelopmentTarget{
		Namespace: profile.Metadata.Namespace, ApprovedProfiles: []string{profile.Metadata.Name},
		EchoPackage: repository + "@" + digest, Trigger: example.Spec.Trigger,
	}
	custom := PackageChoice{Repository: repository, Digest: digest}
	echo, err := target.Deployment(example.Metadata.Name, profile, PackageChoice{Sample: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := target.Deployment(example.Metadata.Name, profile, custom)
	if err != nil {
		t.Fatal(err)
	}
	if echo != worker || worker.Spec.AppPackage != target.EchoPackage || worker.Spec.Trigger.Path != "/invoke" {
		t.Fatalf("동일한 digest의 배포 명세가 다릅니다: %+v / %+v", echo, worker)
	}

	for _, choice := range []PackageChoice{
		{Repository: "other.example.test/workers/echo", Digest: digest},
		{Repository: repository, Digest: "latest"},
		{Repository: repository, Digest: digest, Sample: "echo"},
	} {
		if _, err := target.Deployment(example.Metadata.Name, profile, choice); err == nil {
			t.Fatalf("허용할 수 없는 패키지를 승인했습니다: %+v", choice)
		}
	}
	foreign := profile
	foreign.Metadata.Namespace = "other-project"
	if _, err := target.Deployment(example.Metadata.Name, foreign, custom); err == nil {
		t.Fatal("다른 프로젝트의 프로필을 승인했습니다")
	}
	t.Run("HTTPWorker 재시도와 업데이트", func(t *testing.T) {
		ctx := t.Context()
		client := fake.NewSimpleDynamicClient(runtime.NewScheme())
		submit := func(version string, choice PackageChoice) error {
			_, err := target.SubmitDevelopment(ctx, client, "org", "project", "app", version, profile, choice, false)
			return err
		}
		created, err := target.SubmitDevelopment(ctx, client, "org", "project", "app", "", profile, custom, false)
		if err != nil {
			t.Fatal(err)
		}
		firstRead := true
		client.PrependReactor("get", workerResource.Resource, func(kubetesting.Action) (bool, runtime.Object, error) {
			if firstRead {
				firstRead = false
				return true, nil, apierrors.NewNotFound(workerResource.GroupResource(), created.GetName())
			}
			return false, nil, nil
		})
		if err := submit("", PackageChoice{Sample: "echo"}); err != nil {
			t.Fatal(err)
		}
		replacement := PackageChoice{Repository: repository, Digest: "sha256:" + strings.Repeat("3", 64)}
		if err := submit("", replacement); !errors.Is(err, ErrDeploymentChanged) {
			t.Fatalf("배포 버전 누락 시 충돌을 예상했습니다: %v", err)
		}
		created.SetResourceVersion("42")
		workers := client.Resource(workerResource).Namespace(target.Namespace)
		if _, err := workers.Update(ctx, created, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := submit("41", replacement); !errors.Is(err, ErrDeploymentChanged) {
			t.Fatalf("오래된 배포 버전의 충돌을 예상했습니다: %v", err)
		}
		if err := submit("42", replacement); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			retried, err := target.SubmitDevelopment(ctx, client, "org", "project", "app", "42", profile, replacement, true)
			if err != nil || retried.GetAnnotations()[platform.DeploymentRetryAnnotation] != "42" {
				t.Fatalf("동일 리비전 재시도 표시가 저장돼야 합니다: %v", err)
			}
		}

		if _, err := target.ReadDevelopment(ctx, client, "org", "another-project", "app"); !errors.Is(err, ErrDeploymentNotFound) {
			t.Fatalf("다른 프로젝트의 배포 조회를 거부해야 합니다: %v", err)
		}
		current, err := workers.Get(ctx, created.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		current.SetGeneration(2)
		current.Object["status"] = map[string]any{
			"observedGeneration": int64(2), "observedAppPackage": repository + "@" + replacement.Digest,
			"active": map[string]any{"generation": int64(1), "revision": "v1", "appPackage": repository + "@" + digest},
			"conditions": []any{map[string]any{
				"type": "Ready", "status": "False", "observedGeneration": int64(2),
				"reason": "DeploymentPending", "lastTransitionTime": "2026-11-25T00:00:00Z",
			}},
		}
		if _, err := workers.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		got, err := target.ReadDevelopment(ctx, client, "org", "project", "app")
		if err != nil {
			t.Fatal(err)
		}
		if got.AppID != "app" || got.ProjectID != "project" || got.Environment != "development" ||
			got.Operation != (HTTPOperation{Method: "POST", Path: "/invoke"}) ||
			got.Active == nil || got.Active.AppPackage != repository+"@"+digest ||
			got.LatestAttempt.State != "failed" || got.LatestAttempt.AppPackage != repository+"@"+replacement.Digest {
			t.Fatalf("활성 배포와 실패한 시도의 응답을 확인하세요: %+v", got)
		}
		unstructured.RemoveNestedField(current.Object, "status")
		if _, err := workers.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		got, err = target.ReadDevelopment(ctx, client, "org", "project", "app")
		if err != nil || got.Active != nil || got.LatestAttempt.State != "queued" || got.LatestAttempt.Generation != 2 {
			t.Fatalf("첫 상태 기록 전 조회 결과가 다릅니다: %+v, %v", got, err)
		}

	})
	target.ApprovedProfiles = nil
	if _, err := target.Deployment(example.Metadata.Name, profile, custom); err == nil {
		t.Fatal("승인 범위 밖의 프로필을 허용했습니다")
	}
}

// 컨트롤러처럼 클러스터 리소스를 내부 명세 버전으로 읽는다.
func internalVersion(manifest string) string {
	return strings.Replace(manifest, "workerplatform.imprun.dev/v1alpha3", platform.WorkerVersion, 1)
}
