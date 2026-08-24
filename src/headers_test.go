package main

import (
	"net/http"
	"regexp"
	"testing"
)

func TestInferInitiator(t *testing.T) {
	for _, test := range []struct {
		payload string
		want    string
	}{
		{payload: `{"messages":[{"role":"assistant"},{"role":"user","content":"next"}]}`, want: "user"},
		{payload: `{"messages":[{"role":"user"},{"role":"assistant","content":"next"}]}`, want: "agent"},
		{payload: `{"messages":[{"role":"assistant"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"done"}]}]}`, want: "user"},
		{payload: `{"input":[{"role":"user","content":[{"type":"input_text","text":"next"}]}]}`, want: "user"},
		{payload: `{}`, want: "user"},
	} {
		if got := inferInitiator([]byte(test.payload)); got != test.want {
			t.Fatalf("inferInitiator(%s) = %q, want %q", test.payload, got, test.want)
		}
	}
}

func TestVisionDetectionAcrossProtocols(t *testing.T) {
	for _, payload := range []string{
		`{"messages":[{"content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,x"}}]}]}`,
		`{"input":[{"content":[{"type":"input_image","image_url":"data:image/png;base64,x"}]}]}`,
		`{"messages":[{"content":[{"type":"tool_result","content":[{"type":"image","source":{"type":"base64"}}]}]}]}`,
	} {
		if !containsVisionContent([]byte(payload)) {
			t.Fatalf("vision not detected in %s", payload)
		}
	}
	if containsVisionContent([]byte(`{"messages":[{"content":"text"}]}`)) {
		t.Fatal("text-only payload detected as vision")
	}
}

func TestInferenceHeadersRequireVisionCapabilityAndContent(t *testing.T) {
	imagePayload := []byte(`{"messages":[{"content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,x"}}]}]}`)
	withoutCapability := inferenceHeadersForRoute("session", modelRoute{Format: formatOpenAI}, imagePayload, nil, formatOpenAI)
	if got := withoutCapability.Get("Copilot-Vision-Request"); got != "" {
		t.Fatalf("vision header without capability = %q", got)
	}
	withCapability := inferenceHeadersForRoute("session", modelRoute{Format: formatOpenAI, Vision: true}, imagePayload, nil, formatOpenAI)
	if got := withCapability.Get("Copilot-Vision-Request"); got != "true" {
		t.Fatalf("vision header with capability and content = %q", got)
	}
	textOnly := inferenceHeadersForRoute("session", modelRoute{Format: formatOpenAI, Vision: true}, []byte(`{"messages":[{"content":"text"}]}`), nil, formatOpenAI)
	if got := textOnly.Get("Copilot-Vision-Request"); got != "" {
		t.Fatalf("vision header without image content = %q", got)
	}
}

func TestInferenceHeadersProtectAuthorization(t *testing.T) {
	headers := inferenceHeaders("real-session", formatClaude, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), http.Header{
		"Authorization":      []string{"Bearer attacker"},
		"X-Api-Key":          []string{"attacker"},
		"Anthropic-Beta":     []string{"feature-test"},
		"X-Interaction-Type": []string{"agent-session"},
	})
	if headers.Get("Authorization") != "Bearer real-session" || headers.Get("X-Api-Key") != "" {
		t.Fatalf("authorization headers = %#v", headers)
	}
	if headers.Get("X-Initiator") != "user" {
		t.Fatalf("X-Initiator = %q", headers.Get("X-Initiator"))
	}
	// "agent-session" is not in the pinned VS Code vocabulary, so it must fall back to the
	// Claude-output default rather than reach the wire unchanged.
	if headers.Get("X-Interaction-Type") != "messages-proxy" || headers.Get("Openai-Intent") != "messages-proxy" {
		t.Fatalf("interaction headers = %#v", headers)
	}
	// Arbitrary caller-supplied betas must never reach the wire; only source-proven betas are computed.
	if headers.Get("Anthropic-Beta") != interleavedThinkingBeta {
		t.Fatalf("Anthropic-Beta = %q", headers.Get("Anthropic-Beta"))
	}
}

func TestInferenceHeadersProtectCanonicalIdentity(t *testing.T) {
	route := modelRoute{Format: formatOpenAI}
	headers := inferenceHeadersForRoute("real-session", route, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), http.Header{
		"Authorization": []string{"Bearer attacker"},
	}, formatOpenAI)
	if headers.Get("User-Agent") != copilotUserAgent || headers.Get("Editor-Version") != copilotEditorVersion ||
		headers.Get("Editor-Plugin-Version") != copilotPluginVersion || headers.Get("Copilot-Integration-Id") != copilotIntegrationID {
		t.Fatalf("identity headers = %#v, want canonical versioned identity", headers)
	}
	if headers.Get("Authorization") != "Bearer real-session" || headers.Get("Openai-Intent") != "conversation-other" {
		t.Fatalf("protected headers = %#v", headers)
	}
}

func TestInferenceHeadersUseVSCode1134Identity(t *testing.T) {
	headers := inferenceHeaders("session", formatOpenAI, []byte(`{}`), nil)
	if headers.Get("User-Agent") != "GitHubCopilotChat/0.62.0" || headers.Get("Editor-Version") != "vscode/1.134.0" ||
		headers.Get("Editor-Plugin-Version") != "copilot-chat/0.62.0" || headers.Get("Copilot-Integration-Id") != "vscode-chat" ||
		headers.Get("X-GitHub-Api-Version") != "2026-08-01" {
		t.Fatalf("identity headers = %#v", headers)
	}
}

func TestBrokerHeadersUseVSCodeTokenAPI(t *testing.T) {
	headers := brokerHeaders("github-token")
	if headers.Get("Authorization") != "token github-token" || headers.Get("X-GitHub-Api-Version") != "2025-04-01" {
		t.Fatalf("broker headers = %#v", headers)
	}
}

func TestInferenceHeadersOmitAnthropicVersion(t *testing.T) {
	headers := inferenceHeaders("session", formatClaude, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), nil)
	if got := headers.Get("Anthropic-Version"); got != "" {
		t.Fatalf("Anthropic-Version = %q, want absent (pinned VS Code source never sends it)", got)
	}
}

var testUUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func TestInferenceHeadersRequestCorrelation(t *testing.T) {
	headers := inferenceHeaders("session", formatOpenAI, []byte(`{}`), nil)
	requestID := headers.Get("X-Request-Id")
	if !testUUIDPattern.MatchString(requestID) {
		t.Fatalf("X-Request-Id = %q is not a UUID", requestID)
	}
	if headers.Get("X-Agent-Task-Id") != requestID {
		t.Fatalf("X-Agent-Task-Id = %q, want %q", headers.Get("X-Agent-Task-Id"), requestID)
	}
	if headers.Get("X-Interaction-Id") != requestID {
		t.Fatalf("X-Interaction-Id = %q, want request id %q when caller supplies none", headers.Get("X-Interaction-Id"), requestID)
	}

	callerInteractionID := "5b1b6c8e-2e77-4a25-9f0f-8f6a6d1e2b3a"
	withCaller := inferenceHeaders("session", formatOpenAI, []byte(`{}`), http.Header{"X-Interaction-Id": []string{callerInteractionID}})
	if withCaller.Get("X-Interaction-Id") != callerInteractionID {
		t.Fatalf("X-Interaction-Id = %q, want caller UUID %q", withCaller.Get("X-Interaction-Id"), callerInteractionID)
	}
	withInvalidCaller := inferenceHeaders("session", formatOpenAI, []byte(`{}`), http.Header{"X-Interaction-Id": []string{"not-a-uuid"}})
	if withInvalidCaller.Get("X-Interaction-Id") != withInvalidCaller.Get("X-Request-Id") {
		t.Fatalf("invalid caller X-Interaction-Id was not replaced by the request id: %#v", withInvalidCaller)
	}

	other := inferenceHeaders("session", formatOpenAI, []byte(`{}`), nil)
	if other.Get("X-Request-Id") == requestID {
		t.Fatal("X-Request-Id was reused across independent requests")
	}
}

func TestInferenceHeadersMapOpenAIClientRequestID(t *testing.T) {
	clientRequestID := "3a23a4b5-6c7d-4e8f-9012-3456789abcde"
	callerInteractionID := "6b1b6c8e-2e77-4a25-9f0f-8f6a6d1e2b3a"
	headers := inferenceHeaders("session", formatOpenAI, []byte(`{}`), http.Header{
		"X-Client-Request-Id": []string{clientRequestID},
		"X-Interaction-Id":    []string{callerInteractionID},
	})
	if headers.Get("X-Request-Id") != clientRequestID || headers.Get("X-Agent-Task-Id") != clientRequestID {
		t.Fatalf("OpenAI client request ID was not mapped to Copilot request headers: %#v", headers)
	}
	if headers.Get("X-Interaction-Id") != callerInteractionID {
		t.Fatalf("X-Interaction-Id = %q, want explicit caller value %q", headers.Get("X-Interaction-Id"), callerInteractionID)
	}

	invalid := inferenceHeaders("session", formatOpenAI, []byte(`{}`), http.Header{
		"X-Client-Request-Id": []string{"codex-trace-123"},
	})
	if !testUUIDPattern.MatchString(invalid.Get("X-Request-Id")) || invalid.Get("X-Agent-Task-Id") != invalid.Get("X-Request-Id") {
		t.Fatalf("invalid OpenAI client request ID did not fall back to a generated Copilot UUID: %#v", invalid)
	}
}

func TestInferenceHeadersRestrictInteractionTypeVocabulary(t *testing.T) {
	for _, test := range []struct {
		name         string
		outputFormat string
		callerValue  string
		want         string
	}{
		{name: "responses default", outputFormat: formatOpenAIResponse, want: "responses-proxy"},
		{name: "messages default", outputFormat: formatClaude, want: "messages-proxy"},
		{name: "chat default", outputFormat: formatOpenAI, want: "conversation-other"},
		{name: "valid override", outputFormat: formatOpenAI, callerValue: "conversation-subagent", want: "conversation-subagent"},
		{name: "invalid override falls back", outputFormat: formatOpenAIResponse, callerValue: "not-a-real-type", want: "responses-proxy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			caller := http.Header{}
			if test.callerValue != "" {
				caller.Set("X-Interaction-Type", test.callerValue)
			}
			headers := inferenceHeadersForRoute("session", modelRoute{Format: test.outputFormat}, []byte(`{}`), caller, test.outputFormat)
			if headers.Get("X-Interaction-Type") != test.want || headers.Get("Openai-Intent") != test.want {
				t.Fatalf("interaction headers = %#v, want %q", headers, test.want)
			}
		})
	}
}

func TestInferenceHeadersRestrictInitiatorVocabulary(t *testing.T) {
	agentPayload := []byte(`{"messages":[{"role":"user"},{"role":"assistant","content":"next"}]}`)
	if got := inferenceHeaders("session", formatOpenAI, agentPayload, http.Header{"X-Initiator": []string{"USER"}}).Get("X-Initiator"); got != "user" {
		t.Fatalf("X-Initiator = %q, want caller override honored case-insensitively", got)
	}
	if got := inferenceHeaders("session", formatOpenAI, agentPayload, http.Header{"X-Initiator": []string{"bogus"}}).Get("X-Initiator"); got != "agent" {
		t.Fatalf("X-Initiator = %q, want semantic fallback for an invalid caller value", got)
	}
}

func TestAnthropicBetaHeaderIsSourceProven(t *testing.T) {
	toolSearchPayload := []byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"tool_search","input_schema":{"type":"object"}}]}`)
	contextManagementPayload := []byte(`{"messages":[{"role":"user","content":"hi"}],"context_management":{"edits":[]}}`)
	extendedCachePayload := []byte(`{"messages":[{"role":"user","content":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]}`)
	plainPayload := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	trueValue := true

	for _, test := range []struct {
		name    string
		route   modelRoute
		payload []byte
		caller  http.Header
		want    string
	}{
		{name: "non-adaptive route always sends interleaved thinking", route: modelRoute{Format: formatClaude}, payload: plainPayload, want: interleavedThinkingBeta},
		{name: "adaptive route never sends interleaved thinking", route: modelRoute{Format: formatClaude, AdaptiveThinking: true}, payload: plainPayload, want: ""},
		{name: "tool search beta requires capability and body evidence", route: modelRoute{Format: formatClaude, AdaptiveThinking: true, SupportsToolSearch: &trueValue}, payload: toolSearchPayload, want: advancedToolUseBeta},
		{name: "tool search beta absent without capability", route: modelRoute{Format: formatClaude, AdaptiveThinking: true}, payload: toolSearchPayload, want: ""},
		{name: "context management beta requires capability and body evidence", route: modelRoute{Format: formatClaude, AdaptiveThinking: true, SupportsContextEditing: &trueValue}, payload: contextManagementPayload, want: contextManagementBeta},
		{name: "context management beta absent without body evidence", route: modelRoute{Format: formatClaude, AdaptiveThinking: true, SupportsContextEditing: &trueValue}, payload: plainPayload, want: ""},
		{name: "extended cache beta requires ttl 1h in body", route: modelRoute{Format: formatClaude, AdaptiveThinking: true}, payload: extendedCachePayload, want: extendedCacheTTLBeta},
		{name: "arbitrary caller beta values are ignored", route: modelRoute{Format: formatClaude, AdaptiveThinking: true}, payload: plainPayload, caller: http.Header{"Anthropic-Beta": []string{"whatever-caller-wants"}}, want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			headers := inferenceHeadersForRoute("session", test.route, test.payload, test.caller, formatClaude)
			if got := headers.Get("Anthropic-Beta"); got != test.want {
				t.Fatalf("Anthropic-Beta = %q, want %q", got, test.want)
			}
		})
	}
}
