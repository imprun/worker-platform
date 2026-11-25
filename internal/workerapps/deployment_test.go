package workerapps

import (
	"os"
	"strings"
	"testing"

	"github.com/imprun/worker-platform/internal/platform"
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
	target.ApprovedProfiles = nil
	if _, err := target.Deployment(example.Metadata.Name, profile, custom); err == nil {
		t.Fatal("승인 범위 밖의 프로필을 허용했습니다")
	}
}

// 컨트롤러처럼 클러스터 리소스를 내부 명세 버전으로 읽는다.
func internalVersion(manifest string) string {
	return strings.Replace(manifest, "workerplatform.imprun.dev/v1alpha3", platform.WorkerVersion, 1)
}
