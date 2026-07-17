# CLIProxyAPI GitHub Copilot Plugin Plan

## 1. Goal and compatibility boundary

Build a Go `c-shared` CLIProxyAPI ABI v1 plugin that exposes a GitHub Copilot
subscription as the provider `github-copilot`. The plugin will provide all three
capabilities required for a provider-native integration:

- `auth_provider`: GitHub device authorization, credential parsing, Copilot
  session refresh, and refresh scheduling.
- `model_provider`: account-scoped model discovery from Copilot `/models`.
- `executor`: non-streaming and streaming inference through the account's
  Copilot API endpoint.

The implementation targets the current CLIProxyAPI v7 plugin ABI and uses its
public `sdk/pluginabi`, `sdk/pluginapi`, and `sdk/translator` packages. The
plugin will accept the CLIProxyAPI formats `openai`, `openai-response`, and
`claude` and use the built-in translator registry when the selected Copilot
model requires a different upstream protocol.

## 2. Project layout

- `main.go`: C ABI entry points, RPC dispatch, lifecycle configuration, and
  registration.
- `host.go`: typed wrappers for `host.http.*` and `host.stream.*` callbacks.
- `auth.go`: credential schema, `auth.parse`, device flow state machine, token
  exchange, refresh, and safe auth metadata construction.
- `endpoints.go`: GitHub host validation, broker URL derivation, and safe
  `proxy-ep` parsing.
- `models.go`: `/models` decoding, filtering, endpoint selection, model
  metadata conversion, and per-auth route caching.
- `executor.go`: payload protocol conversion, Copilot headers, endpoint
  selection, non-streaming execution, and guarded raw HTTP execution.
- `stream.go`: upstream stream bridge, SSE framing, stateful response
  translation, downstream chunk emission, and deterministic cleanup.
- `*_test.go`: pure-function, RPC contract, fake-host HTTP, and stream tests.
- `Makefile` and `README.md`: reproducible build/install/configuration usage.

## 3. Authentication design

### 3.1 Persisted credential

Persist provider-owned JSON with semantic field names:

```json
{
  "type": "github-copilot",
  "github_access_token": "...",
  "copilot_session_token": "...",
  "refresh_after": 0,
  "github_host": "github.com",
  "api_base_url": "https://api.individual.githubcopilot.com",
  "account": "login",
  "models": []
}
```

`StorageJSON` is the only place that carries tokens. Host-managed `Metadata`,
`Attributes`, log fields, errors, and labels will contain only non-secret
provider/account information. Legacy pi-style fields (`refresh`, `access`,
`expires`, `enterpriseUrl`, `availableModelIds`) will be accepted and
normalized when parsing existing files.

### 3.2 Explicit parser ownership

`auth.parse` will first decode the top-level JSON and inspect `type`.

- Missing, malformed, or a different `type`: return `{Handled:false}`.
- `type == "github-copilot"`: always return `{Handled:true}`.
- A recognized file with missing required long-lived credentials is returned
  as a disabled auth record instead of silently falling through to another
  parser. This preserves the explicit ownership decision while preventing use
  of an incomplete credential.

No recognized parse failure will be represented only as an RPC error, because
that would erase the `Handled` decision at the host adapter boundary.

### 3.3 Device flow

1. `auth.login.start` calls `POST /login/device/code` through
   `host.http.do` with form encoding and the configured public OAuth client ID.
2. Validate all response types and require a normalized HTTPS verification URL
   on the configured GitHub host.
3. Generate a cryptographically random, host-compatible OAuth state. Keep the
   device code only in an in-memory session keyed by that state. Return a
   verification URL with `user_code` prefilled where supported.
4. `auth.login.poll` implements a locked state machine with first-poll delay,
   `authorization_pending`, both `slow_down` variants, expiry, duplicate-poll
   suppression, and retryable network/server errors.
5. On GitHub token success, exchange it through
   `GET /copilot_internal/v2/token`, also through `host.http.do`.
6. Best-effort enable known model policies, discover the account model set,
   fetch a non-secret GitHub login for labeling, and return the complete auth.

The long-lived GitHub token is never sent to a model endpoint. The short-lived
Copilot session token is scheduled for refresh five minutes before the broker's
`expires_at`, with a floor for unusually short sessions.

### 3.4 Refresh

`auth.refresh` decodes `StorageJSON`, exchanges the GitHub token through the
host bridge, validates the entire broker response, discovers models, and only
then returns replacement auth data. Any failure returns no partial credential,
so the host retains the prior stored credential.

## 4. Endpoint and trust rules

- Default GitHub host: `github.com`.
- Enterprise host is configuration-only, normalized to a hostname, and must
  not contain credentials, paths, ports, IP literals, or non-HTTPS schemes.
- GitHub.com broker: `https://api.github.com/copilot_internal/v2/token`.
- Enterprise broker: `https://api.<host>/copilot_internal/v2/token`.
- Prefer the Copilot session token's `proxy-ep`; accept only a syntactically
  valid hostname with a GitHub/Copilot suffix and convert leading `proxy.` to
  `api.`. Otherwise use the documented account base URL fallback.
- Raw executor HTTP requests may target only the credential's resolved API
  origin, preventing bearer-token forwarding to an arbitrary URL.

## 5. Model discovery and provider capability

`model.for_auth` calls `<api-base>/models` through `host.http.do`, validates the
response, and exposes only models satisfying all of:

- `model_picker_enabled == true`;
- `policy.state != "disabled"`;
- `capabilities.supports.tool_calls != false`;
- at least one supported or safely inferred inference endpoint.

Model metadata will include context/output limits, input modalities, reasoning
levels/budgets, and the account-supported endpoint. The selected route is
stored with the credential and cached by `(auth_id, model_id)`. A discovery
failure may use a non-empty last-known-good credential catalog. A structurally
valid empty catalog is preserved as an account with no available models and
does not fall back to inferred routes; an invalid response remains an error.

`model.static` returns no account-independent models because the executor scope
is `oauth`; this avoids advertising models before an eligible credential has
been discovered. Declaring `model_provider` alongside `auth_provider` and the
OAuth executor satisfies the host's provider-native registration path.

## 6. Execution and protocol routing

1. Resolve the model route from credential storage, then the per-auth cache,
   then conservative model-name inference.
2. Normalize the request format aliases to `openai`, `openai-response`, or
   `claude`.
3. If the client format differs from the model endpoint, require a registered
   CLIProxyAPI request transformer and convert the structured payload.
4. Set the resolved model and stream flag using JSON APIs.
5. Build centralized Copilot headers: static client identity, Bearer session
   token, API version, `Openai-Intent`, last-message `X-Initiator`, and
   conditional `Copilot-Vision-Request`. Caller headers cannot replace
   `Authorization` or the upstream origin.
6. Send only through `host.http.do` or `host.http.do_stream`.
7. Translate responses back to the client-requested format. Preserve upstream
   HTTP status through ABI error `http_status` without embedding response bodies
   or credentials in error text.

Streaming execution will open the upstream stream synchronously, then run an
asynchronous forwarding goroutine. For cross-format streams it will frame
complete SSE events, retain translator state across events, emit translated
frames with `host.stream.emit`, and close both host-owned streams on every
success/error/cancellation path.

## 7. Logging and secret handling

- The plugin will not log `RawJSON`, `StorageJSON`, request authorization
  headers, GitHub access tokens, Copilot session tokens, device codes, or
  upstream response bodies.
- Errors use stable categories and HTTP status only. JSON decoder errors are
  wrapped with field/context names but never the source payload.
- Tests will scan emitted host-log requests and error strings with sentinel
  secrets. Structured operational diagnostics use `host.log` for lifecycle,
  auth freshness, model discovery/cache, routing, and stream outcomes; request
  and response body logging remains the host bridge's responsibility and policy.

## 8. Verification matrix

### Unit and contract tests

- Parser: unrelated/malformed files are `Handled:false`; recognized valid and
  invalid files are `Handled:true`; legacy migration; no secret metadata.
- Endpoints: GitHub.com, Enterprise fallback, valid individual/business
  `proxy-ep`, malformed token host, IP/userinfo/scheme rejection.
- Device flow: form payload, first-poll delay, pending, both `slow_down` paths,
  duplicate polls, expiry, denial, retryable transport failure, and hostile
  verification URI.
- Token exchange: non-2xx, missing fields, expiry safety margin, discovery
  failure preserving the old credential, and no token in errors.
- Models: schema validation, picker/policy/tool filtering, route priority,
  limits/modalities/reasoning mapping, cached fallback, and empty response.
- Headers: user/agent decision for all three protocols, tool-result behavior,
  image detection, protected authorization, and model normalization.
- Execution: endpoint per protocol, request and non-stream response translation,
  upstream status propagation, origin guard, and host bridge usage.
- Streaming: split/multiple SSE frames, pass-through, translated frames,
  upstream error, downstream emit error, and both stream-close callbacks.

### Build and host integration

- `go test ./...`
- `go test -race ./...` (business logic; c-shared build remains a separate gate)
- `go vet ./...`
- `make build` with `-buildmode=c-shared`
- Load the built library with CLIProxyAPI's actual plugin host and verify
  registration, `auth.parse` ownership, model-provider/executor registration,
  and a fake-upstream non-stream/stream RPC round trip.

## 9. Plan review

### Risks found and mitigations

1. **Global executor formats cannot express a per-model upstream protocol.**
   Mitigation: resolve the route per auth/model inside the executor and use the
   public built-in translator registry in both directions.
2. **The RPC host strips the Go `HTTPClient` interface.**
   Mitigation: every network operation carries the injected
   `host_callback_id` into the explicit `host.http.*` RPC bridge.
3. **Returning an RPC error from `auth.parse` loses parser ownership.**
   Mitigation: recognized invalid files return `Handled:true` and a disabled
   record; only unrecognized inputs return `Handled:false`.
4. **The management login response does not expose plugin metadata such as the
   device user code.** Mitigation: prefill `user_code` in the validated browser
   URL and retain polling data only in plugin memory.
5. **HTTP stream chunks are not guaranteed to align to SSE events.**
   Mitigation: bounded incremental SSE framing before stateful translation.
6. **A model catalog can differ across accounts.** Mitigation: persist routes
   in each credential and key the in-memory cache by auth ID and model ID.
7. **Dynamic URLs plus bearer tokens create an exfiltration risk.**
   Mitigation: strict endpoint derivation and same-origin enforcement for the
   raw HTTP executor path.
8. **Refresh and discovery can partially succeed.** Mitigation: validate and
   assemble a complete replacement before returning it; never persist a partial
   session.

### Review conclusion

The capability set matches CLIProxyAPI's provider-native lifecycle, every
upstream network path is routed through the host bridge, parser ownership is
explicit, secret material is isolated to `StorageJSON` and Authorization
headers, and the three Copilot wire protocols have a defined conversion and
streaming strategy. No unresolved design blocker remains; implementation can
proceed in the order above.
