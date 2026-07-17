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
		return nil, failure
	}
	resp, errHTTP := (hostClient{bridge: s.bridge, callbackID: req.HostCallbackID}).do(pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     prepared.upstreamURL,
		Headers: prepared.headers,
		Body:    prepared.upstreamPayload,
	})
	if errHTTP != nil {
		return nil, &pluginFailure{code: "upstream_network_error", message: "GitHub Copilot request failed", retryable: true, httpStatus: http.StatusBadGateway}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, upstreamFailure("upstream_http_error", "GitHub Copilot request failed", resp.StatusCode)
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
		upstreamPayload:  payload,
		headers:          inferenceHeaders(storage.CopilotSessionToken, route.Format, payload, req.Headers),
	}, nil
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
		return nil, &pluginFailure{code: "invalid_auth", message: "GitHub Copilot credential is invalid", httpStatus: http.StatusUnauthorized}
	}
	githubHost, errHost := normalizeGitHubHost(storage.GitHubHost)
	if errHost != nil || githubHost != s.loadedConfig().GitHubHost ||
		(storage.ExpiresAt > 0 && !s.now().UTC().Before(timeFromUnixMilli(storage.ExpiresAt))) {
		return nil, &pluginFailure{code: "invalid_auth", message: "GitHub Copilot credential is not valid for this configuration", httpStatus: http.StatusUnauthorized}
	}
	baseURL := apiBaseFromSessionToken(storage.CopilotSessionToken, storage.GitHubHost)
	if baseURL == "" || !sameOrigin(req.URL, baseURL) {
		return nil, &pluginFailure{code: "invalid_request", message: "GitHub Copilot HTTP request must target the credential API origin", httpStatus: http.StatusBadRequest}
	}
	model := modelFromPayload(req.Body)
	format := inferModelFormat(model)
	headers := inferenceHeaders(storage.CopilotSessionToken, format, req.Body, req.Headers)
	resp, errHTTP := (hostClient{bridge: s.bridge, callbackID: req.HostCallbackID}).do(pluginapi.HTTPRequest{
		Method:  valueOr(strings.TrimSpace(req.Method), http.MethodPost),
		URL:     req.URL,
		Headers: headers,
		Body:    append([]byte(nil), req.Body...),
	})
	if errHTTP != nil {
		return nil, &pluginFailure{code: "upstream_network_error", message: "GitHub Copilot HTTP request failed", retryable: true, httpStatus: http.StatusBadGateway}
	}
	return okEnvelope(pluginapi.ExecutorHTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    cloneResponseHeaders(resp.Headers, "application/json"),
		Body:       append([]byte(nil), resp.Body...),
	})
}
