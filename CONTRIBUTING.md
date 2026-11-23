# Contributing

Run these checks before submitting a change.

```sh
gofmt -w cmd internal
go vet ./...
go test -race ./...
```

The tests cover manifest validation, ATE readiness, the Actor target header, revision reuse, and the HTTP worker contract. Deployment tests use a fake ATE Control client. Include the observed `Gateway` and `HTTPRoute` conditions and the ATE and Gateway controller versions when reporting a cluster interoperability issue. Remove credentials and private hostnames from logs.

CI runs the PostgreSQL Job tests with a real PostgreSQL service. The ATE smoke entry point checks for an integration change before contacting the dedicated test environment. The setup and the previous PoC evidence are documented in [테스트 기준과 PoC 검증 재사용](docs/testing.md).

For a new trigger or lifecycle feature, describe its failure and ownership semantics before changing the public manifest contract. See [architecture](docs/architecture.md).
