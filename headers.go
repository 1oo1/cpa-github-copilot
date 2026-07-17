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
		if beta := strings.TrimSpace(caller.Get("Anthropic-Beta")); beta != "" {
			headers.Set("Anthropic-Beta", beta)
		}
	}
	if interaction := strings.TrimSpace(caller.Get("X-Interaction-Type")); interaction != "" {
		headers.Set("X-Interaction-Type", interaction)
	}
	return headers
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
