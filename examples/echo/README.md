# ATE Echo 예제

Echo 앱은 JSON의 `message`를 읽고 `message`와 `version`을 반환한다. 공통 Actor 런타임이 OCI 레지스트리에서 Echo를 로드하고 HTTP 요청을 앱에 전달한다. Gateway는 mTLS로 ATE Router에 연결한다. [Echo 앱 소스](app/main.go) · [공통 런타임](../../cmd/actor-runtime/main.go) · [앱 배포 설계](../../docs/architecture.md#공통-런타임의-앱-로드)

## 공통 런타임으로 Echo 패키지 배포

먼저 개발계에 CRD, `worker-controller`, 전용 ARM64 gVisor 풀, Gateway를 설치한다. ATE Worker가 런타임 이미지를 당겨올 레지스트리와 Actor가 읽을 패키지 레지스트리를 준비한다. Actor는 `WorkerProfile.spec.egress`의 `tlsPassthrough` 정책으로 패키지 레지스트리의 HTTPS 호스트와 포트에 접근한다. 개발계는 ATE egress에서 읽기 전용 HTTPS 주소를 사용했다. 비공개 CA를 사용하면 `WorkerProfile.spec.appRegistryCA`에 서버 신뢰에 사용할 인증서 PEM을 넣는다. [개발계 컨트롤러 설치](../../docs/ate-controller.md#개발계-설치)

저장소 루트에서 런타임 이미지를 한 번 게시하고, Linux ARM64용 Echo 실행 파일로 v1·v2 OCI 패키지를 각각 게시한다. `crane`이 앱 패키지를 게시할 수 있도록 로컬 레지스트리 인증을 준비한다. v2에는 전환 중 기존 응답을 확인할 수 있도록 4초짜리 시작 점검을 넣는다. [런타임 Dockerfile](../../Dockerfile.actor-runtime) · [패키지 빌더](../../scripts/build-app-package.py)

```sh
RUNTIME_REPOSITORY=registry.example.test/workers/actor-runtime
PACKAGE_HOST=packages.example.test
PACKAGE_REPOSITORY="$PACKAGE_HOST/workers/echo"
build_dir="$(mktemp -d)"

docker buildx build --platform linux/arm64 -f Dockerfile.actor-runtime \
  -t "$RUNTIME_REPOSITORY:echo-example" --push .
runtime_image="$RUNTIME_REPOSITORY@$(crane digest "$RUNTIME_REPOSITORY:echo-example")"

publish_package() {
  version="$1"
  delay="$2"
  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
    -ldflags "-s -w -X main.version=$version -X main.readyDelay=$delay" \
    -o "$build_dir/echo-$version" ./examples/echo/app
  python3 scripts/build-app-package.py "$build_dir/echo-$version" "$build_dir/$version"
  crane push "$build_dir/$version" "$PACKAGE_REPOSITORY:$version"
}

publish_package v1 0s
publish_package v2 4s
package_v1="$PACKAGE_REPOSITORY@$(crane digest "$PACKAGE_REPOSITORY:v1")"
package_v2="$PACKAGE_REPOSITORY@$(crane digest "$PACKAGE_REPOSITORY:v2")"
```

전용 개발계의 새 `HTTPWorker`에서 시작한다. `packaged.yaml`의 `WorkerProfile`에 런타임 digest와 registry egress를 한 번 설정한 뒤 `appPackage`만 v1 → v2 → v1로 바꾼다. 기본 예제는 패키지 레지스트리의 443 포트를 사용한다. [패키지 배포 명세](packaged.yaml)

```sh
k() { kubectl --context "$KUBE_CONTEXT" "$@"; }
ns=wp-echo-example

apply_package() {
  sed -e "s|runtimeImage: .*|runtimeImage: $runtime_image|" \
      -e "s|hostname: packages.example.test|hostname: $PACKAGE_HOST|" \
      -e "s|appPackage: .*|appPackage: $1|" examples/echo/packaged.yaml |
    k apply -f -
}

wait_package() {
  generation="$(k -n "$ns" get httpworker echo -o jsonpath='{.metadata.generation}')"
  k -n "$ns" wait --for="jsonpath={.status.active.generation}=$generation" \
    --timeout=120s httpworker/echo
}

actor_uid() { k -n "$ns" get httpworker echo -o jsonpath='{.status.active.actorUID}'; }

apply_package "$package_v1"
wait_package
first_actor="$(actor_uid)"
```

별도 터미널에서 Gateway Service의 로컬 포트를 연다. 전환을 확인하는 동안 이 포트포워드를 유지한다.

```sh
gateway_service="$(kubectl --context "$KUBE_CONTEXT" -n wp-echo-example get svc \
  -l gateway.networking.k8s.io/gateway-name=echo -o jsonpath='{.items[0].metadata.name}')"
kubectl --context "$KUBE_CONTEXT" -n wp-echo-example port-forward "svc/$gateway_service" 8080:80
```

같은 URL을 호출한다. v2가 시작 점검을 하는 동안에는 기존 v1이 응답한다. `wait_package` 이후에는 v2가 응답하고, 롤백 뒤에는 같은 URL과 첫 v1 Actor UID로 돌아온다.

```sh
call_echo() {
  curl -fsS -H 'Host: echo.example.test' -H 'Content-Type: application/json' \
    --data '{"message":"echo-example"}' http://127.0.0.1:8080/invoke
}

call_echo
apply_package "$package_v2"
call_echo
wait_package
call_echo
apply_package "$package_v1"
wait_package
test "$(actor_uid)" = "$first_actor"
call_echo
```

패키지는 `app.json`의 `contractVersion: v1`과 실행 가능한 `entrypoint`를 제공한다. 런타임은 진입점의 `--check`가 성공한 뒤 `/readyz`를 공개한다. 각 호출에서 진입점은 표준 입력의 `{"input":{"message":"echo-example"}}`을 처리하고 표준 출력에 `{"message":"echo-example","version":"v1"}`처럼 JSON 하나를 쓴다. Job 호출은 같은 입력에 `runId`, `attempt`, 필요한 경우 `siteSession`을 포함한다.

같은 OCI 패키지로 단일 Job을 접수할 수 있다. Echo는 Job 결과에 `runId`와 `attempt`를 돌려준다. Worker 장애 재현용 입력 `waitSeconds`는 첫 Attempt만 최대 75초 대기시킨다. [Echo Job 설정과 재시도 검증](../../docs/job-api.md)

사이트 세션을 연결하면 Echo는 만료된 모의 쿠키에 재인증을 요청한다. 갱신 후에는 새 Actor에서 처리에 사용한 `siteSessionVersion`을 결과에 포함한다. [HITL 재인증과 v1 리비전 재개](../../docs/site-session-reauth.md)

시험을 마치면 포트포워드를 종료하고 아래 명령으로 컨트롤러를 중단한 뒤 소유한 배포 선언을 제거한다. 이어 예제 Atespace의 Actor와 ActorTemplate을 정리한다. [예제 리소스 정리 순서](#예제-정리)

```sh
k -n "$ns" delete deployment worker-controller
k delete -f examples/echo/packaged.yaml
```

## 개발계 준비

이 절부터는 HTTP 러너와 Echo를 한 이미지에 담은 독립 검증 서버로 ATE 호출 경로를 확인한다. 서버와 테스트는 `examples/echo/standalone`에 있다. [독립 서버 소스](standalone/main.go) · [이미지 레시피](../../Dockerfile.echo)

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

### 공통 런타임과 Echo 앱 패키지

별도 ARM64 gVisor 풀에서 런타임 이미지 digest `sha256:b689b5c190cd…`를 고정하고, OCI 패키지 digest만 `3fc553ef89a9…` → `18c6390a6793…` → `3fc553ef89a9…`로 바꿨다. 두 ActorTemplate에서 같은 런타임 이미지와 각각의 앱 digest를 확인했다.

| 적용 | 같은 Gateway URL에서 관찰한 결과 |
| --- | --- |
| v2 배포 | 새 Actor가 `RESUMING`인 동안 v1 응답을 10회 확인했다. 새 Actor가 `RUNNING`이고 준비 확인을 통과한 뒤 v2 응답을 받았다. |
| v1 롤백 | 첫 v1 Actor UID `932ee36d-be13-40fe-b089-b5bf8008e2ae`와 같은 HTTPRoute UID를 확인했다. 롤백 전후 Actor·ActorTemplate UID 목록이 같았다. |
| 반복 전환 | 같은 URL에서 `v1 → v1 → v2 → v2 → v1`을 관찰했고, 다섯 요청 모두 HTTP 200이었다. |

초기 v2 후보에는 13초짜리 시작 점검을 넣었다. 20m CPU 프로필에서 패키지 로드와 점검이 준비 제한을 넘겼고 Gateway는 v1을 유지했다. 실행 파일의 디버그 정보를 제외하고 시작 점검을 4초로 조정한 뒤 같은 런타임으로 전환을 확인했다.

시험용 Atespace와 풀을 정리했다. 시험 전후 기존 Actor 17개의 UID·상태·Worker 배정이 일치했다.

### 독립 이미지

`/usr/local/bin/echo-worker`를 진입점으로 지정한 Linux ARM64 OCI 이미지로 `run.sh`를 두 번 실행했다. Gateway가 두 번 모두 HTTP 200과 Echo JSON을 반환했고, 같은 digest를 다시 배포했을 때 Actor UID가 유지됐다. 시험 리소스를 정리한 뒤 기존 Actor 17개의 UID·상태·Worker 배정을 대조했다.
