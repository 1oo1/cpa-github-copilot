package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

const (
	copilotUserAgent        = "GitHubCopilotChat/0.35.0"
	copilotEditorVersion    = "vscode/1.107.0"
	copilotPluginVersion    = "copilot-chat/0.35.0"
	copilotIntegrationID    = "vscode-chat"
	copilotAPIVersion       = "2026-06-01"
	copilotOpenAIIntent     = "conversation-edits"
	defaultAnthropicVersion = "2023-06-01"
	fineGrainedToolBeta     = "fine-grained-tool-streaming-2025-05-14"
	interleavedThinkingBeta = "interleaved-thinking-2025-05-14"
	advisorToolBeta         = "advisor-tool-2026-03-01"
)

func brokerHeaders(githubToken string) http.Header {
	headers := copilotIdentityHeaders()
	headers.Set("Accept", "application/json")
	headers.Set("Authorization", "Bearer "+githubToken)
	return headers
}

func copilotIdentityHeaders() http.Header {
	return http.Header{
		"User-Agent":             []string{copilotUserAgent},
		"Editor-Version":         []string{copilotEditorVersion},
		"Editor-Plugin-Version":  []string{copilotPluginVersion},
		"Copilot-Integration-Id": []string{copilotIntegrationID},
	}
}

func inferenceHeaders(sessionToken, format string, payload []byte, caller http.Header) http.Header {
	headers := copilotIdentityHeaders()
	headers.Set("Accept", "application/json")
	headers.Set("Content-Type", "application/json")
	headers.Set("Authorization", "Bearer "+sessionToken)
	headers.Set("X-GitHub-Api-Version", copilotAPIVersion)
	headers.Set("Openai-Intent", copilotOpenAIIntent)
	headers.Set("X-Initiator", inferInitiator(payload))
	if containsVisionContent(payload) {
		headers.Set("Copilot-Vision-Request", "true")
	}
	if format == formatClaude {
		headers.Set("Anthropic-Version", defaultAnthropicVersion)
		model := modelFromPayload(payload)
		if usesAnthropicLegacyCompatibility(model) {
			beta := strings.Join(caller.Values("Anthropic-Beta"), ",")
			if usesAnthropicBudgetThinking(model) {
				beta = ""
			}
			if !supportsAnthropicEagerToolInputStreaming(model) && hasAnthropicTools(payload) {
				beta = appendAnthropicBeta(beta, fineGrainedToolBeta)
			}
			if usesAnthropicBudgetThinking(model) && hasEnabledAnthropicThinking(payload) {
				beta = appendAnthropicBeta(beta, interleavedThinkingBeta)
			}
			if beta = strings.TrimSpace(beta); beta != "" {
				headers.Set("Anthropic-Beta", beta)
			}
		} else if beta := supportedAnthropicBetas(model, caller.Get("Anthropic-Beta")); beta != "" {
			headers.Set("Anthropic-Beta", beta)
		}
	}
	if interaction := strings.TrimSpace(caller.Get("X-Interaction-Type")); interaction != "" {
		headers.Set("X-Interaction-Type", interaction)
	}
	return headers
}

func hasAnthropicTools(payload []byte) bool {
	var root struct {
		Tools []json.RawMessage `json:"tools"`
	}
	return json.Unmarshal(payload, &root) == nil && len(root.Tools) > 0
}

func hasEnabledAnthropicThinking(payload []byte) bool {
	var root struct {
		Thinking struct {
			Type string `json:"type"`
		} `json:"thinking"`
	}
	return json.Unmarshal(payload, &root) == nil && strings.EqualFold(strings.TrimSpace(root.Thinking.Type), "enabled")
}

func appendAnthropicBeta(raw, required string) string {
	values := make([]string, 0, 2)
	seen := make(map[string]struct{})
	for _, value := range append(strings.Split(raw, ","), required) {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		values = append(values, value)
	}
	return strings.Join(values, ",")
}

func supportedAnthropicBetas(model, raw string) string {
	if !strings.EqualFold(normalizeModelID(model), "claude-opus-4.8") {
		return strings.TrimSpace(raw)
	}
	values := make([]string, 0, len(strings.Split(raw, ",")))
	for _, value := range strings.Split(raw, ",") {
		value = strings.TrimSpace(value)
		if value == "" || strings.EqualFold(value, advisorToolBeta) {
			continue
		}
		values = append(values, value)
	}
	return strings.Join(values, ",")
}

func inferInitiator(payload []byte) string {
	var root map[string]any
	if json.Unmarshal(payload, &root) != nil {
		return "user"
	}
	for _, key := range []string{"messages", "input"} {
		items, ok := root[key].([]any)
		if !ok || len(items) == 0 {
			continue
		}
		last, _ := items[len(items)-1].(map[string]any)
		if strings.EqualFold(stringValue(last["role"]), "user") {
			return "user"
		}
		return "agent"
	}
	return "user"
}

func containsVisionContent(payload []byte) bool {
	var root any
	if json.Unmarshal(payload, &root) != nil {
		return false
	}
	return walkForVision(root)
}

func walkForVision(value any) bool {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if walkForVision(item) {
				return true
			}
		}
	case map[string]any:
		typeName := strings.ToLower(stringValue(typed["type"]))
		switch typeName {
		case "image", "image_url", "input_image":
			return true
		}
		for key, item := range typed {
			if strings.EqualFold(key, "image_url") && item != nil {
				return true
			}
			if walkForVision(item) {
				return true
			}
		}
	}
	return false
}

func stringValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}
