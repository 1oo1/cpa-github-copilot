# Repository Guidelines

## Project Structure & Module Organization

This repository builds the `github-copilot-go` Go `c-shared` plugin for
CLIProxyAPI v7. Production code and colocated `*_test.go` files live in `src/`:
`main.go` exposes the C ABI, `service.go` registers capabilities, `auth.go`
handles GitHub Device Flow, and `models.go` discovers and routes models.
`executor.go`, `headers.go`, `endpoints.go`, and `stream.go` implement inference
requests, validation, and SSE forwarding. Keep host callback wrappers in
`host.go` and shared RPC types in `types.go`.

`registry.json` defines the Plugin Store entry; `src/compatibility.json` contains
the built-in compatibility rules. Generated libraries belong in `bin/` and must
not be committed. Consult `README.md` for user-facing setup and
`PI_GITHUB_COPILOT_COMPARISON.md` for compatibility rationale.

## Build, Test, and Development Commands

- `make test` — run the complete Go test suite.
- `make vet` — run `go vet ./...`.
- `make build` — clean `bin/` and cross-build Linux amd64 and arm64 libraries.
- `make build-native` — create a c-shared library for the current platform.
- `make integration` — build native output and load it through CLIProxyAPI.
- `go test -race ./...` — check concurrent auth, cache, and streaming paths.

Use Go 1.26+, CGO, and a C compiler. Linux builds require the matching cross
compiler or Docker. Development depends on an adjacent `../CLIProxyAPI` checkout
because `go.mod` uses a local `replace` directive.

## Coding Style & Security Boundaries

Run `gofmt -w src/*.go` before submitting. Use tabs, standard Go naming
(`PascalCase` exported, `camelCase` internal), and descriptive tests such as
`TestExecuteRejectsNoncanonicalInferencePath`. Keep behavior in its owning file
and prefer small local helpers.

All upstream HTTP and streaming traffic must use `hostClient`; do not introduce
direct `http.Client` calls. Never log or surface tokens, authorization headers,
device codes, `RawJSON`, `StorageJSON`, or upstream response bodies.

## Testing, Commits, and Pull Requests

Use Go's `testing` package and fake host callbacks. Cover success, malformed
payloads, non-2xx responses, cancellation, and secret redaction for changed
paths. Run `make test` and `make vet`; also run race and integration checks when
touching ABI, authentication, routing, or streaming.

Use concise imperative commit subjects, for example `Fix device flow retry
scheduling`. Pull requests should describe behavior and security/configuration
impact, list tests run, and link relevant issues. Screenshots are only needed for
host-facing UI changes.
