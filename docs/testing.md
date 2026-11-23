# 테스트 기준과 PoC 검증 재사용

## PostgreSQL 회귀 테스트

모든 PR과 `main` 푸시의 기본 CI가 PostgreSQL 17에서 단일 Job 계약을 검증한다. 서로 다른 컨트롤러가 같은 풀의 마지막 슬롯을 요청할 때 한 번만 예약되는지, 대기 중인 풀을 건너 다른 풀이 진행되는지 검사한다. 이전 스키마 v2로 저장한 완료 결과와 실행 중인 Attempt를 v3로 이관한 뒤 조회한다. 기존 Attempt가 정리될 때까지 새 Run이 그 슬롯을 사용할 수 없는지 확인한다. [예약 경합 테스트](../internal/jobs/pool_slots_test.go) · [이관 테스트](../internal/jobs/pool_migration_test.go)

로컬의 시험 PostgreSQL을 사용할 때는 `WORKER_JOB_TEST_DSN`을 지정한다. 이관 테스트는 연결한 DB에 고유한 시험 스키마를 생성하고 종료할 때 제거한다.

```sh
WORKER_JOB_TEST_DSN='postgresql://jobs:jobs@127.0.0.1:5432/jobs?sslmode=disable' \
  go test -race -count=1 -run '^TestPostgres' -v ./internal/jobs
```

## 이전 PoC 검증 증거

| 검증한 동작 | 이어서 사용하는 증거와 스크립트 |
| --- | --- |
| 복제본 간 공유 슬롯과 포화 풀 격리 | WX의 두 저장소 연결이 동일한 PostgreSQL에서 한도를 다투는 회귀 테스트를 기준으로 삼는다. Worker Platform CI에서 현재 저장소의 예약과 이관을 검증한다. [WX 풀 한도 테스트](https://github.com/imprun/imprun-cloud/blob/3f2b20131fe325c09c391b6f8661f576e74f13f3/runtime/internal/executor/postgres/pool_slots_test.go) |
| ARM64 gVisor에서 Worker 손실 후 새 Actor로 재시도 | 기존 실증의 같은 실행 ID, 결과 한 건, Actor·슬롯 반환 기록을 사용한다. 재검증 진입점은 검증된 `verify_wx_failover.ps1` 커밋에 남아 있다. Worker Platform의 연동 스모크는 이 스크립트의 클러스터·시험 리소스 UID 대조를 적용한다. [실증 기록](https://github.com/imprun/imprun-cloud/blob/2e6d70c1466b6cd791af197038283da88fd5cc6e/docs/wx/deploy-operations.md) · [PoC 검증 스크립트](https://github.com/imprun/imprun-cloud/blob/2e6d70c1466b6cd791af197038283da88fd5cc6e/deploy/ax/verify_wx_failover.ps1) |
| OCI 패키지 전환, Ready 조건과 기존 Actor 롤백 | 동일 런타임으로 패키지 digest를 바꾼 실증을 기준으로 삼는다. PoC 스모크의 대상 헤더 덮어쓰기와 공개 `/readyz` 차단 검사를 현재 Echo 스모크에서 실행한다. [PoC 배포 가이드](https://github.com/imprun/scraping-platform/blob/defe5211b91e67c315c1acbee76e01f7a0f01d59/deploy/dev/README.md#oci-앱-패키지) · [PoC 스모크](https://github.com/imprun/scraping-platform/blob/defe5211b91e67c315c1acbee76e01f7a0f01d59/test/smoke.ps1) · [Echo 스모크](../examples/echo/run.sh) |

## ATE 실환경 테스트

변경 감지는 ATE Control·Router 클라이언트, 배포 컨트롤러, 템플릿·WorkerPool·Gateway 설정, 런타임 이미지·OCI 패키징, 해당 연동의 Go 도구와 SDK 의존성을 본다. Job 예약·스키마와 문서·테스트 수정은 기본 CI에서 검사한다. 변경 기준은 실제 배포할 후보 커밋과 비교할 기준 커밋이다. [변경 판정 코드](../scripts/ate_integration.py)

```sh
python3 scripts/ate_integration.py "$base" HEAD
```

`ATE integration` 워크플로는 `main` 푸시에서 직전 SHA와 후보 SHA를 비교한다. 수동 실행에서는 `base` 커밋을 받는다. 대상 변경이 있고 저장소 변수 `ATE_INTEGRATION_ENABLED=true`가 설정된 경우에만 전용 Linux 러너의 `ate-integration` 레이블에서 실환경 스모크를 실행한다. 기본 CI와 변경 판정은 클러스터 자격 없이 실행된다. [실환경 워크플로](../.github/workflows/ate-integration.yml)

로컬에서 스모크를 실행할 때도 동일한 판정을 사용한다. 검증할 커밋을 체크아웃하고 [Echo 개발계 준비](../examples/echo/README.md#개발계-준비)로 전용 테스트 환경을 마련한다. 기존 Echo 스크립트의 자격 파일과 `KUBE_CONTEXT`를 설정한다. 실험 환경에 맞는 `ATE_TEST_CLUSTER_UID`의 kube-system UID, `ATE_TEST_NAMESPACE_UID`, `ATE_TEST_POOL_UID`, `ATE_TEST_SPACE_UID`를 전달한다. `kubectl ate`는 이 컨텍스트의 ATE를 조회할 수 있어야 한다. 스모크는 해당 UID들과 Actor·ActorTemplate·HTTPRoute가 비어 있는지 확인한 뒤 Echo를 배포한다.

```sh
python3 scripts/ate_integration.py --run "$base" HEAD
```

이 진입점은 실제 ATE에 독립 Echo Actor를 배포하고 Gateway 경유 응답·대상 헤더 덮어쓰기·공개 준비 경로 차단을 확인한다. 실험이 끝나면 전용 환경의 소유자가 예제의 정리 절차를 사용한다. OCI 로딩 계약이나 Worker 장애 처리의 연동이 바뀌면 앞 표의 검증을 해당 변경에 맞춰 재실행한다. [Echo 실험 정리](../examples/echo/README.md#예제-정리)
