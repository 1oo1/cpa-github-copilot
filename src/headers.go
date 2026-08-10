package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

const (
	copilotUserAgent     = "GitHubCopilotChat/0.60.0"
	copilotEditorVersion = "vscode/1.132.0"
	copilotPluginVersion = "copilot-chat/0.60.0"
	copilotIntegrationID = "vscode-chat"
	copilotAPIVersion    = "2026-06-01"

	// 以下四个 beta 都是 VS Code 1.132.0 源码（chatEndpoint.ts getAnthropicBetaHeader）可证明的值。
	interleavedThinkingBeta = "interleaved-thinking-2025-05-14"
	advancedToolUseBeta     = "advanced-tool-use-2025-11-20"
	contextManagementBeta   = "context-management-2025-06-27"
	extendedCacheTTLBeta    = "extended-cache-ttl-2025-04-11"

	defaultInteractionType = "conversation-other"
	toolSearchToolName     = "tool_search"
)

// vsCodeInteractionTypes 是 VS Code 1.132.0 中 locationToIntent 与
// InteractionTypeOverride 支持的完整词表；X-Interaction-Type 只接受这些值，
// 其余调用方取值一律回退到 resolveInteractionType 的默认值。
var vsCodeInteractionTypes = map[string]bool{
	"conversation-panel":      true,
	"conversation-inline":     true,
	"conversation-edits":      true,
	"conversation-notebook":   true,
	"conversation-terminal":   true,
	"conversation-other":      true,
	"conversation-agent":      true,
	"responses-proxy":         true,
	"messages-proxy":          true,
	"conversation-subagent":   true,
	"conversation-compaction": true,
	"conversation-background": true,
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

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
	return inferenceHeadersForRoute(sessionToken, modelRoute{Format: format}, payload, caller, format)
}

// inferenceHeadersForRoute 构造发往 GitHub Copilot 的推理请求头。outputFormat 是调用方实际
// 使用的公共协议（openai/openai-response/claude），只用于在调用方未提供合法 X-Interaction-Type
// 时选择默认值，可以与 route.Format（上游协议）不同。
func inferenceHeadersForRoute(sessionToken string, route modelRoute, payload []byte, caller http.Header, outputFormat string) http.Header {
	headers := copilotIdentityHeaders()
	headers.Set("Accept", "application/json")
	headers.Set("Content-Type", "application/json")
	headers.Set("Authorization", "Bearer "+sessionToken)
	headers.Set("X-GitHub-Api-Version", copilotAPIVersion)

	requestID := newRequestID()
	if candidate := strings.TrimSpace(caller.Get("X-Client-Request-Id")); looksLikeUUID(candidate) {
		requestID = candidate
	}
	headers.Set("X-Request-Id", requestID)
	headers.Set("X-Agent-Task-Id", requestID)
	interactionID := requestID
	if candidate := strings.TrimSpace(caller.Get("X-Interaction-Id")); looksLikeUUID(candidate) {
		interactionID = candidate
	}
	headers.Set("X-Interaction-Id", interactionID)

	interactionType := resolveInteractionType(caller, outputFormat)
	headers.Set("X-Interaction-Type", interactionType)
	headers.Set("Openai-Intent", interactionType)
	headers.Set("X-Initiator", resolveInitiator(caller, payload))

	if route.Vision && containsVisionContent(payload) {
		headers.Set("Copilot-Vision-Request", "true")
	}
	if route.Format == formatClaude {
		if beta := anthropicBetaHeader(route, payload); beta != "" {
			headers.Set("Anthropic-Beta", beta)
		}
	}
	return headers
}

// anthropicBetaHeader 只计算 VS Code 1.132.0 源码证明存在的四个 beta，不透传调用方任意值。
func anthropicBetaHeader(route modelRoute, payload []byte) string {
	var betas []string
	if !route.AdaptiveThinking {
		// 与 pinned 源码一致的已知“不精确”行为：非自适应 Messages 路由总是发送该 beta，
		// 不判断本次请求是否真正开启 thinking（VS Code 源码 TODO 记录的差异）。
		betas = append(betas, interleavedThinkingBeta)
	}
	if optionalBool(route.SupportsToolSearch) && payloadUsesToolSearch(payload) {
		betas = append(betas, advancedToolUseBeta)
	}
	if optionalBool(route.SupportsContextEditing) && payloadHasContextManagement(payload) {
		betas = append(betas, contextManagementBeta)
	}
	if payloadRequestsExtendedCacheTTL(payload) {
		betas = append(betas, extendedCacheTTLBeta)
	}
	return strings.Join(betas, ",")
}

// resolveInteractionType 只接受 VS Code 固定词表；调用方缺省或非法时按公共协议选择默认值。
func resolveInteractionType(caller http.Header, outputFormat string) string {
	if candidate := strings.TrimSpace(caller.Get("X-Interaction-Type")); vsCodeInteractionTypes[candidate] {
		return candidate
	}
	switch outputFormat {
	case formatOpenAIResponse:
		return "responses-proxy"
	case formatClaude:
		return "messages-proxy"
	default:
		return defaultInteractionType
	}
}

// resolveInitiator 只接受 user|agent；调用方缺省或非法时才使用消息语义 fallback。
func resolveInitiator(caller http.Header, payload []byte) string {
	if value := strings.ToLower(strings.TrimSpace(caller.Get("X-Initiator"))); value == "user" || value == "agent" {
		return value
	}
	return inferInitiator(payload)
}

func looksLikeUUID(value string) bool {
	return uuidPattern.MatchString(value)
}

// newRequestID 生成 RFC 4122 v4 UUID，用作 X-Request-Id/X-Agent-Task-Id 及
// X-Interaction-Id 的缺省值。crypto/rand.Read 在真实系统上不会失败，忽略错误的做法
// 与 google/uuid 一致。
func newRequestID() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

func payloadUsesToolSearch(payload []byte) bool {
	var root struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if json.Unmarshal(payload, &root) != nil {
		return false
	}
	for _, tool := range root.Tools {
		if strings.EqualFold(strings.TrimSpace(tool.Name), toolSearchToolName) {
			return true
		}
	}
	return false
}

func payloadHasContextManagement(payload []byte) bool {
	var root struct {
		ContextManagement json.RawMessage `json:"context_management"`
	}
	if json.Unmarshal(payload, &root) != nil {
		return false
	}
	trimmed := strings.TrimSpace(string(root.ContextManagement))
	return trimmed != "" && trimmed != "null"
}

func payloadRequestsExtendedCacheTTL(payload []byte) bool {
	var root any
	if json.Unmarshal(payload, &root) != nil {
		return false
	}
	return walkForExtendedCacheTTL(root)
}

func walkForExtendedCacheTTL(value any) bool {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if walkForExtendedCacheTTL(item) {
				return true
			}
		}
	case map[string]any:
		if cache, ok := typed["cache_control"].(map[string]any); ok && strings.EqualFold(stringValue(cache["ttl"]), "1h") {
			return true
		}
		for _, item := range typed {
			if walkForExtendedCacheTTL(item) {
				return true
			}
		}
	}
	return false
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
