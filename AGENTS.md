# Repository Guidelines

## Scope and Ownership

This repository builds the `github-copilot-go` Go `c-shared` plugin for
CLIProxyAPI v7. Production code and colocated tests live in `src/`; generated
libraries live in ignored `bin/`. `registry.json` is the Plugin Store entry.

Keep changes in the file that owns the behavior: C ABI/lifecycle, auth,
model routing, endpoint/header validation, execution, streaming, host callbacks,
or shared RPC types. All upstream HTTP and streaming must use `hostClient`;
never add a direct `http.Client`.

## Validation

```bash
make test
make vet
go test -race ./...
make integration
```

Run test and vet for every change. Also run race and integration when changing
ABI, auth, routing, raw HTTP, or streaming. `make integration` builds the native
library and loads it through CLIProxyAPI. Go 1.26+, CGO, a C compiler, and the
adjacent `../CLIProxyAPI` checkout are required; cross-platform build details
belong in `Makefile`.

## Code and Security

Run `gofmt -w src/*.go`. Use standard Go naming, small local helpers, and
descriptive tests such as `TestExecuteRejectsNoncanonicalInferencePath`.
Use Go's `testing` package with fake host callbacks. Changed paths should cover
success, malformed input, non-2xx responses, cancellation, and secret redaction
where applicable.

Never log or surface tokens, authorization headers, device codes, `RawJSON`,
`StorageJSON`, or upstream response bodies. Preserve endpoint, same-origin,
canonical-path, caller-header, and terminal-event validation.

## Commits and Documentation

Use concise imperative commit subjects. Pull requests should describe behavior,
security/configuration impact, tests run, and linked issues; screenshots are only
needed for host-facing UI changes.

[README.md](README.md) owns setup and user-visible behavior.
[PI_GITHUB_COPILOT_COMPARISON.md](PI_GITHUB_COPILOT_COMPARISON.md) owns the
compatibility source hierarchy and upgrade procedure. Do not duplicate source
inventories, test matrices, completed plans, or point-in-time pass snapshots in
Markdown.
