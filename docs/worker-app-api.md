# 개발 워커 앱 API

앱 서버에는 인증된 요청에서 조직과 프로젝트를 결정하는 권한 검사, 프로젝트별 승인 프로필과 앱별 개발 호스트 설정, PostgreSQL 및 범위가 제한된 Kubernetes 클라이언트가 필요하다. Run API의 `-worker-app-config-file` 플래그로 경로를 활성화한다. API Platform은 권한 검사를 활성화하고 `worker-app-access` 경로를 제공해야 한다. 워커 API는 해당 HTTPS 서버에 사용자 Bearer 토큰을 전달하여 조직·프로젝트 범위를 확인한다.

콘솔이 `X-Org-Id`를 보낼 때 Run API는 그 조직도 권한 서버에 전달한다. 인증된 조직과 일치할 때만 프로젝트 조회나 변경이 가능하다.

같은 네임스페이스에 `deploy/run-api/worker-app-rbac.yaml`을 적용한다. 활성화한 Run API Deployment에 `serviceAccountName: worker-app-api`, `automountServiceAccountToken: true` 및 설정 파일 볼륨을 지정한다. 설정의 예는 다음과 같다.

```json
{
  "platformURL": "https://platform.example.test",
  "developmentDomain": "workers.dev.example.test",
  "echoPackage": "packages.example.test/demo/echo@sha256:1111111111111111111111111111111111111111111111111111111111111111",
  "gateway": {"name":"worker-dev","namespace":"worker-dev","sectionName":"https"},
  "router": {"name":"actor-router","namespace":"ate-system","port":443},
  "projects": [
    {"organizationId":"org-uuid","projectId":"project-uuid","approvedProfiles":["arm64-dev"]}
  ]
}
```

프로젝트는 API Platform의 고정 조직·프로젝트 ID로 설정한다. 개발 Gateway의 리스너, DNS와 TLS 인증서는 앱 ID를 하위 도메인으로 사용하는 호스트를 처리해야 한다.

## 등록과 배포

`POST /projects/demo/worker-apps`

```json
{
  "name": "배송 결과",
  "profile": "arm64-dev",
  "package": {
    "repository": "packages.example.test/demo/shipping",
    "digest": "sha256:1111111111111111111111111111111111111111111111111111111111111111"
  }
}
```

Echo를 등록할 때는 `package`에 `{"sample":"echo"}`를 보낸다. 앱 이름, 프로필과 패키지가 일치하는 등록 재시도는 동일한 앱 ID를 반환한다. 같은 프로젝트·이름으로 다른 프로필이나 패키지를 등록하면 `409`를 반환한다. 등록 결과는 `GET /projects/demo/worker-apps`에서 조회한다.

등록 응답의 `id`로 `PUT /projects/demo/worker-apps/{app}/environments/development`에 `{}`를 보내면 등록한 패키지의 개발 배포가 시작된다. 응답은 `202`다. 패키지를 바꿀 때는 같은 URL에 새 `package`와 직전 조회 응답의 `resourceVersion`을 보낸다. 버전 충돌 시 최신 상태를 다시 조회한다.

## 배포 상태 예시

`GET /projects/demo/worker-apps/{app}/environments/development`에서 v2 시도가 실패하고 v1이 마지막 활성 리비전인 상태의 예다.

```json
{
  "appId": "app-432a17e30a00f4e540234a812bdaa00bc1ef",
  "projectId": "project-demo",
  "environment": "development",
  "operation": {"method": "POST", "path": "/invoke"},
  "resourceVersion": "48283",
  "active": {
    "generation": 1,
    "revision": "echo-v1",
    "appPackage": "packages.example.test/demo/shipping@sha256:1111111111111111111111111111111111111111111111111111111111111111"
  },
  "latestAttempt": {
    "generation": 2,
    "appPackage": "packages.example.test/demo/shipping@sha256:2222222222222222222222222222222222222222222222222222222222222222",
    "state": "failed",
    "reason": "DeploymentPending"
  }
}
```

`latestAttempt.state`는 `queued`, `preparing`, `failed`, `ready` 중 하나다. 첫 게시 전에는 `active`가 생략된다. 상태의 `ready`는 컨트롤러가 게시를 확인했다는 의미다. Gateway 인증과 실제 JSON 왕복은 호출 기록에서 확인한다.

같은 패키지의 재시도를 즉시 요청하려면 `resourceVersion`과 현재 시도한 `package`를 그대로 보내고 `retry: true`를 추가한다. 같은 리소스 버전의 중복 재시도는 하나로 처리한다. 컨트롤러는 기존 배포와 Actor 리비전을 재사용하고 준비 점검을 다시 실행한다.

## API 연산 연결 대상 조회

`GET /projects/demo/worker-apps/{app}/environments/development/target`는 프로젝트에 등록된 앱의 `appId`, `projectId`, `environment`, `operation`을 반환한다. 같은 프로젝트의 등록 앱은 첫 배포 전에도 조회할 수 있다. 활성 배포가 준비되면 응답에 내부 HTTPS `url`과 활성 `revision`이 추가된다. 다른 프로젝트의 앱 ID는 `404`를 반환한다.

Gateway에 연결할 때는 등록 프로젝트와 지원 연산을 확인하고, 개발 배포 전에는 URL과 활성 리비전이 모두 있는지 다시 확인한다. 같은 앱의 활성 리비전이 바뀌어도 대상 URL은 유지된다.

## 콘솔 JSON 테스트

`POST /projects/demo/worker-apps/{app}/environments/development/invoke`에 64 KiB 이내 JSON 값을 보낸다. 프로젝트 변경 권한과 활성 배포가 필요하다. Run API는 설정된 내부 개발 Gateway의 HTTPS 앱 호스트만 사용한다. 개발 앱 Gateway는 Run API에서 접근할 수 있는 내부 리스너로 구성한다.

Echo 요청과 응답 예시는 다음과 같다.

```json
{"hello":"world"}
```

```json
{"status":200,"body":{"hello":"world"}}
```

`status`는 앱에서 받은 HTTP 상태다. Echo는 JSON을 Actor 로컬 `requests.jsonl`에 남긴다. 호출 응답은 앱에서 받은 값이며 로그 파일의 별도 백업이나 API Platform Gateway의 배포·인증 상태를 나타내지 않는다.
