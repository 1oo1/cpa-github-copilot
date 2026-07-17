# Repository Guidelines

## Project Structure & Module Organization

This repository is a single-package Go `c-shared` plugin for CLIProxyAPI. `main.go` implements the C ABI entry points, while `service.go` registers and dispatches plugin capabilities. Authentication and GitHub Device Flow live in `auth.go`; model discovery and routing are in `models.go`; request execution, headers, endpoint validation, and SSE handling are split across `executor.go`, `headers.go`, `endpoints.go`, and `stream.go`. Host callback wrappers are centralized in `host.go`, and shared RPC types are in `types.go`.

Tests are colocated as `*_test.go`. `integration_test.go` loads the compiled library through CLIProxyAPI's real plugin loader. Generated libraries belong in `bin/`; design and usage documentation are in `PLAN.md` and `README.md`.

## Build, Test, and Development Commands

- `make test`: run all Go unit and contract tests.
- `make vet`: run Go's static analyzer.
- `make build`: build the platform-specific shared library in `bin/`.
- `make integration`: build and load the library with the real CLIProxyAPI host.
- `go test -race ./...`: check concurrent login, cache, and streaming paths.
- `make clean`: remove generated binaries.

Use Go 1.26+, CGO, and a working C compiler. `go.mod` currently replaces CLIProxyAPI with the adjacent `../CLIProxyAPI` checkout.

## Coding Style & Naming Conventions

Run `gofmt -w *.go` before submitting changes; Go formatting uses tabs. Follow standard Go naming: exported identifiers use `PascalCase`, internal identifiers use `camelCase`, and tests use `TestBehaviorName`. Keep capability logic in its owning file and prefer small helpers over cross-cutting abstractions.

All upstream HTTP and stream operations must use `hostClient`; do not introduce direct `http.Client` calls. Never log credentials, `RawJSON`, `StorageJSON`, authorization headers, device codes, or upstream response bodies.

## Testing Guidelines

Use Go's `testing` package and fake host callbacks for upstream behavior. Cover success, malformed responses, non-2xx statuses, cancellation, and secret-redaction paths. There is no enforced coverage floor; preserve or improve the current roughly 71% coverage for touched code. Run unit, race, vet, and integration checks for changes affecting ABI, auth, routing, or streaming.

## Commit & Pull Request Guidelines

This checkout has no Git metadata, so no repository-specific commit convention can be inferred. Use concise imperative subjects, such as `Fix device flow retry scheduling`. Pull requests should explain behavior changes, security implications, configuration changes, and tests run. Link relevant issues; screenshots are unnecessary unless a host-facing UI changes.
