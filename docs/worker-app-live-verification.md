# 개발 Gateway 실환경 검증

검증 범위는 [워커 앱 제품 목표 #1](https://github.com/imprun/worker-platform/issues/1)과 [실환경 검증 #4](https://github.com/imprun/worker-platform/issues/4)이다. 실행마다 워커 저장소와 API 저장소의 전체 SHA를 함께 남긴다.

## 현재 검증 근거

- 개발 Kubernetes API `v1.36.6`에 지정 TCP CONNECT 프록시를 통해 접속했다. kubeconfig CA와 서버 이름 검증을 유지했다. `ate-system`은 조회됐고 `wp-echo-example`, `HTTPWorker`, `WorkerProfile`은 당시 `404`를 반환했다. 클러스터는 변경하지 않았다.
- [Echo PR #5](https://github.com/imprun/worker-platform/pull/5)에서 로컬 `POST /invoke`의 HTTP 200, 원본과 같은 JSON 값, Actor 로컬 `requests.jsonl` 기록을 확인했다. PR의 JSON Echo 패키지가 개발계에 올라가야 동일 계약을 실환경에서 재검증할 수 있다.
- 격리된 PostgreSQL을 사용한 로컬 HTTP 테스트에서 동시 등록·배포, 타 프로젝트 조회 거부, 승인 프로필 제한을 확인했다. Kubernetes 클라이언트는 테스트 대역이었다.
- 두 저장소를 함께 배포한 GitHub Actions 실행, 인증된 개발 Gateway 호출과 사용자 관찰 결과는 아직 없다.

## 자동 실행에서 남길 항목

1. 같은 프로젝트의 게시 OCI 패키지를 v1으로 배포한다. 개발 API의 고정 외부 POST 경로를 워커 앱의 `POST /invoke`에 연결하고 개발 Gateway가 배포를 승인할 때까지 기다린다.
2. 허가된 호출자에게 발급한 테스트 자격으로 외부 URL에 JSON을 보내 HTTP 상태와 응답 값을 확인한다. 워커 패키지가 v1임은 런타임이 검증한 `X-Imprun-Worker-Package-Digest` 응답 헤더로 확인한다. 연결 등록 응답의 상태값만으로 호출 성공을 판정하지 않는다.
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
