# Architecture

Worker Platform gives versioned HTTP workers a stable Gateway API endpoint. The initial deployer is a CLI so GitOps and operator workflows can invoke the same implementation. `HTTPWorker` is a versioned file format read by `workerctl`.

| Component | Responsibility |
| --- | --- |
| Kubernetes Gateway | Accept HTTP requests at the declared hostname and route. Set the Actor target and use mTLS to ATE. |
| `workerctl` | Validate the manifest, derive the AX Task name, wait for readiness, and publish the owned HTTPRoute. |
| AX | Manage the release's Task and report the Actor identity and readiness. |
| ATE | Run the Actor in a sandbox on a Worker and route requests to it. |
| Application | Process the request and return its HTTP response. |

## Release identity and publication

1. A release name hashes the worker namespace and name, Atespace, and AX Task specification. The OCI image digest, resources, and optional AX Gateway for egress are part of that specification.
2. The deployer reads the current HTTPRoute to record its identity, generation, and owner. It then creates or reads the expected AX Task, confirms the stored specification matches, and requests resume for a suspended Task.
3. The deployer waits for `Running`, an assigned Actor UID, and `Ready=True`. A timeout, Task failure, or identity mismatch ends the deployment with the existing HTTPRoute still selected.
4. The deployer creates or patches the HTTPRoute. The patch tests the Route UID, generation, and ownership annotation atomically. An intervening publication causes a conflict. CI can serialize desired-state changes for each worker to preserve the order of commits.
5. The Gateway controller reconciles the HTTPRoute asynchronously. Its status and an HTTP smoke request confirm that the route became usable. The previous Task stays available for the operator's rollback and retention policy.

The generated rule matches one hostname and one exact `POST` path. It sets `ate-target-actor` to the trusted target. The request flows from the Gateway directly to ATE, and the CLI does not need to run after publication. [AX runner contract](https://github.com/google/ax/blob/main/docs/runner.md) · [ATE Router](https://github.com/agent-substrate/substrate/blob/main/docs/api-guide.md)

## Worker contract

The image exposes `/usr/local/bin/ax-task-runner`, serves port 80, and returns HTTP 200 on `/healthz` and on `/readyz` after it can accept work. It handles the declared trigger path. The Echo example implements that HTTP contract without workspace dependencies.

Rollbacks reuse available Tasks. Keep the desired number of old revisions available, allow for their resource usage, and explicitly retire old Tasks after the rollback window. The initial CLI leaves old Task ownership and lifecycle with the operator.

## Durable asynchronous work

The planned async contract accepts an idempotency key, returns an execution ID, and stores the payload and release identity before acknowledging admission. Results are read by execution ID. A durable lease and monotonically increasing attempt number let a replacement worker recover after a crash and fence stale result writes. The release that accepted a request remains pinned through retries, even if newer HTTP traffic is routed to a later release. A worker application with external side effects needs its own idempotency at that boundary.

This contract can be implemented by a worker runtime and a shared durable store on the same direct Gateway-to-Actor path. Admission, claim, retry and result lookup belong to that contract. The first milestone exposes synchronous HTTP triggers.

## Milestones

- **Alpha** — Deploy and roll back digest-pinned workers through an HTTPRoute, with tests for readiness and route ownership.
- **Cluster conformance** — Run the echo upgrade and rollback against a documented GatewayClass, including mTLS and rejection of a forged Actor target.
- **Lifecycle management** — Add reported status, an explicit revision retention policy, and a continuous Kubernetes reconciler when the file contract stabilizes.
- **Durable triggers** — Define and test admission, worker loss, retry, and result reads using release-pinned execution IDs.
