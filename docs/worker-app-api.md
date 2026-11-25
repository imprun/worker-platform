# 개발 워커 앱 API

앱 서버에는 인증된 요청에서 조직과 프로젝트를 결정하는 권한 검사, 프로젝트별 승인 프로필과 앱별 개발 호스트 설정, PostgreSQL 및 범위가 제한된 Kubernetes 클라이언트가 필요하다. 아래 경로와 JSON은 워커 앱 HTTP 처리 코드의 계약이며, 서버에 연결한 뒤 사용할 수 있다.

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
