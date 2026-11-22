# 공개 알파 클러스터 설정

이 가이드는 공개 알파의 `workerctl → AX → ATE` 배포와 `Kubernetes Gateway → ATE Router → Actor` 호출에 해당한다. 운영자가 Gateway와 Router 연결 정책을 관리하고, `workerctl`이 앱 HTTPRoute를 게시한다. ATE 직접 설치의 지원 조합은 첫 운영 릴리스의 인수 시험에서 확정한다. [아키텍처](architecture.md) · [설치 인수 기준](requirements.md#기능과-인수-기준)

## 사전 준비

- 동작 중인 AX와 ATE, 전용 테스트 Atespace, 요청 CPU·메모리를 수용할 샌드박스 WorkerPool이 필요하다. 호환성 시험은 `go.mod`에 고정한 AX 버전을 기준으로 한다. [AX 설치](https://github.com/google/ax) · [의존성 버전](../go.mod)
- `Gateway.spec.tls.backend.clientCertificateRef`를 사용하려면 Gateway API v1.5 이상의 Standard Channel CRD가 필요하다. 지원할 Gateway 구현체가 `GatewayBackendClientCertificate`, `BackendTLSPolicy`, HTTP 메서드 매칭과 `RequestHeaderModifier`를 실제로 구현하는지 확인한다. CRD는 리소스 유효성 검증에 사용하고 연결은 실제 요청으로 확인한다. [Gateway API TLS](https://gateway-api.sigs.k8s.io/guides/user-guides/tls/) · [구현별 지원 기능](https://gateway-api.sigs.k8s.io/docs/implementations/versions/v1.5/)
- Gateway 네임스페이스에는 Router가 허가한 Gateway 클라이언트 인증서를 담은 `kubernetes.io/tls` Secret을 둔다. Router Service의 네임스페이스에는 Router 서버 인증서의 CA를 `ca.crt`에 담은 ConfigMap을 둔다. `BackendTLSPolicy.spec.validation.hostname`은 Router 서버 인증서의 DNS 이름과 일치해야 한다. [ATE ingress 인증 구조](https://ate.dev/docs/security/threat-model/)
- 배포 주체에는 워커 네임스페이스의 HTTPRoute에 `get`, `create`, `patch`를 부여한다. 로컬 AX 연결에는 포트 포워딩을 사용한다. 원격 AX는 TLS를 사용하며 별도 신뢰 체인이나 인증서 이름이 필요하면 `-ax-ca`와 `-ax-server-name`을 지정한다.

## 예제 리소스

[Gateway 예제](../examples/cluster/gateway.yaml)는 전용 개발 Gateway, Router Service의 `BackendTLSPolicy`, 교차 네임스페이스 `ReferenceGrant`를 정의한다. GatewayClass, Router Service와 DNS 이름, 인증서 참조를 시험 설치 값으로 바꾼다. Router Service에 적용할 `BackendTLSPolicy`는 해당 Service 운영자와 조율한다.

예제는 개발용 HTTP 리스너다. 인터넷에 노출하는 리스너에는 HTTPS와 호출자 인증·인가를 구성한다. Gateway의 연결 신원으로 Router에 접근할 수 있는 범위는 라우트 게시 권한으로 관리한다. 신뢰된 배포 주체에게만 해당 리스너의 HTTPRoute 쓰기 권한을 부여한다. [HTTPRoute 연결 규칙](https://gateway-api.sigs.k8s.io/reference/api-types/httproute/) · [ReferenceGrant](https://gateway-api.sigs.k8s.io/reference/api-types/referencegrant/)

## 배포와 연결 확인

1. Gateway의 `Accepted`와 `Programmed`를 확인한다. 준비된 Atespace와 WorkerPool에 태그에 있는 이미지 레시피와 Echo 배포 명세를 사용한다. [공개 알파 Echo 예제](https://github.com/imprun/worker-platform/blob/v0.1.0-alpha.1/README.md#deploy-the-example)
2. HTTPRoute의 지정한 Gateway·리스너에 대해 현재 `metadata.generation`과 같은 `observedGeneration`의 `Accepted=True`, `ResolvedRefs=True`를 확인한다. [Gateway API 상태와 세대](https://gateway-api.sigs.k8s.io/docs/concepts/troubleshooting/)
3. `POST /invoke`로 Echo JSON을 확인한다. 다른 시험 Actor를 가리키는 `ate-target-actor`를 요청에 포함해 다시 호출하고 같은 Echo가 처리하는지 확인한다.
4. 라우트가 준비됐는데 요청에 실패하면 Gateway가 Router에 올바른 클라이언트 인증서를 제시하는지, Router CA와 서버 이름을 확인하는지, 양쪽에서 허용된 연결 신원을 쓰는지 점검한다.

롤백할 리비전의 AX Task는 사용 기간 동안 유지한다. 기존 라우트가 가리키는 Task와 보존한 롤백 대상은 운영자가 확인하고 정리한다.
