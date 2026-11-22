# ATE 직접 배포

`workerctl`은 `HTTPWorker` v1alpha2와 운영자가 준비한 `WorkerProfile`로 ATE Actor를 배포한다. Kubernetes Gateway의 동기 `POST` 호출은 ATE Router를 거쳐 해당 Actor로 전달된다. 실행 설정이 같은 명세는 리비전과 Actor를 재사용한다. [아키텍처](architecture.md)

## Echo 예제

전용 Atespace, ARM64 gVisor 풀, mTLS Gateway를 구성하고 Echo를 빌드·배포·호출하는 명령이 예제 디렉터리에 있다. [Echo 전체 예제](../examples/echo/README.md)

## 실행 설정

운영자가 Atespace와 배포 주체의 `editor` 권한을 준비한다. Kubernetes 배포 주체에는 지정한 WorkerPool과 Gateway의 `get`, 앱 네임스페이스 HTTPRoute의 `get`, `create`, `patch`를 부여한다. [ATE 인증](https://ate.dev/docs/guides/authentication/) · [ATE 접근 정책](https://ate.dev/docs/guides/authorization/)

프로필은 CPU·메모리, 풀, gVisor 아키텍처와 egress를 정한다. `egress`를 생략하면 목적지를 허용하지 않는 빈 정책을 적용한다. 목적지가 필요하면 `protocol`에 `http` 또는 `tlsPassthrough`, `hostname`에 정확한 DNS 이름, `port`에 목적지 포트를 설정한다. 이미지의 진입점은 포트 80에서 `GET /readyz`와 선언한 `POST`를 제공한다.

풀의 `metadata.labels`에 고유한 `workerplatform.imprun.dev/pool-id`를 설정한다. 풀을 다시 만들 때는 새 값을 사용한다. `HTTPWorker`와 `WorkerProfile`의 네임스페이스는 같아야 한다. Gateway는 Router가 허용한 클라이언트 인증서를 제시하며 `BackendTLSPolicy`의 CA와 DNS 이름으로 Router를 확인한다. [ATE 풀 선택](https://ate.dev/docs/reference/api-guide/) · [Gateway API TLS](https://gateway-api.sigs.k8s.io/guides/user-guides/tls/)

## 명세 전환

이전 `HTTPWorker` 파일을 옮길 때는 `apiVersion`을 `workerplatform.imprun.dev/v1alpha2`로 지정한다. 이미지와 트리거는 `HTTPWorker`에 두고, Atespace와 CPU·메모리·egress는 같은 네임스페이스의 `WorkerProfile`에 작성한다. 워커의 `spec.profile`에 프로필 이름을 넣고 `-f`와 `-profile`을 함께 전달한다. 기존 라우트가 있는 환경에서는 별도 개발 호스트에서 검증한다. 직접 배포 CLI는 자신과 소유권이 일치하는 HTTPRoute만 갱신한다. [워커 명세](../examples/echo/ate-worker.yaml) · [프로필 명세](../examples/echo/ate-profile.yaml)

## 배포와 호출

`workerctl render`로 유효 설정을 미리 확인한다. `-output`은 `template`, `actor`, `policy`, `route` 또는 `all`을 받는다. ATE 리소스는 Control API 요청 형식이며, HTTPRoute는 Kubernetes 리소스다.

```sh
go run ./cmd/workerctl render \
  -f examples/echo/ate-worker.yaml -profile examples/echo/ate-profile.yaml \
  -output route
```

아래는 ATE Control과 Router 각각에 로컬 포트포워드를 연 경우다. 포트포워드 대상과 `-server-name` 값은 실제 설치에서 확인한 서비스와 인증서 이름을 사용한다. `ATE_TOKEN_FILE`은 전용 Atespace의 배포 토큰 파일이다. `ROUTER_CERT`와 `ROUTER_KEY`는 CLI가 비공개 준비 요청에 사용하는 클라이언트 인증 정보다.

```sh
kubectl --context "$KUBE_CONTEXT" -n ate-system port-forward svc/api 17443:443
# 다른 터미널에서 실행
kubectl --context "$KUBE_CONTEXT" -n ate-system port-forward svc/wp-echo-example-router 18443:443
```

```sh
go run ./cmd/workerctl deploy \
  -f examples/echo/ate-worker.yaml -profile examples/echo/ate-profile.yaml \
  -kube-context "$KUBE_CONTEXT" \
  -ate-address 127.0.0.1:17443 -ate-server-name api.ate-system.svc \
  -ate-ca "$ATE_CA" -ate-token-file "$ATE_TOKEN_FILE" \
  -router-url https://127.0.0.1:18443 -router-server-name atenet-router.ate-system.svc \
  -router-ca "$ROUTER_CA" -router-cert "$ROUTER_CERT" -router-key "$ROUTER_KEY"
```

CLI는 풀·아키텍처를 검증하고 ActorTemplate, Actor와 egress 정책을 준비한다. 배정된 Worker와 mTLS 준비 응답을 확인한 뒤 HTTPRoute를 갱신한다. 결과의 `status=published`는 지정한 리스너의 현재 Route 세대에서 `Accepted=True`와 `ResolvedRefs=True`, Gateway의 `Programmed=True`를 확인했다는 뜻이다.

개발 Gateway의 ClusterIP Service를 로컬로 포트포워드한 뒤 앱 URL을 호출한다.

```sh
kubectl --context "$KUBE_CONTEXT" -n wp-echo-example port-forward svc/echo 18080:80
# 다른 터미널에서 실행
curl -fsS -H 'Host: echo.example.test' -H 'Content-Type: application/json' \
  -d '{"message":"ate-direct-arm64-mtls"}' http://127.0.0.1:18080/invoke
```

새 이미지로 배포할 때는 같은 경로의 명세에서 digest를 바꾼다. 후보가 준비되기 전에 실패하면 기존 HTTPRoute가 유지된다. 보존된 리비전으로 롤백하려면 이전 명세와 프로필로 다시 배포한다. CLI는 다른 배포자가 소유한 라우트와 불일치한 기존 ATE 리소스를 거부한다. 게시 이후 Gateway에서 문제가 발생한 경우 운영자가 이전 명세를 다시 배포할 수 있다. 지속 조정과 리비전 정리의 자동화는 [운영 설계](architecture.md#리비전-게시와-롤백)에 따른다.

## 개발계 검증 기록

2026년 11월 23일 별도 ARM64 gVisor 풀에서 ATE Control API로 Echo를 배포했다. 시험 조합은 Kubernetes v1.36.6, Gateway API v1.6.1, agentgateway v1.7.0, ATE v1.0.0이다. 전용 풀의 예약 가용 CPU는 60m였고 기존 템플릿의 최소 요청량은 70m였다.

| 검증 | 관찰 결과 |
| --- | --- |
| 배치 | ATE가 시험 풀의 ARM64 gVisor Worker에 Echo Actor를 배정했다. 같은 명세를 다시 적용했을 때 Actor UID가 유지됐다. |
| Gateway 호출 | 현재 세대의 Gateway·HTTPRoute·BackendTLSPolicy 상태를 확인했다. `POST /invoke`가 HTTP 200과 `{"message":"ate-direct-arm64-mtls"}`를 반환했다. 클라이언트가 시험 범위의 다른 대상 값을 보내도 같은 Echo 응답이 왔다. |
| Router mTLS | 같은 Router TLS 엔드포인트에서 인증서가 없는 접속은 `certificate required`로 거부됐다. 허가된 인증서로 읽은 준비 응답은 HTTP 200이었다. |
| 격리 | 기존 Actor 17개의 UID, 상태와 Worker 배정을 시험 전후에 대조했다. 모두 같은 값을 유지했다. |
