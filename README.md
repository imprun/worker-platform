# Worker Platform

Kubernetes Gateway API로 버전이 고정된 OCI 워커를 호출하고 ATE에서 실행합니다.

[![CI](https://github.com/imprun/worker-platform/actions/workflows/ci.yml/badge.svg)](https://github.com/imprun/worker-platform/actions/workflows/ci.yml)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

**현재 `main`**의 `workerctl`은 `HTTPWorker` v1alpha2와 `WorkerProfile`로 ATE에 직접 배포합니다. Worker 배정과 Actor 준비를 확인한 뒤 동기 `POST` HTTPRoute를 게시합니다. [ATE 직접 배포 가이드](docs/ate-deployment.md)

이미지 빌드, ARM64 gVisor 풀, Gateway mTLS 설정, 실제 호출을 한 흐름으로 따라 할 수 있습니다. [Echo 전체 예제](examples/echo/)

| 문서 | 내용 |
| --- | --- |
| [아키텍처](docs/architecture.md) | Gateway 호출 경로, ATE 정책, 리비전 배포와 롤백 |
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

예제 가이드대로 전용 개발 리소스와 인증서를 준비한 뒤 스크립트를 실행합니다. 스크립트는 이미지를 digest로 고정하고 직접 배포한 다음 Gateway에서 HTTP 200과 Echo JSON을 확인합니다. [개발계 준비와 정리](examples/echo/README.md)

```sh
ECHO_REPOSITORY=registry.example.test/workers/echo sh examples/echo/run.sh
```

배포에는 `workerplatform.imprun.dev/v1alpha2`의 `HTTPWorker`와 `WorkerProfile`을 사용합니다. [명세 전환](docs/ate-deployment.md#명세-전환) · [기여와 테스트](CONTRIBUTING.md) · [보안 문의](SECURITY.md)
