# 설계 검토와 검증 근거

검토 기준은 ATE 직접 배포 코드, ATE `v1.0.0`, 그리고 아래에 고정한 kagent·Testkube 소스다. 운영 릴리스는 ATE 직접 배포, Kubernetes Gateway API, 공통 Actor 런타임의 앱 패키지 로드와 하나의 영속 Run으로 관리하는 단일 Job을 기준으로 한다. [아키텍처](architecture.md) · [요구사항](requirements.md) · [상세 설계](job-execution-design.md)

## 발견한 문제와 설계 결정

| 검토에서 확인한 문제 | 문서에 반영한 결정 |
| --- | --- |
| 초기 문서는 Echo 검증에 사용한 독립 이미지 형식을 앱 팀의 기본 배포 계약으로 규정했다. 이 형식에서는 앱을 갱신할 때 HTTP 러너가 담긴 이미지도 다시 게시한다. | 플랫폼은 공통 Actor 런타임 이미지를 제공하고, 앱 팀은 OCI 앱 패키지를 게시한다. 배포와 Run은 두 digest를 함께 고정하며, 앱 버전은 패키지 digest 변경으로 갱신한다. [앱 로드 설계](architecture.md#공통-런타임의-앱-로드) |
| 기존 아키텍처에는 Actor가 Job 접수와 결과 조회를 맡는 안이 있었고, 상세 설계에는 별도 Job API가 있었다. | 동기 호출은 Gateway → ATE Router → Actor, Job 접수·상태·결과는 Gateway → Job API → PostgreSQL로 통일했다. Actor를 교체하거나 중단해도 Run이 남는다. |
| ATE v1.0.0의 재개 요청은 이름으로 Actor를 찾는다. Router의 준비 확인 요청도 재개를 일으킬 수 있다. | 시도마다 고유한 Actor 이름을 부여한다. 생성·재개 의도를 DB에 기록하고, 늦은 생성은 별도 정리 기록에서 추적한다. 재개 가능성이 있었던 시도는 ATE의 배정 반환을 확인할 때까지 슬롯을 유지한다. |
| 서로 다른 복제본이 멱등 키, 빈 슬롯, Run 상태를 다른 순서로 확인하면 중복 접수나 `429` 오응답이 가능하다. | 새 요청은 멱등 영수증을 먼저 선점한다. 한도는 물리 풀과 워커별 행을 같은 순서로 잠가 적용한다. 시작·결과·취소는 시도 세대와 상태를 재확인하고 한 트랜잭션에서 처리한다. |
| Gateway 라우트와 DB의 게시 포인터는 별도로 갱신된다. 라우트 갱신 후 컨트롤러가 종료될 수 있다. | 게시 의도, 이전과 대상 리비전, Route UID·세대를 저장한다. 해당 세대의 Gateway 상태로 게시를 확정하거나 이전 라우트를 복구한다. 게시 중에는 DB의 기존 리비전으로 Job을 접수한다. |
| 대기 건수와 실제 점유 슬롯을 같은 값으로 취급하면 HITL·백오프·Worker 장애를 운영자가 구분하기 어렵다. | 접수 한도는 미완료 Run, 슬롯 한도는 자원을 예약한 Actor, 실제 배정은 ATE를 기준으로 본다. 슬롯 반환을 확인한 뒤 사용자 대기나 종료를 공개한다. |
| 하나의 계정이 여러 Run에서 만료되면 중복 인증과 늦은 콜백이 새 로그인 상태를 덮어쓸 수 있다. | 계정별 갱신 lease와 세션 버전을 둔다. 승인된 현재 요청만 세션을 갱신하며 같은 Run은 한 번만 대기열로 돌아간다. 같은 계정의 다른 대기 Run은 새 세션과 자신의 권한을 다시 확인한다. |

## 현재 구현에서 확인한 범위

| 확인 대상 | 관찰 결과 |
| --- | --- |
| 배포 코드 | v1alpha2 명세 검증, Actor 대상 헤더 설정, 후보 준비 실패 시 기존 라우트 보존, 롤백의 Actor 재사용, 라우트 UID·세대·소유권 사전조건을 Go 테스트에서 확인했다. [배포 테스트](../internal/platform/ate_deploy_test.go) · [라우트와 명세 테스트](../internal/platform/platform_test.go) |
| ATE 직접 배포의 첫 구현 | 파일 기반 v1alpha2 명세로 전용 ARM64 gVisor 풀에 Echo를 배포했다. Gateway 경유 HTTP 200, Router의 클라이언트 인증서 검증과 같은 리비전의 Actor 재사용을 확인했다. [배포 실증](ate-deployment.md#개발계-검증-기록) |
| CRD 배포 컨트롤러 | v1alpha3 워커와 프로필을 적용해 동기 HTTP 배포를 검증했다. 후보 준비 중 기존 Gateway 응답이 유지됐고, 롤백 후 기존 Actor UID와 HTTPRoute UID를 확인했다. [컨트롤러 실증](ate-controller.md#개발계-검증) |
| 공통 런타임과 Echo 앱 패키지 | ARM64 gVisor 풀에서 런타임 digest를 고정하고 앱 digest만 v1 → v2 → v1으로 바꿨다. 새 Actor가 준비된 뒤 같은 URL에서 응답이 전환됐고 롤백은 기존 v1 Actor를 사용했다. Go 테스트에서 Job 리비전에도 두 digest가 고정되는지 확인했다. [Echo 배포 검증](../examples/echo/README.md#공통-런타임과-echo-앱-패키지) · [Job 리비전 테스트](../internal/platform/packaged_test.go) |
| 단일 Job 구현 | PostgreSQL 접수와 리비전 고정, Attempt Actor UID 기록, Worker 장애 후 새 Actor 재시도를 검증했다. 공통 런타임의 Echo 패키지를 v2로 전환한 뒤에도 기존 Run은 처음 접수한 v1 결과 한 건을 반환했다. [패키지 Job 개발계 검증](job-api.md#공통-런타임과-echo-앱-패키지) |
| 사이트 세션과 HITL 구현 | 재인증 대기 중 Actor·슬롯을 반환했다. 공통 런타임에서 Echo 패키지를 v2로 바꾼 뒤에도 같은 Run ID가 새 Actor에서 v1으로 재개돼 결과 한 건을 반환했다. [패키지 HITL 개발계 검증](site-session-reauth.md#공통-런타임과-echo-앱-패키지) |
| 첫 운영 릴리스 | 풀별 한도와 운영 대시보드는 아래 인수 기준으로 검증한다. |

## 출시 인수 시험

격리한 개발 환경에 전용 Gateway 리스너, Atespace, ARM64 gVisor 다중 Worker 및 모의 사이트를 준비한다. 장애 대상은 플랫폼이 생성한 시험용 리소스의 UID로 식별한다. CI에서 실행 가능한 시험 스크립트에 실행 ID, 리비전, 시도 번호, Worker 배정, 결과 개수와 마지막 슬롯 상태를 남긴다. Testkube를 사용하는 환경은 동일한 스크립트와 증거를 TestWorkflow 실행에 연결할 수 있다. [Testkube 아티팩트 수집](https://docs.testkube.io/articles/logs-and-artifacts)

| 요구사항 | 시험과 수집할 증거 |
| --- | --- |
| R1 설치·호출 | Echo를 ATE Control API로 직접 배포한다. 실제 Router TLS 신원과 Gateway 응답을 확인한다. 위조 대상 헤더, 허용·거부 목적지, 동기 경로에서 사설 `/attempt` 접근 거부를 각각 시험한다. |
| R2 배포·롤백 | 런타임 이미지 digest를 고정하고 패키지 digest만 v1 → v2 → v1으로 바꾸며 같은 URL을 반복 호출한다. 준비가 늦은 패키지, 검증·로드에 실패한 패키지, 같은 경로를 요청한 다른 워커, Route 갱신 직후 컨트롤러 종료를 시험한다. 두 digest, Gateway 조건의 세대, 전환 시점과 롤백 Actor UID를 남긴다. |
| R3 멱등·조회 | 접수 응답 직후 API를 재시작한다. 동시 중복, 다른 입력의 `409`, 전체 접수 한도에서 기존 키 조회, 새 리비전에서 스키마가 바뀐 뒤 기존 키 재요청과 결과 조회를 검사한다. |
| R4 Worker 장애·회수 | 80초 v1 실행의 실제 Worker를 확인한 뒤 앱 패키지 v2를 게시하고 해당 테스트 Worker를 종료한다. 다른 Worker의 새 Actor가 접수 당시의 런타임과 v1 앱 패키지를 사용하는지 확인한다. 원래 ID에서 결과 한 건을 조회하고, 해당 Run의 슬롯·남은 Actor가 0개인지 확인한다. 결과 확정 후 이전 시도의 늦은 보고를 보내도 같아야 한다. |
| R4 응답 유실·재시작 | 생성 응답이 유실된 Actor의 등록을 지연시킨다. 재개·시작·완료 응답을 각각 유실시키고 컨트롤러를 재시작한다. 정리 여부가 불명확한 동안 예약이 유지되는지, 늦은 등록과 재개가 추가 배정·실행권·결과를 만들지 않는지 관찰한다. 최종적으로 소유 Actor와 해당 Run 슬롯은 0개다. |
| R5 풀 용량 | HTTP·브라우저 풀과 서비스 복제본 둘 이상을 사용한다. 브라우저 풀을 포화시켜 상한, 신규 요청의 `429`, HTTP 작업 진행과 실제 배정을 확인한다. |
| R6 환경·권한 | 개발에서 검증한 런타임 이미지와 앱 패키지의 digest를 운영계용 시험 환경에 게시한다. 두 앱과 분리된 환경으로 동일 키, 실행 ID와 계정 참조를 교차 사용해 허용 범위와 거부 응답을 확인한다. |
| R7 계정 세션·HITL | 모의 사이트에서 Actor 교체 후 계정을 재사용한다. 세션 만료와 사용자 대기 동안 기존 Run의 슬롯 0개를 확인한다. 재인증 후 접수 당시의 두 digest와 갱신된 세션을 새 Actor가 사용하는지 확인한다. 다른 Run의 동시 만료, 중복·지연된 완료, 대기 중 API 재시작 및 인증 중 취소를 시험해 갱신권과 재개 횟수를 확인한다. |
| R8 운영 조회 | Run 타임라인에서 재시도와 슬롯 반환을 찾는다. ATE 조회 단절을 표시한 뒤 회복 시 실제 배정을 표시한다. 상태·결과·운영 조회 전후로 시작·재개 RPC 건수를 비교한다. 취소 반복과 완료·취소 경합 후 최종 상태·이력·슬롯을 대조한다. |

### 구현과 함께 확정할 운영값

- **지원 버전과 신뢰 경계.** GatewayClass와 구현 버전을 실제 mTLS, HTTP 메서드·헤더 수정, 상태 세대, 인증·인가까지 확인해 고정한다. ATE v1.0.0 계열의 실제 배포판에서 Worker 장애 시 배정 회수와 Actor JWT 주입을 확인한다. [Gateway API 구현별 기능](https://gateway-api.sigs.k8s.io/docs/implementations/versions/v1.5/) · [ATE Actor JWT](https://ate.dev/docs/guides/actor-jwts/)
- **리소스와 기한.** HTTP·브라우저 풀의 슬롯·접수 기본값, 앱 실행·준비·인증 기한과 재시도 백오프를 시험 부하로 정한다. 입력·결과·로그와 멱등 영수증, 리비전 보존 기간을 공개 운영 계약에 명시한다.
- **데이터 복구.** 배포 환경에 맞는 PostgreSQL 백업·복제와 장애 복구 목표를 정한다. 백업 복원 후 이전 실행권을 차단하고 플랫폼이 소유한 Actor를 대조하는 절차와, 복구 목표에 따라 보존할 수 있는 접수 범위를 시험한다.

## 상위 프로젝트에서 적용한 패턴

### kagent

kagent `v1.0.1`의 커밋 `558e3d761d8ed2b0d23bfef1867a9d1915210121`에서 컨트롤러·Go 러너·SQL·React UI와 관련 테스트를 읽었다. kagent Go 모듈은 ATE 모듈을 `github.com/kagent-dev/substrate`로 치환한다. Worker Platform은 `go.ate.dev/substrate v1.0.0`의 Control API 클라이언트를 직접 사용한다. [kagent 릴리스](https://github.com/kagent-dev/kagent/releases/tag/v1.0.1) · [kagent 의존성](https://github.com/kagent-dev/kagent/blob/558e3d761d8ed2b0d23bfef1867a9d1915210121/go/go.mod#L510-L515) · [이 저장소 의존성](../go.mod)

| 코드에서 확인한 동작 | 적용 |
| --- | --- |
| 리콘실러가 소유권을 먼저 기록하고 불변 템플릿과 UID를 검증한다. 풀은 이름·네임스페이스 라벨로 선택한다. [리콘실러](https://github.com/kagent-dev/kagent/blob/558e3d761d8ed2b0d23bfef1867a9d1915210121/go/core/internal/controller/sandbox_reconciler.go#L121-L216) · [풀 선택](https://github.com/kagent-dev/kagent/blob/558e3d761d8ed2b0d23bfef1867a9d1915210121/go/core/internal/substrate/actor_template.go#L221-L229) | 기존 명세 검증과 Ready 이후 게시 흐름을 이어가고, ATE 템플릿 해석과 소유권 검증에 적용한다. |
| Sandbox 서비스는 저장된 작업 소유권을 확인한다. Actor 정책을 적용한 뒤 재개하고, 정리에 UID 조건을 건다. 게스트 프로세스는 kagent 전용 RPC로 실행한다. [수명주기](https://github.com/kagent-dev/kagent/blob/558e3d761d8ed2b0d23bfef1867a9d1915210121/go/core/internal/substrate/lifecycle.go#L146-L214) · [프로세스 실행 RPC](https://github.com/kagent-dev/kagent/blob/558e3d761d8ed2b0d23bfef1867a9d1915210121/go/core/internal/service/sandbox/guest.go#L68-L109) | ATE 호출 순서를 적용하고, 워커 이미지는 우리 HTTP 러너 계약을 구현한다. |
| 예약 실행은 작업을 선점해 A2A 전송 권한을 기록한다. 전송이 불확실하면 이력을 읽는다. 세션 복구는 같은 Actor의 스냅샷을 복원하고 완료되지 않은 작업은 실패로 기록한다. [예약 실행](https://github.com/kagent-dev/kagent/blob/558e3d761d8ed2b0d23bfef1867a9d1915210121/go/core/internal/controller/scheduledrun/controller.go#L130-L218) · [세션 복구](https://github.com/kagent-dev/kagent/blob/558e3d761d8ed2b0d23bfef1867a9d1915210121/go/core/internal/database/session_recovery.go#L131-L184) | 불명확한 호출은 영수증을 확인한다. Run·Attempt가 새 Actor 재배치와 리비전 고정을 맡는다. |
| HITL 응답은 저장된 인증 요청의 ID와 대조한다. TaskStore는 Actor와 세션 바인딩·버전을 검사한다. [HITL](https://github.com/kagent-dev/kagent/blob/558e3d761d8ed2b0d23bfef1867a9d1915210121/go/adk/pkg/a2a/hitl.go#L573-L645) · [TaskStore](https://github.com/kagent-dev/kagent/blob/558e3d761d8ed2b0d23bfef1867a9d1915210121/go/core/internal/service/taskstore/service.go#L45-L67) | 현재 시도와 재인증 요청을 검증하고, 사이트 계정 세션·체크포인트는 Run과 함께 관리한다. |
| Substrate 운영 화면은 권한을 확인해 풀과 배정을 읽고, 조회 오류를 정상적인 빈 결과와 구분한다. [운영 조회 API](https://github.com/kagent-dev/kagent/blob/558e3d761d8ed2b0d23bfef1867a9d1915210121/go/core/internal/service/system/substrate.go#L91-L153) · [Substrate 화면 코드](https://github.com/kagent-dev/kagent/blob/558e3d761d8ed2b0d23bfef1867a9d1915210121/ui/src/pages/SubstratePage.tsx#L777-L824) | Run 상세의 ATE 배정과 풀 현황에 적용한다. |

kagent를 포크하면 Agent·Harness·Session, A2A와 SQL 모델을 계속 유지하면서 Run과 Attempt를 따로 추가해야 한다. 검토한 런타임 종류는 `agent`와 `sandbox`이며 활성 Task의 유일성도 A2A 이력 기준이다. 현재 저장소를 유지하며 공개 ATE API로 필요한 패턴만 적용하는 편이 변경 범위가 작다. kagent의 Go `internal` 패키지는 외부 모듈에서 직접 가져올 수 없다. 구현에 발췌할 코드가 있다면 원래 라이선스와 출처를 보존한다. [런타임과 Task 제약](https://github.com/kagent-dev/kagent/blob/558e3d761d8ed2b0d23bfef1867a9d1915210121/go/core/pkg/migrations/core/000001_initial.sql#L132-L219) · [kagent 라이선스](https://github.com/kagent-dev/kagent/blob/558e3d761d8ed2b0d23bfef1867a9d1915210121/LICENSE)

관련 테스트에서 중복 Actor 생성, 전송 응답 유실, 세션 복구 경합과 운영 조회 경계를 대조했다. [Actor 생성 테스트](https://github.com/kagent-dev/kagent/blob/558e3d761d8ed2b0d23bfef1867a9d1915210121/go/core/internal/substrate/lifecycle_test.go#L129-L183) · [전송 유실 테스트](https://github.com/kagent-dev/kagent/blob/558e3d761d8ed2b0d23bfef1867a9d1915210121/go/core/internal/controller/scheduledrun/controller_test.go#L239-L296) · [세션 복구 테스트](https://github.com/kagent-dev/kagent/blob/558e3d761d8ed2b0d23bfef1867a9d1915210121/go/core/internal/service/session/recovery_test.go#L157-L193) · [운영 조회 테스트](https://github.com/kagent-dev/kagent/blob/558e3d761d8ed2b0d23bfef1867a9d1915210121/go/core/internal/service/system/substrate_paging_test.go#L385-L421)

### Testkube

Testkube `2.15.2`의 커밋 `06e7ddebe433f5ae8f9e83adf8211e37d435b618`에서 TestWorkflow를 Kubernetes Job으로 실행하는 흐름을 확인했다. Job 생성 시 `AlreadyExists`가 발생하면 기존 Job과 Runner ID를 대조한다. 실행 레코드에는 해석한 명세, 결과, 리포트가 연결되며 재실행은 기본으로 저장된 명세를 사용한다. [Job 생성](https://github.com/kubeshop/testkube/blob/06e7ddebe433f5ae8f9e83adf8211e37d435b618/pkg/testworkflows/testworkflowprocessor/bundle.go#L85-L110) · [중복 시작 처리](https://github.com/kubeshop/testkube/blob/06e7ddebe433f5ae8f9e83adf8211e37d435b618/pkg/testworkflows/executionworker/kubernetesworker/worker.go#L196-L243) · [실행 모델](https://github.com/kubeshop/testkube/blob/06e7ddebe433f5ae8f9e83adf8211e37d435b618/pkg/api/v1/testkube/model_test_workflow_execution.go#L16-L56) · [재실행 명세 선택](https://github.com/kubeshop/testkube/blob/06e7ddebe433f5ae8f9e83adf8211e37d435b618/internal/app/api/v1/testworkflows.go#L604-L638)

Worker Platform은 실행 상세에서 한 ID 아래 시도·이벤트·로그·결과를 잇는 구성을 참고한다. Testkube는 독립적인 인수 시험 실행에 활용하고, 제품의 Job은 ATE Actor에서 수행한다. 이번 검토에서는 상위 프로젝트의 소스와 문서를 읽었다. [Testkube 실행 상세](https://docs.testkube.io/articles/testkube-dashboard-execution-details) · [Job 운영 화면](job-execution-design.md#운영-조회와-대시보드)

### 초기 요구사항의 비교 기준

| 프로젝트 | 요구사항에 반영한 기능 |
| --- | --- |
| Windmill | 단일 Job, 워커 그룹, 환경별 설정과 아티팩트의 승격. [Job](https://www.windmill.dev/docs/core_concepts/jobs) · [워커 그룹](https://www.windmill.dev/docs/core_concepts/worker_groups) · [배포](https://www.windmill.dev/docs/advanced/canonical_deployment_setups) |
| Apify | 실행 ID로 상태와 결과를 추적하고 앱의 입력 형식을 정의한다. [실행 모델](https://docs.apify.com/actors/running/runs-and-builds) · [입력 스키마](https://docs.apify.com/actors/development/actor-definition/input-schema) |
| Knative Serving | 리비전 간 전환과 이전 리비전으로의 롤백에서 호출 주소를 유지한다. [트래픽 전환](https://knative.dev/docs/getting-started/first-traffic-split/) |
| OpenFaaS | HTTP 연결과 독립된 비동기 실행을 제공하고 과부하와 재시도 동작을 정한다. [비동기 호출](https://docs.openfaas.com/reference/async/) |
| Temporal | 이미 접수한 실행과 배포 버전의 관계를 유지하는 패턴을 참고한다. [워커 버전 관리](https://docs.temporal.io/best-practices/worker) |
