package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestParseDiscoveredModelsFiltersAndMapsCapabilities(t *testing.T) {
	toolFalse := false
	_ = toolFalse
	raw := mustJSON(t, map[string]any{"data": []any{
		remoteModelFixture("gpt-5.4", true, "", true, []string{"/responses"}),
		remoteModelFixture("claude-sonnet-4.6", true, "", true, []string{"/v1/messages"}),
		remoteModelFixture("disabled", true, "disabled", true, []string{"/chat/completions"}),
		remoteModelFixture("hidden", false, "", true, []string{"/chat/completions"}),
		remoteModelFixture("no-tools", true, "", false, []string{"/chat/completions"}),
	}})
	models, errParse := parseDiscoveredModels(raw)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if len(models) != 2 || models[0].ID != "claude-sonnet-4.6" || models[0].Format != formatClaude || models[1].Format != formatOpenAIResponse {
		t.Fatalf("models = %#v", models)
	}
	infos := modelInfos(models)
	if len(infos) != 2 || infos[0].ContextLength != 100000 || infos[0].MaxCompletionTokens != 10000 || infos[0].Thinking == nil {
		t.Fatalf("model infos = %#v", infos)
	}
}

func TestSelectModelFormatUsesSupportedEndpointAndInference(t *testing.T) {
	for _, test := range []struct {
		id        string
		endpoints []string
		want      string
	}{
		{id: "gpt-5.4", endpoints: []string{"/responses", "/chat/completions"}, want: formatOpenAIResponse},
		{id: "claude-sonnet-4.6", endpoints: []string{"/v1/messages"}, want: formatClaude},
		{id: "gpt-4.1", endpoints: []string{"/responses"}, want: formatOpenAIResponse},
		{id: "gpt-4.1", endpoints: nil, want: formatOpenAI},
		{id: "gpt-4.1", endpoints: []string{"/embeddings"}, want: ""},
	} {
		if got := selectModelFormat(test.id, test.endpoints); got != test.want {
			t.Fatalf("selectModelFormat(%q, %#v) = %q, want %q", test.id, test.endpoints, got, test.want)
		}
	}
}

func TestResolveModelRouteRejectsModelOutsideDiscoveredCatalog(t *testing.T) {
	service := newPluginService(nil)
	storage := copilotStorage{Models: []storedModel{{ID: "gpt-4.1", Format: formatOpenAI}}}
	if route := service.resolveModelRoute("auth", "gpt-5.4", storage); route.Path != "" {
		t.Fatalf("unexpected inferred route outside account catalog: %#v", route)
	}
}

func TestResolveModelRouteRejectsInferenceAfterEmptyDiscovery(t *testing.T) {
	service := newPluginService(nil)
	storage := copilotStorage{ModelsFetchedAt: time.Now().UnixMilli()}
	if route := service.resolveModelRoute("auth", "gpt-5.4", storage); route.Path != "" {
		t.Fatalf("unexpected inferred route after empty discovery: %#v", route)
	}
}

func TestModelsForAuthUsesFreshCredentialCache(t *testing.T) {
	bridge := &fakeBridge{handler: func(method string, _ any) (any, error) {
		t.Fatalf("unexpected host call %s", method)
		return nil, nil
	}}
	service := newPluginService(bridge)
	service.now = func() time.Time { return time.Unix(30_000, 0).UTC() }
	storage := copilotStorage{
		Type:                pluginIdentifier,
		GitHubAccessToken:   "ghu_secret",
		CopilotSessionToken: "tid=session;proxy-ep=proxy.individual.githubcopilot.com",
		GitHubHost:          "github.com",
		ModelsFetchedAt:     service.now().UnixMilli(),
		Models:              []storedModel{{ID: "gpt-4.1", Name: "GPT 4.1", Format: formatOpenAI}},
	}
	raw, errModels := service.modelsForAuth(mustJSON(t, rpcAuthModelRequest{AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-1", StorageJSON: mustJSON(t, storage)}}))
	if errModels != nil {
		t.Fatal(errModels)
	}
	result := decodePluginResult[pluginapi.ModelResponse](t, raw)
	if len(result.Models) != 1 || result.Models[0].ID != "gpt-4.1" || len(result.AuthUpdate.StorageJSON) != 0 {
		t.Fatalf("model response = %#v", result)
	}
	if route := service.resolveModelRoute("auth-1", "gpt-4.1", copilotStorage{}); route.Format != formatOpenAI {
		t.Fatalf("cached route = %#v", route)
	}
}

func TestModelsForAuthFallsBackToStaleNonEmptyCache(t *testing.T) {
	bridge := &fakeBridge{handler: func(method string, payload any) (any, error) {
		if method != pluginabi.MethodHostHTTPDo {
			t.Fatalf("method = %s", method)
		}
		req := payload.(rpcHostHTTPRequest)
		if !strings.HasSuffix(req.URL, "/models") {
			t.Fatalf("URL = %s", req.URL)
		}
		return pluginapi.HTTPResponse{StatusCode: http.StatusServiceUnavailable}, nil
	}}
	service := newPluginService(bridge)
	service.now = func() time.Time { return time.Unix(40_000, 0).UTC() }
	storage := copilotStorage{
		Type: pluginIdentifier, GitHubAccessToken: "ghu", CopilotSessionToken: "tid=x;proxy-ep=proxy.individual.githubcopilot.com",
		GitHubHost: "github.com", ModelsFetchedAt: service.now().Add(-time.Hour).UnixMilli(),
		Models: []storedModel{{ID: "gpt-4.1", Name: "GPT 4.1", Format: formatOpenAI}},
	}
	raw, errModels := service.modelsForAuth(mustJSON(t, rpcAuthModelRequest{AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-1", StorageJSON: mustJSON(t, storage)}, HostCallbackID: "models"}))
	if errModels != nil {
		t.Fatal(errModels)
	}
	result := decodePluginResult[pluginapi.ModelResponse](t, raw)
	if len(result.Models) != 1 || result.Models[0].ID != "gpt-4.1" {
		t.Fatalf("fallback response = %#v", result)
	}
	logEntry := findLogEvent(t, bridge.snapshotLogs(), "models.discovery.fallback")
	if logEntry.Fields["failure_code"] != "model_discovery_http_error" || logEntry.Fields["cached_model_count"] != 1 {
		t.Fatalf("fallback log fields = %#v", logEntry.Fields)
	}
	assertLogsExclude(t, bridge.snapshotLogs(), storage.GitHubAccessToken, storage.CopilotSessionToken)
}

func TestDiscoverModelsAcceptsValidEmptyCatalog(t *testing.T) {
	bridge := &fakeBridge{handler: func(_ string, _ any) (any, error) {
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"data":[]}`)}, nil
	}}
	service := newPluginService(bridge)
	models, failure := service.discoverModels(hostClient{bridge: bridge}, copilotStorage{
		CopilotSessionToken: "tid=x;proxy-ep=proxy.individual.githubcopilot.com",
		GitHubHost:          "github.com",
	})
	if failure != nil || models == nil || len(models) != 0 {
		t.Fatalf("models = %#v, failure = %#v", models, failure)
	}
}

func remoteModelFixture(id string, picker bool, policy string, tools bool, endpoints []string) map[string]any {
	return map[string]any{
		"id": id, "name": id, "version": id + "-2026-01-01", "model_picker_enabled": picker,
		"supported_endpoints": endpoints, "policy": map[string]any{"state": policy},
		"capabilities": map[string]any{
			"family": "test-family",
			"limits": map[string]any{
				"max_context_window_tokens": 100000, "max_prompt_tokens": 90000, "max_output_tokens": 10000,
				"vision": map[string]any{"supported_media_types": []string{"image/png"}},
			},
			"supports": map[string]any{
				"tool_calls": tools, "vision": true, "adaptive_thinking": true,
				"reasoning_effort": []string{"low", "high"},
			},
		},
	}
}

func TestStoredModelsJSONContainsNoSessionOutsideStorageBoundary(t *testing.T) {
	model := storedModel{ID: "gpt-4.1", Name: "GPT", Format: formatOpenAI}
	raw, _ := json.Marshal(model)
	if strings.Contains(string(raw), "token") {
		t.Fatalf("model route unexpectedly has credential field: %s", raw)
	}
}
