# 테스트 기준과 검증 기록

## PostgreSQL 회귀 테스트

모든 PR과 `main` 푸시의 기본 CI가 PostgreSQL 17에서 단일 Job 계약을 검증한다. 서로 다른 컨트롤러가 같은 풀의 마지막 슬롯을 요청할 때 한 번만 예약되는지, 대기 중인 풀을 건너 다른 풀이 진행되는지 검사한다. 이전 스키마 v2로 저장한 완료 결과와 실행 중인 Attempt를 v3로 이관한 뒤 조회한다. 기존 Attempt가 정리될 때까지 새 Run이 그 슬롯을 사용할 수 없는지 확인한다. [예약 경합 테스트](../internal/jobs/pool_slots_test.go) · [이관 테스트](../internal/jobs/pool_migration_test.go)

로컬의 시험 PostgreSQL을 사용할 때는 `WORKER_JOB_TEST_DSN`을 지정한다. 이관 테스트는 연결한 DB에 고유한 시험 스키마를 생성하고 종료할 때 제거한다.

```sh
WORKER_JOB_TEST_DSN='postgresql://jobs:jobs@127.0.0.1:5432/jobs?sslmode=disable' \
  go test -race -count=1 -run '^TestPostgres' -v ./internal/jobs
```

## Worker Platform에서 확인한 동작

| 검증한 동작 | 재현 방법과 관찰 결과 |
| --- | --- |
| Gateway에서 ATE Router로 mTLS 연결 | 독립 Echo 예제를 배포해 Gateway에서 HTTP 200과 Echo JSON을 확인했다. 같은 명세를 재적용했을 때 Actor UID가 유지됐다. [실행 스크립트](../examples/echo/run.sh) · [ATE 직접 배포 검증](ate-deployment.md#개발계-검증-기록) |
| OCI 패키지 전환과 롤백 | 런타임 이미지 digest를 고정한 상태에서 앱 v1 → v2 → v1을 적용했다. v2 준비 중 v1 응답을 확인했고, 롤백 시 기존 v1 Actor UID와 같은 HTTPRoute UID를 확인했다. [배포 절차](../examples/echo/README.md#공통-런타임으로-echo-패키지-배포) · [실측 기록](../examples/echo/README.md#공통-런타임과-echo-앱-패키지) |
| Worker 손실 후 단일 Job 복구 | 실행 중 v2를 배포한 뒤 시험 Worker를 종료했다. 다른 Worker의 새 Actor가 같은 Run ID의 v1 리비전을 실행했다. 결과는 한 건이었으며 완료 후 Job Actor와 슬롯은 0개였다. [장애 재현 절차](job-api.md#개발계에서-켜기) · [실측 기록](job-api.md#공통-런타임과-echo-앱-패키지) |
| HITL 대기 후 재개 | 재인증 대기 중 Job Actor와 슬롯이 0개였다. 패키지 v2 배포와 모의 재인증을 거친 뒤에도 새 Actor가 같은 Run ID로 v1 결과 한 건을 기록했다. [재인증 실행 절차](site-session-reauth.md#만료와-모의-재인증-호출) · [실측 기록](site-session-reauth.md#공통-런타임과-echo-앱-패키지) |
| 복제본 간 슬롯 경합과 포화 격리 | PostgreSQL CI가 마지막 슬롯 예약 경합과 데이터 이관을 확인한다. 두 전용 ARM64 gVisor 풀에서 A의 슬롯이 모두 찬 동안 B의 Job이 완료됐다. [CI 테스트](../internal/jobs/pool_slots_test.go) · [실측 기록](job-api.md#풀-두-개와-복제본-두-개-검증) |

새 검증 결과는 해당 기능의 가이드에 남긴다. 재현에 필요한 커밋, 설치 버전, 명령과 관찰한 Run ID·Actor UID·슬롯 수를 확인 가능한 범위에서 기록한다. 반복 재현이 필요한 동작에는 이 저장소에 픽스처와 테스트를 추가한다.

## ATE 실환경 테스트

변경 감지는 ATE Control·Router 클라이언트, 배포 컨트롤러, 템플릿·WorkerPool·Gateway 설정, 공통 런타임 이미지·OCI 패키징, 해당 연동의 Go 도구와 SDK 의존성을 본다. Job 예약·스키마, 독립 Echo 검증 서버, 문서·테스트 수정은 기본 CI에서 검사한다. 변경 기준은 실제 배포할 후보 커밋과 비교할 기준 커밋이다. [변경 판정 코드](../scripts/ate_integration.py)

```sh
python3 scripts/ate_integration.py "$base" HEAD
```

`ATE integration` 워크플로는 `main` 푸시에서 직전 SHA와 후보 SHA를 비교한다. 수동 실행에서는 `base` 커밋을 받는다. 대상 변경이 있고 저장소 변수 `ATE_INTEGRATION_ENABLED=true`가 설정된 경우에만 전용 Linux 러너의 `ate-integration` 레이블에서 실환경 스모크를 실행한다. 기본 CI와 변경 판정은 클러스터 자격 없이 실행된다. [실환경 워크플로](../.github/workflows/ate-integration.yml)

로컬에서 스모크를 실행할 때도 동일한 판정을 사용한다. 검증할 커밋을 체크아웃하고 전용 테스트 환경을 마련한다. Echo 스크립트의 자격 파일과 `KUBE_CONTEXT`를 설정한다. 환경에 맞는 kube-system 네임스페이스 UID를 `ATE_TEST_CLUSTER_UID`에 지정하고 `ATE_TEST_NAMESPACE_UID`, `ATE_TEST_POOL_UID`, `ATE_TEST_SPACE_UID`도 설정한다. `kubectl ate`는 이 컨텍스트의 ATE를 조회할 수 있어야 한다. 스모크는 UID가 일치하고 전용 공간의 Actor·ActorTemplate·HTTPRoute가 비어 있는지 확인한 뒤 배포한다. [Echo 개발계 준비](../examples/echo/README.md#개발계-준비)

전용 레지스트리에 게시할 수 있는 Docker Buildx와 `crane`, Go, Python 3, `jq`, `curl`을 러너에 준비한다. `RUNTIME_REPOSITORY`와 `PACKAGE_REPOSITORY`는 ATE가 각각 런타임 이미지와 앱 패키지를 읽는 저장소다. 게시 주소가 다르면 같은 저장소의 쓰기 주소를 `RUNTIME_PUSH_REPOSITORY`, `PACKAGE_PUSH_REPOSITORY`에 지정한다. 패키지 레지스트리의 HTTPS에 사설 CA를 쓰면 `PACKAGE_REGISTRY_CA`에 CA PEM 파일 경로를 설정한다. 앱 레지스트리의 호스트와 포트는 패키지 참조에서 읽어 테스트 프로필의 egress에 설정한다.

```sh
export RUNTIME_REPOSITORY=registry.example.test/workers/actor-runtime
export PACKAGE_REPOSITORY=packages.example.test/workers/echo
python3 scripts/ate_integration.py --run "$base" HEAD
```

스모크는 체크아웃한 커밋에서 Linux ARM64 공통 런타임 이미지와 `examples/echo/app` OCI 패키지를 빌드하고 게시한다. 패키지의 게시 digest가 로컬 매니페스트 digest와 일치하는지 확인한 뒤, 이미지와 앱의 digest를 각각 고정해 `workerctl`로 배포한다. Gateway 경유 응답의 `version`으로 이번에 빌드한 패키지가 실행됐는지 검사하며 대상 헤더 덮어쓰기와 공개 준비 경로 차단도 확인한다. 실험이 끝나면 전용 환경의 소유자가 예제의 정리 절차를 사용한다. [Echo 실험 정리](../examples/echo/README.md#예제-정리)
