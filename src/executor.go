package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	_ "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/builtin"
)

type preparedInference struct {
	request          pluginapi.ExecutorRequest
	storage          copilotStorage
	model            string
	inputFormat      string
	outputFormat     string
	upstreamFormat   string
	translatorFormat string
	upstreamURL      string
	upstreamPath     string
	upstreamPayload  []byte
	headers          http.Header
}

func (s *pluginService) execute(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, &pluginFailure{code: "invalid_request", message: "decode executor request"}
	}
	prepared, failure := s.prepareInference(req.ExecutorRequest, false)
	if failure != nil {
		s.logFailure(req.HostCallbackID, "inference.rejected", failure, inferenceRequestLogFields(req.ExecutorRequest, false, s.now().UTC()))
		return nil, failure
	}
	s.logEvent(req.HostCallbackID, "debug", "inference.started", preparedInferenceLogFields(prepared, false))
	resp, errHTTP := (hostClient{bridge: s.bridge, callbackID: req.HostCallbackID}).do(pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     prepared.upstreamURL,
		Headers: prepared.headers,
		Body:    prepared.upstreamPayload,
	})
	if errHTTP != nil {
		failure = &pluginFailure{code: "upstream_network_error", message: "GitHub Copilot request failed", retryable: true, httpStatus: http.StatusBadGateway}
		s.logFailure(req.HostCallbackID, "inference.failed", failure, preparedInferenceLogFields(prepared, false))
		return nil, failure
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		failure = upstreamFailure("upstream_http_error", "GitHub Copilot request failed", resp.StatusCode)
		s.logFailure(req.HostCallbackID, "inference.failed", failure, preparedInferenceLogFields(prepared, false))
		return nil, failure
	}
	payload := append([]byte(nil), resp.Body...)
	responsesSourceStatus := ""
	if prepared.upstreamFormat == formatOpenAIResponse {
		status, terminal := responsesNonStreamTerminalStatus(payload)
		if !terminal || (prepared.translatorFormat != prepared.outputFormat && (status == "response.failed" || status == "error")) {
			code := "upstream_protocol_error"
			message := "GitHub Copilot Responses request ended without a terminal response"
			if terminal {
				code = "upstream_response_failed"
				message = "GitHub Copilot Responses request failed"
			}
			failure = &pluginFailure{code: code, message: message, httpStatus: http.StatusBadGateway}
			fields := preparedInferenceLogFields(prepared, false)
			fields["responses_terminal_status"] = valueOr(status, "missing")
			s.logFailure(req.HostCallbackID, "inference.failed", failure, fields)
			return nil, failure
		}
		responsesSourceStatus = status
	}
	if prepared.translatorFormat != prepared.outputFormat {
		sourceStatus := ""
		sourceReason := ""
		if prepared.upstreamFormat != formatOpenAIResponse && prepared.outputFormat == formatOpenAIResponse {
			status, reason, terminal := sourceNonStreamTerminalStatus(payload, prepared.translatorFormat)
			if !terminal || status == "response.failed" || status == "error" {
				code := "upstream_protocol_error"
				message := "GitHub Copilot response ended without a terminal status"
				if terminal {
					code = "upstream_response_failed"
					message = "GitHub Copilot upstream response failed"
				}
				failure = &pluginFailure{code: code, message: message, httpStatus: http.StatusBadGateway}
				fields := preparedInferenceLogFields(prepared, false)
				fields["source_terminal_status"] = valueOr(status, "missing")
				s.logFailure(req.HostCallbackID, "inference.failed", failure, fields)
				return nil, failure
			}
			sourceStatus = status
			sourceReason = reason
		}
		if !sdktranslator.HasNonStreamResponseTransformer(
			sdktranslator.Format(prepared.outputFormat),
			sdktranslator.Format(prepared.translatorFormat),
		) {
			return nil, &pluginFailure{code: "format_mismatch", message: "GitHub Copilot response format cannot be converted"}
		}
		original := prepared.request.OriginalRequest
		if len(original) == 0 {
			original = prepared.request.Payload
		}
		translatorPayload := payload
		if prepared.upstreamFormat == formatOpenAIResponse && prepared.translatorFormat == string(sdktranslator.FormatCodex) {
			translatorPayload = wrapResponsesNonStreamEvent(payload)
		} else if prepared.translatorFormat == formatClaude && prepared.outputFormat == formatOpenAIResponse {
			translatorPayload = wrapClaudeNonStreamResponse(payload)
		}
		payload = sdktranslator.TranslateNonStream(
			context.Background(),
			sdktranslator.Format(prepared.translatorFormat),
			sdktranslator.Format(prepared.outputFormat),
			prepared.model,
			original,
			prepared.upstreamPayload,
			translatorPayload,
			nil,
		)
		if len(payload) == 0 {
			return nil, &pluginFailure{code: "format_mismatch", message: "GitHub Copilot response conversion produced no output"}
		}
		if sourceStatus == "response.incomplete" {
			var rewritten bool
			payload, rewritten = rewriteResponsesNonStreamIncomplete(payload, sourceReason)
			if !rewritten {
				return nil, &pluginFailure{code: "format_mismatch", message: "GitHub Copilot response conversion produced an invalid terminal response"}
			}
		}
	}
	headers := cloneResponseHeaders(resp.Headers, "application/json")
	completedFields := preparedInferenceLogFields(prepared, false)
	completedFields["upstream_status"] = resp.StatusCode
	completedFields["output_bytes"] = len(payload)
	if responsesSourceStatus != "" {
		completedFields["responses_terminal_status"] = responsesSourceStatus
	}
	s.logEvent(req.HostCallbackID, "debug", "inference.completed", completedFields)
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: headers,
		Metadata: map[string]any{
			"upstream_format": prepared.upstreamFormat,
			"upstream_status": resp.StatusCode,
		},
	})
}

func (s *pluginService) prepareInference(req pluginapi.ExecutorRequest, stream bool) (preparedInference, *pluginFailure) {
	// VS Code 1.132.0 没有独立 /responses/compact endpoint 的源码证据，GitHub Copilot 提供该
	// endpoint 也无从证实；在发出 HTTP 请求前拒绝，不误发普通 /responses 也不臆造新 endpoint。
	if strings.TrimSpace(req.Alt) == "responses/compact" {
		return preparedInference{}, &pluginFailure{code: "unsupported_feature", message: "GitHub Copilot does not support a separate Responses compaction endpoint", httpStatus: http.StatusNotImplemented}
	}
	storage, errStorage := decodeCopilotStorage(req.StorageJSON)
	if errStorage != nil || strings.TrimSpace(storage.GitHubAccessToken) == "" {
		return preparedInference{}, &pluginFailure{code: "invalid_auth", message: "GitHub Copilot credential is invalid", httpStatus: http.StatusUnauthorized}
	}
	if strings.TrimSpace(storage.CopilotSessionToken) == "" || (storage.ExpiresAt > 0 && !s.now().UTC().Before(timeFromUnixMilli(storage.ExpiresAt))) {
		return preparedInference{}, &pluginFailure{code: "reauth_required", message: "GitHub Copilot session requires refresh", httpStatus: http.StatusUnauthorized}
	}
	githubHost, errHost := normalizeGitHubHost(storage.GitHubHost)
	if errHost != nil || githubHost != s.loadedConfig().GitHubHost {
		return preparedInference{}, &pluginFailure{code: "invalid_auth", message: "GitHub Copilot credential host does not match plugin configuration"}
	}
	model := normalizeModelID(req.Model)
	if model == "" {
		model = modelFromPayload(req.Payload)
	}
	if model == "" {
		return preparedInference{}, &pluginFailure{code: "invalid_request", message: "GitHub Copilot request is missing model", httpStatus: http.StatusBadRequest}
	}
	inputFormat := normalizeFormat(req.SourceFormat)
	if inputFormat == "" {
		inputFormat = normalizeFormat(req.Format)
	}
	outputFormat := normalizeFormat(req.Format)
	if outputFormat == "" {
		outputFormat = inputFormat
	}
	if inputFormat == "" || outputFormat == "" {
		return preparedInference{}, &pluginFailure{code: "format_mismatch", message: "GitHub Copilot request format is unsupported", httpStatus: http.StatusBadRequest}
	}
	route := s.resolveModelRoute(req.AuthID, model, storage)
	if route.Path == "" || route.Format == "" {
		return preparedInference{}, &pluginFailure{code: "model_not_supported", message: "GitHub Copilot model has no supported endpoint", httpStatus: http.StatusBadRequest}
	}
	if stream && !optionalBool(route.Streaming) {
		return preparedInference{}, &pluginFailure{code: "unsupported_feature", message: "GitHub Copilot model does not support streaming", httpStatus: http.StatusBadRequest}
	}
	payload := append([]byte(nil), req.Payload...)
	if len(payload) == 0 || !json.Valid(payload) {
		return preparedInference{}, &pluginFailure{code: "invalid_request", message: "GitHub Copilot request payload must be a JSON object", httpStatus: http.StatusBadRequest}
	}
	translationTarget := translatorTargetFormat(route.Format, inputFormat)
	requestedReasoningEffort := reasoningEffortFromPayload(payload, inputFormat)
	// 带有 Responses stateful/opaque continuation 的请求只能在完全原生的 openai-response 链路上
	// 无损传递；一旦涉及任何跨格式转换，就必须在发出 HTTP 请求前 fail closed，而不是依赖
	// translator 碰巧保留这些字段。只在调用方本身就是 openai-response 协议时检查，避免和
	// Anthropic/Chat 自身同名但语义不同的字段（例如 Anthropic 的 context_management）误判。
	if inputFormat == formatOpenAIResponse && responsesStatefulMarkersPresent(payload) &&
		(outputFormat != formatOpenAIResponse || route.Format != formatOpenAIResponse || translationTarget != formatOpenAIResponse) {
		return preparedInference{}, &pluginFailure{code: "format_mismatch", message: "GitHub Copilot cannot preserve Responses stateful continuation across a cross-format request", httpStatus: http.StatusBadRequest}
	}
	if inputFormat != translationTarget {
		if !sdktranslator.HasRequestTransformer(sdktranslator.Format(inputFormat), sdktranslator.Format(translationTarget)) {
			return preparedInference{}, &pluginFailure{code: "format_mismatch", message: "GitHub Copilot request format cannot be converted", httpStatus: http.StatusBadRequest}
		}
		payload = sdktranslator.TranslateRequest(
			sdktranslator.Format(inputFormat),
			sdktranslator.Format(translationTarget),
			model,
			payload,
			stream,
		)
	}
	if route.Format == formatClaude && declaresReasoningLevel(route.ReasoningLevels, requestedReasoningEffort) {
		payload = preserveAnthropicReasoningEffort(payload, requestedReasoningEffort)
	}
	payload, errPrepare := normalizeInferencePayloadForRoute(payload, model, route, stream, s.loadedConfig().EnableResponsesContextManagement)
	if errPrepare != nil {
		return preparedInference{}, &pluginFailure{code: "invalid_request", message: errPrepare.Error(), httpStatus: http.StatusBadRequest}
	}
	baseURL := apiBaseFromSessionToken(storage.CopilotSessionToken, githubHost)
	if baseURL == "" {
		return preparedInference{}, &pluginFailure{code: "invalid_auth", message: "GitHub Copilot API endpoint is invalid"}
	}
	if stream && translationTarget != outputFormat && !sdktranslator.HasStreamResponseTransformer(
		sdktranslator.Format(outputFormat), sdktranslator.Format(translationTarget),
	) {
		return preparedInference{}, &pluginFailure{code: "format_mismatch", message: "GitHub Copilot stream format cannot be converted", httpStatus: http.StatusBadRequest}
	}
	return preparedInference{
		request:          req,
		storage:          storage,
		model:            model,
		inputFormat:      inputFormat,
		outputFormat:     outputFormat,
		upstreamFormat:   route.Format,
		translatorFormat: translationTarget,
		upstreamURL:      baseURL + route.Path,
		upstreamPath:     route.Path,
		upstreamPayload:  payload,
		headers:          inferenceHeadersForRoute(storage.CopilotSessionToken, route, payload, req.Headers, outputFormat),
	}, nil
}

func inferenceRequestLogFields(req pluginapi.ExecutorRequest, stream bool, now time.Time) map[string]any {
	model := normalizeModelID(req.Model)
	if model == "" {
		model = modelFromPayload(req.Payload)
	}
	fields := map[string]any{
		"auth_id":       req.AuthID,
		"model":         model,
		"format":        normalizeFormat(req.Format),
		"source_format": normalizeFormat(req.SourceFormat),
		"stream":        stream,
		"input_bytes":   len(req.Payload),
	}
	storage, errStorage := decodeCopilotStorage(req.StorageJSON)
	fields["storage_valid"] = errStorage == nil
	if errStorage == nil {
		for key, value := range authLogFields(storage, now) {
			fields[key] = value
		}
	}
	return fields
}

func preparedInferenceLogFields(prepared preparedInference, stream bool) map[string]any {
	fields := map[string]any{
		"auth_id":            prepared.request.AuthID,
		"model":              prepared.model,
		"input_format":       prepared.inputFormat,
		"output_format":      prepared.outputFormat,
		"upstream_format":    prepared.upstreamFormat,
		"translator_format":  prepared.translatorFormat,
		"translation_needed": prepared.translatorFormat != prepared.outputFormat,
		"endpoint_path":      prepared.upstreamPath,
		"stream":             stream,
		"upstream_bytes":     len(prepared.upstreamPayload),
	}
	// 只在原生 Responses 路由记录压缩诊断字段，不记录 prompt/tool 内容。
	if prepared.upstreamFormat == formatOpenAIResponse {
		if threshold, enabled := responsesCompactionThresholdFromPayload(prepared.upstreamPayload); enabled {
			fields["responses_context_management_enabled"] = true
			fields["responses_compact_threshold"] = threshold
		} else {
			fields["responses_context_management_enabled"] = false
		}
		original := prepared.request.OriginalRequest
		if len(original) == 0 {
			original = prepared.request.Payload
		}
		fields["responses_state_present"] = responsesStatefulMarkersPresent(original)
	}
	return fields
}

// responsesCompactionThresholdFromPayload 从最终上行 body 中读取 compaction 阈值，仅用于
// 日志诊断，不影响请求语义。
func responsesCompactionThresholdFromPayload(payload []byte) (threshold int64, enabled bool) {
	var root struct {
		ContextManagement []struct {
			Type             string  `json:"type"`
			CompactThreshold float64 `json:"compact_threshold"`
		} `json:"context_management"`
	}
	if json.Unmarshal(payload, &root) != nil {
		return 0, false
	}
	for _, item := range root.ContextManagement {
		if item.Type == "compaction" {
			return int64(item.CompactThreshold), true
		}
	}
	return 0, false
}

func normalizeInferencePayloadForRoute(raw []byte, model string, route modelRoute, stream bool, responsesContextManagementEnabled bool) ([]byte, error) {
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil || payload == nil {
		return nil, fmt.Errorf("GitHub Copilot request payload must be a JSON object")
	}
	payload["model"] = model
	payload["stream"] = stream
	if route.Format == formatOpenAI {
		normalizeOpenAICompatibility(payload, route)
	}
	if route.Format == formatClaude {
		if _, exists := payload["max_tokens"]; !exists {
			payload["max_tokens"] = 4096
		}
		delete(payload, "stream_options")
		normalizeAnthropicPayloadForRoute(payload, route)
	}
	if route.Format == formatOpenAIResponse {
		normalizeOpenAIResponsesCompatibility(payload, route, responsesContextManagementEnabled)
	}
	out, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("encode GitHub Copilot request payload")
	}
	return out, nil
}

// responsesStatefulMarkersPresent 报告请求中是否携带客户端管理的 Responses 状态：
// previous_response_id、context_management 或 input 中的 compaction/encrypted reasoning item。
func responsesStatefulMarkersPresent(raw []byte) bool {
	var root struct {
		PreviousResponseID string            `json:"previous_response_id"`
		ContextManagement  json.RawMessage   `json:"context_management"`
		Input              []json.RawMessage `json:"input"`
	}
	if json.Unmarshal(raw, &root) != nil {
		return false
	}
	if strings.TrimSpace(root.PreviousResponseID) != "" {
		return true
	}
	if trimmed := strings.TrimSpace(string(root.ContextManagement)); trimmed != "" && trimmed != "null" {
		return true
	}
	for _, rawItem := range root.Input {
		var item struct {
			Type             string `json:"type"`
			EncryptedContent string `json:"encrypted_content"`
		}
		if json.Unmarshal(rawItem, &item) != nil {
			continue
		}
		if item.Type == "compaction" {
			return true
		}
		if item.Type == "reasoning" && strings.TrimSpace(item.EncryptedContent) != "" {
			return true
		}
	}
	return false
}

func normalizeOpenAICompatibility(payload map[string]any, route modelRoute) bool {
	changed := false
	if _, exists := payload["store"]; exists {
		delete(payload, "store")
		changed = true
	}
	// 只保留 route 声明过的 reasoning_effort 取值，不支持的值一律删除，避免未声明
	// reasoning 能力的模型收到无法识别的字段。
	if effort, exists := payload["reasoning_effort"]; exists {
		if !declaresReasoningLevel(route.ReasoningLevels, stringValue(effort)) {
			delete(payload, "reasoning_effort")
			changed = true
		}
	}
	if messages, ok := payload["messages"].([]any); ok {
		for _, rawMessage := range messages {
			message, ok := rawMessage.(map[string]any)
			if ok && strings.EqualFold(stringValue(message["role"]), "developer") {
				message["role"] = "system"
				changed = true
			}
		}
	}
	return changed
}

func declaresReasoningLevel(levels []string, value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return false
	}
	for _, level := range levels {
		if strings.EqualFold(level, value) {
			return true
		}
	}
	return false
}

func reasoningEffortFromPayload(raw []byte, format string) string {
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	switch format {
	case formatOpenAI:
		return strings.ToLower(strings.TrimSpace(stringValue(payload["reasoning_effort"])))
	case formatOpenAIResponse:
		if reasoning, ok := payload["reasoning"].(map[string]any); ok {
			return strings.ToLower(strings.TrimSpace(stringValue(reasoning["effort"])))
		}
	case formatClaude:
		if outputConfig, ok := payload["output_config"].(map[string]any); ok {
			return strings.ToLower(strings.TrimSpace(stringValue(outputConfig["effort"])))
		}
	}
	return ""
}

func preserveAnthropicReasoningEffort(raw []byte, effort string) []byte {
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return raw
	}
	outputConfig, _ := payload["output_config"].(map[string]any)
	if outputConfig == nil {
		outputConfig = map[string]any{}
	}
	if strings.TrimSpace(stringValue(outputConfig["effort"])) == "" {
		outputConfig["effort"] = effort
		payload["output_config"] = outputConfig
	}
	out, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return raw
	}
	return out
}

// responsesContextManagementExcludedFamilies 是 VS Code 1.132.0 源码证明的排除集合
// （openai.ts modelsWithoutResponsesContextManagement）。
var responsesContextManagementExcludedFamilies = map[string]bool{
	"gpt-5":   true,
	"gpt-5.1": true,
	"gpt-5.2": true,
}

// responsesCompactionThreshold 计算 VS Code feature-on 行为使用的 compaction 阈值：
// floor(0.9 * max prompt tokens)，没有有效 prompt window 时回退到 50000。
func responsesCompactionThreshold(route modelRoute) int64 {
	if route.MaxPromptTokens > 0 {
		return route.MaxPromptTokens * 9 / 10
	}
	return 50000
}

func normalizeOpenAIResponsesCompatibility(payload map[string]any, route modelRoute, contextManagementEnabled bool) bool {
	changed := payload["store"] != false
	payload["store"] = false
	if reasoning, ok := payload["reasoning"].(map[string]any); ok {
		effort := strings.ToLower(stringValue(reasoning["effort"]))
		if len(reasoning) == 0 || effort == "none" || effort == "off" {
			delete(payload, "reasoning")
			changed = true
		} else if effort != "" && !declaresReasoningLevel(route.ReasoningLevels, effort) {
			delete(reasoning, "effort")
			if len(reasoning) == 0 {
				delete(payload, "reasoning")
			}
			changed = true
		}
	}
	// truncation/include 只在缺失时补齐 VS Code feature-on 默认值，保留有效 caller 值。
	if _, exists := payload["truncation"]; !exists {
		payload["truncation"] = "disabled"
		changed = true
	}
	if _, exists := payload["include"]; !exists {
		payload["include"] = []any{"reasoning.encrypted_content"}
		changed = true
	}
	// context_management 只在缺失时按 90% max prompt tokens 补齐，已有 caller 值则逐字保留。
	family := strings.ToLower(strings.TrimSpace(route.Family))
	if _, exists := payload["context_management"]; !exists && contextManagementEnabled && family != "" &&
		!responsesContextManagementExcludedFamilies[family] {
		payload["context_management"] = []any{map[string]any{
			"type":              "compaction",
			"compact_threshold": responsesCompactionThreshold(route),
		}}
		changed = true
	}
	return changed
}

func normalizeAnthropicPayloadForRoute(payload map[string]any, route modelRoute) bool {
	changed := normalizeAnthropicTemperature(payload)
	if normalizeAnthropicTools(payload) {
		changed = true
	}
	if normalizeAnthropicSystemMessages(payload) {
		changed = true
	}
	if _, exists := payload["context_management"]; exists && !optionalBool(route.SupportsContextEditing) {
		delete(payload, "context_management")
		changed = true
	}
	if route.AdaptiveThinking {
		if normalizeAnthropicAdaptiveThinking(payload, route) {
			changed = true
		}
		return changed
	}

	thinking, hasThinking := payload["thinking"].(map[string]any)
	thinkingType := strings.ToLower(stringValue(thinking["type"]))
	budgetThinking := route.MinThinking > 0 || route.MaxThinking > 0
	if !budgetThinking {
		if hasThinking && (thinkingType == "adaptive" || thinkingType == "enabled") {
			delete(payload, "thinking")
			changed = true
		}
		if normalizeAnthropicReasoningEffort(payload, route, false) {
			changed = true
		}
		return changed
	}
	if thinkingType == "adaptive" {
		budget := anthropicThinkingBudget(payload, route)
		if budget <= 0 {
			delete(payload, "thinking")
			thinkingType = ""
		} else {
			payload["thinking"] = map[string]any{
				"type":          "enabled",
				"budget_tokens": budget,
				"display":       "summarized",
			}
			thinkingType = "enabled"
		}
		changed = true
	}
	if normalizeAnthropicReasoningEffort(payload, route, thinkingType == "enabled") {
		changed = true
	}
	return changed
}

func anthropicThinkingBudget(payload map[string]any, route modelRoute) int {
	budget := 16000
	if route.MinThinking > 0 && budget < route.MinThinking {
		budget = route.MinThinking
	}
	if route.MaxThinking > 0 && budget > route.MaxThinking {
		budget = route.MaxThinking
	}
	maxTokens := intFromJSONNumber(payload["max_tokens"])
	if maxTokens > 0 && budget >= maxTokens {
		budget = maxTokens - 1
	}
	return budget
}

func normalizeAnthropicAdaptiveThinking(payload map[string]any, route modelRoute) bool {
	thinking, ok := payload["thinking"].(map[string]any)
	if !ok {
		return normalizeAnthropicReasoningEffort(payload, route, false)
	}
	changed := false
	thinkingType := strings.ToLower(stringValue(thinking["type"]))
	if thinkingType == "enabled" {
		normalized := map[string]any{"type": "adaptive"}
		if display := stringValue(thinking["display"]); display != "" {
			normalized["display"] = display
		}
		payload["thinking"] = normalized
		thinkingType = "adaptive"
		changed = true
	}
	if normalizeAnthropicReasoningEffort(payload, route, thinkingType == "adaptive") {
		changed = true
	}
	return changed
}

func normalizeAnthropicReasoningEffort(payload map[string]any, route modelRoute, thinkingEnabled bool) bool {
	outputConfig, ok := payload["output_config"].(map[string]any)
	if !ok {
		if !thinkingEnabled {
			return false
		}
		outputConfig = map[string]any{}
	}
	effort := strings.ToLower(strings.TrimSpace(stringValue(outputConfig["effort"])))
	changed := false
	if !thinkingEnabled || (!route.ReasoningLevelsDeclared && !route.AdaptiveThinking) ||
		(effort != "" && route.ReasoningLevelsDeclared && !declaresReasoningLevel(route.ReasoningLevels, effort)) {
		if _, exists := outputConfig["effort"]; exists {
			delete(outputConfig, "effort")
			changed = true
		}
	} else if effort == "" {
		defaultEffort := defaultReasoningEffort(route.ReasoningLevels)
		if defaultEffort == "" && route.AdaptiveThinking && !route.ReasoningLevelsDeclared {
			defaultEffort = "high"
		}
		if defaultEffort != "" {
			outputConfig["effort"] = defaultEffort
			changed = true
		}
	} else if stringValue(outputConfig["effort"]) != effort {
		outputConfig["effort"] = effort
		changed = true
	}
	if len(outputConfig) == 0 {
		if _, exists := payload["output_config"]; exists {
			delete(payload, "output_config")
			changed = true
		}
		return changed
	}
	payload["output_config"] = outputConfig
	return changed
}

func defaultReasoningEffort(levels []string) string {
	levels = cleanLevels(levels)
	if len(levels) == 0 {
		return ""
	}
	if declaresReasoningLevel(levels, "medium") {
		return "medium"
	}
	return levels[(len(levels)-1)/2]
}

func intFromJSONNumber(value any) int {
	switch number := value.(type) {
	case float64:
		return int(number)
	case int:
		return number
	case int64:
		return int(number)
	default:
		return 0
	}
}

func normalizeAnthropicSystemMessages(payload map[string]any) bool {
	messages, ok := payload["messages"].([]any)
	if !ok {
		return false
	}
	system, ok := anthropicSystemBlocks(payload["system"])
	if !ok {
		return false
	}
	kept := make([]any, 0, len(messages))
	moved := false
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok || !strings.EqualFold(stringValue(message["role"]), "system") {
			kept = append(kept, rawMessage)
			continue
		}
		blocks, convertible := anthropicSystemBlocks(message["content"])
		if !convertible {
			kept = append(kept, rawMessage)
			continue
		}
		system = append(system, blocks...)
		moved = true
	}
	if !moved {
		return false
	}
	payload["messages"] = kept
	if len(system) == 0 {
		delete(payload, "system")
	} else {
		payload["system"] = system
	}
	return true
}

func anthropicSystemBlocks(content any) ([]any, bool) {
	switch typed := content.(type) {
	case nil:
		return nil, true
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil, true
		}
		return []any{map[string]any{"type": "text", "text": typed}}, true
	case []any:
		blocks := make([]any, 0, len(typed))
		for _, rawBlock := range typed {
			switch block := rawBlock.(type) {
			case string:
				if strings.TrimSpace(block) != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": block})
				}
			case map[string]any:
				if !strings.EqualFold(stringValue(block["type"]), "text") {
					return nil, false
				}
				if text, ok := block["text"].(string); !ok {
					return nil, false
				} else if strings.TrimSpace(text) != "" {
					blocks = append(blocks, block)
				}
			default:
				return nil, false
			}
		}
		return blocks, true
	default:
		return nil, false
	}
}

func normalizeAnthropicTools(payload map[string]any) bool {
	rawTools, exists := payload["tools"]
	if !exists {
		return false
	}
	tools, ok := rawTools.([]any)
	if !ok {
		return false
	}
	if len(tools) == 0 {
		delete(payload, "tools")
		return true
	}

	changed := false
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		if _, exists := tool["strict"]; exists {
			delete(tool, "strict")
			changed = true
		}
		if schema, ok := tool["input_schema"].(map[string]any); ok {
			properties, hasProperties := schema["properties"]
			if !hasProperties {
				properties = map[string]any{}
			}
			required, hasRequired := schema["required"]
			if !hasRequired {
				required = []any{}
			}
			if len(schema) != 3 || schema["type"] != "object" || !hasProperties || !hasRequired {
				changed = true
			}
			tool["input_schema"] = map[string]any{
				"type":       "object",
				"properties": properties,
				"required":   required,
			}
		}
		if _, exists := tool["eager_input_streaming"]; exists {
			delete(tool, "eager_input_streaming")
			changed = true
		}
	}
	return changed
}

func normalizeAnthropicTemperature(payload map[string]any) bool {
	if _, exists := payload["temperature"]; !exists {
		return false
	}
	delete(payload, "temperature")
	return true
}

func normalizeFormat(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "openai", "chat-completions", "openai-completions":
		return formatOpenAI
	case "openai-response", "openai-responses", "responses", "response":
		return formatOpenAIResponse
	case "claude", "anthropic", "anthropic-messages", "messages":
		return formatClaude
	default:
		return ""
	}
}

func translatorTargetFormat(wireFormat, inputFormat string) string {
	if wireFormat == formatOpenAIResponse && inputFormat != formatOpenAIResponse {
		return string(sdktranslator.FormatCodex)
	}
	return wireFormat
}

func wrapResponsesNonStreamEvent(raw []byte) []byte {
	var response map[string]any
	if json.Unmarshal(raw, &response) != nil || response == nil {
		return raw
	}
	if responseType := stringValue(response["type"]); responseType != "" {
		return raw
	}
	if object, _ := response["object"].(string); object != "response" {
		return raw
	}
	eventType, terminal := responsesNonStreamTerminalStatus(raw)
	if !terminal || eventType == "response.failed" || eventType == "error" {
		return raw
	}
	wrapper, errMarshal := json.Marshal(map[string]any{"type": eventType, "response": response})
	if errMarshal != nil {
		return raw
	}
	return wrapper
}

func wrapClaudeNonStreamResponse(raw []byte) []byte {
	var message map[string]any
	if json.Unmarshal(raw, &message) != nil || stringValue(message["type"]) != "message" {
		return raw
	}
	events := []map[string]any{{"type": "message_start", "message": message}}
	content, _ := message["content"].([]any)
	for index, rawBlock := range content {
		block, ok := rawBlock.(map[string]any)
		if !ok {
			continue
		}
		events = append(events, map[string]any{"type": "content_block_start", "index": index, "content_block": block})
		switch stringValue(block["type"]) {
		case "text":
			events = append(events, map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "text_delta", "text": stringValue(block["text"])}})
			citations, _ := block["citations"].([]any)
			for _, citation := range citations {
				events = append(events, map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "citations_delta", "citation": citation}})
			}
		case "tool_use":
			input := []byte(`{}`)
			if value, exists := block["input"]; exists && value != nil {
				if encoded, errMarshal := json.Marshal(value); errMarshal == nil {
					input = encoded
				}
			}
			events = append(events, map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}})
		case "thinking":
			events = append(events, map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "thinking_delta", "thinking": stringValue(block["thinking"])}})
		}
		events = append(events, map[string]any{"type": "content_block_stop", "index": index})
	}
	events = append(events,
		map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": message["stop_reason"], "stop_sequence": message["stop_sequence"]}},
		map[string]any{"type": "message_stop"},
	)
	var wrapped []byte
	for _, event := range events {
		encoded, errMarshal := json.Marshal(event)
		if errMarshal != nil {
			return raw
		}
		wrapped = append(wrapped, "data: "...)
		wrapped = append(wrapped, encoded...)
		wrapped = append(wrapped, '\n')
	}
	return wrapped
}

func rewriteResponsesNonStreamIncomplete(raw []byte, reason string) ([]byte, bool) {
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil || root == nil {
		return nil, false
	}
	response := root
	if nested, ok := root["response"].(map[string]any); ok {
		response = nested
	} else if stringValue(root["object"]) != "response" {
		return nil, false
	}
	if _, exists := root["type"]; exists {
		root["type"] = "response.incomplete"
	}
	response["status"] = "incomplete"
	response["incomplete_details"] = map[string]any{"reason": reason}
	encoded, errMarshal := json.Marshal(root)
	return encoded, errMarshal == nil
}

func responsesNonStreamTerminalStatus(raw []byte) (string, bool) {
	if !hasUniqueJSONFieldNames(raw) {
		return "", false
	}
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil || root == nil {
		return "", false
	}
	if typeValue, hasType := root["type"]; hasType {
		eventType, ok := typeValue.(string)
		if !ok {
			return "", false
		}
		if eventType == "error" {
			if !hasAnyField(root, "response", "object", "status") && nonEmptyErrorObject(root["error"]) {
				return "error", true
			}
			return "", false
		}
		if eventType == "response.completed" || eventType == "response.incomplete" || eventType == "response.failed" {
			if hasAnyField(root, "object", "status", "error") {
				return "", false
			}
			response, ok := root["response"].(map[string]any)
			if !ok {
				return "", false
			}
			status, terminal := responsesObjectTerminalStatus(response)
			if terminal && status == eventType {
				return eventType, true
			}
			return "", false
		}
		return "", false
	}
	if _, hasError := root["error"]; hasError {
		if _, hasObject := root["object"]; !hasObject {
			if !hasAnyField(root, "response", "status") && nonEmptyErrorObject(root["error"]) {
				return "error", true
			}
			return "", false
		}
	}
	return responsesObjectTerminalStatus(root)
}

func hasUniqueJSONFieldNames(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if consumeUniqueJSONValue(decoder) != nil {
		return false
	}
	_, errToken := decoder.Token()
	return errToken == io.EOF
}

func consumeUniqueJSONValue(decoder *json.Decoder) error {
	token, errToken := decoder.Token()
	if errToken != nil {
		return errToken
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delimiter {
	case '{':
		fields := make(map[string]struct{})
		for decoder.More() {
			keyToken, errKey := decoder.Token()
			if errKey != nil {
				return errKey
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("invalid JSON object key")
			}
			if _, duplicate := fields[key]; duplicate {
				return fmt.Errorf("duplicate JSON object key")
			}
			fields[key] = struct{}{}
			if errValue := consumeUniqueJSONValue(decoder); errValue != nil {
				return errValue
			}
		}
	case '[':
		for decoder.More() {
			if errValue := consumeUniqueJSONValue(decoder); errValue != nil {
				return errValue
			}
		}
	default:
		return fmt.Errorf("invalid JSON delimiter")
	}
	_, errToken = decoder.Token()
	return errToken
}

func nonEmptyErrorObject(value any) bool {
	errorObject, ok := value.(map[string]any)
	return ok && len(errorObject) > 0
}

func hasAnyField(value map[string]any, fields ...string) bool {
	for _, field := range fields {
		if _, exists := value[field]; exists {
			return true
		}
	}
	return false
}

func responsesObjectTerminalStatus(response map[string]any) (string, bool) {
	object, objectOK := response["object"].(string)
	status, statusOK := response["status"].(string)
	if !objectOK || object != "response" || !statusOK || hasAnyField(response, "type") {
		return "", false
	}
	errorValue, hasError := response["error"]
	incompleteDetails, hasIncompleteDetails := response["incomplete_details"]
	switch status {
	case "completed":
		if (!hasError || errorValue == nil) && (!hasIncompleteDetails || incompleteDetails == nil) {
			return "response.completed", true
		}
	case "incomplete":
		details, ok := incompleteDetails.(map[string]any)
		reason, reasonOK := details["reason"].(string)
		if ok && reasonOK && (reason == "max_output_tokens" || reason == "content_filter") && (!hasError || errorValue == nil) {
			return "response.incomplete", true
		}
	case "failed":
		if nonEmptyErrorObject(errorValue) && (!hasIncompleteDetails || incompleteDetails == nil) {
			return "response.failed", true
		}
	}
	return "", false
}

func normalizeModelID(raw string) string {
	model := strings.TrimSpace(raw)
	for _, prefix := range []string{pluginIdentifier + "/", pluginIdentifier + ":"} {
		if strings.HasPrefix(strings.ToLower(model), prefix) {
			return strings.TrimSpace(model[len(prefix):])
		}
	}
	return model
}

func modelFromPayload(raw []byte) string {
	var payload struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	return normalizeModelID(payload.Model)
}

func timeFromUnixMilli(value int64) time.Time {
	return time.UnixMilli(value).UTC()
}

func cloneResponseHeaders(input http.Header, contentType string) http.Header {
	out := make(http.Header)
	allowed := map[string]string{
		"content-type":        "Content-Type",
		"request-id":          "Request-Id",
		"x-request-id":        "X-Request-Id",
		"x-github-request-id": "X-GitHub-Request-Id",
		"retry-after":         "Retry-After",
	}
	for key, values := range input {
		canonical, ok := allowed[strings.ToLower(strings.TrimSpace(key))]
		if !ok {
			continue
		}
		for _, value := range values {
			out.Add(canonical, value)
		}
	}
	if out.Get("Content-Type") == "" {
		out.Set("Content-Type", contentType)
	}
	return out
}

func (s *pluginService) executeHTTPRequest(raw []byte) ([]byte, error) {
	var req rpcExecutorHTTPRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, &pluginFailure{code: "invalid_request", message: "decode executor HTTP request"}
	}
	storage, errStorage := decodeCopilotStorage(req.StorageJSON)
	if errStorage != nil || strings.TrimSpace(storage.CopilotSessionToken) == "" {
		failure := &pluginFailure{code: "invalid_auth", message: "GitHub Copilot credential is invalid", httpStatus: http.StatusUnauthorized}
		s.logFailure(req.HostCallbackID, "http_request.rejected", failure, map[string]any{"stage": "credential_validation"})
		return nil, failure
	}
	githubHost, errHost := normalizeGitHubHost(storage.GitHubHost)
	if errHost != nil || githubHost != s.loadedConfig().GitHubHost ||
		(storage.ExpiresAt > 0 && !s.now().UTC().Before(timeFromUnixMilli(storage.ExpiresAt))) {
		failure := &pluginFailure{code: "invalid_auth", message: "GitHub Copilot credential is not valid for this configuration", httpStatus: http.StatusUnauthorized}
		s.logFailure(req.HostCallbackID, "http_request.rejected", failure, authLogFields(storage, s.now().UTC()))
		return nil, failure
	}
	baseURL := apiBaseFromSessionToken(storage.CopilotSessionToken, storage.GitHubHost)
	if baseURL == "" || !sameOrigin(req.URL, baseURL) {
		failure := &pluginFailure{code: "invalid_request", message: "GitHub Copilot HTTP request must target the credential API origin", httpStatus: http.StatusBadRequest}
		s.logFailure(req.HostCallbackID, "http_request.rejected", failure, map[string]any{"stage": "origin_validation"})
		return nil, failure
	}
	endpointFormat, compactEndpoint, exactEndpoint := classifyInferenceEndpoint(req.URL)
	if endpointFormat != "" && !exactEndpoint {
		failure := &pluginFailure{code: "invalid_request", message: "GitHub Copilot inference HTTP request path must be canonical", httpStatus: http.StatusBadRequest}
		s.logFailure(req.HostCallbackID, "http_request.rejected", failure, map[string]any{"stage": "endpoint_validation"})
		return nil, failure
	}
	if compactEndpoint {
		failure := &pluginFailure{code: "unsupported_feature", message: "GitHub Copilot does not support a separate Responses compaction endpoint", httpStatus: http.StatusNotImplemented}
		s.logFailure(req.HostCallbackID, "http_request.rejected", failure, map[string]any{"stage": "endpoint_validation"})
		return nil, failure
	}
	if endpointFormat != "" && req.URL != baseURL+endpointPath(endpointFormat) {
		failure := &pluginFailure{code: "invalid_request", message: "GitHub Copilot inference HTTP request must target the exact credential endpoint", httpStatus: http.StatusBadRequest}
		s.logFailure(req.HostCallbackID, "http_request.rejected", failure, map[string]any{"stage": "endpoint_validation"})
		return nil, failure
	}
	if endpointFormat != "" && req.Method != http.MethodPost {
		failure := &pluginFailure{code: "invalid_request", message: "GitHub Copilot inference HTTP requests must use POST", httpStatus: http.StatusBadRequest}
		s.logFailure(req.HostCallbackID, "http_request.rejected", failure, map[string]any{"stage": "method_validation"})
		return nil, failure
	}
	model := modelFromPayload(req.Body)
	route := s.resolveModelRoute(req.AuthID, model, storage)
	format := route.Format
	if format == "" {
		format = inferModelFormat(model)
		route.Format = format
	}
	body := append([]byte(nil), req.Body...)
	requestedStream := false
	if endpointFormat != "" {
		if model == "" {
			failure := &pluginFailure{code: "invalid_request", message: "GitHub Copilot request is missing model", httpStatus: http.StatusBadRequest}
			s.logFailure(req.HostCallbackID, "http_request.rejected", failure, map[string]any{"stage": "body_validation"})
			return nil, failure
		}
		if route.Path == "" || route.Format != endpointFormat || route.Path != endpointPath(endpointFormat) {
			failure := &pluginFailure{code: "model_not_supported", message: "GitHub Copilot model does not support the requested endpoint", httpStatus: http.StatusBadRequest}
			s.logFailure(req.HostCallbackID, "http_request.rejected", failure, map[string]any{"stage": "route_validation", "model": model})
			return nil, failure
		}
		requestedStream = streamRequested(body)
		if requestedStream && !optionalBool(route.Streaming) {
			failure := &pluginFailure{code: "unsupported_feature", message: "GitHub Copilot model does not support streaming", httpStatus: http.StatusBadRequest}
			s.logFailure(req.HostCallbackID, "http_request.rejected", failure, map[string]any{"stage": "capability_validation", "model": model})
			return nil, failure
		}
		var errNormalize error
		body, errNormalize = normalizeInferencePayloadForRoute(body, model, route, requestedStream, s.loadedConfig().EnableResponsesContextManagement)
		if errNormalize != nil {
			failure := &pluginFailure{code: "invalid_request", message: errNormalize.Error(), httpStatus: http.StatusBadRequest}
			s.logFailure(req.HostCallbackID, "http_request.rejected", failure, map[string]any{"stage": "body_validation", "model": model})
			return nil, failure
		}
		format = endpointFormat
	}
	headers := inferenceHeadersForRoute(storage.CopilotSessionToken, route, body, req.Headers, format)
	resp, errHTTP := (hostClient{bridge: s.bridge, callbackID: req.HostCallbackID}).do(pluginapi.HTTPRequest{
		Method:  valueOr(strings.TrimSpace(req.Method), http.MethodPost),
		URL:     req.URL,
		Headers: headers,
		Body:    body,
	})
	if errHTTP != nil {
		failure := &pluginFailure{code: "upstream_network_error", message: "GitHub Copilot HTTP request failed", retryable: true, httpStatus: http.StatusBadGateway}
		s.logFailure(req.HostCallbackID, "http_request.failed", failure, map[string]any{"model": model})
		return nil, failure
	}
	if endpointFormat == formatOpenAIResponse && !requestedStream && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		status, terminal := responsesNonStreamTerminalStatus(resp.Body)
		if !terminal {
			failure := &pluginFailure{code: "upstream_protocol_error", message: "GitHub Copilot Responses request ended without a terminal response", httpStatus: http.StatusBadGateway}
			s.logFailure(req.HostCallbackID, "http_request.failed", failure, map[string]any{
				"model":                     model,
				"upstream_format":           format,
				"upstream_status":           resp.StatusCode,
				"responses_terminal_status": valueOr(status, "missing"),
			})
			return nil, failure
		}
	}
	s.logEvent(req.HostCallbackID, "debug", "http_request.completed", map[string]any{
		"model":           model,
		"upstream_format": format,
		"upstream_status": resp.StatusCode,
		"output_bytes":    len(resp.Body),
	})
	return okEnvelope(pluginapi.ExecutorHTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    cloneResponseHeaders(resp.Headers, "application/json"),
		Body:       append([]byte(nil), resp.Body...),
	})
}

func streamRequested(raw []byte) bool {
	var payload struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(raw, &payload) == nil && payload.Stream
}
