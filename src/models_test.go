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
	models, errParse := parseDiscoveredModels(raw, false)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if len(models) != 2 || models[0].ID != "claude-sonnet-4.6" || models[0].Format != formatClaude || models[1].Format != formatOpenAIResponse {
		t.Fatalf("models = %#v", models)
	}
	infos := modelInfos(models)
	if len(infos) != 2 || infos[0].ContextLength != 1_000_000 || infos[0].MaxCompletionTokens != 10000 || infos[0].Thinking == nil {
		t.Fatalf("model infos = %#v", infos)
	}
}

func TestKnownCopilotModelsMatchPiCatalog(t *testing.T) {
	want := []string{
		"claude-fable-5", "claude-haiku-4.5", "claude-opus-4.5", "claude-opus-4.6", "claude-opus-4.7",
		"claude-opus-4.8", "claude-opus-5", "claude-sonnet-4", "claude-sonnet-4.5", "claude-sonnet-4.6",
		"claude-sonnet-5", "gemini-3.1-pro-preview", "gemini-3.5-flash", "gemini-3.6-flash", "gpt-4.1",
		"gpt-5-mini", "gpt-5.2", "gpt-5.2-codex", "gpt-5.3-codex", "gpt-5.4", "gpt-5.4-mini",
		"gpt-5.4-nano", "gpt-5.5", "gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "grok-4.5",
		"kimi-k2.7-code", "mai-code-1-flash-picker",
	}
	if strings.Join(knownCopilotModels, ",") != strings.Join(want, ",") {
		t.Fatalf("known models = %v, want %v", knownCopilotModels, want)
	}
}

func TestParseDiscoveredModelsAppliesStaticClaudeCompatibility(t *testing.T) {
	model := remoteModelFixture("claude-opus-4.8", true, "", true, []string{"/v1/messages"})
	capabilities := model["capabilities"].(map[string]any)
	supports := capabilities["supports"].(map[string]any)
	supports["adaptive_thinking"] = false
	raw := mustJSON(t, map[string]any{"data": []any{model}})
	models, errParse := parseDiscoveredModels(raw, false)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if len(models) != 1 || !models[0].AdaptiveThinking {
		t.Fatalf("models = %#v", models)
	}
	infos := modelInfos(models)
	if len(infos) != 1 || infos[0].Thinking == nil || !infos[0].Thinking.DynamicAllowed ||
		strings.Contains(strings.Join(infos[0].SupportedParameters, ","), "temperature") {
		t.Fatalf("model infos = %#v", infos)
	}

	for _, modelID := range []string{"gemini-3.6-flash", "gpt-4.1", "kimi-k2.7-code"} {
		infos := modelInfos([]storedModel{{ID: modelID, Format: formatOpenAI, ReasoningLevels: []string{"high"}}})
		if len(infos) != 1 || infos[0].Thinking != nil || strings.Contains(strings.Join(infos[0].SupportedParameters, ","), "reasoning_effort") {
			t.Fatalf("%s model info = %#v", modelID, infos)
		}
	}
}

func TestClaudeAdaptiveThinkingLevelsMatchPi(t *testing.T) {
	for _, test := range []struct {
		model string
		want  []string
	}{
		{model: "claude-fable-5", want: []string{"minimal", "low", "medium", "high", "xhigh", "max"}},
		{model: "claude-opus-4.6", want: []string{"minimal", "low", "medium", "high", "max"}},
		{model: "claude-opus-4.7", want: []string{"minimal", "low", "medium", "high", "xhigh", "max"}},
		{model: "claude-opus-4.8", want: []string{"minimal", "low", "medium", "high", "xhigh", "max"}},
		{model: "claude-opus-5", want: []string{"minimal", "low", "medium", "high", "xhigh", "max"}},
		{model: "claude-sonnet-4.6", want: []string{"minimal", "low", "medium", "high", "max"}},
		{model: "claude-sonnet-5", want: []string{"minimal", "low", "medium", "high", "xhigh", "max"}},
	} {
		t.Run(test.model, func(t *testing.T) {
			model := remoteModelFixture(test.model, true, "", true, []string{"/v1/messages"})
			supports := model["capabilities"].(map[string]any)["supports"].(map[string]any)
			supports["adaptive_thinking"] = false
			supports["reasoning_effort"] = []string{"low", "high"}
			models, errParse := parseDiscoveredModels(mustJSON(t, map[string]any{"data": []any{model}}), false)
			if errParse != nil || len(models) != 1 {
				t.Fatalf("models = %#v, error = %v", models, errParse)
			}
			if got := models[0].ReasoningLevels; strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("discovered levels = %v, want %v", got, test.want)
			}

			infos := modelInfos([]storedModel{{ID: test.model, Format: formatClaude}})
			if len(infos) != 1 || infos[0].Thinking == nil || strings.Join(infos[0].Thinking.Levels, ",") != strings.Join(test.want, ",") {
				t.Fatalf("cached model info = %#v, want levels %v", infos, test.want)
			}
		})
	}
}

func TestCopilotGPTThinkingLevelsMatchPi(t *testing.T) {
	for _, test := range []struct {
		model string
		want  []string
	}{
		{model: "gpt-5-mini", want: []string{"minimal", "low", "medium", "high"}},
		{model: "gpt-5.2", want: []string{"minimal", "low", "medium", "high", "xhigh"}},
		{model: "gpt-5.2-codex", want: []string{"minimal", "low", "medium", "high", "xhigh"}},
		{model: "gpt-5.3-codex", want: []string{"minimal", "low", "medium", "high", "xhigh"}},
		{model: "gpt-5.4", want: []string{"minimal", "low", "medium", "high", "xhigh"}},
		{model: "gpt-5.4-mini", want: []string{"minimal", "low", "medium", "high", "xhigh"}},
		{model: "gpt-5.4-nano", want: []string{"minimal", "low", "medium", "high", "xhigh"}},
		{model: "gpt-5.5", want: []string{"minimal", "low", "medium", "high", "xhigh"}},
		{model: "gpt-5.6-luna", want: []string{"minimal", "low", "medium", "high", "xhigh", "max"}},
		{model: "gpt-5.6-sol", want: []string{"minimal", "low", "medium", "high", "xhigh", "max"}},
		{model: "gpt-5.6-terra", want: []string{"minimal", "low", "medium", "high", "xhigh", "max"}},
	} {
		t.Run(test.model, func(t *testing.T) {
			model := remoteModelFixture(test.model, true, "", true, []string{"/responses"})
			supports := model["capabilities"].(map[string]any)["supports"].(map[string]any)
			supports["reasoning_effort"] = []string{"low", "high"}
			models, errParse := parseDiscoveredModels(mustJSON(t, map[string]any{"data": []any{model}}), false)
			if errParse != nil || len(models) != 1 || strings.Join(models[0].ReasoningLevels, ",") != strings.Join(test.want, ",") {
				t.Fatalf("models = %#v, error = %v", models, errParse)
			}

			infos := modelInfos([]storedModel{{ID: test.model, Format: formatOpenAIResponse}})
			if len(infos) != 1 || infos[0].Thinking == nil || infos[0].Thinking.ZeroAllowed ||
				strings.Join(infos[0].Thinking.Levels, ",") != strings.Join(test.want, ",") {
				t.Fatalf("cached model info = %#v, want levels %v", infos, test.want)
			}
		})
	}
}

func TestExtendedCopilotContextWindowsMatchPi(t *testing.T) {
	for _, modelID := range []string{
		"claude-fable-5", "claude-opus-4.6", "claude-opus-4.7", "claude-opus-4.8", "claude-opus-5",
		"claude-sonnet-4.6", "claude-sonnet-5", "gpt-5.3-codex", "gpt-5.4", "gpt-5.5",
	} {
		t.Run(modelID, func(t *testing.T) {
			format := inferModelFormat(modelID)
			model := remoteModelFixture(modelID, true, "", true, []string{endpointPath(format)})
			models, errParse := parseDiscoveredModels(mustJSON(t, map[string]any{"data": []any{model}}), false)
			if errParse != nil || len(models) != 1 || models[0].ContextWindow != 1_000_000 {
				t.Fatalf("models = %#v, error = %v", models, errParse)
			}

			infos := modelInfos([]storedModel{{ID: modelID, Format: format, ContextWindow: 100_000}})
			if len(infos) != 1 || infos[0].ContextLength != 1_000_000 {
				t.Fatalf("cached model info = %#v", infos)
			}
		})
	}

	infos := modelInfos([]storedModel{{ID: "gpt-4.1", Format: formatOpenAI, ContextWindow: 100_000}})
	if len(infos) != 1 || infos[0].ContextLength != 100_000 {
		t.Fatalf("ordinary model info = %#v", infos)
	}
}

func TestSelectModelFormatUsesSupportedEndpointAndInference(t *testing.T) {
	for _, test := range []struct {
		id        string
		endpoints []string
		want      string
	}{
		{id: "gpt-5.4", endpoints: []string{"/responses", "/chat/completions"}, want: formatOpenAIResponse},
		{id: "grok-4.5", endpoints: []string{"/responses", "/chat/completions"}, want: formatOpenAIResponse},
		{id: "claude-fable-5", endpoints: []string{"/v1/messages", "/chat/completions"}, want: formatClaude},
		{id: "claude-sonnet-4.6", endpoints: []string{"/v1/messages"}, want: formatClaude},
		{id: "claude-fable-5", endpoints: nil, want: formatClaude},
		{id: "claude-custom-5", endpoints: nil, want: formatOpenAI},
		{id: "claude-fable-6", endpoints: nil, want: formatOpenAI},
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

func TestCachedFableRouteMigratesToAnthropicMessages(t *testing.T) {
	service := newPluginService(nil)
	legacy := storedModel{ID: "claude-fable-5", Format: formatOpenAI}
	storage := copilotStorage{Models: []storedModel{legacy}}

	route := service.resolveModelRoute("auth", legacy.ID, storage)
	if route.Format != formatClaude || route.Path != "/v1/messages" || !route.AdaptiveThinking {
		t.Fatalf("legacy cached route = %#v", route)
	}

	infos := modelInfos(storage.Models)
	if len(infos) != 1 || infos[0].Thinking == nil || !infos[0].Thinking.DynamicAllowed {
		t.Fatalf("legacy cached model info = %#v", infos)
	}
}

func TestModelsForAuthUsesFreshCredentialCache(t *testing.T) {
	bridge := &fakeBridge{handler: func(method string, _ any) (any, error) {
		t.Fatalf("unexpected host call %s", method)
		return nil, nil
	}}
	service := newPluginService(bridge)
	config := service.loadedConfig()
	config.EnableRemoteCompatibility = false
	service.config = config
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

func TestModelsForAuthAppliesAndPersistsRemoteCompatibility(t *testing.T) {
	now := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	bridge := &fakeBridge{handler: func(method string, payload any) (any, error) {
		if method != pluginabi.MethodHostHTTPDo {
			t.Fatalf("method = %s", method)
		}
		req := payload.(rpcHostHTTPRequest)
		if req.URL != remoteCompatibilityURL {
			t.Fatalf("URL = %s", req.URL)
		}
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{
			"schema_version": 1,
			"generated_at": "2026-08-07T00:00:00Z",
			"models": {"gpt-4.1": {"context_window": 256000}}
		}`)}, nil
	}}
	service := newPluginService(bridge)
	config := service.loadedConfig()
	config.EnableRemoteCompatibility = true
	service.config = config
	service.now = func() time.Time { return now }
	storage := copilotStorage{
		Type:                pluginIdentifier,
		GitHubAccessToken:   "ghu_secret",
		CopilotSessionToken: "tid=session;proxy-ep=proxy.individual.githubcopilot.com",
		GitHubHost:          "github.com",
		ModelsFetchedAt:     now.UnixMilli(),
		Models:              []storedModel{{ID: "gpt-4.1", Format: formatOpenAI, ContextWindow: 128_000}},
	}
	raw, errModels := service.modelsForAuth(mustJSON(t, rpcAuthModelRequest{
		AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "auth-1", StorageJSON: mustJSON(t, storage)},
		HostCallbackID:   "models",
	}))
	if errModels != nil {
		t.Fatal(errModels)
	}
	result := decodePluginResult[pluginapi.ModelResponse](t, raw)
	if len(result.Models) != 1 || result.Models[0].ContextLength != 256_000 {
		t.Fatalf("models = %#v", result.Models)
	}
	updated, errStorage := decodeCopilotStorage(result.AuthUpdate.StorageJSON)
	if errStorage != nil || len(updated.Models) != 1 || updated.Models[0].ContextWindow != 128_000 || len(updated.CompatibilityManifest) == 0 {
		t.Fatalf("updated storage = %#v, error = %v", updated, errStorage)
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
	config := service.loadedConfig()
	config.EnableRemoteCompatibility = false
	service.config = config
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

func TestDiscoverModelsFallsBackToEnabledPoliciesForIndividualAccounts(t *testing.T) {
	raw := mustJSON(t, map[string]any{"data": []any{
		remoteModelFixture("gpt-4.1", false, "enabled", true, []string{"/chat/completions"}),
		remoteModelFixture("disabled", false, "disabled", true, []string{"/chat/completions"}),
		remoteModelFixture("no-tools", false, "enabled", false, []string{"/chat/completions"}),
	}})
	bridge := &fakeBridge{handler: func(_ string, _ any) (any, error) {
		return pluginapi.HTTPResponse{StatusCode: 200, Body: raw}, nil
	}}
	service := newPluginService(bridge)
	models, failure := service.discoverModels(hostClient{bridge: bridge}, copilotStorage{
		CopilotSessionToken: "tid=x;proxy-ep=proxy.individual.githubcopilot.com",
		GitHubHost:          "github.com",
	})
	if failure != nil || len(models) != 1 || models[0].ID != "gpt-4.1" {
		t.Fatalf("models = %#v, failure = %#v", models, failure)
	}
}

func TestDiscoverModelsPrefersNonEmptyPickerCatalogForIndividualAccounts(t *testing.T) {
	raw := mustJSON(t, map[string]any{"data": []any{
		remoteModelFixture("gpt-4.1", true, "", true, []string{"/chat/completions"}),
		remoteModelFixture("gpt-5.4", false, "enabled", true, []string{"/responses"}),
	}})
	bridge := &fakeBridge{handler: func(_ string, _ any) (any, error) {
		return pluginapi.HTTPResponse{StatusCode: 200, Body: raw}, nil
	}}
	service := newPluginService(bridge)
	models, failure := service.discoverModels(hostClient{bridge: bridge}, copilotStorage{
		CopilotSessionToken: "tid=x;proxy-ep=proxy.individual.githubcopilot.com",
		GitHubHost:          "github.com",
	})
	if failure != nil || len(models) != 1 || models[0].ID != "gpt-4.1" {
		t.Fatalf("models = %#v, failure = %#v", models, failure)
	}
}

func TestDiscoverModelsKeepsStrictPickerSemanticsForBusinessAccounts(t *testing.T) {
	raw := mustJSON(t, map[string]any{"data": []any{
		remoteModelFixture("gpt-4.1", false, "enabled", true, []string{"/chat/completions"}),
	}})
	bridge := &fakeBridge{handler: func(_ string, _ any) (any, error) {
		return pluginapi.HTTPResponse{StatusCode: 200, Body: raw}, nil
	}}
	service := newPluginService(bridge)
	models, failure := service.discoverModels(hostClient{bridge: bridge}, copilotStorage{
		CopilotSessionToken: "tid=x;proxy-ep=proxy.business.githubcopilot.com",
		GitHubHost:          "github.com",
	})
	if failure != nil || len(models) != 0 {
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
