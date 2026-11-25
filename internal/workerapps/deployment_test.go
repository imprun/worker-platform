package workerapps

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/imprun/worker-platform/internal/platform"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
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
			_, err := target.SubmitDevelopment(ctx, client, "org", "project", "app", version, profile, choice)
			return err
		}
		created, err := target.SubmitDevelopment(ctx, client, "org", "project", "app", "", profile, custom)
		if err != nil {
			t.Fatal(err)
		}
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
