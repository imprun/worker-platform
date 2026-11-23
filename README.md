# Worker Platform

Kubernetes Gateway API로 워커를 호출하고 ATE에서 실행합니다. 공통 Actor 런타임 이미지와 OCI 앱 패키지를 독립적으로 버전 관리합니다. [앱 배포 구조](docs/architecture.md#공통-런타임의-앱-로드)

[![CI](https://github.com/imprun/worker-platform/actions/workflows/ci.yml/badge.svg)](https://github.com/imprun/worker-platform/actions/workflows/ci.yml)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

`worker-controller`는 v1alpha3 `HTTPWorker.spec.appPackage`와 `WorkerProfile.spec.runtimeImage`를 하나의 리비전으로 묶어 ATE에 배포합니다. Worker 배정과 Actor의 `/readyz`를 확인한 뒤 동기 `POST` HTTPRoute를 게시합니다. 독립 이미지 워커에는 `HTTPWorker.spec.image`를 사용합니다. [컨트롤러 설치·업데이트·롤백](docs/ate-controller.md)

`spec.jobEnabled`를 켠 앱에는 PostgreSQL 기반 단일 Job을 접수할 수 있습니다. 재시도는 처음 접수한 리비전을 사용하고 결과는 Run ID로 조회합니다. [Job 접수·재시도 예제](docs/job-api.md)

Echo는 공통 런타임 위에서 JSON을 처리하는 앱 패키지 예제입니다. ARM64 gVisor 풀에서 런타임을 고정하고 앱 패키지만 업데이트하거나 롤백할 수 있습니다. [Echo 앱 패키지 예제](examples/echo/README.md#공통-런타임으로-echo-패키지-배포)

| 문서 | 내용 |
| --- | --- |
| [아키텍처](docs/architecture.md) | 공통 런타임과 앱 패키지, Gateway 호출, 리비전 배포와 롤백 |
| [요구사항](docs/requirements.md) | 운영 범위 및 기능별 인수 기준 |
| [단일 Job 실행 상세 설계](docs/job-execution-design.md) | API, 시도와 슬롯, 재시도, 사이트 세션과 운영 화면 |
| [설계 검토와 검증 근거](docs/design-review.md) | 검토 결과, kagent·Testkube 소스, 출시 시험 |
| [ATE 직접 배포](docs/ate-deployment.md) | 전용 풀과 Gateway 설정, CLI 배포, 실제 호출 검증 |

<a name="try-it-locally"></a>

## 로컬에서 Echo 실행

`go.mod`에 지정한 버전 이상의 Go를 설치하고 이 저장소를 내려받습니다. 첫 터미널에서 Echo를 실행합니다.

```sh
go run ./cmd/echo -listen 127.0.0.1:8080
```

다른 터미널에서 Echo를 호출하고 배포 객체를 출력합니다.

```sh
curl -fsS -H 'Content-Type: application/json' \
  -d '{"message":"hello"}' http://127.0.0.1:8080/invoke
go run ./cmd/workerctl render \
  -f examples/echo/ate-worker.yaml -profile examples/echo/ate-profile.yaml -output route
```

Echo는 `{"message":"hello"}`를 반환합니다. `render`는 생성할 Kubernetes HTTPRoute를 로컬에서 출력합니다.

<a name="deploy-the-example"></a>

## 개발 클러스터에 Echo 배포

컨트롤러와 전용 개발계를 준비한 뒤 Echo 앱 패키지를 빌드하고 `HTTPWorker`로 배포합니다. [앱 빌드·배포·롤백](examples/echo/README.md#공통-런타임으로-echo-패키지-배포)

독립 이미지로 ATE 호출 경로를 점검할 때는 아래 스크립트를 사용합니다. [개발계 준비와 정리](examples/echo/README.md#개발계-준비)

```sh
ECHO_REPOSITORY=registry.example.test/workers/echo sh examples/echo/run.sh
```

로컬에서 `workerctl`로 렌더링하거나 직접 배포할 때는 `workerplatform.imprun.dev/v1alpha2` 파일을 사용합니다. [CLI 배포 가이드](docs/ate-deployment.md) · [기여와 테스트](CONTRIBUTING.md) · [보안 문의](SECURITY.md)
