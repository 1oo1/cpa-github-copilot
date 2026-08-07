package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestParseCompatibilityManifestRejectsUnsafeOrUnknownFields(t *testing.T) {
	for _, raw := range []string{
		`{"schema_version":1,"generated_at":"2026-08-06T00:00:00Z","models":{"gpt-4.1":{"base_url":"https://attacker.example"}}}`,
		`{"schema_version":1,"generated_at":"2026-08-06T00:00:00Z","models":{"gpt-4.1":{"headers":{"Authorization":"stolen"}}}}`,
		`{"schema_version":1,"generated_at":"2026-08-06T00:00:00Z","models":{"gpt-4.1":{"headers":{"Host":"attacker.example"}}}}`,
		`{"schema_version":1,"generated_at":"2026-08-06T00:00:00Z","models":{"gpt-4.1":{"headers":{"X-Unknown":"value"}}}}`,
		`{"schema_version":1,"generated_at":"2026-08-06T00:00:00Z","models":{"gpt-4.1":{"headers":{"User-Agent":"valid\r\nAuthorization: stolen"}}}}`,
		`{"schema_version":1,"generated_at":"2026-08-06T00:00:00Z","models":{"gpt-4.1":{"format":"custom"}}}`,
	} {
		if _, errParse := parseCompatibilityManifest([]byte(raw)); errParse == nil {
			t.Fatalf("unsafe manifest was accepted: %s", raw)
		}
	}
}

func TestApplyCompatibilityManifestIncludesApprovedHeaders(t *testing.T) {
	manifest, errParse := parseCompatibilityManifest([]byte(`{
		"schema_version":1,
		"generated_at":"2026-08-06T00:00:00Z",
		"models":{"gpt-4.1":{
			"id":"gpt-4.1",
			"api":"openai-completions",
			"provider":"github-copilot",
			"headers":{
				"User-Agent":"GitHubCopilotChat/remote",
				"Editor-Version":"vscode/remote",
				"Editor-Plugin-Version":"copilot-chat/remote",
				"Copilot-Integration-Id":"remote-chat"
			}
		}}
	}`))
	if errParse != nil {
		t.Fatal(errParse)
	}
	models := applyCompatibilityManifest([]storedModel{{ID: "gpt-4.1", Format: formatOpenAI}}, manifest)
	headers := models[0].CompatibilityHeaders
	if headers["User-Agent"] != "GitHubCopilotChat/remote" || headers["Copilot-Integration-Id"] != "remote-chat" {
		t.Fatalf("compatibility headers = %#v", headers)
	}
}

func TestApplyCompatibilityManifestOnlyOverridesDiscoveredModels(t *testing.T) {
	manifest, errParse := parseCompatibilityManifest([]byte(`{
		"schema_version": 1,
		"generated_at": "2026-08-06T00:00:00Z",
		"models": {
			"claude-fable-5": {
				"format": "claude",
				"context_window": 1000000,
				"max_output_tokens": 128000,
				"reasoning_levels": ["minimal", "low", "medium", "high", "xhigh", "max"],
				"force_adaptive_thinking": true,
				"supports_temperature": false,
				"supports_eager_tool_input_streaming": false,
				"supports_xhigh_effort": true
			},
			"not-in-account": {"format": "openai-response"}
		}
	}`))
	if errParse != nil {
		t.Fatal(errParse)
	}
	models := applyCompatibilityManifest([]storedModel{
		{ID: "claude-fable-5", Format: formatOpenAI, ContextWindow: 100_000, MaxOutputTokens: 8_192},
		{ID: "gpt-4.1", Format: formatOpenAI},
	}, manifest)
	if len(models) != 2 {
		t.Fatalf("models = %#v", models)
	}
	fable := models[0]
	if fable.Format != formatClaude || fable.ContextWindow != 1_000_000 || fable.MaxOutputTokens != 128_000 ||
		strings.Join(fable.ReasoningLevels, ",") != "minimal,low,medium,high,xhigh,max" {
		t.Fatalf("fable = %#v", fable)
	}
	if fable.ForceAdaptiveThinking == nil || !*fable.ForceAdaptiveThinking ||
		fable.SupportsTemperature == nil || *fable.SupportsTemperature ||
		fable.SupportsEagerToolInputStreaming == nil || *fable.SupportsEagerToolInputStreaming ||
		fable.SupportsXHighEffort == nil || !*fable.SupportsXHighEffort {
		t.Fatalf("fable compat = %#v", fable)
	}
	if models[1].ID != "gpt-4.1" || models[1].Format != formatOpenAI {
		t.Fatalf("ordinary model changed = %#v", models[1])
	}
}

func TestCompatibilityManifestMustNotPredateBuiltin(t *testing.T) {
	manifest, errParse := parseCompatibilityManifest([]byte(`{
		"schema_version": 1,
		"generated_at": "2026-08-05T23:59:59Z",
		"models": {}
	}`))
	if errParse != nil {
		t.Fatal(errParse)
	}
	if compatibilityManifestIsUsable(manifest, time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("older remote manifest was accepted")
	}
}

func TestCompatibilityManifestExplicitMetadataOverridesStaticFallbacks(t *testing.T) {
	manifest, errParse := parseCompatibilityManifest([]byte(`{
		"schema_version":1,
		"generated_at":"2026-08-07T00:00:00Z",
		"models":{"claude-opus-4.8":{"context_window":200000,"reasoning_levels":["low","high"]}}
	}`))
	if errParse != nil {
		t.Fatal(errParse)
	}
	models := applyCompatibilityManifest([]storedModel{{
		ID: "claude-opus-4.8", Format: formatClaude, ContextWindow: 1_000_000,
	}}, manifest)
	infos := modelInfos(models)
	if len(infos) != 1 || infos[0].ContextLength != 200_000 || infos[0].Thinking == nil ||
		strings.Join(infos[0].Thinking.Levels, ",") != "low,high" {
		t.Fatalf("model info = %#v", infos)
	}
}

func TestLoadCompatibilityManifestCachesCredentialFreeFixedOriginResponse(t *testing.T) {
	now := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	remote := []byte(`{"schema_version":1,"generated_at":"2026-08-07T00:00:00Z","models":{"gpt-4.1":{"context_window":256000}}}`)
	bridge := &fakeBridge{handler: func(method string, payload any) (any, error) {
		if method != pluginabi.MethodHostHTTPDo {
			t.Fatalf("method = %s", method)
		}
		req := payload.(rpcHostHTTPRequest)
		if req.URL != remoteCompatibilityURL || req.Method != http.MethodGet {
			t.Fatalf("request = %#v", req)
		}
		if http.Header(req.Headers).Get("Authorization") != "" {
			t.Fatal("compatibility request included authorization")
		}
		return pluginapi.HTTPResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Etag": []string{`"compat-1"`}},
			Body:       remote,
		}, nil
	}}
	service := newPluginService(bridge)
	config := service.loadedConfig()
	config.EnableRemoteCompatibility = true
	service.config = config
	service.now = func() time.Time { return now }
	storage := copilotStorage{}
	manifest, changed := service.loadCompatibilityManifest(hostClient{bridge: bridge}, &storage)
	if !changed || manifest.GeneratedAt != now || storage.CompatibilityCheckedAt != now.UnixMilli() || storage.CompatibilityETag != `"compat-1"` {
		t.Fatalf("manifest = %#v, storage = %#v, changed = %t", manifest, storage, changed)
	}
	if len(storage.CompatibilityManifest) == 0 {
		t.Fatal("remote manifest was not cached")
	}
	assertLogsExclude(t, bridge.snapshotLogs(), string(remote))

	service.loadCompatibilityManifest(hostClient{bridge: bridge}, &storage)
	if calls := bridge.snapshot(); len(calls) != 1 {
		t.Fatalf("fresh cache made %d host calls", len(calls))
	}
}

func TestCompatibilityETagIsBoundedAndSafe(t *testing.T) {
	if got := safeCompatibilityETag(` "compat-1" `); got != `"compat-1"` {
		t.Fatalf("etag = %q", got)
	}
	for _, value := range []string{strings.Repeat("x", 257), "valid\nInjected: true"} {
		if got := safeCompatibilityETag(value); got != "" {
			t.Fatalf("unsafe etag was accepted: %q", got)
		}
	}
}

func TestLoadCompatibilityManifestFallsBackToCachedManifest(t *testing.T) {
	now := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	cached := []byte(`{"schema_version":1,"generated_at":"2026-08-07T00:00:00Z","models":{"gpt-4.1":{"context_window":256000}}}`)
	bridge := &fakeBridge{handler: func(_ string, _ any) (any, error) {
		return pluginapi.HTTPResponse{StatusCode: http.StatusServiceUnavailable}, nil
	}}
	service := newPluginService(bridge)
	config := service.loadedConfig()
	config.EnableRemoteCompatibility = true
	service.config = config
	service.now = func() time.Time { return now }
	storage := copilotStorage{
		CompatibilityManifest:  cached,
		CompatibilityCheckedAt: now.Add(-5 * time.Hour).UnixMilli(),
		CompatibilityETag:      `"compat-1"`,
	}
	manifest, changed := service.loadCompatibilityManifest(hostClient{bridge: bridge}, &storage)
	if changed || manifest.GeneratedAt != now.Add(-24*time.Hour) {
		t.Fatalf("manifest = %#v, changed = %t", manifest, changed)
	}
	calls := bridge.snapshot()
	if len(calls) != 1 || http.Header(calls[0].Payload.(rpcHostHTTPRequest).Headers).Get("If-None-Match") != `"compat-1"` {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestLoadCompatibilityManifestCanBeDisabled(t *testing.T) {
	bridge := &fakeBridge{handler: func(method string, _ any) (any, error) {
		t.Fatalf("unexpected host call %s", method)
		return nil, nil
	}}
	service := newPluginService(bridge)
	config := service.loadedConfig()
	config.EnableRemoteCompatibility = false
	service.config = config
	manifest, changed := service.loadCompatibilityManifest(hostClient{bridge: bridge}, &copilotStorage{})
	if changed || manifest.GeneratedAt != builtinCompatibility.GeneratedAt {
		t.Fatalf("manifest = %#v, changed = %t", manifest, changed)
	}
}
