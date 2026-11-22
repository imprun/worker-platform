# 사이트 세션과 HITL 재인증

Job 접수 요청의 `Site-Session-Ref`에 운영자가 등록한 사이트 계정 참조를 지정한다. `worker-controller`는 네임스페이스와 HTTPWorker UID에 속하는 세션을 찾아 Run에 연결한다. PostgreSQL에는 로그인 상태를 AES-256-GCM 암호문으로 저장한다. Controller는 실행할 Attempt에만 mTLS로 로그인 상태를 전달하며 상태 조회에는 세션 버전이 표시된다. 같은 DB를 쓰는 컨트롤러는 동일한 키 Secret을 사용한다. [Job API](job-api.md) · [세션 저장소](../internal/jobs/site_sessions.go)

```mermaid
stateDiagram-v2
    RUNNING --> RELEASING: 세션 만료 또는 결과 수신
    RELEASING --> WAITING_AUTH: Actor 삭제 확인과 슬롯 반환
    WAITING_AUTH --> WAITING_AUTH: 사용자 재인증 시작
    WAITING_AUTH --> QUEUED: 인증 제공자 완료 커밋
    WAITING_AUTH --> FAILED: Run 기한 만료
    QUEUED --> STARTING: 고정 리비전의 새 Attempt
    STARTING --> RUNNING: 갱신된 세션 전달
    RELEASING --> SUCCEEDED: Actor 회수와 결과 한 건 확정
```

## 러너와 인증 제공자

- 러너는 사설 `POST /attempt`에서 `siteSession.ref`, `siteSession.version`, `siteSession.state`를 받는다. 사이트에서 만료를 확인하면 HTTP 428과 `{"code":"site_session_expired"}`를 반환한다. 성공 결과에는 앱의 처리 결과를 담는다.
- 컨트롤러는 처음 Actor의 삭제가 확인되기 전까지 `RELEASING`을 유지한다. 이후 `GET /v1/runs/{id}`에 `WAITING_AUTH`, `occupiedSlots: 0`, `reauth.requestId`가 표시된다. `occupiedSlots`는 해당 Run에 속한 `STARTING`, `RUNNING`, `RELEASING` Attempt의 수다. Run 기한 안에 인증을 마칠 수 있다.
- 호출자는 Job 토큰으로 `POST /v1/runs/{id}/reauth/{requestId}/start`를 호출한다. 같은 세션의 이미 시작된 갱신이 있으면 `409`를 반환한다. 갱신이 끝난 세션을 사용할 수 있으면 그 버전으로 대기 중인 Run을 다시 큐에 올린다.
- 인증 제공자는 보호된 내부 경로에서 별도 토큰으로 완료를 등록한다. 같은 Run의 활성 요청과 세션 버전을 확인한 후 한 트랜잭션에서 세션을 갱신하고 Run을 재개한다. 이미 완료한 요청은 기존 영수증을 반환한다. [재인증 트랜잭션](../internal/jobs/site_sessions.go)

## 개발계 설정

Job 개발계의 PostgreSQL, ATE Router mTLS, Job 토큰 및 Gateway를 준비한다. `SITE_KEY_FILE`에는 무작위 32바이트를 base64로 인코딩한 값을, `REAUTH_TOKEN_FILE`에는 Job 토큰과 구분된 32자 이상의 무작위 인증 제공자 토큰을 둔다. `CONTROLLER_IMAGE`에는 이 버전의 컨트롤러 이미지 digest를 사용한다. [Job 개발계 가이드](job-api.md#개발계에서-켜기)

```sh
k -n "$ns" create secret generic worker-job-site-sessions \
  --from-file="session-key=$SITE_KEY_FILE" --from-file="provider-token=$REAUTH_TOKEN_FILE"

sed '/-router-url=/c\
            - -router-url=https://wp-echo-example-router.ate-system.svc:443\
            - -router-server-name=atenet-router.ate-system.svc' deploy/controller/deployment.yaml |
  k patch --local -f - --type=json --patch-file examples/echo/jobs-controller-patch.yaml -o yaml |
  k patch --local -f - --type=json --patch-file examples/echo/site-sessions-controller-patch.yaml -o yaml |
  k set image --local -f - controller="$CONTROLLER_IMAGE" -o yaml |
  k -n "$ns" apply -f -
k -n "$ns" rollout status deployment/worker-controller --timeout=90s
```

Job 개발계 가이드에 따라 모의 세션을 지원하는 Echo 이미지를 `spec.jobEnabled: true` 워커로 게시한다. Job Gateway는 로컬 8080번에, 인증 제공자 전용 접속은 별도 터미널의 `k -n "$ns" port-forward svc/worker-jobs 8084:80`으로 연결한다. 예제 Gateway에 게시하는 Job 경로는 `/v1`이다. [Echo 모의 세션](../cmd/echo/main.go) · [Gateway 정의](../examples/echo/jobs-gateway.yaml)

## 만료와 모의 재인증 호출

`SITE_EXPIRES_AT`을 미래의 RFC 3339 시각으로 설정한다. 예제는 Echo에서 먼저 거부할 모의 쿠키를 등록하고, 사용자가 재인증을 시작하면 승인된 모의 쿠키로 세션을 갱신한다.

```sh
private=http://127.0.0.1:8084
gateway=http://127.0.0.1:8080
provider="Authorization: Bearer $(cat "$REAUTH_TOKEN_FILE")"
caller="Authorization: Bearer $(cat "$JOB_TOKEN_FILE")"
host='Host: echo.example.test'
json='Content-Type: application/json'

site_state() {
  jq -nc --arg cookie "$1" --arg expires "$SITE_EXPIRES_AT" \
    '{state:{mockCookie:$cookie},expiresAt:$expires}'
}

curl -fsS -H "$provider" -H "$json" --data "$(site_state expired-for-echo)" \
  "$private/internal/v1/workers/echo/site-sessions/demo-account"
receipt="$(curl -fsS -H "$host" -H "$caller" -H "$json" \
  -H 'Site-Session-Ref: demo-account' -H 'Idempotency-Key: site-reauth-1' \
  --data '{"message":"hello"}' "$gateway/v1/workers/echo/runs")"
id="$(printf '%s' "$receipt" | jq -r .runId)"
curl -fsS -H "$host" -H "$caller" "$gateway/v1/runs/$id"
```

Run이 `WAITING_AUTH`가 되면 동일 Run ID의 상태에서 `requestId`와 슬롯 0개를 확인한다. 재인증 시작 요청은 Gateway로, 모의 완료는 인증 제공자 접속으로 보낸다.

```sh
status="$(curl -fsS -H "$host" -H "$caller" "$gateway/v1/runs/$id")"
printf '%s' "$status" | jq -e '.state == "WAITING_AUTH" and .occupiedSlots == 0'
request="$(printf '%s' "$status" | jq -r .reauth.requestId)"
curl -fsS -X POST -H "$host" -H "$caller" \
  "$gateway/v1/runs/$id/reauth/$request/start"
curl -fsS -H "$provider" -H "$json" --data "$(site_state approved-for-echo)" \
  "$private/internal/v1/runs/$id/reauth/$request/complete"
curl -fsS -H "$host" -H "$caller" "$gateway/v1/runs/$id"
# SUCCEEDED 확인 후 기존 ID의 결과를 조회한다.
curl -fsS -H "$host" -H "$caller" "$gateway/v1/runs/$id/result"
```

## ARM64 gVisor 검증

별도 풀의 Echo에 모의 세션 버전 1로 Job을 접수했다. Echo가 세션 만료를 반환한 뒤 Run은 `WAITING_AUTH`로 전환됐다. API와 PostgreSQL에서 Run의 점유 슬롯이 0개였고 ATE에서 그 Run의 Actor는 0개였다. 풀의 실제 할당 수는 테스트 HTTP 배포 Actor 한 개가 있던 기준값으로 돌아왔다.

사용자가 재인증을 시작한 상태에서도 슬롯과 Job Actor는 0개였다. 완료 콜백을 동시에 두 번 보내자 두 요청이 같은 완료 영수증을 반환했다. 새 Actor가 세션 버전 2를 사용했고, 최초 Run ID와 리비전으로 결과를 조회했다. PostgreSQL에서 Run 1건, Attempt 2건, 완료 인증 요청 1건, 결과 1건을 확인했다. 시험 DB에 이전 스키마로 저장한 완료 Run의 결과도 마이그레이션 뒤 조회됐다.
