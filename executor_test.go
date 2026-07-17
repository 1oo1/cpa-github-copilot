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
