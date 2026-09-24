# Repository Guidelines

## Scope

This repository builds a standalone, Copilot-only Go HTTP service with an embedded React management UI. There is no native plugin ABI, plugin loader/store, external CPA checkout dependency, or unrelated OAuth provider.

- `internal/copilot/`: tested Copilot auth, catalogue, inference, normalization, and streaming. All upstream operations go through `hostClient`.
- `internal/transport/`: the single owner of outbound HTTP/proxy/redirect policy. Do not add direct HTTP clients elsewhere.
- `internal/app/`: credential persistence, budget admission, scheduling, management/API handlers, and usage.
- `internal/conversion/`, `internal/protocol/`, and supporting packages: client wire conversion only. Claude/OpenAI/Codex names here are protocol/model identifiers, not enabled upstream providers.
- `web/`: focused React management UI, compiled to one HTML file and embedded with `go:embed`.
- `cmd/copilot-proxy/`: executable entrypoint. Build artifacts belong in ignored `bin/`.

## Validation

```bash
make test
make vet
make race
make integration
cd web && bun run verify
```

Use `make build` to produce the single binary, or `make release` for supported platforms. Go 1.26+ and Bun 1.3.14 are build requirements; CGO and a C compiler are not. Run gofmt on changed Go files. Tests must use synthetic credentials and injected transports, never paid live inference.

## Safety and compatibility

Never log tokens, Authorization headers, device codes, raw credential JSON, request bodies, or upstream response bodies. Preserve same-origin, canonical-path, caller-header, terminal-event, and opaque-state validation. Protect credentials, configuration and budget state with atomic private-file writes.

Budget protection is independent of manual disablement. A confirmed over-99% Additional Budget observation remains latched across errors and restarts until a verified new period allows recovery. Missing or stale data must never release a latch. Reject late observations after credential replacement.

Preserve the three client API formats, Copilot credential migration fields, priority and account proxy settings. Do not reintroduce other OAuth providers, external executors, dynamic plugins, or Home distributed dispatch.

README.md owns setup and user-visible behavior. PI_GITHUB_COPILOT_COMPARISON.md owns upstream evidence and upgrade procedure. Do not commit completed plans or point-in-time pass snapshots; local plans and migration backups stay in ignored `.local/`. Retain third-party MIT notices in LICENSES/.
