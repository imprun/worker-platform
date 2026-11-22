# Worker Platform

**Run versioned HTTP workers behind Kubernetes Gateway API with AX and Agent Substrate.**

[![CI](https://github.com/imprun/worker-platform/actions/workflows/ci.yml/badge.svg)](https://github.com/imprun/worker-platform/actions/workflows/ci.yml)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

Declare a worker image by digest and give it an HTTPS or HTTP endpoint. `workerctl` makes the AX Task, waits for its Actor to become Ready, then publishes the HTTPRoute. Deploying a new image keeps the URL stable. Rolling back to a retained revision reuses its AX Task.

```text
Control plane: HTTPWorker file → workerctl → AX Task → ATE Actor
                                        └→ Kubernetes HTTPRoute

Request path:  Client → Kubernetes Gateway → ATE Router → Actor
```

**Status** — Alpha. The initial release supports one exact `POST` trigger per HTTPWorker. The CLI takes a manifest from your workstation or CI, and the Actor returns the result in the HTTP response. See [architecture and scope](docs/architecture.md) for the async execution design and verification boundaries.

## Try it locally

Install the Go version or newer declared in `go.mod`. Clone this repository and run the example worker in one terminal.

```sh
go run ./cmd/echo -listen 127.0.0.1:8080
```

In a second terminal, invoke the worker and render the deployment objects.

```sh
curl -fsS -H 'Content-Type: application/json' \
  -d '{"message":"hello"}' http://127.0.0.1:8080/invoke
go run ./cmd/workerctl render -f examples/echo/worker.yaml
```

The response is `{"message":"hello"}`. Rendering prints an AX Task and a Kubernetes HTTPRoute and runs locally. Use `-output route` or `-output task` for a single JSON document.

To install the CLI from the alpha release, run `go install github.com/imprun/worker-platform/cmd/workerctl@v0.1.0-alpha.1`.

## Deploy the example

1. Prepare AX, ATE, an Atespace and a WorkerPool. Set up a Gateway whose implementation supports upstream mTLS, and authorize that Gateway's trust and identity at the ATE Router. See [cluster setup](docs/cluster-setup.md) for the required Gateway API objects and checks.
2. Build [the example image](Dockerfile.echo) for the WorkerPool CPU architecture and push it to your registry. For example, use `docker buildx build --platform linux/arm64 -f Dockerfile.echo -t "$IMAGE_TAG" --push .`. Replace the example image reference in `worker.yaml` with the resulting `repository@sha256:digest` and set the Atespace, Gateway and hostname for your cluster.
3. Run `workerctl deploy` with an AX API endpoint and a kubeconfig permitted to manage HTTPRoutes in the worker namespace. This example uses a local AX port-forward and an explicit Kubernetes context.

   ```sh
   kubectl --context "$KUBE_CONTEXT" -n "$AX_NAMESPACE" port-forward svc/ax-server 18082:8080
   ```

   ```sh
   go run ./cmd/workerctl deploy -f examples/echo/worker.yaml \
     -ax-address 127.0.0.1:18082 -kube-context "$KUBE_CONTEXT"
   kubectl --context "$KUBE_CONTEXT" -n workers get httproute echo -o yaml
   ```

4. Wait for `Accepted=True` and `ResolvedRefs=True` on the HTTPRoute, then send the echo request to the Gateway hostname. When testing an HTTP listener by address, set `Host` to the configured hostname.

   ```sh
   curl -fsS -H 'Host: echo.example.test' -H 'Content-Type: application/json' \
     -d '{"message":"hello"}' "http://$GATEWAY_ADDRESS/invoke"
   ```

To roll back, deploy the earlier manifest while its AX Task remains available. [Contributing](CONTRIBUTING.md) describes the test suite and [security](SECURITY.md) explains the Gateway trust boundary and reporting process.
