package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	if prepared.translatorFormat != prepared.outputFormat {
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
	}
	headers := cloneResponseHeaders(resp.Headers, "application/json")
	completedFields := preparedInferenceLogFields(prepared, false)
	completedFields["upstream_status"] = resp.StatusCode
	completedFields["output_bytes"] = len(payload)
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
	payload := append([]byte(nil), req.Payload...)
	if len(payload) == 0 || !json.Valid(payload) {
		return preparedInference{}, &pluginFailure{code: "invalid_request", message: "GitHub Copilot request payload must be a JSON object", httpStatus: http.StatusBadRequest}
	}
	translationTarget := translatorTargetFormat(route.Format, inputFormat)
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
	payload, errPrepare := normalizeInferencePayload(payload, model, route.Format, stream)
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
		headers:          inferenceHeaders(storage.CopilotSessionToken, route.Format, payload, req.Headers),
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
	return map[string]any{
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
}

func normalizeInferencePayload(raw []byte, model, format string, stream bool) ([]byte, error) {
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil || payload == nil {
		return nil, fmt.Errorf("GitHub Copilot request payload must be a JSON object")
	}
	payload["model"] = model
	payload["stream"] = stream
	if format == formatClaude {
		if _, exists := payload["max_tokens"]; !exists {
			payload["max_tokens"] = 4096
		}
		delete(payload, "stream_options")
		normalizeAnthropicPayload(payload, model)
	}
	if format == formatOpenAIResponse {
		if _, exists := payload["store"]; !exists {
			payload["store"] = false
		}
	}
	out, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("encode GitHub Copilot request payload")
	}
	return out, nil
}

func normalizeAnthropicPayload(payload map[string]any, model string) bool {
	changed := normalizeAnthropicToolInputStreaming(payload, model)
	if !usesAnthropicBudgetThinking(model) {
		return changed
	}
	thinking, hasThinking := payload["thinking"].(map[string]any)
	adaptiveThinking := hasThinking && strings.EqualFold(stringValue(thinking["type"]), "adaptive")
	budget := 0
	if adaptiveThinking {
		budget = anthropicThinkingBudget(payload, thinking)
	}
	if normalizeAnthropicSystemMessages(payload) {
		changed = true
	}
	if _, exists := payload["context_management"]; exists {
		delete(payload, "context_management")
		changed = true
	}
	if outputConfig, ok := payload["output_config"].(map[string]any); ok {
		if _, exists := outputConfig["effort"]; exists {
			delete(outputConfig, "effort")
			changed = true
		}
		if len(outputConfig) == 0 {
			delete(payload, "output_config")
		}
	}
	if !adaptiveThinking {
		return changed
	}

	if budget < 1024 {
		delete(payload, "thinking")
		return true
	}
	payload["thinking"] = map[string]any{
		"type":          "enabled",
		"budget_tokens": budget,
		"display":       "summarized",
	}
	return true
}

func anthropicThinkingBudget(payload, thinking map[string]any) int {
	effort := ""
	if outputConfig, ok := payload["output_config"].(map[string]any); ok {
		effort = stringValue(outputConfig["effort"])
	}
	if effort == "" {
		effort = stringValue(thinking["effort"])
	}
	budget := 16384
	switch strings.ToLower(effort) {
	case "minimal":
		budget = 1024
	case "low":
		budget = 2048
	case "medium":
		budget = 8192
	}
	maxTokens := intFromJSONNumber(payload["max_tokens"])
	if maxTokens > 0 && budget > maxTokens-1024 {
		budget = maxTokens - 1024
	}
	return budget
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

func normalizeAnthropicToolInputStreaming(payload map[string]any, model string) bool {
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
	supportsEager := supportsAnthropicEagerToolInputStreaming(model)
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		if supportsEager {
			if eager, exists := tool["eager_input_streaming"]; !exists || eager != true {
				tool["eager_input_streaming"] = true
				changed = true
			}
			continue
		}
		if _, exists := tool["eager_input_streaming"]; exists {
			delete(tool, "eager_input_streaming")
			changed = true
		}
	}
	return changed
}

func normalizeAnthropicPayloadBytes(raw []byte, model string) []byte {
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil || payload == nil || !normalizeAnthropicPayload(payload, model) {
		return raw
	}
	out, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return raw
	}
	return out
}

func supportsAnthropicEagerToolInputStreaming(model string) bool {
	switch strings.ToLower(normalizeModelID(model)) {
	case "claude-haiku-4.5", "claude-sonnet-4", "claude-sonnet-4.5":
		return false
	default:
		return true
	}
}

func usesAnthropicBudgetThinking(model string) bool {
	switch strings.ToLower(normalizeModelID(model)) {
	case "claude-haiku-4.5", "claude-opus-4.5", "claude-sonnet-4", "claude-sonnet-4.5":
		return true
	default:
		return false
	}
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
	if responseType, _ := response["type"].(string); responseType == "response.completed" || responseType == "response.incomplete" {
		return raw
	}
	if object, _ := response["object"].(string); object != "response" {
		return raw
	}
	eventType := "response.completed"
	if status, _ := response["status"].(string); status == "incomplete" || status == "failed" {
		eventType = "response.incomplete"
	}
	wrapper, errMarshal := json.Marshal(map[string]any{"type": eventType, "response": response})
	if errMarshal != nil {
		return raw
	}
	return wrapper
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
	model := modelFromPayload(req.Body)
	format := inferModelFormat(model)
	body := append([]byte(nil), req.Body...)
	if format == formatClaude {
		body = normalizeAnthropicPayloadBytes(body, model)
	}
	headers := inferenceHeaders(storage.CopilotSessionToken, format, body, req.Headers)
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
