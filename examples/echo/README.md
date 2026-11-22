# ATE Echo 예제

Go로 작성한 Echo를 ARM64 이미지로 빌드하고, `workerctl`로 ATE Actor를 배포한다. `POST /invoke`는 Kubernetes Gateway를 거치며, Gateway는 ATE Router의 서버 인증서를 검증하고 클라이언트 인증서를 제시한다. 같은 환경에서 CRD를 적용하려면 컨트롤러 배포 가이드를 따른다. [Echo 소스](../../cmd/echo/main.go) · [이미지 레시피](../../Dockerfile.echo) · [kubectl apply 예제](../../docs/ate-controller.md)

## 개발계 준비

- Kubernetes에 ATE와 ARM64 gVisor 환경, Gateway API CRD와 agentgateway가 설치되어 있어야 한다. 예제 GatewayClass 이름은 `agentgateway`이고 데이터 플레인 Service는 `ClusterIP`다. 시험 조합은 Kubernetes v1.36.6, Gateway API v1.6.1, agentgateway v1.7.0, ATE v1.0.0이다. [ATE 설치](https://ate.dev/docs/guides/operator-install/) · [agentgateway 설치](https://agentgateway.dev/docs/kubernetes/latest/documentation/install/helm/)
- Atespace를 만들 수 있는 운영자 계정과 `kubectl ate`를 준비한다. ATE Control과 Router의 서버 CA, Router에서 허가한 클라이언트 인증서와 키가 필요하다. 검증 스크립트는 POSIX 셸, `kubectl`, Go, `curl`을 사용한다. 새 이미지를 빌드하고 게시할 때는 Docker Buildx와 `jq`가 추가로 필요하다. [ATE 인증](https://ate.dev/docs/guides/authentication/) · [Gateway에서 백엔드 mTLS 설정](https://gateway-api.sigs.k8s.io/guides/user-guides/tls/)
- 예제는 `wp-echo-example` 네임스페이스와 Atespace, `echo-arm64-dev` 풀을 사용한다. 풀의 CPU 가용 한도는 60m이며 예제 Actor는 20m를 요청한다. 공유 설치에서는 기존 Actor 템플릿이 새 풀에 배치될 수 있는지 먼저 확인한다. [WorkerPool](workerpool.yaml) · [WorkerProfile](ate-profile.yaml)

저장소 루트에서 아래 명령을 실행한다. 환경 변수는 사용할 개발계와 로컬 인증 파일을 가리킨다.

```sh
export KUBE_CONTEXT=your-development-context
export ATE_ADMIN_TOKEN_FILE=/path/to/admin.token
export ATE_CA=/path/to/ate-ca.pem
export ROUTER_CA=/path/to/router-ca.pem
export ROUTER_CERT=/path/to/router-client.crt
export ROUTER_KEY=/path/to/router-client.key

k() { kubectl --context "$KUBE_CONTEXT" "$@"; }
ate_admin() { kubectl ate --context "$KUBE_CONTEXT" --token-file "$ATE_ADMIN_TOKEN_FILE" "$@"; }

k create -f examples/echo/namespace.yaml
ate_admin create atespace wp-echo-example
ate_admin create access-policy -a wp-echo-example -f examples/echo/access-policy.yaml

umask 077
export ATE_TOKEN_FILE="$(mktemp)"
k -n wp-echo-example create token echo-deployer --audience api.ate-system.svc \
  --duration=20m > "$ATE_TOKEN_FILE"
k -n wp-echo-example create secret tls router-client --cert="$ROUTER_CERT" --key="$ROUTER_KEY"
k -n ate-system create configmap wp-echo-example-router --from-file="ca.crt=$ROUTER_CA"
k create -f examples/echo/workerpool.yaml
k create -f examples/echo/gateway.yaml
```

예제 Service는 `ate-system`의 `app=atenet-router` Pod와 8443 포트를 선택한다. 설치 값이 다르면 Service와 서버 DNS 이름을 바꾼다. CLI의 TLS 서버 이름을 바꿀 때는 `ATE_SERVER_NAME` 또는 `ROUTER_SERVER_NAME`을 설정한다. Router Pod가 기존 ingress 정책으로 격리되어 있으면 예제 Gateway Pod만 허용하는 규칙을 추가한다. [Gateway와 Router Service](gateway.yaml)

```sh
k create -f examples/echo/router-ingress.yaml
```

## 빌드·배포·호출

```sh
ECHO_REPOSITORY=registry.example.test/workers/echo sh examples/echo/run.sh
```

스크립트는 ARM64 이미지를 빌드해 레지스트리에 올리고 digest를 워커 명세에 넣는다. ATE Control과 Router를 로컬로 포트포워드해 `workerctl deploy`를 호출한다. 이어서 Gateway에서 HTTP 200과 아래 JSON을 확인하고, 클라이언트가 보낸 대상 헤더를 라우트가 덮어쓰는지도 검사한다. 첫 출력 줄은 `actor`와 `actorUID`를 담은 배포 결과이고 마지막 줄은 Echo 응답이다. [워커 명세](ate-worker.yaml) · [실행 스크립트](run.sh)

```json
{"message":"echo-example"}
```

이미 게시한 이미지를 배포할 때는 실제 digest를 넣은 `ECHO_IMAGE`로 실행한다. 같은 digest와 프로필을 다시 사용하면 Actor UID가 유지된다.

```sh
ECHO_IMAGE="registry.example.test/workers/echo@$DIGEST" sh examples/echo/run.sh
```

## 예제 정리

스크립트는 종료할 때 자신이 시작한 포트포워드를 닫는다. 이번 예제에서 생성한 리소스는 아래 순서로 정리한다. `ECHO_ACTOR`에는 배포 결과의 `actor` 값을 넣는다. 여러 리비전을 배포했다면 예제 Atespace의 목록을 확인하고 각 Actor와 대응하는 ActorTemplate을 정리한다.

```sh
k -n wp-echo-example delete httproute echo --ignore-not-found
ate_admin get actors -a wp-echo-example
ate_admin delete actor -a wp-echo-example "$ECHO_ACTOR"
ate_admin delete actor-template -a wp-echo-example "$ECHO_ACTOR"
ate_admin delete atespace wp-echo-example
k delete -f examples/echo/router-ingress.yaml --ignore-not-found
k delete -f examples/echo/gateway.yaml
k delete -f examples/echo/workerpool.yaml
k -n ate-system delete configmap wp-echo-example-router
k delete namespace wp-echo-example
rm -f -- "$ATE_TOKEN_FILE"
```

## 검증 결과

`/usr/local/bin/echo-worker`를 진입점으로 지정한 Linux ARM64 OCI 이미지로 `run.sh`를 두 번 실행했다. Gateway가 두 번 모두 HTTP 200과 Echo JSON을 반환했고, 같은 digest를 다시 배포했을 때 Actor UID가 유지됐다. 시험 리소스를 정리한 뒤 기존 Actor 17개의 UID·상태·Worker 배정을 대조했다.
