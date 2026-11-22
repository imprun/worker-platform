# Cluster setup

The cluster operator owns the Gateway and the ATE Router's upstream TLS configuration. Worker Platform writes the application HTTPRoute using that configuration.

## Prerequisites

- A working AX install, an ATE Router, and sandbox-capable WorkerPool capacity for the requested CPU and memory. Use the AX version pinned in `go.mod` when testing compatibility. [AX setup](https://github.com/google/ax)
- Version 1.5 or later of the Gateway API Standard Channel for `Gateway.spec.tls.backend.clientCertificateRef`. Choose a Gateway implementation that supports the `GatewayBackendClientCertificate` feature, `BackendTLSPolicy`, `HTTPRouteMethodMatching`, and `RequestHeaderModifier`. A CRD accepting a field establishes schema support. Check your implementation and validate actual mTLS traffic before opening the endpoint. [Gateway API TLS guide](https://gateway-api.sigs.k8s.io/guides/user-guides/tls/)
- An operator-issued `kubernetes.io/tls` Secret with a Gateway client certificate that ATE authorizes and a ConfigMap containing the CA that signed the ATE Router certificate. Set `validation.hostname` to a name in that server certificate. [ATE ingress authentication](https://github.com/agent-substrate/substrate/blob/main/docs/architecture.md)
- A trusted namespace for workers. Grant the deployer `get`, `create`, and `patch` on `httproutes` in that namespace. Use a local port-forward for cleartext AX. For a remote AX connection, configure a TLS endpoint and use `-ax-ca` and `-ax-server-name` when you need custom trust or name validation.

## Reference manifests

[The example](../examples/cluster/gateway.yaml) defines a dedicated Gateway, a backend TLS policy for the Router Service, and a cross-namespace grant. Replace `YOUR_GATEWAY_CLASS`, the Service and DNS names, and the certificate references for your installation. If that Service already has a `BackendTLSPolicy`, coordinate with its owner so the Service has one intended TLS policy. An HTTP listener is shown for development. For an Internet-facing listener, terminate HTTPS and configure caller authentication in the Gateway or application.

Both the Gateway SSL client identity and the reference grant are trust-sensitive. Keep HTTPRoute write access limited to trusted publishers in the granting namespace. The Gateway's backend identity may be used for its other backends, so use a dedicated Gateway for this Router trust relationship. [Gateway API cross-namespace references](https://gateway-api.sigs.k8s.io/reference/api-types/referencegrant/)

## Verify the connection

1. Check `Accepted` and `Programmed` on the selected Gateway.
2. Deploy the [echo example](../README.md#deploy-the-example). Check `Accepted` and `ResolvedRefs` on its HTTPRoute and confirm that the status reflects its current generation.
3. Send `POST /invoke` and compare the response with the input. Repeat with a client-supplied `ate-target-actor` for a nonexistent Actor. The expected response is the same echo payload because the route overwrites the header.
4. If the route is accepted but the request fails, check whether the Gateway presented the accepted client identity, trusted the Router CA, and used the Router certificate's DNS name as SNI.
