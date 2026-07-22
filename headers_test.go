package main

import (
	"net/http"
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
	if headers.Get("Anthropic-Beta") != "feature-test" || headers.Get("X-Initiator") != "user" || headers.Get("Openai-Intent") != copilotOpenAIIntent {
		t.Fatalf("Copilot headers = %#v", headers)
	}
}

func TestInferenceHeadersKeepNewAnthropicModelsOnOriginalPath(t *testing.T) {
	headers := inferenceHeaders("real-session", formatClaude, []byte(`{
		"model":"claude-opus-4.8",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"lookup","input_schema":{"type":"object"}}]
	}`), http.Header{
		"Anthropic-Beta": []string{"feature-one", "feature-two"},
	})
	if beta := headers.Get("Anthropic-Beta"); beta != "feature-one" {
		t.Fatalf("Anthropic-Beta = %q", beta)
	}
}
