package workerapps

import (
	"errors"
	"slices"

	"github.com/imprun/worker-platform/internal/apppackage"
	"github.com/imprun/worker-platform/internal/platform"
)

type PackageChoice struct {
	Repository string `json:"repository,omitempty"`
	Digest     string `json:"digest,omitempty"`
	Sample     string `json:"sample,omitempty"`
}

// DevelopmentTarget는 서버가 프로젝트 권한을 확인한 뒤 불러오는 배포 설정이다.
type DevelopmentTarget struct {
	Namespace        string
	ApprovedProfiles []string
	EchoPackage      string
	Trigger          platform.HTTPTrigger
}

// Deployment는 등록된 앱의 리소스 이름을 유지하면서 개발 배포 명세를 만든다.
func (target DevelopmentTarget) Deployment(name string, profile platform.WorkerProfile, choice PackageChoice) (platform.Worker, error) {
	if profile.Metadata.Namespace != target.Namespace || !slices.Contains(target.ApprovedProfiles, profile.Metadata.Name) {
		return platform.Worker{}, errors.New("이 프로젝트에서 승인된 워커 프로필이 아닙니다")
	}

	var reference string
	switch {
	case choice.Sample == "" && choice.Repository != "" && choice.Digest != "":
		reference = choice.Repository + "@" + choice.Digest
	case choice.Sample == "echo" && choice.Repository == "" && choice.Digest == "" && target.EchoPackage != "":
		reference = target.EchoPackage
	default:
		return platform.Worker{}, errors.New("게시된 패키지 주소와 digest 또는 제공되는 Echo 샘플을 선택하세요")
	}
	pkg, err := apppackage.ParseReference(reference)
	if err != nil {
		return platform.Worker{}, errors.New("패키지는 SHA-256 digest로 고정된 레지스트리 주소여야 합니다")
	}

	trigger := target.Trigger
	trigger.Path = "/invoke"
	worker := platform.Worker{
		APIVersion: platform.WorkerVersion,
		Kind:       "HTTPWorker",
		Metadata:   platform.ObjectName{Namespace: target.Namespace, Name: name},
		Spec: platform.WorkerSpec{
			Profile:    profile.Metadata.Name,
			AppPackage: pkg.String(),
			Trigger:    trigger,
		},
	}
	// 컨트롤러와 같은 검증을 사용해 런타임과 레지스트리 허용 범위를 확인한다.
	if _, err := platform.BuildATEPlan(worker, profile); err != nil {
		return platform.Worker{}, err
	}
	return worker, nil
}
