package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type storedModel struct {
	ID               string   `json:"id"`
	Name             string   `json:"name,omitempty"`
	Version          string   `json:"version,omitempty"`
	Family           string   `json:"family,omitempty"`
	Format           string   `json:"format"`
	ContextWindow    int64    `json:"context_window,omitempty"`
	MaxPromptTokens  int64    `json:"max_prompt_tokens,omitempty"`
	MaxOutputTokens  int64    `json:"max_output_tokens,omitempty"`
	InputModalities  []string `json:"input_modalities,omitempty"`
	ReasoningLevels  []string `json:"reasoning_levels,omitempty"`
	MinThinking      int      `json:"min_thinking,omitempty"`
	MaxThinking      int      `json:"max_thinking,omitempty"`
	AdaptiveThinking bool     `json:"adaptive_thinking,omitempty"`
}

type modelRoute struct {
	Format string
	Path   string
}

type remoteModelsResponse struct {
	Data []json.RawMessage `json:"data"`
}

type remoteModel struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Version            string   `json:"version"`
	ModelPickerEnabled bool     `json:"model_picker_enabled"`
	SupportedEndpoints []string `json:"supported_endpoints"`
	Policy             struct {
		State string `json:"state"`
	} `json:"policy"`
	Capabilities struct {
		Family string `json:"family"`
		Limits struct {
			MaxContextWindowTokens int64 `json:"max_context_window_tokens"`
			MaxOutputTokens        int64 `json:"max_output_tokens"`
			MaxPromptTokens        int64 `json:"max_prompt_tokens"`
			Vision                 struct {
				SupportedMediaTypes []string `json:"supported_media_types"`
			} `json:"vision"`
		} `json:"limits"`
		Supports struct {
			AdaptiveThinking  bool     `json:"adaptive_thinking"`
			MaxThinkingBudget int      `json:"max_thinking_budget"`
			MinThinkingBudget int      `json:"min_thinking_budget"`
			ReasoningEffort   []string `json:"reasoning_effort"`
			Streaming         *bool    `json:"streaming"`
			StructuredOutputs *bool    `json:"structured_outputs"`
			ToolCalls         *bool    `json:"tool_calls"`
			Vision            bool     `json:"vision"`
		} `json:"supports"`
	} `json:"capabilities"`
}

var knownCopilotModels = []string{
	"claude-fable-5", "claude-haiku-4.5", "claude-opus-4.5", "claude-opus-4.6",
	"claude-opus-4.7", "claude-opus-4.8", "claude-sonnet-4", "claude-sonnet-4.5",
	"claude-sonnet-4.6", "claude-sonnet-5", "gemini-2.5-pro", "gemini-3-flash-preview",
	"gemini-3.1-pro-preview", "gemini-3.5-flash", "gpt-4.1", "gpt-5-mini", "gpt-5.2",
	"gpt-5.2-codex", "gpt-5.3-codex", "gpt-5.4", "gpt-5.4-mini", "gpt-5.4-nano",
	"gpt-5.5", "gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "kimi-k2.7-code",
	"mai-code-1-flash-picker",
}

func (s *pluginService) modelsForAuth(raw []byte) ([]byte, error) {
	var req rpcAuthModelRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, &pluginFailure{code: "invalid_request", message: "decode model discovery request"}
	}
	storage, errStorage := decodeCopilotStorage(req.StorageJSON)
	if errStorage != nil || strings.TrimSpace(storage.CopilotSessionToken) == "" {
		return nil, &pluginFailure{code: "invalid_auth", message: "GitHub Copilot credential requires refresh", httpStatus: http.StatusUnauthorized}
	}
	if host, errHost := normalizeGitHubHost(storage.GitHubHost); errHost != nil || host != s.loadedConfig().GitHubHost {
		return nil, &pluginFailure{code: "invalid_auth", message: "GitHub Copilot credential host does not match plugin configuration"}
	}
	models := append([]storedModel(nil), storage.Models...)
	fetched := false
	if !s.modelCacheFresh(storage) {
		discovered, failure := s.discoverModels(hostClient{bridge: s.bridge, callbackID: req.HostCallbackID}, storage)
		if failure == nil {
			models = discovered
			storage.Models = discovered
			storage.ModelsFetchedAt = s.now().UTC().UnixMilli()
			fetched = true
		} else if len(models) == 0 {
			return nil, failure
		}
	}
	s.setModelRoutes(req.AuthID, models)
	response := pluginapi.ModelResponse{Provider: pluginIdentifier, Models: modelInfos(models)}
	if fetched {
		response.AuthUpdate = authDataFromStorage(storage, authDataDefaults{
			ID:         req.AuthID,
			FileName:   fileNameFromAttributes(req.Attributes, req.AuthID),
			Metadata:   req.Metadata,
			Attributes: req.Attributes,
		})
	}
	return okEnvelope(response)
}

func (s *pluginService) modelCacheFresh(storage copilotStorage) bool {
	if len(storage.Models) == 0 || storage.ModelsFetchedAt <= 0 {
		return false
	}
	ttl := s.loadedConfig().ModelCacheTTL
	if ttl == 0 {
		return false
	}
	return s.now().UTC().Before(time.UnixMilli(storage.ModelsFetchedAt).Add(time.Duration(ttl) * time.Second))
}

func (s *pluginService) discoverModels(client hostClient, storage copilotStorage) ([]storedModel, *pluginFailure) {
	baseURL := apiBaseFromSessionToken(storage.CopilotSessionToken, storage.GitHubHost)
	if baseURL == "" {
		return nil, &pluginFailure{code: "invalid_auth", message: "GitHub Copilot API endpoint is invalid"}
	}
	headers := copilotIdentityHeaders()
	headers.Set("Accept", "application/json")
	headers.Set("Authorization", "Bearer "+storage.CopilotSessionToken)
	headers.Set("X-GitHub-Api-Version", copilotAPIVersion)
	resp, errHTTP := client.do(pluginapi.HTTPRequest{Method: http.MethodGet, URL: baseURL + "/models", Headers: headers})
	if errHTTP != nil {
		return nil, &pluginFailure{code: "model_discovery_network_error", message: "GitHub Copilot model discovery is temporarily unavailable", retryable: true}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, upstreamFailure("model_discovery_http_error", "GitHub Copilot model discovery failed", resp.StatusCode)
	}
	models, errParse := parseDiscoveredModels(resp.Body)
	if errParse != nil {
		return nil, &pluginFailure{code: "model_discovery_invalid", message: errParse.Error(), httpStatus: http.StatusBadGateway}
	}
	return models, nil
}

func parseDiscoveredModels(raw []byte) ([]storedModel, error) {
	var response remoteModelsResponse
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil || response.Data == nil {
		return nil, fmt.Errorf("GitHub Copilot returned an invalid model catalog")
	}
	models := make([]storedModel, 0, len(response.Data))
	seen := make(map[string]struct{})
	for _, rawModel := range response.Data {
		var model remoteModel
		if json.Unmarshal(rawModel, &model) != nil {
			continue
		}
		model.ID = strings.TrimSpace(model.ID)
		if model.ID == "" || !model.ModelPickerEnabled || strings.EqualFold(strings.TrimSpace(model.Policy.State), "disabled") ||
			(model.Capabilities.Supports.ToolCalls != nil && !*model.Capabilities.Supports.ToolCalls) {
			continue
		}
		if _, exists := seen[model.ID]; exists {
			continue
		}
		format := selectModelFormat(model.ID, model.SupportedEndpoints)
		if format == "" {
			continue
		}
		modalities := []string{"text"}
		if model.Capabilities.Supports.Vision || hasImageMediaType(model.Capabilities.Limits.Vision.SupportedMediaTypes) {
			modalities = append(modalities, "image")
		}
		levels := cleanLevels(model.Capabilities.Supports.ReasoningEffort)
		models = append(models, storedModel{
			ID:               model.ID,
			Name:             valueOr(strings.TrimSpace(model.Name), model.ID),
			Version:          strings.TrimSpace(model.Version),
			Family:           strings.TrimSpace(model.Capabilities.Family),
			Format:           format,
			ContextWindow:    positiveInt64(model.Capabilities.Limits.MaxContextWindowTokens, model.Capabilities.Limits.MaxPromptTokens),
			MaxPromptTokens:  maxInt64(model.Capabilities.Limits.MaxPromptTokens, 0),
			MaxOutputTokens:  maxInt64(model.Capabilities.Limits.MaxOutputTokens, 0),
			InputModalities:  modalities,
			ReasoningLevels:  levels,
			MinThinking:      max(model.Capabilities.Supports.MinThinkingBudget, 0),
			MaxThinking:      max(model.Capabilities.Supports.MaxThinkingBudget, 0),
			AdaptiveThinking: model.Capabilities.Supports.AdaptiveThinking,
		})
		seen[model.ID] = struct{}{}
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

func selectModelFormat(modelID string, endpoints []string) string {
	available := make(map[string]bool)
	hadDeclaredEndpoint := false
	for _, endpoint := range endpoints {
		if strings.TrimSpace(endpoint) != "" {
			hadDeclaredEndpoint = true
		}
		switch strings.TrimSpace(strings.ToLower(endpoint)) {
		case "/v1/messages", "/messages":
			available[formatClaude] = true
		case "/responses", "/v1/responses":
			available[formatOpenAIResponse] = true
		case "/chat/completions", "/v1/chat/completions":
			available[formatOpenAI] = true
		}
	}
	inferred := inferModelFormat(modelID)
	if len(available) == 0 {
		if hadDeclaredEndpoint {
			return ""
		}
		return inferred
	}
	if available[inferred] {
		return inferred
	}
	for _, format := range []string{formatClaude, formatOpenAIResponse, formatOpenAI} {
		if available[format] {
			return format
		}
	}
	return ""
}

func inferModelFormat(modelID string) string {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if strings.HasPrefix(id, "claude-") && id != "claude-fable-5" {
		return formatClaude
	}
	if strings.HasPrefix(id, "gpt-5") || strings.HasPrefix(id, "oswe") || strings.HasPrefix(id, "mai-") {
		return formatOpenAIResponse
	}
	return formatOpenAI
}

func endpointPath(format string) string {
	switch format {
	case formatClaude:
		return "/v1/messages"
	case formatOpenAIResponse:
		return "/responses"
	case formatOpenAI:
		return "/chat/completions"
	default:
		return ""
	}
}

func modelInfos(models []storedModel) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		if strings.TrimSpace(model.ID) == "" {
			continue
		}
		parameters := []string{"max_tokens", "temperature", "top_p", "tools", "tool_choice", "stream"}
		var thinking *pluginapi.ThinkingSupport
		if len(model.ReasoningLevels) > 0 || model.MaxThinking > 0 || model.AdaptiveThinking {
			thinking = &pluginapi.ThinkingSupport{
				Min:            model.MinThinking,
				Max:            model.MaxThinking,
				ZeroAllowed:    true,
				DynamicAllowed: model.AdaptiveThinking,
				Levels:         append([]string(nil), model.ReasoningLevels...),
			}
			parameters = append(parameters, "reasoning_effort")
		}
		out = append(out, pluginapi.ModelInfo{
			ID:                         model.ID,
			Object:                     "model",
			OwnedBy:                    pluginIdentifier,
			Type:                       valueOr(model.Family, "chat"),
			DisplayName:                valueOr(model.Name, model.ID),
			Name:                       model.ID,
			Version:                    model.Version,
			Description:                "GitHub Copilot account model",
			InputTokenLimit:            model.MaxPromptTokens,
			OutputTokenLimit:           model.MaxOutputTokens,
			SupportedGenerationMethods: []string{"chat"},
			ContextLength:              model.ContextWindow,
			MaxCompletionTokens:        model.MaxOutputTokens,
			SupportedParameters:        parameters,
			SupportedInputModalities:   append([]string(nil), model.InputModalities...),
			SupportedOutputModalities:  []string{"text"},
			Thinking:                   thinking,
		})
	}
	return out
}

func (s *pluginService) setModelRoutes(authID string, models []storedModel) {
	authID = strings.TrimSpace(authID)
	s.mu.Lock()
	for key := range s.routes {
		if key.AuthID == authID {
			delete(s.routes, key)
		}
	}
	for _, model := range models {
		if model.ID != "" && endpointPath(model.Format) != "" {
			s.routes[routeKey{AuthID: authID, ModelID: model.ID}] = modelRoute{Format: model.Format, Path: endpointPath(model.Format)}
		}
	}
	s.mu.Unlock()
}

func (s *pluginService) resolveModelRoute(authID, modelID string, storage copilotStorage) modelRoute {
	for _, model := range storage.Models {
		if model.ID == modelID && endpointPath(model.Format) != "" {
			return modelRoute{Format: model.Format, Path: endpointPath(model.Format)}
		}
	}
	if len(storage.Models) > 0 || storage.ModelsFetchedAt > 0 {
		return modelRoute{}
	}
	s.mu.RLock()
	route := s.routes[routeKey{AuthID: strings.TrimSpace(authID), ModelID: strings.TrimSpace(modelID)}]
	s.mu.RUnlock()
	if route.Path != "" {
		return route
	}
	format := inferModelFormat(modelID)
	return modelRoute{Format: format, Path: endpointPath(format)}
}

func (s *pluginService) enableKnownModels(client hostClient, storage copilotStorage) {
	baseURL := apiBaseFromSessionToken(storage.CopilotSessionToken, storage.GitHubHost)
	if baseURL == "" {
		return
	}
	jobs := make(chan string)
	var wait sync.WaitGroup
	for range 4 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for modelID := range jobs {
				headers := copilotIdentityHeaders()
				headers.Set("Content-Type", "application/json")
				headers.Set("Authorization", "Bearer "+storage.CopilotSessionToken)
				headers.Set("Openai-Intent", "chat-policy")
				headers.Set("X-Interaction-Type", "chat-policy")
				_, _ = client.do(pluginapi.HTTPRequest{
					Method:  http.MethodPost,
					URL:     baseURL + "/models/" + modelID + "/policy",
					Headers: headers,
					Body:    []byte(`{"state":"enabled"}`),
				})
			}
		}()
	}
	for _, modelID := range knownCopilotModels {
		jobs <- modelID
	}
	close(jobs)
	wait.Wait()
}

func hasImageMediaType(values []string) bool {
	for _, value := range values {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "image/") {
			return true
		}
	}
	return false
}

func cleanLevels(levels []string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(levels))
	for _, level := range levels {
		level = strings.ToLower(strings.TrimSpace(level))
		if level == "" {
			continue
		}
		if _, exists := seen[level]; !exists {
			seen[level] = struct{}{}
			out = append(out, level)
		}
	}
	return out
}

func positiveInt64(values ...int64) int64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}
