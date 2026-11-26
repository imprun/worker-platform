# 개발 Gateway 실환경 검증

검증 범위는 [워커 앱 제품 목표 #1](https://github.com/imprun/worker-platform/issues/1)과 [실환경 검증 #4](https://github.com/imprun/worker-platform/issues/4)이다. 실행마다 워커 저장소와 API 저장소의 전체 SHA를 함께 남긴다.

## 현재 검증 근거

- 개발 Kubernetes API `v1.36.6`에 지정 TCP CONNECT 프록시를 통해 접속했다. kubeconfig CA와 서버 이름 검증을 유지했다. `ate-system`과 `HTTPRoute` 정의는 조회됐고, `wp-echo-example`, `HTTPWorker`, `WorkerProfile`은 `404`를 반환했다. 클러스터는 변경하지 않았다.
- [Echo PR #5](https://github.com/imprun/worker-platform/pull/5)에서 로컬 `POST /invoke`의 HTTP 200, 원본과 같은 JSON 값, Actor 로컬 `requests.jsonl` 기록을 확인했다. PR의 JSON Echo 패키지가 개발계에 올라가야 동일 계약을 실환경에서 재검증할 수 있다.
- 격리된 PostgreSQL을 사용한 로컬 HTTP 테스트에서 동시 등록·배포, 타 프로젝트 조회 거부, 승인 프로필 제한을 확인했다. Kubernetes 클라이언트는 테스트 대역이었다.
- 두 저장소를 함께 배포한 GitHub Actions 실행, 인증된 개발 Gateway 호출과 사용자 관찰 결과는 아직 없다.

## 자동 실행에서 남길 항목

1. 같은 프로젝트의 게시 OCI 패키지를 v1으로 배포한다. OpenAPI 문서의 외부 `POST /verify`를 워커 앱의 `POST /invoke`에 연결해 가져온다. 저장된 명세와 워커 앱 영향 목록을 조회한 뒤 개발 Gateway에 배포하고 영향 목록에 배포가 표시되는지 확인한다.
2. 허가된 호출자에게 발급한 테스트 자격으로 외부 URL에 JSON을 보내 HTTP 상태와 응답 값을 확인한다. 워커 패키지가 v1임은 런타임이 검증한 `X-Imprun-Worker-Package-Digest` 응답 헤더로 확인한다. 연결된 워커 앱 삭제가 HTTP 409로 거부된 뒤에도 인증된 v1 호출이 성공해야 한다.
3. 시작 준비에서 실패하는 새 digest를 제출한다. 최근 시도 실패와 활성 v1을 조회하고 같은 URL에서 실제 v1 응답을 확인한다. 준비가 늦은 정상 v2를 제출해 준비 중 v1 응답과 Ready 후 v2 응답을 각각 확인한다.
4. 동일한 URL과 API 배포를 유지하면서 v1 digest를 다시 제출한다. HTTP 200, Echo JSON과 실제 v1 digest를 대조한다.
5. 실행 URL, 두 SHA, 사용한 워커 패키지 digest, 요청과 응답, 상태별 관찰 패키지, 소유 리소스 정리 결과를 기록한다. 대기·스킵·실패는 결과에 구분하여 남긴다. 토큰과 인증 헤더, kubeconfig는 기록에서 제외한다.

기존 `ate-integration.yml`은 워커 `main`에서 변경 감지와 `ATE_INTEGRATION_ENABLED=true`를 만족할 때 `[self-hosted, linux, ate-integration]` 러너에서 ATE 직접 배포 스모크를 실행한다. 이 스모크는 별도 API Platform 배포 및 인증된 Gateway 왕복의 증거가 아니다.

## 사용자 과제

실환경 자동 검증이 통과한 뒤 워커 앱을 처음 사용하는 앱 개발자 한 명과 API 담당자 한 명에게 각자의 테스트 프로젝트를 제공한다. 처음에는 목표만 알려주고, 도움이 필요하면 요청한 시점과 안내한 내용을 기록한다.

| 참여자 | 과제 | 관찰 항목 |
| --- | --- | --- |
| 앱 개발자 | 자기 OCI 패키지의 주소와 digest로 워커 앱을 등록해 개발 환경에 배포하고, API 초안을 만들어 인증된 개발 Gateway에서 첫 JSON 응답을 받는다. | 등록부터 첫 응답까지 시간, 승인 프로필 선택, 앱 테스트와 Gateway 테스트 구분, 응답 리비전 식별 여부 |
| API 담당자 | 다중 연산 OpenAPI 초안을 작성하다 워커 앱을 배포하고 원래 초안으로 돌아와 한 POST 연산을 연결한 다음 개발 Gateway를 호출한다. | 복귀 전후 입력과 파일 유지, 외부 경로 선택, 배포와 게시 구분, 첫 응답까지 시간, 활성 리비전 식별 여부 |

참여자별 과제 완료 여부, 소요 시간, 도움을 요청한 단계, 활성 버전 식별 결과를 #4에 남긴다. 자동 실행 결과와 사용자 관찰은 별도로 기록한다.

## GitHub Actions 실행

워커 제품 검증은 기존 `ATE integration`의 별도 작업이다. 워커 `main`에서 기존 `ATE_INTEGRATION_ENABLED=true` 조건을 사용한다. 워커 변경으로 자동 실행할 때는 저장소 변수 `ATE_PRODUCT_API_SHA`에 함께 검증할 API Platform의 전체 SHA를 설정한다. API Platform만 변경된 경우에는 워커 `main`에서 수동 실행하고 `base`에는 현재 워커 `main` SHA, `api_sha`에는 새 API Platform 전체 SHA를 입력한다. 워커 변경 감지가 비어도 제품 작업을 실행한다. 러너 공유 잠금으로 기존 ATE 스모크와 동시에 실행하지 않는다.

`api_sha`를 지정한 수동 실행에서는 호스팅 러너가 먼저 `main`, `ATE_INTEGRATION_ENABLED=true`와 두 저장소 SHA의 형식을 검사한다. 조건이 빠지면 대기열에 실환경 작업을 보내기 전에 해당 조건을 표시하고 실패 처리한다.

Actions는 기존 self-hosted ATE 러너에서 이미 준비된 테스트 서비스를 검증한다. 검토한 두 커밋을 빌드한 뒤 전용 `wp-product-verification` 네임스페이스에 배포한다. Run API, 워커 컨트롤러, API Platform, 개발 Gateway Deployment에는 `spec.template.metadata.annotations.imprun.dev/source-sha`와 이미지 digest를 고정한다. API Gateway 등록의 ID·버전 및 HTTPS 엔드포인트도 테스트 구성과 일치해야 한다. 일반 워커 Echo 스모크의 `wp-echo-example` 리소스는 제품 실행에서 사용하지 않는다.

러너에 `KUBE_CONTEXT`, `ATE_TOKEN_FILE`, 그리고 아래 JSON 파일의 경로인 `ATE_PRODUCT_FIXTURE_FILE`을 제공한다. 파일 자체에는 토큰을 담지 않는다.

| 키 | 값 |
| --- | --- |
| `cluster_uid`, `namespace_uid`, `pool_uid`, `atespace_uid` | 격리 테스트 클러스터, 네임스페이스, WorkerPool, Atespace의 확인된 UID |
| `worker_workload`, `controller_workload`, `api_workload`, `gateway_workload` | 각각 Run API, 워커 컨트롤러, API Platform, 개발 Gateway Deployment 이름 |
| `worker_url`, `api_url`, `gateway_url` | 각 서비스의 검증된 HTTPS origin |
| `profile`, `pool`, `runtime_image`, `gateway`, `gateway_version` | 전용 ARM64 gVisor 프로필 이름, 풀 이름, 런타임 이미지 digest 참조, 개발 Gateway 핸들과 버전 |
| `project`, `organization_id`, `project_id` | 신규 테스트 전용 프로젝트 핸들과 인증된 조직·프로젝트의 고정 ID |
| `management_token_file`, `gateway_credential_file`, 선택 `gateway_header` | 프로젝트 관리 Bearer 토큰 파일, 테스트 Gateway 호출 자격 파일, 기본 `Authorization` 헤더 |
| `pg_service_file`, `pg_service`, `pg_database` | 워커 앱 테스트 DB로만 연결하는 PostgreSQL 서비스 파일·이름·데이터베이스명 |
| `package_v1`, `package_failure`, `package_v2` | 사용자 방식으로 게시한 서로 다른 고정 OCI 참조. v1·v2는 동일 JSON을 반환하고 실패 후보는 시작 확인에서 실패해야 한다. 정상 v2는 준비 중 v1 호출을 확인할 만큼 시작 확인을 지연한다. |

실행 전 검사는 기존 Actor, ActorTemplate, HTTPWorker, HTTPRoute와 워커 앱 등록이 없는 전용 공간을 요구한다. Actor 런타임과 패키지는 JSON Echo PR #5와 응답 digest 헤더 변경을 포함해 빌드한다. 개발 Gateway는 이 테스트 경로에 인증을 요구해야 한다. 제품 작업의 검증 요약에는 실제 Gateway HTTP 결과와 두 SHA가 포함된다. 시작 전 오류나 정리 실패도 작업을 실패 처리한다. 정리할 때 이 실행에서 만든 Gateway 배포를 해제한다. 제어 서버의 해제 승인과 테스트 URL의 404 응답을 확인한 뒤 테스트 API와 워커 앱을 삭제한다. 해제에 실패하면 해당 앱을 유지하며, 실패 후 재실행 전에 남은 테스트 소유 리소스를 확인한다.

검토된 소스에 JSON Echo PR #5의 코드가 포함되면 `PACKAGE_REPOSITORY`와 실행별 고유 `PACKAGE_TAG_PREFIX`를 설정하고 `sh examples/echo/build-verification-packages.sh`로 세 ARM64 앱 패키지를 게시할 수 있다. 게시한 레지스트리가 Actor 주소와 다를 때만 `PACKAGE_PUSH_REPOSITORY`를 설정한다. 스크립트는 레지스트리가 반환한 digest를 OCI 패키지의 digest와 대조하고 러너 설정에 넣을 세 참조를 출력한다. 실패 후보는 시작 확인을 실패시키고 정상 v2는 기본 8초 동안 시작 확인을 지연한다.

## 격리된 로컬 Kubernetes API 검증

임시 Kubernetes API 1.35.0과 PostgreSQL 17을 시작하고 저장소의 `HTTPWorker`·`WorkerProfile` CRD를 적용했다. Run API의 워커 앱 처리기를 루프백 HTTP 서버로 열어 등록·동시 최초 배포, 승인 프로필 제한, 프로젝트 분리, v2 제출 뒤 같은 요청 재시도와 오래된 버전의 다른 요청 거부를 확인했다. 실제 API 서버에서 생성 충돌을 처리할 때 첫 404가 남는 문제를 재현해 수정했다. 새 리소스 세대가 기록되어도 마지막 활성 v1이 조회됐다. 임시 API 서버·DB와 제한 권한의 연결 파일은 테스트 종료 시 정리했다.

이 시험에서 워커 준비 상태는 테스트가 CRD 상태에 기록했고, 프로젝트 권한은 테스트용 검사가 처리했다. 실제 ATE Actor 및 인증된 개발 Gateway 검증은 Actions 실행에서 확인한다.

같은 테스트를 실행하려면 PostgreSQL 테스트 DSN을 `WORKER_APP_TEST_DSN`에 지정한다. 선택적으로 `WORKER_APP_TEST_KUBE_FILE`에 소유한 임시 kube-apiserver의 연결 JSON 파일을 지정한다. JSON은 `Host`, `CAData`, `CertData`, `KeyData` 필드를 받으며 인증서·키 바이트는 Base64로 인코딩한다. TLS가 검증되는 루프백 주소와 설치된 두 CRD가 필요하다. 파일을 지정하지 않으면 Kubernetes 테스트 대역을 사용한다.

```sh
go test -count=1 -run '^TestWorkerAppPostgresHTTP$' ./internal/workerapps
```
