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
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
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
		if headers := http.Header(req.Headers); headers.Get("User-Agent") != copilotUserAgent || headers.Get("Editor-Version") != copilotEditorVersion {
			t.Fatalf("versioned identity headers = %#v", headers)
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

func TestExecuteRejectsFailedResponsesBeforeChatTranslation(t *testing.T) {
	const sentinel = "PRIVATE_RESPONSES_FAILURE"
	service := newPluginService(nil)
	service.now = func() time.Time { return time.Unix(65_000, 0).UTC() }
	storage := executorStorage(service.now(), storedModel{ID: "gpt-5.4", Format: formatOpenAIResponse})
	bridge := &fakeBridge{handler: func(_ string, _ any) (any, error) {
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{
			"id":"resp_failed","object":"response","status":"failed",
			"error":{"code":"server_error","message":"` + sentinel + `"}
		}`)}, nil
	}}
	service.bridge = bridge
	payload := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}]}`)
	_, failure := service.execute(mustJSON(t, rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthID: "auth", Model: "gpt-5.4", Format: formatOpenAI, SourceFormat: formatOpenAI,
		OriginalRequest: payload, Payload: payload, StorageJSON: mustJSON(t, storage),
	}}))
	if failure == nil || failure.(*pluginFailure).code != "upstream_response_failed" {
		t.Fatalf("failure = %#v", failure)
	}
	if strings.Contains(failure.Error(), sentinel) {
		t.Fatalf("failure leaked upstream body: %v", failure)
	}
	assertLogsExclude(t, bridge.snapshotLogs(), sentinel)
}

func TestExecuteNativeResponsesRequiresAndPreservesTerminalStatus(t *testing.T) {
	for _, test := range []struct {
		name        string
		body        string
		wantFailure string
	}{
		{name: "missing status", body: `{"id":"resp-1","object":"response","output":[]}`, wantFailure: "upstream_protocol_error"},
		{name: "in progress", body: `{"id":"resp-1","object":"response","status":"in_progress","output":[]}`, wantFailure: "upstream_protocol_error"},
		{name: "contradictory event", body: `{"type":"response.completed","response":{"id":"resp-1","object":"response","status":"in_progress","output":[]}}`, wantFailure: "upstream_protocol_error"},
		{name: "contradictory outer status", body: `{"type":"response.completed","status":"in_progress","response":{"id":"resp-1","object":"response","status":"completed","output":[]}}`, wantFailure: "upstream_protocol_error"},
		{name: "contradictory outer error", body: `{"type":"response.completed","error":{"code":"server_error"},"response":{"id":"resp-1","object":"response","status":"completed","output":[]}}`, wantFailure: "upstream_protocol_error"},
		{name: "unknown event type", body: `{"type":"response.created","id":"resp-1","object":"response","status":"completed","output":[]}`, wantFailure: "upstream_protocol_error"},
		{name: "null event type", body: `{"type":null,"id":"resp-1","object":"response","status":"completed","output":[]}`, wantFailure: "upstream_protocol_error"},
		{name: "blank event type", body: `{"type":"","id":"resp-1","object":"response","status":"completed","output":[]}`, wantFailure: "upstream_protocol_error"},
		{name: "normalized event type", body: `{"type":" RESPONSE.COMPLETED ","response":{"id":"resp-1","object":"response","status":"completed","output":[]}}`, wantFailure: "upstream_protocol_error"},
		{name: "empty error event", body: `{"type":"error","error":{}}`, wantFailure: "upstream_protocol_error"},
		{name: "completed with error", body: `{"id":"resp-1","object":"response","status":"completed","error":{"code":"server_error"},"output":[]}`, wantFailure: "upstream_protocol_error"},
		{name: "duplicate nested status", body: `{"type":"response.completed","response":{"id":"resp-1","object":"response","status":"in_progress","status":"completed","output":[]}}`, wantFailure: "upstream_protocol_error"},
		{name: "duplicate root error", body: `{"id":"resp-1","object":"response","status":"completed","error":{"message":"PRIVATE_DUPLICATE_SENTINEL"},"error":null,"output":[]}`, wantFailure: "upstream_protocol_error"},
		{name: "failed without error", body: `{"id":"resp-1","object":"response","status":"failed","output":[]}`, wantFailure: "upstream_protocol_error"},
		{name: "incomplete without details", body: `{"id":"resp-1","object":"response","status":"incomplete","output":[]}`, wantFailure: "upstream_protocol_error"},
		{name: "completed", body: `{"id":"resp-1","object":"response","status":"completed","output":[]}`},
		{name: "incomplete", body: `{"id":"resp-1","object":"response","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`},
		{name: "failed", body: `{"id":"resp-1","object":"response","status":"failed","error":{"code":"server_error"},"output":[]}`},
		{name: "completed event", body: `{"type":"response.completed","response":{"id":"resp-1","object":"response","status":"completed","output":[]}}`},
		{name: "top level error", body: `{"type":"error","error":{"code":"server_error"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Unix(66_000, 0).UTC()
			bridge := &fakeBridge{handler: func(_ string, _ any) (any, error) {
				return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(test.body)}, nil
			}}
			service := newPluginService(bridge)
			service.now = func() time.Time { return now }
			payload := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hello"}]}`)
			raw, failure := service.execute(mustJSON(t, rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
				AuthID: "auth", Model: "gpt-5.4", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
				OriginalRequest: payload, Payload: payload,
				StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-5.4", Format: formatOpenAIResponse})),
			}}))
			if test.wantFailure != "" {
				if failure == nil || failure.(*pluginFailure).code != test.wantFailure {
					t.Fatalf("failure = %#v", failure)
				}
				return
			}
			if failure != nil {
				t.Fatal(failure)
			}
			result := decodePluginResult[pluginapi.ExecutorResponse](t, raw)
			if string(result.Payload) != test.body {
				t.Fatalf("native terminal payload changed: got=%s want=%s", result.Payload, test.body)
			}
		})
	}
}

func TestResponsesNonStreamTerminalStatusUsesExactGrammar(t *testing.T) {
	for _, test := range []struct {
		name         string
		body         string
		wantStatus   string
		wantTerminal bool
	}{
		{name: "completed", body: `{"id":"resp-1","object":"response","status":"completed","error":null,"incomplete_details":null}`, wantStatus: "response.completed", wantTerminal: true},
		{name: "incomplete max output", body: `{"id":"resp-1","object":"response","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}`, wantStatus: "response.incomplete", wantTerminal: true},
		{name: "incomplete content filter", body: `{"id":"resp-1","object":"response","status":"incomplete","incomplete_details":{"reason":"content_filter"}}`, wantStatus: "response.incomplete", wantTerminal: true},
		{name: "failed", body: `{"id":"resp-1","object":"response","status":"failed","error":{"code":"server_error"}}`, wantStatus: "response.failed", wantTerminal: true},
		{name: "completed event", body: `{"type":"response.completed","response":{"id":"resp-1","object":"response","status":"completed"}}`, wantStatus: "response.completed", wantTerminal: true},
		{name: "incomplete event", body: `{"type":"response.incomplete","response":{"id":"resp-1","object":"response","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`, wantStatus: "response.incomplete", wantTerminal: true},
		{name: "failed event", body: `{"type":"response.failed","response":{"id":"resp-1","object":"response","status":"failed","error":{"code":"server_error"}}}`, wantStatus: "response.failed", wantTerminal: true},
		{name: "typed error", body: `{"type":"error","error":{"code":"server_error"}}`, wantStatus: "error", wantTerminal: true},
		{name: "untyped error", body: `{"error":{"code":"server_error"}}`, wantStatus: "error", wantTerminal: true},
		{name: "malformed JSON", body: `{`},
		{name: "non-string type", body: `{"type":1,"object":"response","status":"completed"}`},
		{name: "padded type", body: `{"type":" response.completed ","response":{"object":"response","status":"completed"}}`},
		{name: "padded object", body: `{"object":" response ","status":"completed"}`},
		{name: "padded status", body: `{"object":"response","status":" completed "}`},
		{name: "missing nested response", body: `{"type":"response.completed"}`},
		{name: "nested event type", body: `{"type":"response.completed","response":{"type":"response.completed","object":"response","status":"completed"}}`},
		{name: "outer response object", body: `{"type":"response.completed","object":"response","response":{"object":"response","status":"completed"}}`},
		{name: "outer matching status", body: `{"type":"response.completed","status":"completed","response":{"object":"response","status":"completed"}}`},
		{name: "outer null error", body: `{"type":"response.completed","error":null,"response":{"object":"response","status":"completed"}}`},
		{name: "completed with details", body: `{"object":"response","status":"completed","incomplete_details":{"reason":"max_output_tokens"}}`},
		{name: "incomplete unknown reason", body: `{"object":"response","status":"incomplete","incomplete_details":{"reason":"other"}}`},
		{name: "incomplete blank reason", body: `{"object":"response","status":"incomplete","incomplete_details":{"reason":" "}}`},
		{name: "failed empty error", body: `{"object":"response","status":"failed","error":{}}`},
		{name: "failed non-object error", body: `{"object":"response","status":"failed","error":"server_error"}`},
		{name: "ambiguous untyped error", body: `{"error":{"code":"server_error"},"response":{"object":"response","status":"completed"}}`},
		{name: "duplicate root status", body: `{"object":"response","status":"in_progress","status":"completed"}`},
		{name: "duplicate nested status", body: `{"type":"response.completed","response":{"object":"response","status":"in_progress","status":"completed"}}`},
		{name: "duplicate escaped key", body: `{"object":"response","st\u0061tus":"in_progress","status":"completed"}`},
		{name: "duplicate error", body: `{"object":"response","status":"completed","error":{"code":"server_error"},"error":null}`},
		{name: "duplicate array object key", body: `{"object":"response","status":"completed","output":[{"type":"message","type":"message"}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, terminal := responsesNonStreamTerminalStatus([]byte(test.body))
			if status != test.wantStatus || terminal != test.wantTerminal {
				t.Fatalf("terminal = (%q, %t), want (%q, %t)", status, terminal, test.wantStatus, test.wantTerminal)
			}
		})
	}
}

func TestExecuteRejectsTranslatedSourceErrorsBeforeResponsesConversion(t *testing.T) {
	const sentinel = "PRIVATE_SOURCE_RESPONSE_FAILURE"
	for _, test := range []struct {
		name           string
		model          string
		upstreamFormat string
		body           string
	}{
		{name: "chat", model: "gpt-4.1", upstreamFormat: formatOpenAI, body: `{"error":{"type":"server_error","message":"` + sentinel + `"}}`},
		{name: "claude", model: "claude-sonnet-4.6", upstreamFormat: formatClaude, body: `{"type":"error","error":{"type":"api_error","message":"` + sentinel + `"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, failure, bridge := executeSourceResponseAsResponses(t, test.model, test.upstreamFormat, []byte(test.body))
			if failure == nil || failure.(*pluginFailure).code != "upstream_response_failed" {
				t.Fatalf("failure = %#v", failure)
			}
			if strings.Contains(failure.Error(), sentinel) {
				t.Fatalf("failure leaked upstream body: %v", failure)
			}
			assertLogsExclude(t, bridge.snapshotLogs(), sentinel)
		})
	}
}

func TestExecuteTranslatedResponsesPreservesIncompleteReasons(t *testing.T) {
	for _, test := range []struct {
		name           string
		model          string
		upstreamFormat string
		body           string
		wantStatus     string
		wantReason     string
	}{
		{
			name: "chat length", model: "gpt-4.1", upstreamFormat: formatOpenAI,
			body:       `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4.1","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"length"}]}`,
			wantStatus: "incomplete", wantReason: "max_output_tokens",
		},
		{
			name: "chat content filter", model: "gpt-4.1", upstreamFormat: formatOpenAI,
			body:       `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4.1","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"content_filter"}]}`,
			wantStatus: "incomplete", wantReason: "content_filter",
		},
		{
			name: "claude max tokens", model: "claude-sonnet-4.6", upstreamFormat: formatClaude,
			body:       `{"id":"msg-1","type":"message","role":"assistant","model":"claude-sonnet-4.6","content":[{"type":"text","text":"hello"}],"stop_reason":"max_tokens","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`,
			wantStatus: "incomplete", wantReason: "max_output_tokens",
		},
		{
			name: "claude context window", model: "claude-sonnet-4.6", upstreamFormat: formatClaude,
			body:       `{"id":"msg-1","type":"message","role":"assistant","model":"claude-sonnet-4.6","content":[{"type":"text","text":"hello"}],"stop_reason":"model_context_window_exceeded","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`,
			wantStatus: "incomplete", wantReason: "max_output_tokens",
		},
		{
			name: "claude end turn", model: "claude-sonnet-4.6", upstreamFormat: formatClaude,
			body:       `{"id":"msg-1","type":"message","role":"assistant","model":"claude-sonnet-4.6","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`,
			wantStatus: "completed",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, failure, _ := executeSourceResponseAsResponses(t, test.model, test.upstreamFormat, []byte(test.body))
			if failure != nil {
				t.Fatal(failure)
			}
			result := decodePluginResult[pluginapi.ExecutorResponse](t, raw)
			var response map[string]any
			if errDecode := json.Unmarshal(result.Payload, &response); errDecode != nil {
				t.Fatalf("decode translated response: %v; payload=%s", errDecode, result.Payload)
			}
			if response["status"] != test.wantStatus {
				t.Fatalf("status = %#v; payload=%s", response["status"], result.Payload)
			}
			if !strings.Contains(string(result.Payload), "hello") {
				t.Fatalf("translated content missing: %s", result.Payload)
			}
			details, _ := response["incomplete_details"].(map[string]any)
			if test.wantReason != "" && (details == nil || details["reason"] != test.wantReason) {
				t.Fatalf("incomplete_details = %#v; payload=%s", response["incomplete_details"], result.Payload)
			}
		})
	}
}

func TestExecuteTranslatedClaudeNonStreamPreservesStructuredContent(t *testing.T) {
	body := []byte(`{
		"id":"msg-structured","type":"message","role":"assistant","model":"claude-sonnet-4.6",
		"content":[
			{"type":"thinking","thinking":"consider","signature":"signed-reasoning"},
			{"type":"text","text":"hello"},
			{"type":"tool_use","id":"tool-1","name":"lookup","input":{"query":"value"}}
		],
		"stop_reason":"tool_use","stop_sequence":null,
		"usage":{"input_tokens":7,"output_tokens":5}
	}`)
	raw, failure, _ := executeSourceResponseAsResponses(t, "claude-sonnet-4.6", formatClaude, body)
	if failure != nil {
		t.Fatal(failure)
	}
	result := decodePluginResult[pluginapi.ExecutorResponse](t, raw)
	var response struct {
		Status string `json:"status"`
		Output []struct {
			Type             string `json:"type"`
			EncryptedContent string `json:"encrypted_content"`
			Arguments        string `json:"arguments"`
			CallID           string `json:"call_id"`
			Name             string `json:"name"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if errDecode := json.Unmarshal(result.Payload, &response); errDecode != nil {
		t.Fatalf("decode translated response: %v; payload=%s", errDecode, result.Payload)
	}
	if response.Status != "completed" || len(response.Output) != 3 {
		t.Fatalf("translated response = %#v; payload=%s", response, result.Payload)
	}
	if response.Output[0].Type != "reasoning" || response.Output[0].EncryptedContent != "signed-reasoning" ||
		response.Output[1].Type != "message" || response.Output[2].Type != "function_call" ||
		response.Output[2].Arguments != `{"query":"value"}` || response.Output[2].CallID != "tool-1" || response.Output[2].Name != "lookup" {
		t.Fatalf("translated output = %#v; payload=%s", response.Output, result.Payload)
	}
	if response.Usage.InputTokens != 7 || response.Usage.OutputTokens != 5 || !strings.Contains(string(result.Payload), "consider") || !strings.Contains(string(result.Payload), "hello") {
		t.Fatalf("translated content or usage missing: %s", result.Payload)
	}
}

func executeSourceResponseAsResponses(t *testing.T, model, upstreamFormat string, upstreamBody []byte) ([]byte, error, *fakeBridge) {
	t.Helper()
	now := time.Unix(67_000, 0).UTC()
	bridge := &fakeBridge{handler: func(_ string, _ any) (any, error) {
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: upstreamBody}, nil
	}}
	service := newPluginService(bridge)
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"` + model + `","input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	raw, failure := service.execute(mustJSON(t, rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthID: "auth", Model: model, Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
		OriginalRequest: payload, Payload: payload,
		StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: model, Format: upstreamFormat})),
	}}))
	return raw, failure, bridge
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

func TestPrepareInferenceRoutesStandaloneAnthropicWebSearchToResponses(t *testing.T) {
	service := newPluginService(nil)
	now := time.Unix(71_000, 0).UTC()
	service.now = func() time.Time { return now }
	storage := executorStorage(now,
		storedModel{ID: "claude-sonnet-5", Format: formatClaude},
		storedModel{ID: "gpt-5.6-terra", Format: formatOpenAIResponse, Family: "gpt-5.6-terra", MaxPromptTokens: 922_000},
	)
	payload := []byte(`{
		"model":"claude-sonnet-5",
		"max_tokens":64000,
		"messages":[{"role":"user","content":"Perform a web search"}],
		"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":1}],
		"tool_choice":{"type":"tool","name":"web_search"}
	}`)
	prepared, failure := service.prepareInference(pluginapi.ExecutorRequest{
		AuthID: "auth", Model: "claude-sonnet-5", Format: formatClaude, SourceFormat: formatClaude,
		OriginalRequest: payload, Payload: payload, StorageJSON: mustJSON(t, storage),
	}, true)
	if failure != nil {
		t.Fatal(failure)
	}
	if prepared.model != "gpt-5.6-terra" || prepared.upstreamFormat != formatOpenAIResponse ||
		prepared.translatorFormat != string(sdktranslator.FormatCodex) || prepared.outputFormat != formatClaude ||
		!strings.HasSuffix(prepared.upstreamURL, "/responses") {
		t.Fatalf("prepared route = %#v", prepared)
	}
	if prepared.headers.Get("X-Interaction-Type") != "messages-proxy" {
		t.Fatalf("interaction type = %q", prepared.headers.Get("X-Interaction-Type"))
	}
	var upstream map[string]any
	if errDecode := json.Unmarshal(prepared.upstreamPayload, &upstream); errDecode != nil {
		t.Fatal(errDecode)
	}
	tools, ok := upstream["tools"].([]any)
	if !ok || len(tools) != 1 || tools[0].(map[string]any)["type"] != "web_search" || upstream["model"] != "gpt-5.6-terra" {
		t.Fatalf("upstream payload = %s", prepared.upstreamPayload)
	}
	fields := preparedInferenceLogFields(prepared, true)
	if fields["requested_model"] != "claude-sonnet-5" || fields["web_search_routed"] != true {
		t.Fatalf("prepared log fields = %#v", fields)
	}
}

func TestPrepareInferenceAnthropicWebSearchRoutingGuards(t *testing.T) {
	now := time.Unix(71_500, 0).UTC()
	standalone := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"search"}],"tools":[{"type":"web_search_20260209","name":"web_search"}]}`
	for _, test := range []struct {
		name       string
		config     string
		models     []storedModel
		payload    string
		wantCode   string
		wantModel  string
		wantFormat string
	}{
		{
			name: "ordinary Anthropic tool stays on Claude", models: []storedModel{{ID: "claude-sonnet-5", Format: formatClaude}},
			payload:   `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"lookup"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`,
			wantModel: "claude-sonnet-5", wantFormat: formatClaude,
		},
		{
			name: "mixed tools fail closed", models: []storedModel{{ID: "claude-sonnet-5", Format: formatClaude}},
			payload:  `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"search"}],"tools":[{"type":"web_search_20250305","name":"web_search"},{"name":"lookup","input_schema":{"type":"object"}}]}`,
			wantCode: "unsupported_feature",
		},
		{
			name: "disabled route fails closed", config: `web_search_model: ""`, models: []storedModel{{ID: "claude-sonnet-5", Format: formatClaude}},
			payload: standalone, wantCode: "unsupported_feature",
		},
		{
			name: "missing configured model fails closed", models: []storedModel{{ID: "claude-sonnet-5", Format: formatClaude}},
			payload: standalone, wantCode: "model_not_supported",
		},
		{
			name: "configured model must use Responses", models: []storedModel{{ID: "claude-sonnet-5", Format: formatClaude}, {ID: "gpt-5.6-terra", Format: formatOpenAI}},
			payload: standalone, wantCode: "unsupported_feature",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := newPluginService(nil)
			service.now = func() time.Time { return now }
			if test.config != "" {
				if errConfigure := service.configure(mustJSON(t, lifecycleRequest{ConfigYAML: []byte(test.config)})); errConfigure != nil {
					t.Fatal(errConfigure)
				}
			}
			prepared, failure := service.prepareInference(pluginapi.ExecutorRequest{
				AuthID: "auth", Model: "claude-sonnet-5", Format: formatClaude, SourceFormat: formatClaude,
				Payload: []byte(test.payload), StorageJSON: mustJSON(t, executorStorage(now, test.models...)),
			}, true)
			if test.wantCode != "" {
				if failure == nil || failure.code != test.wantCode {
					t.Fatalf("failure = %#v, want code %q", failure, test.wantCode)
				}
				return
			}
			if failure != nil {
				t.Fatal(failure)
			}
			if prepared.model != test.wantModel || prepared.upstreamFormat != test.wantFormat {
				t.Fatalf("prepared route = %#v", prepared)
			}
		})
	}
}

func TestClassifyAnthropicServerWebSearchRequest(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want anthropicWebSearchRequestClass
	}{
		{name: "absent", body: `{"messages":[]}`, want: anthropicWebSearchNone},
		{name: "ordinary", body: `{"tools":[{"name":"lookup"}]}`, want: anthropicWebSearchNone},
		{name: "20250305", body: `{"tools":[{"type":"web_search_20250305","name":"web_search"}]}`, want: anthropicWebSearchOnly},
		{name: "20260209", body: `{"tools":[{"type":"web_search_20260209","name":"web_search"}]}`, want: anthropicWebSearchOnly},
		{name: "mixed", body: `{"tools":[{"type":"web_search_20250305","name":"web_search"},{"name":"lookup"}]}`, want: anthropicWebSearchMixed},
		{name: "invalid name", body: `{"tools":[{"type":"web_search_20250305","name":"browser_search"}]}`, want: anthropicWebSearchMixed},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyAnthropicServerWebSearchRequest([]byte(test.body)); got != test.want {
				t.Fatalf("class = %v, want %v", got, test.want)
			}
		})
	}
}

func TestPrepareInferencePreservesNativeAnthropicXHighEffort(t *testing.T) {
	service := newPluginService(nil)
	now := time.Unix(72_000, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{
		"model":"claude-opus-4.8",
		"max_tokens":64000,
		"reasoning_effort":"xhigh",
		"messages":[{"role":"user","content":"hi"}]
	}`)
	prepared, failure := service.prepareInference(pluginapi.ExecutorRequest{
		AuthID: "auth", Model: "claude-opus-4.8", Format: formatOpenAI, SourceFormat: formatOpenAI,
		OriginalRequest: payload, Payload: payload,
		StorageJSON: mustJSON(t, executorStorage(now, storedModel{
			ID: "claude-opus-4.8", Format: formatClaude, AdaptiveThinking: true,
			ReasoningLevels: []string{"low", "medium", "high", "xhigh", "max"}, ReasoningLevelsDeclared: true,
		})),
	}, false)
	if failure != nil {
		t.Fatal(failure)
	}
	var upstream map[string]any
	if errDecode := json.Unmarshal(prepared.upstreamPayload, &upstream); errDecode != nil {
		t.Fatal(errDecode)
	}
	thinking, _ := upstream["thinking"].(map[string]any)
	outputConfig, _ := upstream["output_config"].(map[string]any)
	if thinking["type"] != "adaptive" || outputConfig["effort"] != "xhigh" {
		t.Fatalf("upstream payload = %s", prepared.upstreamPayload)
	}
}

func TestResponsesReasoningEffortUsesCatalogLevels(t *testing.T) {
	for _, test := range []struct {
		name          string
		reasoning     string
		wantReasoning bool
		wantEffort    string
	}{
		{name: "disabled reasoning is omitted", reasoning: "none", wantReasoning: false},
		{name: "pi minimal compatibility is removed", reasoning: "minimal", wantReasoning: false},
		{name: "enabled reasoning is preserved", reasoning: "high", wantReasoning: true, wantEffort: "high"},
		{name: "future catalog level is preserved", reasoning: "ultra", wantReasoning: true, wantEffort: "ultra"},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := []byte(`{
				"model":"gpt-5.4",
				"store":true,
				"reasoning":{"effort":"` + test.reasoning + `"},
				"input":[{"role":"user","content":"hi"}]
			}`)
			route := modelRoute{Format: formatOpenAIResponse, ReasoningLevels: []string{"low", "high", "ultra"}, ReasoningLevelsDeclared: true}
			normalized, errNormalize := normalizeInferencePayloadForRoute(raw, "gpt-5.4", route, false, true)
			if errNormalize != nil {
				t.Fatal(errNormalize)
			}
			var payload map[string]any
			if errDecode := json.Unmarshal(normalized, &payload); errDecode != nil {
				t.Fatal(errDecode)
			}
			if payload["store"] != false {
				t.Fatalf("store = %#v", payload["store"])
			}
			_, hasReasoning := payload["reasoning"]
			if hasReasoning != test.wantReasoning {
				t.Fatalf("payload = %#v", payload)
			}
			if hasReasoning && payload["reasoning"].(map[string]any)["effort"] != test.wantEffort {
				t.Fatalf("payload = %#v", payload)
			}
		})
	}
}

func TestAnthropicMessagesNormalizationUsesCatalogCapabilities(t *testing.T) {
	service := newPluginService(nil)
	now := time.Unix(75_000, 0).UTC()
	service.now = func() time.Time { return now }

	prepare := func(t *testing.T, model, payload string) preparedInference {
		t.Helper()
		stored := storedModel{ID: model, Format: formatClaude}
		if model == "claude-opus-4.8" {
			stored.AdaptiveThinking = true
			stored.ReasoningLevels = []string{"low", "medium", "high", "xhigh"}
			stored.ReasoningLevelsDeclared = true
		}
		prepared, failure := service.prepareInference(pluginapi.ExecutorRequest{
			AuthID: "auth", Model: model, Format: formatClaude, SourceFormat: formatClaude,
			Payload: []byte(payload), StorageJSON: mustJSON(t, executorStorage(now, stored)),
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

	t.Run("does not invent eager input streaming or undeclared context editing", func(t *testing.T) {
		prepared := prepare(t, "claude-opus-4.8", `{
			"model":"claude-opus-4.8",
			"messages":[
				{"role":"user","content":"Use the tool"},
				{"role":"system","content":"keep this message in place"}
			],
			"tools":[{"name":"lookup","description":"Look up a value","input_schema":{"type":"object"}}],
			"thinking":{"type":"adaptive","display":"omitted"},
			"output_config":{"effort":"xhigh"},
			"context_management":{"edits":[]}
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
			t.Fatalf("eager_input_streaming was invented: %#v", tool)
		}
		if _, exists := payload["output_config"]; !exists {
			t.Fatal("newer model output_config was removed")
		}
		if _, exists := payload["context_management"]; exists {
			t.Fatal("undeclared context_management was retained")
		}
		messages, ok := payload["messages"].([]any)
		if !ok || len(messages) != 1 || payload["system"] == nil {
			t.Fatalf("system message was not normalized: %#v", payload)
		}
		if beta := prepared.headers.Get("Anthropic-Beta"); beta != "" {
			t.Fatalf("Anthropic-Beta = %q", beta)
		}
	})

	t.Run("removes unsupported strict tool metadata", func(t *testing.T) {
		prepared := prepare(t, "claude-opus-4.8", `{
			"model":"claude-opus-4.8",
			"messages":[{"role":"user","content":"Use the tool"}],
			"tools":[{
				"name":"lookup",
				"strict":true,
				"input_schema":{
					"type":"object",
					"properties":{"query":{"type":"string"}},
					"required":["query"],
					"additionalProperties":false,
					"title":"StrictLookupInput"
				}
			}]
		}`)
		payload := decode(t, prepared)
		tool := payload["tools"].([]any)[0].(map[string]any)
		if _, exists := tool["strict"]; exists {
			t.Fatalf("strict reached Copilot: %#v", tool)
		}
		schema := tool["input_schema"].(map[string]any)
		if _, exists := schema["additionalProperties"]; exists || schema["title"] != nil {
			t.Fatalf("strict schema reached Copilot: %#v", schema)
		}
		if schema["type"] != "object" || schema["properties"] == nil || schema["required"] == nil {
			t.Fatalf("legacy schema = %#v", schema)
		}
	})

	t.Run("converts Claude Code enabled thinking for adaptive models", func(t *testing.T) {
		contextEditing := true
		prepared, failure := service.prepareInference(pluginapi.ExecutorRequest{
			AuthID: "auth", Model: "claude-opus-4.8", Format: formatClaude, SourceFormat: formatClaude,
			Payload: []byte(`{
				"model":"claude-opus-4.8",
				"max_tokens":32000,
				"messages":[{"role":"user","content":"Hello"}],
				"tools":[{"name":"lookup","input_schema":{"type":"object"}}],
				"thinking":{"type":"enabled","budget_tokens":31999,"display":"omitted"},
				"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}
			}`),
			StorageJSON: mustJSON(t, executorStorage(now, storedModel{
				ID: "claude-opus-4.8", Format: formatClaude, AdaptiveThinking: true,
				ReasoningLevels: []string{"xhigh"}, ReasoningLevelsDeclared: true,
				SupportsContextEditing: &contextEditing,
			})),
			Headers: http.Header{"Anthropic-Beta": []string{
				"claude-code-20250219,context-1m-2025-08-07,context-management-2025-06-27,advisor-tool-2026-03-01",
			}},
		}, true)
		if failure != nil {
			t.Fatal(failure)
		}
		payload := decode(t, prepared)
		thinking, ok := payload["thinking"].(map[string]any)
		if !ok || thinking["type"] != "adaptive" || thinking["display"] != "omitted" {
			t.Fatalf("thinking = %#v", payload["thinking"])
		}
		if _, exists := thinking["budget_tokens"]; exists {
			t.Fatalf("legacy thinking budget was retained: %#v", thinking)
		}
		outputConfig, ok := payload["output_config"].(map[string]any)
		if !ok || outputConfig["effort"] != "xhigh" {
			t.Fatalf("output_config = %#v", payload["output_config"])
		}
		contextManagement, ok := payload["context_management"].(map[string]any)
		edits, editsOK := contextManagement["edits"].([]any)
		if !ok || !editsOK || len(edits) != 1 {
			t.Fatalf("context_management = %#v", payload["context_management"])
		}
		if beta := prepared.headers.Get("Anthropic-Beta"); beta != contextManagementBeta {
			t.Fatalf("Anthropic-Beta = %q", beta)
		}
	})

	t.Run("sends interleaved thinking beta unconditionally for non-adaptive routes", func(t *testing.T) {
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
				if beta := prepared.headers.Get("Anthropic-Beta"); beta != interleavedThinkingBeta {
					t.Fatalf("Anthropic-Beta = %q", beta)
				}
			})
		}
	})

	t.Run("omits empty tools and still sends interleaved thinking beta for non-adaptive routes", func(t *testing.T) {
		prepared := prepare(t, "claude-haiku-4.5", `{
			"model":"claude-haiku-4.5",
			"messages":[{"role":"user","content":"Hello"}],
			"tools":[]
		}`)
		payload := decode(t, prepared)
		if _, exists := payload["tools"]; exists {
			t.Fatalf("empty tools were retained: %#v", payload["tools"])
		}
		if beta := prepared.headers.Get("Anthropic-Beta"); beta != interleavedThinkingBeta {
			t.Fatalf("Anthropic-Beta = %q", beta)
		}
	})
}

func TestBudgetThinkingNormalizesFromCatalogCapabilities(t *testing.T) {
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
		Payload: payload, StorageJSON: mustJSON(t, executorStorage(now, storedModel{
			ID: model, Format: formatClaude, MinThinking: 1024, MaxThinking: 32000,
		})),
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
	if !ok || thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(16000) || thinking["display"] != "summarized" {
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

func TestAnthropicBudgetThinkingUsesCatalogLimits(t *testing.T) {
	for _, test := range []struct {
		name            string
		min, max, limit int
		want            int
	}{
		{name: "default", min: 1024, max: 32000, limit: 32000, want: 16000},
		{name: "catalog minimum", min: 20000, max: 32000, limit: 32000, want: 20000},
		{name: "catalog maximum", min: 1024, max: 8000, limit: 32000, want: 8000},
		{name: "output room", min: 1024, max: 32000, limit: 4096, want: 4095},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := map[string]any{"max_tokens": test.limit, "thinking": map[string]any{"type": "adaptive"}}
			route := modelRoute{MinThinking: test.min, MaxThinking: test.max}
			if !normalizeAnthropicPayloadForRoute(payload, route) {
				t.Fatal("payload was not normalized")
			}
			thinking := payload["thinking"].(map[string]any)
			if thinking["budget_tokens"] != test.want {
				t.Fatalf("thinking = %#v", thinking)
			}
		})
	}
}

func TestAnthropicAdaptiveThinkingUsesCatalogEffortLevels(t *testing.T) {
	for _, test := range []struct {
		name     string
		levels   []string
		declared bool
		effort   string
		want     string
	}{
		{name: "future level", levels: []string{"low", "ultra"}, declared: true, effort: "ultra", want: "ultra"},
		{name: "catalog medium default", levels: []string{"low", "medium", "high"}, declared: true, want: "medium"},
		{name: "catalog midpoint default", levels: []string{"high", "max"}, declared: true, want: "high"},
		{name: "missing metadata follows VS Code adaptive default", want: "high"},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": 32768, "display": "omitted"}}
			if test.effort != "" {
				payload["output_config"] = map[string]any{"effort": test.effort}
			}
			route := modelRoute{AdaptiveThinking: true, ReasoningLevels: test.levels, ReasoningLevelsDeclared: test.declared}
			if !normalizeAnthropicPayloadForRoute(payload, route) {
				t.Fatal("payload was not normalized")
			}
			thinking := payload["thinking"].(map[string]any)
			if thinking["type"] != "adaptive" || thinking["display"] != "omitted" {
				t.Fatalf("thinking = %#v", thinking)
			}
			if got := payload["output_config"].(map[string]any)["effort"]; got != test.want {
				t.Fatalf("effort = %q, want %q", got, test.want)
			}
		})
	}
}

func TestAnthropicThinkingIsNotForcedByModelName(t *testing.T) {
	payload := map[string]any{
		"max_tokens": 32000,
		"thinking": map[string]any{
			"type":          "enabled",
			"budget_tokens": 16384,
		},
	}
	if !normalizeAnthropicPayloadForRoute(payload, modelRoute{}) {
		t.Fatal("payload was not normalized")
	}
	if _, exists := payload["thinking"]; exists {
		t.Fatalf("undeclared thinking was retained: %#v", payload)
	}
}

func TestAnthropicMessagesOmitsTemperatureLikeVSCodeBuilder(t *testing.T) {
	payload := map[string]any{"temperature": 0.7}
	if !normalizeAnthropicPayloadForRoute(payload, modelRoute{}) {
		t.Fatal("payload was not normalized")
	}
	if _, exists := payload["temperature"]; exists {
		t.Fatalf("temperature was retained: %#v", payload)
	}
}

func TestGitHubCopilotOpenAICompatibility(t *testing.T) {
	for _, model := range []string{"gemini-3.6-flash", "gpt-4.1", "kimi-k2.7-code"} {
		t.Run(model, func(t *testing.T) {
			raw := []byte(`{
				"model":"` + model + `",
				"store":true,
				"reasoning_effort":"high",
				"messages":[{"role":"developer","content":"system prompt"},{"role":"user","content":"hello"}]
			}`)
			normalized, errNormalize := normalizeInferencePayloadForRoute(raw, model, modelRoute{Format: formatOpenAI}, false, true)
			if errNormalize != nil {
				t.Fatal(errNormalize)
			}
			var payload map[string]any
			if errDecode := json.Unmarshal(normalized, &payload); errDecode != nil {
				t.Fatal(errDecode)
			}
			_, hasStore := payload["store"]
			_, hasReasoning := payload["reasoning_effort"]
			messages := payload["messages"].([]any)
			role := messages[0].(map[string]any)["role"]
			if hasStore || hasReasoning || role != "system" {
				t.Fatalf("payload = %#v", payload)
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
	bridge := &fakeBridge{handler: func(_ string, _ any) (any, error) {
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK}, nil
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

func TestExecuteHTTPRequestRejectsResponsesCompactWithoutHostCall(t *testing.T) {
	service := newPluginService(nil)
	now := time.Unix(91_000, 0).UTC()
	service.now = func() time.Time { return now }
	storage := executorStorage(now, storedModel{ID: "gpt-5.4", Format: formatOpenAIResponse, Family: "gpt-5.4"})
	bridge := &fakeBridge{handler: func(_ string, _ any) (any, error) {
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK}, nil
	}}
	service.bridge = bridge
	_, failure := service.executeHTTPRequest(mustJSON(t, rpcExecutorHTTPRequest{ExecutorHTTPRequest: pluginapi.ExecutorHTTPRequest{
		URL:         "https://api.individual.githubcopilot.com/responses/compact",
		Method:      http.MethodPost,
		Body:        []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hi"}]}`),
		StorageJSON: mustJSON(t, storage),
	}}))
	if failure == nil || failure.(*pluginFailure).code != "unsupported_feature" {
		t.Fatalf("failure = %#v", failure)
	}
	if len(bridge.snapshot()) != 0 {
		t.Fatal("unsupported compact request reached host bridge")
	}
}

func TestExecuteHTTPRequestRejectsInferenceNonPOSTWithoutHostCall(t *testing.T) {
	service := newPluginService(nil)
	now := time.Unix(91_500, 0).UTC()
	service.now = func() time.Time { return now }
	storage := executorStorage(now, storedModel{ID: "gpt-5.4", Format: formatOpenAIResponse, Family: "gpt-5.4"})
	bridge := &fakeBridge{handler: func(_ string, _ any) (any, error) {
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK}, nil
	}}
	service.bridge = bridge
	for _, method := range []string{http.MethodGet, "post", " POST ", ""} {
		t.Run(method, func(t *testing.T) {
			_, failure := service.executeHTTPRequest(mustJSON(t, rpcExecutorHTTPRequest{ExecutorHTTPRequest: pluginapi.ExecutorHTTPRequest{
				URL:         "https://api.individual.githubcopilot.com/responses",
				Method:      method,
				Body:        []byte(`{"model":"gpt-5.4","input":[]}`),
				StorageJSON: mustJSON(t, storage),
			}}))
			if failure == nil || failure.(*pluginFailure).code != "invalid_request" {
				t.Fatalf("failure = %#v", failure)
			}
		})
	}
	if len(bridge.snapshot()) != 0 {
		t.Fatal("non-POST inference request reached host bridge")
	}
}

func TestExecuteHTTPRequestRejectsNoncanonicalInferencePathsWithoutHostCall(t *testing.T) {
	service := newPluginService(nil)
	now := time.Unix(91_750, 0).UTC()
	service.now = func() time.Time { return now }
	storage := executorStorage(now, storedModel{ID: "gpt-5.4", Format: formatOpenAIResponse, Family: "gpt-5.4"})
	bridge := &fakeBridge{handler: func(_ string, _ any) (any, error) {
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK}, nil
	}}
	service.bridge = bridge
	for _, endpoint := range []string{
		"/responses/.",
		"/responses/",
		"//responses",
		"/v1/../responses",
		"/%72esponses",
		"/responses?feature=x",
		"/responses#fragment",
		"/v1/responses",
		"/v1/chat/completions",
		"/messages",
	} {
		t.Run(endpoint, func(t *testing.T) {
			_, failure := service.executeHTTPRequest(mustJSON(t, rpcExecutorHTTPRequest{ExecutorHTTPRequest: pluginapi.ExecutorHTTPRequest{
				URL:         "https://api.individual.githubcopilot.com" + endpoint,
				Method:      http.MethodPost,
				Body:        []byte(`{"model":"gpt-5.4","input":[]}`),
				StorageJSON: mustJSON(t, storage),
			}}))
			if failure == nil || failure.(*pluginFailure).code != "invalid_request" {
				t.Fatalf("failure = %#v", failure)
			}
		})
	}
	_, failure := service.executeHTTPRequest(mustJSON(t, rpcExecutorHTTPRequest{ExecutorHTTPRequest: pluginapi.ExecutorHTTPRequest{
		URL:         "https://API.INDIVIDUAL.GITHUBCOPILOT.COM/responses",
		Method:      http.MethodPost,
		Body:        []byte(`{"model":"gpt-5.4","input":[]}`),
		StorageJSON: mustJSON(t, storage),
	}}))
	if failure == nil || failure.(*pluginFailure).code != "invalid_request" {
		t.Fatalf("equivalent but non-exact inference URL failure = %#v", failure)
	}
	if len(bridge.snapshot()) != 0 {
		t.Fatal("noncanonical inference request reached host bridge")
	}
}

func TestExecuteHTTPRequestAppliesResponsesBodyPolicy(t *testing.T) {
	service := newPluginService(nil)
	now := time.Unix(92_000, 0).UTC()
	service.now = func() time.Time { return now }
	storage := executorStorage(now, storedModel{
		ID: "gpt-5.4", Format: formatOpenAIResponse, Family: "gpt-5.4", MaxPromptTokens: 100_000,
	})
	var upstreamBody map[string]any
	bridge := &fakeBridge{handler: func(method string, payload any) (any, error) {
		if method != pluginabi.MethodHostHTTPDo {
			t.Fatalf("method = %s", method)
		}
		req := payload.(rpcHostHTTPRequest)
		if errDecode := json.Unmarshal(req.Body, &upstreamBody); errDecode != nil {
			t.Fatalf("decode upstream body: %v", errDecode)
		}
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"id":"resp-1","object":"response","status":"completed","output":[]}`)}, nil
	}}
	service.bridge = bridge
	_, failure := service.executeHTTPRequest(mustJSON(t, rpcExecutorHTTPRequest{ExecutorHTTPRequest: pluginapi.ExecutorHTTPRequest{
		AuthID:      "auth",
		URL:         "https://api.individual.githubcopilot.com/responses",
		Method:      http.MethodPost,
		Body:        []byte(`{"model":"gpt-5.4","store":true,"input":[{"role":"user","content":"hi"}]}`),
		StorageJSON: mustJSON(t, storage),
	}}))
	if failure != nil {
		t.Fatal(failure)
	}
	if upstreamBody["store"] != false || upstreamBody["truncation"] != "disabled" {
		t.Fatalf("Responses defaults = %#v", upstreamBody)
	}
	include, _ := upstreamBody["include"].([]any)
	if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %#v", upstreamBody["include"])
	}
	if got := responsesCompactionThresholdFromBody(t, upstreamBody); got != 90_000 {
		t.Fatalf("compact threshold = %v, want 90000", got)
	}
}

func TestExecuteHTTPRequestNativeResponsesRequiresTerminalStatus(t *testing.T) {
	for _, test := range []struct {
		name        string
		body        string
		stream      bool
		wantFailure string
	}{
		{name: "missing", body: `{"id":"resp-1","object":"response","output":[]}`, wantFailure: "upstream_protocol_error"},
		{name: "in progress", body: `{"id":"resp-1","object":"response","status":"in_progress","output":[]}`, wantFailure: "upstream_protocol_error"},
		{name: "contradictory event", body: `{"type":"response.completed","response":{"id":"resp-1","object":"response","status":"in_progress","output":[]}}`, wantFailure: "upstream_protocol_error"},
		{name: "contradictory outer status", body: `{"type":"response.completed","status":"in_progress","response":{"id":"resp-1","object":"response","status":"completed","output":[]}}`, wantFailure: "upstream_protocol_error"},
		{name: "contradictory outer error", body: `{"type":"response.completed","error":{"code":"server_error"},"response":{"id":"resp-1","object":"response","status":"completed","output":[]}}`, wantFailure: "upstream_protocol_error"},
		{name: "unknown event type", body: `{"type":"response.created","id":"resp-1","object":"response","status":"completed","output":[]}`, wantFailure: "upstream_protocol_error"},
		{name: "null event type", body: `{"type":null,"id":"resp-1","object":"response","status":"completed","output":[]}`, wantFailure: "upstream_protocol_error"},
		{name: "blank event type", body: `{"type":"","id":"resp-1","object":"response","status":"completed","output":[]}`, wantFailure: "upstream_protocol_error"},
		{name: "normalized event type", body: `{"type":" RESPONSE.COMPLETED ","response":{"id":"resp-1","object":"response","status":"completed","output":[]}}`, wantFailure: "upstream_protocol_error"},
		{name: "empty error event", body: `{"type":"error","error":{}}`, wantFailure: "upstream_protocol_error"},
		{name: "completed with error", body: `{"id":"resp-1","object":"response","status":"completed","error":{"code":"server_error"},"output":[]}`, wantFailure: "upstream_protocol_error"},
		{name: "duplicate nested status", body: `{"type":"response.completed","response":{"id":"resp-1","object":"response","status":"in_progress","status":"completed","output":[]}}`, wantFailure: "upstream_protocol_error"},
		{name: "duplicate root error", body: `{"id":"resp-1","object":"response","status":"completed","error":{"message":"PRIVATE_DUPLICATE_SENTINEL"},"error":null,"output":[]}`, wantFailure: "upstream_protocol_error"},
		{name: "failed without error", body: `{"id":"resp-1","object":"response","status":"failed","output":[]}`, wantFailure: "upstream_protocol_error"},
		{name: "incomplete without details", body: `{"id":"resp-1","object":"response","status":"incomplete","output":[]}`, wantFailure: "upstream_protocol_error"},
		{name: "completed", body: `{"id":"resp-1","object":"response","status":"completed","output":[]}`},
		{name: "incomplete", body: `{"id":"resp-1","object":"response","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`},
		{name: "failed", body: `{"id":"resp-1","object":"response","status":"failed","error":{"code":"server_error"},"output":[]}`},
		{name: "streaming SSE", body: "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"object\":\"response\",\"status\":\"completed\"}}\n\n", stream: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Unix(92_500, 0).UTC()
			storage := executorStorage(now, storedModel{ID: "gpt-5.4", Format: formatOpenAIResponse})
			bridge := &fakeBridge{handler: func(_ string, _ any) (any, error) {
				return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(test.body)}, nil
			}}
			service := newPluginService(bridge)
			service.now = func() time.Time { return now }
			requestBody := fmt.Sprintf(`{"model":"gpt-5.4","input":[{"role":"user","content":"hi"}],"stream":%t}`, test.stream)
			raw, failure := service.executeHTTPRequest(mustJSON(t, rpcExecutorHTTPRequest{ExecutorHTTPRequest: pluginapi.ExecutorHTTPRequest{
				AuthID: "auth", URL: individualCopilotAPIURL + "/responses", Method: http.MethodPost,
				Body:        []byte(requestBody),
				StorageJSON: mustJSON(t, storage),
			}}))
			if test.wantFailure != "" {
				if failure == nil || failure.(*pluginFailure).code != test.wantFailure {
					t.Fatalf("failure = %#v", failure)
				}
				return
			}
			if failure != nil {
				t.Fatal(failure)
			}
			result := decodePluginResult[pluginapi.ExecutorHTTPResponse](t, raw)
			if string(result.Body) != test.body {
				t.Fatalf("native HTTP terminal payload changed: got=%s want=%s", result.Body, test.body)
			}
		})
	}
}

func TestExecuteHTTPRequestAppliesChatBodyPolicy(t *testing.T) {
	service := newPluginService(nil)
	now := time.Unix(93_000, 0).UTC()
	service.now = func() time.Time { return now }
	storage := executorStorage(now, storedModel{ID: "gpt-4.1", Format: formatOpenAI})
	var upstreamBody map[string]any
	bridge := &fakeBridge{handler: func(_ string, payload any) (any, error) {
		req := payload.(rpcHostHTTPRequest)
		if errDecode := json.Unmarshal(req.Body, &upstreamBody); errDecode != nil {
			t.Fatalf("decode upstream body: %v", errDecode)
		}
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"ok":true}`)}, nil
	}}
	service.bridge = bridge
	_, failure := service.executeHTTPRequest(mustJSON(t, rpcExecutorHTTPRequest{ExecutorHTTPRequest: pluginapi.ExecutorHTTPRequest{
		AuthID: "auth", URL: "https://api.individual.githubcopilot.com/chat/completions", Method: http.MethodPost,
		Body:        []byte(`{"model":"gpt-4.1","store":true,"reasoning_effort":"high","messages":[{"role":"developer","content":"rules"}]}`),
		StorageJSON: mustJSON(t, storage),
	}}))
	if failure != nil {
		t.Fatal(failure)
	}
	if _, exists := upstreamBody["store"]; exists {
		t.Fatalf("store reached Chat endpoint: %#v", upstreamBody)
	}
	if _, exists := upstreamBody["reasoning_effort"]; exists {
		t.Fatalf("undeclared reasoning_effort reached Chat endpoint: %#v", upstreamBody)
	}
	messages := upstreamBody["messages"].([]any)
	if messages[0].(map[string]any)["role"] != "system" {
		t.Fatalf("messages = %#v", messages)
	}
}

func TestExecuteHTTPRequestPreservesSameOriginNonInferenceRequest(t *testing.T) {
	service := newPluginService(nil)
	now := time.Unix(94_000, 0).UTC()
	service.now = func() time.Time { return now }
	storage := executorStorage(now)
	const requestBody = "not-json"
	bridge := &fakeBridge{handler: func(_ string, payload any) (any, error) {
		req := payload.(rpcHostHTTPRequest)
		if req.Method != http.MethodGet || req.URL != "https://api.individual.githubcopilot.com/models?editor=vscode" || string(req.Body) != requestBody {
			t.Fatalf("non-inference request = %#v", req)
		}
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"data":[]}`)}, nil
	}}
	service.bridge = bridge
	_, failure := service.executeHTTPRequest(mustJSON(t, rpcExecutorHTTPRequest{ExecutorHTTPRequest: pluginapi.ExecutorHTTPRequest{
		URL:         "https://api.individual.githubcopilot.com/models?editor=vscode",
		Method:      http.MethodGet,
		Body:        []byte(requestBody),
		StorageJSON: mustJSON(t, storage),
	}}))
	if failure != nil {
		t.Fatal(failure)
	}
}

func TestExecuteHTTPRequestPreservesClaudeBodyOnSameOriginNonInferenceRequest(t *testing.T) {
	service := newPluginService(nil)
	now := time.Unix(94_500, 0).UTC()
	service.now = func() time.Time { return now }
	storage := executorStorage(now, storedModel{ID: "claude-haiku-4.5", Format: formatClaude})
	const requestBody = `{ "model":"claude-haiku-4.5", "max_tokens":0, "tools":[{"name":"lookup","eager_input_streaming":true}] }`
	bridge := &fakeBridge{handler: func(_ string, payload any) (any, error) {
		req := payload.(rpcHostHTTPRequest)
		if string(req.Body) != requestBody {
			t.Fatalf("non-inference Claude body changed: got=%s want=%s", req.Body, requestBody)
		}
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"ok":true}`)}, nil
	}}
	service.bridge = bridge
	_, failure := service.executeHTTPRequest(mustJSON(t, rpcExecutorHTTPRequest{ExecutorHTTPRequest: pluginapi.ExecutorHTTPRequest{
		URL:         "https://api.individual.githubcopilot.com/models?editor=vscode",
		Method:      http.MethodGet,
		Body:        []byte(requestBody),
		StorageJSON: mustJSON(t, storage),
	}}))
	if failure != nil {
		t.Fatal(failure)
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
		if beta := http.Header(req.Headers).Get("Anthropic-Beta"); beta != interleavedThinkingBeta {
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

func TestExecuteHTTPRequestAppliesDiscoveredAdaptiveThinking(t *testing.T) {
	service := newPluginService(nil)
	now := time.Unix(96_000, 0).UTC()
	service.now = func() time.Time { return now }
	storage := executorStorage(now, storedModel{
		ID: "claude-opus-4.8", Format: formatClaude, AdaptiveThinking: true,
		ReasoningLevels: []string{"low", "medium", "high"}, ReasoningLevelsDeclared: true,
	})
	bridge := &fakeBridge{handler: func(method string, payload any) (any, error) {
		if method != pluginabi.MethodHostHTTPDo {
			t.Fatalf("method = %s", method)
		}
		req := payload.(rpcHostHTTPRequest)
		if headers := http.Header(req.Headers); headers.Get("Copilot-Integration-Id") != "vscode-chat" ||
			headers.Get("Authorization") != "Bearer tid=session;proxy-ep=proxy.individual.githubcopilot.com" {
			t.Fatalf("upstream headers = %#v", headers)
		}
		var body map[string]any
		if errDecode := json.Unmarshal(req.Body, &body); errDecode != nil {
			t.Fatalf("decode upstream body: %v", errDecode)
		}
		thinking := body["thinking"].(map[string]any)
		if thinking["type"] != "adaptive" {
			t.Fatalf("thinking = %#v", thinking)
		}
		if _, exists := body["temperature"]; exists {
			t.Fatalf("temperature reached HTTP bridge: %#v", body)
		}
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"ok":true}`)}, nil
	}}
	service.bridge = bridge
	body := []byte(`{
		"model":"claude-opus-4.8",
		"max_tokens":32000,
		"messages":[{"role":"user","content":"Hello"}],
		"temperature":0.7,
		"thinking":{"type":"enabled","budget_tokens":16384}
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

func TestExecuteHTTPRequestUsesDiscoveredAnthropicCapabilities(t *testing.T) {
	service := newPluginService(nil)
	now := time.Unix(97_000, 0).UTC()
	service.now = func() time.Time { return now }
	contextEditing := true
	storage := executorStorage(now, storedModel{
		ID: "claude-opus-4.8", Format: formatClaude, AdaptiveThinking: true,
		ReasoningLevels: []string{"low", "medium", "high", "xhigh"}, ReasoningLevelsDeclared: true,
		SupportsContextEditing: &contextEditing,
	})
	body := []byte(`{
		"model":"claude-opus-4.8",
		"messages":[{"role":"user","content":"Use the tool"}],
		"tools":[{"name":"lookup","input_schema":{"type":"object"}}],
		"thinking":{"type":"adaptive","display":"omitted"},
		"output_config":{"effort":"xhigh"},
		"context_management":{"edits":[]}
	}`)
	bridge := &fakeBridge{handler: func(method string, payload any) (any, error) {
		if method != pluginabi.MethodHostHTTPDo {
			t.Fatalf("method = %s", method)
		}
		req := payload.(rpcHostHTTPRequest)
		var upstream map[string]any
		if errDecode := json.Unmarshal(req.Body, &upstream); errDecode != nil {
			t.Fatalf("decode upstream body: %v", errDecode)
		}
		thinking := upstream["thinking"].(map[string]any)
		outputConfig := upstream["output_config"].(map[string]any)
		contextManagement := upstream["context_management"].(map[string]any)
		tools := upstream["tools"].([]any)
		tool := tools[0].(map[string]any)
		if thinking["type"] != "adaptive" || thinking["display"] != "omitted" || outputConfig["effort"] != "xhigh" ||
			contextManagement["edits"] == nil {
			t.Fatalf("newer model body = %#v", upstream)
		}
		if _, exists := tool["eager_input_streaming"]; exists {
			t.Fatalf("eager input streaming was invented: %#v", tool)
		}
		// 任意 caller beta 不透传；只按动态 context-editing capability 生成已知 beta。
		if beta := http.Header(req.Headers).Get("Anthropic-Beta"); beta != contextManagementBeta {
			t.Fatalf("Anthropic-Beta = %q", beta)
		}
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"ok":true}`)}, nil
	}}
	service.bridge = bridge
	_, failure := service.executeHTTPRequest(mustJSON(t, rpcExecutorHTTPRequest{ExecutorHTTPRequest: pluginapi.ExecutorHTTPRequest{
		URL:         "https://api.individual.githubcopilot.com/v1/messages",
		Method:      http.MethodPost,
		Headers:     http.Header{"Anthropic-Beta": []string{"feature-one", "feature-two"}},
		Body:        body,
		StorageJSON: mustJSON(t, storage),
	}}))
	if failure != nil {
		t.Fatal(failure)
	}
}

func executorStorage(now time.Time, models ...storedModel) copilotStorage {
	for index := range models {
		if models[index].Streaming == nil {
			streaming := true
			models[index].Streaming = &streaming
		}
	}
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

func TestPrepareInferenceRejectsResponsesCompactAlt(t *testing.T) {
	service := newPluginService(nil)
	now := time.Unix(150_000, 0).UTC()
	service.now = func() time.Time { return now }
	storage := executorStorage(now, storedModel{ID: "gpt-5.4", Format: formatOpenAIResponse})
	payload := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hi"}]}`)
	_, failure := service.prepareInference(pluginapi.ExecutorRequest{
		AuthID: "auth", Model: "gpt-5.4", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
		Payload: payload, StorageJSON: mustJSON(t, storage), Alt: "responses/compact",
	}, false)
	if failure == nil || failure.code != "unsupported_feature" {
		t.Fatalf("failure = %#v", failure)
	}
}

func TestPrepareInferenceRequiresExplicitStreamingCapability(t *testing.T) {
	service := newPluginService(nil)
	now := time.Unix(150_500, 0).UTC()
	service.now = func() time.Time { return now }
	trueValue := true
	falseValue := false
	payload := []byte(`{"model":"gpt-4.1","messages":[{"role":"user","content":"hi"}]}`)
	for _, test := range []struct {
		name      string
		streaming *bool
		wantError bool
	}{
		{name: "true", streaming: &trueValue},
		{name: "false", streaming: &falseValue, wantError: true},
		{name: "omitted", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			storage := executorStorage(now, storedModel{ID: "gpt-4.1", Format: formatOpenAI, Streaming: test.streaming})
			storage.Models[0].Streaming = test.streaming
			_, failure := service.prepareInference(pluginapi.ExecutorRequest{
				AuthID: "auth", Model: "gpt-4.1", Format: formatOpenAI, SourceFormat: formatOpenAI,
				Payload: payload, StorageJSON: mustJSON(t, storage),
			}, true)
			if (failure != nil) != test.wantError {
				t.Fatalf("failure = %#v, wantError=%v", failure, test.wantError)
			}
			if failure != nil && failure.code != "unsupported_feature" {
				t.Fatalf("failure = %#v", failure)
			}
		})
	}
}

func TestExecuteHTTPRequestRejectsUnsupportedStreamingBody(t *testing.T) {
	service := newPluginService(nil)
	now := time.Unix(150_750, 0).UTC()
	service.now = func() time.Time { return now }
	falseValue := false
	storage := executorStorage(now, storedModel{ID: "gpt-4.1", Format: formatOpenAI, Streaming: &falseValue})
	bridge := &fakeBridge{handler: func(method string, _ any) (any, error) {
		t.Fatalf("unexpected host call %s", method)
		return nil, nil
	}}
	service.bridge = bridge
	_, failure := service.executeHTTPRequest(mustJSON(t, rpcExecutorHTTPRequest{ExecutorHTTPRequest: pluginapi.ExecutorHTTPRequest{
		URL: "https://api.individual.githubcopilot.com/chat/completions", Method: http.MethodPost,
		Body: []byte(`{"model":"gpt-4.1","stream":true,"messages":[]}`), StorageJSON: mustJSON(t, storage),
	}}))
	if failure == nil || failure.(*pluginFailure).code != "unsupported_feature" {
		t.Fatalf("failure = %#v", failure)
	}
	if len(bridge.snapshot()) != 0 {
		t.Fatal("unsupported raw stream request reached host bridge")
	}
}

func TestPrepareInferenceRejectsCrossFormatWithResponsesStatefulMarkers(t *testing.T) {
	service := newPluginService(nil)
	now := time.Unix(151_000, 0).UTC()
	service.now = func() time.Time { return now }
	for _, test := range []struct {
		name    string
		payload string
	}{
		{name: "previous_response_id", payload: `{"model":"gpt-4.1","previous_response_id":"resp_1","input":[{"role":"user","content":"hi"}]}`},
		{name: "context_management", payload: `{"model":"gpt-4.1","context_management":[{"type":"compaction","compact_threshold":1000}],"input":[{"role":"user","content":"hi"}]}`},
		{name: "compaction input item", payload: `{"model":"gpt-4.1","input":[{"type":"compaction","id":"cmp_1","encrypted_content":"enc"},{"role":"user","content":"hi"}]}`},
		{name: "encrypted reasoning input item", payload: `{"model":"gpt-4.1","input":[{"type":"reasoning","id":"rs_1","encrypted_content":"enc"},{"role":"user","content":"hi"}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			// The model routes to Chat Completions, so a Responses-shaped payload forces
			// cross-format translation, which cannot be trusted to preserve Responses opaque
			// continuation state.
			storage := executorStorage(now, storedModel{ID: "gpt-4.1", Format: formatOpenAI})
			_, failure := service.prepareInference(pluginapi.ExecutorRequest{
				AuthID: "auth", Model: "gpt-4.1", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
				Payload: []byte(test.payload), StorageJSON: mustJSON(t, storage),
			}, false)
			if failure == nil || failure.code != "format_mismatch" {
				t.Fatalf("failure = %#v", failure)
			}
		})
	}
}

func TestPrepareInferenceAllowsNativeResponsesStatefulMarkers(t *testing.T) {
	service := newPluginService(nil)
	now := time.Unix(152_000, 0).UTC()
	service.now = func() time.Time { return now }
	storage := executorStorage(now, storedModel{ID: "gpt-5.4", Format: formatOpenAIResponse})
	payload := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_1","input":[{"role":"user","content":"hi"}]}`)
	prepared, failure := service.prepareInference(pluginapi.ExecutorRequest{
		AuthID: "auth", Model: "gpt-5.4", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
		Payload: payload, StorageJSON: mustJSON(t, storage),
	}, false)
	if failure != nil {
		t.Fatal(failure)
	}
	var body map[string]any
	if json.Unmarshal(prepared.upstreamPayload, &body) != nil || body["previous_response_id"] != "resp_1" {
		t.Fatalf("previous_response_id was not preserved: %s", prepared.upstreamPayload)
	}
}

func TestNormalizeOpenAIResponsesDefaultsWhenAbsent(t *testing.T) {
	route := modelRoute{Format: formatOpenAIResponse, Family: "gpt-6", MaxPromptTokens: 200_000}
	out, errNormalize := normalizeInferencePayloadForRoute([]byte(`{"model":"gpt-6","input":[{"role":"user","content":"hi"}]}`), "gpt-6", route, false, true)
	if errNormalize != nil {
		t.Fatal(errNormalize)
	}
	var body map[string]any
	if json.Unmarshal(out, &body) != nil {
		t.Fatalf("decode: %s", out)
	}
	if body["truncation"] != "disabled" {
		t.Fatalf("truncation = %#v", body["truncation"])
	}
	include, ok := body["include"].([]any)
	if !ok || len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %#v", body["include"])
	}
	if got := responsesCompactionThresholdFromBody(t, body); got != 180000 {
		t.Fatalf("compaction threshold = %v, want 180000", got)
	}
}

func TestNormalizeOpenAIResponsesPreservesCallerValues(t *testing.T) {
	route := modelRoute{Format: formatOpenAIResponse, Family: "gpt-6", MaxPromptTokens: 200_000}
	raw := []byte(`{
		"model":"gpt-6",
		"truncation":"auto",
		"include":["reasoning.encrypted_content","foo"],
		"context_management":[{"type":"compaction","compact_threshold":42}],
		"input":[{"role":"user","content":"hi"}]
	}`)
	out, errNormalize := normalizeInferencePayloadForRoute(raw, "gpt-6", route, false, true)
	if errNormalize != nil {
		t.Fatal(errNormalize)
	}
	var body map[string]any
	if json.Unmarshal(out, &body) != nil {
		t.Fatalf("decode: %s", out)
	}
	if body["truncation"] != "auto" {
		t.Fatalf("caller truncation was overridden: %#v", body["truncation"])
	}
	include, ok := body["include"].([]any)
	if !ok || len(include) != 2 {
		t.Fatalf("caller include was overridden: %#v", body["include"])
	}
	if got := responsesCompactionThresholdFromBody(t, body); got != 42 {
		t.Fatalf("caller context_management was not preserved exactly: threshold=%v", got)
	}
}

func TestNormalizeOpenAIResponsesSkipsExcludedFamilies(t *testing.T) {
	for _, family := range []string{"gpt-5", "gpt-5.1", "gpt-5.2"} {
		t.Run(family, func(t *testing.T) {
			route := modelRoute{Format: formatOpenAIResponse, Family: family, MaxPromptTokens: 200_000}
			out, errNormalize := normalizeInferencePayloadForRoute([]byte(`{"model":"m","input":[{"role":"user","content":"hi"}]}`), "m", route, false, true)
			if errNormalize != nil {
				t.Fatal(errNormalize)
			}
			var body map[string]any
			if json.Unmarshal(out, &body) != nil {
				t.Fatalf("decode: %s", out)
			}
			if _, exists := body["context_management"]; exists {
				t.Fatalf("excluded family %s unexpectedly got context_management: %#v", family, body["context_management"])
			}
		})
	}
}

func TestPrepareInferenceLegacyResponsesWithoutFamilyDoesNotEnableCompaction(t *testing.T) {
	service := newPluginService(nil)
	now := time.Unix(154_000, 0).UTC()
	service.now = func() time.Time { return now }
	var legacyModel storedModel
	if errUnmarshal := json.Unmarshal([]byte(`{"id":"gpt-5.2","format":"openai-response"}`), &legacyModel); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	storage := executorStorage(now)
	storage.Models = []storedModel{legacyModel}
	payload := []byte(`{"model":"gpt-5.2","input":[{"role":"user","content":"hi"}]}`)
	prepared, failure := service.prepareInference(pluginapi.ExecutorRequest{
		AuthID: "auth", Model: "gpt-5.2", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
		Payload: payload, StorageJSON: mustJSON(t, storage),
	}, false)
	if failure != nil {
		t.Fatal(failure)
	}
	var body map[string]any
	if json.Unmarshal(prepared.upstreamPayload, &body) != nil {
		t.Fatalf("decode: %s", prepared.upstreamPayload)
	}
	if _, exists := body["context_management"]; exists {
		t.Fatalf("legacy route without family unexpectedly enabled compaction: %#v", body["context_management"])
	}

	callerPayload := []byte(`{"model":"gpt-5.2","context_management":[{"type":"compaction","compact_threshold":42}],"input":[]}`)
	prepared, failure = service.prepareInference(pluginapi.ExecutorRequest{
		AuthID: "auth", Model: "gpt-5.2", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
		Payload: callerPayload, StorageJSON: mustJSON(t, storage),
	}, false)
	if failure != nil {
		t.Fatal(failure)
	}
	if json.Unmarshal(prepared.upstreamPayload, &body) != nil || responsesCompactionThresholdFromBody(t, body) != 42 {
		t.Fatalf("caller context_management was not preserved: %s", prepared.upstreamPayload)
	}
}

func TestNormalizeOpenAIResponsesRespectsConfigSwitch(t *testing.T) {
	route := modelRoute{Format: formatOpenAIResponse, Family: "gpt-6", MaxPromptTokens: 200_000}
	out, errNormalize := normalizeInferencePayloadForRoute([]byte(`{"model":"gpt-6","input":[{"role":"user","content":"hi"}]}`), "gpt-6", route, false, false)
	if errNormalize != nil {
		t.Fatal(errNormalize)
	}
	var body map[string]any
	if json.Unmarshal(out, &body) != nil {
		t.Fatalf("decode: %s", out)
	}
	if _, exists := body["context_management"]; exists {
		t.Fatalf("context_management was added while the config switch is disabled: %#v", body["context_management"])
	}
}

func TestNormalizeOpenAIResponsesThresholdFallback(t *testing.T) {
	route := modelRoute{Format: formatOpenAIResponse, Family: "gpt-6"}
	out, errNormalize := normalizeInferencePayloadForRoute([]byte(`{"model":"gpt-6","input":[{"role":"user","content":"hi"}]}`), "gpt-6", route, false, true)
	if errNormalize != nil {
		t.Fatal(errNormalize)
	}
	var body map[string]any
	if json.Unmarshal(out, &body) != nil {
		t.Fatalf("decode: %s", out)
	}
	if got := responsesCompactionThresholdFromBody(t, body); got != 50000 {
		t.Fatalf("fallback compaction threshold = %v, want 50000", got)
	}
}

func responsesCompactionThresholdFromBody(t *testing.T, body map[string]any) float64 {
	t.Helper()
	items, ok := body["context_management"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("context_management missing: %#v", body["context_management"])
	}
	item, ok := items[0].(map[string]any)
	if !ok || item["type"] != "compaction" {
		t.Fatalf("context_management[0] = %#v", items[0])
	}
	threshold, ok := item["compact_threshold"].(float64)
	if !ok {
		t.Fatalf("compact_threshold = %#v", item["compact_threshold"])
	}
	return threshold
}

func TestNormalizeOpenAIChatPreservesDeclaredReasoningEffort(t *testing.T) {
	route := modelRoute{Format: formatOpenAI, ReasoningLevels: []string{"low", "high"}}
	payload := []byte(`{"model":"custom","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	out, errNormalize := normalizeInferencePayloadForRoute(payload, "custom", route, false, true)
	if errNormalize != nil {
		t.Fatal(errNormalize)
	}
	var body map[string]any
	if json.Unmarshal(out, &body) != nil {
		t.Fatalf("decode: %s", out)
	}
	if body["reasoning_effort"] != "high" {
		t.Fatalf("declared reasoning_effort was removed: %#v", body)
	}
}

func TestNormalizeOpenAIChatRemovesUndeclaredReasoningEffort(t *testing.T) {
	route := modelRoute{Format: formatOpenAI, ReasoningLevels: []string{"low", "high"}}
	payload := []byte(`{"model":"custom","reasoning_effort":"medium","messages":[{"role":"user","content":"hi"}]}`)
	out, errNormalize := normalizeInferencePayloadForRoute(payload, "custom", route, false, true)
	if errNormalize != nil {
		t.Fatal(errNormalize)
	}
	var body map[string]any
	if json.Unmarshal(out, &body) != nil {
		t.Fatalf("decode: %s", out)
	}
	if _, exists := body["reasoning_effort"]; exists {
		t.Fatalf("undeclared reasoning_effort was kept: %#v", body)
	}
}

func TestNormalizeOpenAIChatRemovesReasoningEffortWithoutDeclaredLevels(t *testing.T) {
	route := modelRoute{Format: formatOpenAI}
	payload := []byte(`{"model":"custom","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	out, errNormalize := normalizeInferencePayloadForRoute(payload, "custom", route, false, true)
	if errNormalize != nil {
		t.Fatal(errNormalize)
	}
	var body map[string]any
	if json.Unmarshal(out, &body) != nil {
		t.Fatalf("decode: %s", out)
	}
	if _, exists := body["reasoning_effort"]; exists {
		t.Fatalf("reasoning_effort was kept for a model with no declared levels: %#v", body)
	}
}

func TestPreparedInferenceLogFieldsIncludeResponsesDiagnostics(t *testing.T) {
	service := newPluginService(nil)
	now := time.Unix(200_000, 0).UTC()
	service.now = func() time.Time { return now }
	storage := executorStorage(now, storedModel{ID: "gpt-5.4", Format: formatOpenAIResponse, Family: "gpt-5.4", MaxPromptTokens: 200_000})
	payload := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_1","input":[{"role":"user","content":"hi"}]}`)
	prepared, failure := service.prepareInference(pluginapi.ExecutorRequest{
		AuthID: "auth", Model: "gpt-5.4", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
		Payload: payload, OriginalRequest: payload, StorageJSON: mustJSON(t, storage),
	}, false)
	if failure != nil {
		t.Fatal(failure)
	}
	fields := preparedInferenceLogFields(prepared, false)
	if fields["responses_context_management_enabled"] != true {
		t.Fatalf("responses_context_management_enabled = %#v", fields["responses_context_management_enabled"])
	}
	if fields["responses_compact_threshold"] != int64(180000) {
		t.Fatalf("responses_compact_threshold = %#v", fields["responses_compact_threshold"])
	}
	if fields["responses_state_present"] != true {
		t.Fatalf("responses_state_present = %#v", fields["responses_state_present"])
	}
}

func TestPreparedInferenceLogFieldsOmitResponsesDiagnosticsForOtherFormats(t *testing.T) {
	service := newPluginService(nil)
	now := time.Unix(201_000, 0).UTC()
	service.now = func() time.Time { return now }
	storage := executorStorage(now, storedModel{ID: "gpt-4.1", Format: formatOpenAI})
	payload := []byte(`{"model":"gpt-4.1","messages":[{"role":"user","content":"hi"}]}`)
	prepared, failure := service.prepareInference(pluginapi.ExecutorRequest{
		AuthID: "auth", Model: "gpt-4.1", Format: formatOpenAI, SourceFormat: formatOpenAI,
		Payload: payload, OriginalRequest: payload, StorageJSON: mustJSON(t, storage),
	}, false)
	if failure != nil {
		t.Fatal(failure)
	}
	fields := preparedInferenceLogFields(prepared, false)
	if _, exists := fields["responses_context_management_enabled"]; exists {
		t.Fatalf("unexpected responses diagnostics for chat format: %#v", fields)
	}
	if _, exists := fields["responses_state_present"]; exists {
		t.Fatalf("unexpected responses_state_present for chat format: %#v", fields)
	}
}
