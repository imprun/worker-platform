# Contributing

Run these checks before submitting a change.

```sh
gofmt -w cmd internal
go vet ./...
go test -race ./...
```

The tests cover manifest validation, AX readiness, the actor target header, release reuse, and the HTTP worker contract. Unit tests use an in-process AX-compatible server. Include the observed `Gateway` and `HTTPRoute` conditions and the AX and Gateway controller versions when reporting a cluster interoperability issue. Remove credentials and private hostnames from logs.

For a new trigger or lifecycle feature, describe its failure and ownership semantics before changing the public manifest contract. See [architecture](docs/architecture.md).
