# Worker Platform

Kubernetes Gateway API로 버전이 고정된 OCI 워커를 호출하고 ATE에서 실행합니다.

[![CI](https://github.com/imprun/worker-platform/actions/workflows/ci.yml/badge.svg)](https://github.com/imprun/worker-platform/actions/workflows/ci.yml)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

**현재 공개 알파**의 `workerctl`은 AX Task를 만들고 Actor가 Ready가 되면 워커당 하나의 동기 `POST` HTTPRoute를 게시합니다. 같은 URL에서 리비전을 바꾸고, 보존한 AX Task로 롤백할 수 있습니다.

**첫 운영 릴리스의 설계**에는 ATE 직접 배포, 실행 ID 기반의 단일 Job, 장애 재시도와 HITL 재인증이 포함됩니다. 아래 실행 명령은 공개 알파에 해당합니다.

| 문서 | 내용 |
| --- | --- |
| [아키텍처](docs/architecture.md) | Gateway 호출 경로, ATE 정책, 리비전 배포와 롤백 |
| [요구사항](docs/requirements.md) | 운영 범위 및 기능별 인수 기준 |
| [단일 Job 실행 상세 설계](docs/job-execution-design.md) | API, 시도와 슬롯, 재시도, 사이트 세션과 운영 화면 |
| [설계 검토와 검증 근거](docs/design-review.md) | 검토 결과, kagent·Testkube 소스, 출시 시험 |
| [공개 알파 클러스터 설정](docs/cluster-setup.md) | AX와 Gateway API로 Echo를 배포하고 호출하는 방법 |

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
go run ./cmd/workerctl render -f examples/echo/worker.yaml
```

Echo는 `{"message":"hello"}`를 반환합니다. `render`는 AX Task와 Kubernetes HTTPRoute를 로컬에서 출력합니다. `-output route` 또는 `-output task`를 지정하면 해당 JSON만 출력합니다.

공개 알파판 CLI는 `go install github.com/imprun/worker-platform/cmd/workerctl@v0.1.0-alpha.1`로 설치할 수 있습니다.

<a name="deploy-the-example"></a>

## 개발 클러스터에 알파 배포

1. AX·ATE와 테스트 WorkerPool을 준비하고 Gateway의 Router 연결 신원과 TLS 정책을 설정합니다. [알파 클러스터 설정](docs/cluster-setup.md)
2. 풀의 CPU 아키텍처에 맞춰 [Echo 이미지](Dockerfile.echo)를 빌드한 뒤 레지스트리에 올립니다. 아래는 ARM64 예입니다. [워커 명세](examples/echo/worker.yaml)의 이미지·Atespace·Gateway·호스트를 시험 환경 값으로 바꿉니다.

   ```sh
   docker buildx build --platform linux/arm64 -f Dockerfile.echo -t "$IMAGE_TAG" --push .
   ```

   이미지는 레지스트리가 반환한 `repository@sha256:digest`로 고정합니다.
3. 첫 터미널에서 AX를 로컬로 포트 포워딩한 뒤 두 번째 터미널에서 배포합니다. `KUBE_CONTEXT`와 `AX_NAMESPACE`는 시험 환경의 값입니다.

   ```sh
   kubectl --context "$KUBE_CONTEXT" -n "$AX_NAMESPACE" port-forward svc/ax-server 18082:8080
   ```

   ```sh
   go run ./cmd/workerctl deploy -f examples/echo/worker.yaml \
     -ax-address 127.0.0.1:18082 -kube-context "$KUBE_CONTEXT"
   kubectl --context "$KUBE_CONTEXT" -n workers get httproute echo -o yaml
   ```

4. 대상 HTTPRoute의 현재 세대에서 `Accepted=True`와 `ResolvedRefs=True`를 확인합니다. 예제 HTTP 리스너는 다음 요청을 받습니다.

   ```sh
   curl -fsS -H 'Host: echo.example.test' -H 'Content-Type: application/json' \
     -d '{"message":"hello"}' "http://$GATEWAY_ADDRESS/invoke"
   ```

이전 AX Task가 남아 있으면 이전 명세로 다시 배포해 롤백합니다. [기여와 테스트](CONTRIBUTING.md) · [보안 문의](SECURITY.md)
