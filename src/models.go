package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type storedModel struct {
	ID                              string   `json:"id"`
	Name                            string   `json:"name,omitempty"`
	Version                         string   `json:"version,omitempty"`
	Family                          string   `json:"family,omitempty"`
	Format                          string   `json:"format"`
	ContextWindow                   int64    `json:"context_window,omitempty"`
	MaxPromptTokens                 int64    `json:"max_prompt_tokens,omitempty"`
	MaxOutputTokens                 int64    `json:"max_output_tokens,omitempty"`
	InputModalities                 []string `json:"input_modalities,omitempty"`
	ReasoningLevels                 []string `json:"reasoning_levels,omitempty"`
	MinThinking                     int      `json:"min_thinking,omitempty"`
	MaxThinking                     int      `json:"max_thinking,omitempty"`
	AdaptiveThinking                bool     `json:"adaptive_thinking,omitempty"`
	ForceAdaptiveThinking           *bool    `json:"force_adaptive_thinking,omitempty"`
	SupportsTemperature             *bool    `json:"supports_temperature,omitempty"`
	SupportsEagerToolInputStreaming *bool    `json:"supports_eager_tool_input_streaming,omitempty"`
	SupportsXHighEffort             *bool    `json:"supports_xhigh_effort,omitempty"`
	// Streaming、SupportsToolSearch、SupportsContextEditing 缺失表示未声明该能力，按禁用处理。
	Streaming                 *bool             `json:"streaming,omitempty"`
	SupportsToolSearch        *bool             `json:"supports_tool_search,omitempty"`
	SupportsContextEditing    *bool             `json:"supports_context_editing,omitempty"`
	CompatibilityHeaders      map[string]string `json:"-"`
	ContextWindowOverridden   bool              `json:"-"`
	ReasoningLevelsOverridden bool              `json:"-"`
}

type modelRoute struct {
	Format                          string
	Path                            string
	Headers                         map[string]string
	AdaptiveThinking                bool
	ForceAdaptiveThinking           *bool
	SupportsTemperature             *bool
	SupportsEagerToolInputStreaming *bool
	SupportsXHighEffort             *bool
	// 以下字段供请求构造使用（reasoning_effort 校验、Responses compaction 阈值、Anthropic beta 门控）；
	// 缺失可选能力（nil）一律按禁用处理。
	Family                 string
	MaxPromptTokens        int64
	MaxOutputTokens        int64
	Streaming              *bool
	Vision                 bool
	ReasoningLevels        []string
	SupportsToolSearch     *bool
	SupportsContextEditing *bool
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
			ToolSearch        *bool    `json:"tool_search"`
			ContextEditing    *bool    `json:"context_editing"`
		} `json:"supports"`
	} `json:"capabilities"`
}

var knownCopilotModels = []string{
	"claude-fable-5", "claude-haiku-4.5", "claude-opus-4.5", "claude-opus-4.6",
	"claude-opus-4.7", "claude-opus-4.8", "claude-opus-5", "claude-sonnet-4", "claude-sonnet-4.5",
	"claude-sonnet-4.6", "claude-sonnet-5", "gemini-3.1-pro-preview", "gemini-3.5-flash",
	"gemini-3.6-flash", "gpt-4.1", "gpt-5-mini", "gpt-5.2",
	"gpt-5.2-codex", "gpt-5.3-codex", "gpt-5.4", "gpt-5.4-mini", "gpt-5.4-nano",
	"gpt-5.5", "gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "grok-4.5", "kimi-k2.7-code",
	"mai-code-1-flash-picker",
}

func (s *pluginService) modelsForAuth(raw []byte) ([]byte, error) {
	var req rpcAuthModelRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, &pluginFailure{code: "invalid_request", message: "decode model discovery request"}
	}
	storage, errStorage := decodeCopilotStorage(req.StorageJSON)
	if errStorage != nil || strings.TrimSpace(storage.CopilotSessionToken) == "" {
		failure := &pluginFailure{code: "invalid_auth", message: "GitHub Copilot credential requires refresh", httpStatus: http.StatusUnauthorized}
		s.logFailure(req.HostCallbackID, "models.resolve.failed", failure, map[string]any{"auth_id": req.AuthID, "stage": "credential_validation"})
		return nil, failure
	}
	if host, errHost := normalizeGitHubHost(storage.GitHubHost); errHost != nil || host != s.loadedConfig().GitHubHost {
		failure := &pluginFailure{code: "invalid_auth", message: "GitHub Copilot credential host does not match plugin configuration"}
		s.logFailure(req.HostCallbackID, "models.resolve.failed", failure, map[string]any{"auth_id": req.AuthID, "stage": "github_host_validation"})
		return nil, failure
	}
	client := hostClient{bridge: s.bridge, callbackID: req.HostCallbackID}
	manifest, manifestChanged := s.loadCompatibilityManifest(client, &storage)
	models := applyCompatibilityManifest(storage.Models, manifest)
	storageChanged := manifestChanged
	cacheFresh := s.modelCacheFresh(storage)
	source := "cache"
	if !cacheFresh {
		source = "discovery"
		discovered, failure := s.discoverModels(client, storage)
		if failure == nil {
			models = applyCompatibilityManifest(discovered, manifest)
			storage.Models = discovered
			storage.ModelsFetchedAt = s.now().UTC().UnixMilli()
			storageChanged = true
		} else if len(models) == 0 {
			s.logFailure(req.HostCallbackID, "models.resolve.failed", failure, map[string]any{"auth_id": req.AuthID, "stage": "model_discovery"})
			return nil, failure
		} else {
			source = "stale_cache"
			s.logFailure(req.HostCallbackID, "models.discovery.fallback", failure, map[string]any{
				"auth_id":            req.AuthID,
				"cached_model_count": len(models),
				"cached_model_ids":   storedModelIDs(models),
			})
		}
	}
	s.setModelRoutes(req.AuthID, models)
	s.logEvent(req.HostCallbackID, "info", "models.resolved", map[string]any{
		"auth_id":        req.AuthID,
		"catalog_source": source,
		"cache_fresh":    cacheFresh,
		"model_count":    len(models),
		"model_ids":      storedModelIDs(models),
	})
	response := pluginapi.ModelResponse{Provider: pluginIdentifier, Models: modelInfos(models)}
	if storageChanged {
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
		failure := &pluginFailure{code: "invalid_auth", message: "GitHub Copilot API endpoint is invalid"}
		s.logFailure(client.callbackID, "models.discovery.failed", failure, map[string]any{"stage": "endpoint_validation"})
		return nil, failure
	}
	s.logEvent(client.callbackID, "debug", "models.discovery.started", map[string]any{"github_host": storage.GitHubHost})
	headers := copilotIdentityHeaders()
	headers.Set("Accept", "application/json")
	headers.Set("Authorization", "Bearer "+storage.CopilotSessionToken)
	headers.Set("X-GitHub-Api-Version", copilotAPIVersion)
	resp, errHTTP := client.do(pluginapi.HTTPRequest{Method: http.MethodGet, URL: baseURL + "/models", Headers: headers})
	if errHTTP != nil {
		failure := &pluginFailure{code: "model_discovery_network_error", message: "GitHub Copilot model discovery is temporarily unavailable", retryable: true}
		s.logFailure(client.callbackID, "models.discovery.failed", failure, map[string]any{"stage": "host_http"})
		return nil, failure
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		failure := upstreamFailure("model_discovery_http_error", "GitHub Copilot model discovery failed", resp.StatusCode)
		s.logFailure(client.callbackID, "models.discovery.failed", failure, map[string]any{"stage": "upstream_http"})
		return nil, failure
	}
	models, errParse := parseDiscoveredModels(resp.Body, baseURL == individualCopilotAPIURL)
	if errParse != nil {
		failure := &pluginFailure{code: "model_discovery_invalid", message: errParse.Error(), httpStatus: http.StatusBadGateway}
		s.logFailure(client.callbackID, "models.discovery.failed", failure, map[string]any{"stage": "response_validation"})
		return nil, failure
	}
	s.logEvent(client.callbackID, "info", "models.discovery.completed", map[string]any{
		"model_count": len(models),
		"model_ids":   storedModelIDs(models),
	})
	return models, nil
}

func parseDiscoveredModels(raw []byte, allowPolicyFallback bool) ([]storedModel, error) {
	var response remoteModelsResponse
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil || response.Data == nil {
		return nil, fmt.Errorf("GitHub Copilot returned an invalid model catalog")
	}
	pickerModels := make([]remoteModel, 0, len(response.Data))
	policyModels := make([]remoteModel, 0, len(response.Data))
	for _, rawModel := range response.Data {
		var model remoteModel
		if json.Unmarshal(rawModel, &model) != nil {
			continue
		}
		model.ID = strings.TrimSpace(model.ID)
		if model.ID == "" || (model.Capabilities.Supports.ToolCalls != nil && !*model.Capabilities.Supports.ToolCalls) {
			continue
		}
		policyState := strings.TrimSpace(model.Policy.State)
		if model.ModelPickerEnabled && !strings.EqualFold(policyState, "disabled") {
			pickerModels = append(pickerModels, model)
		}
		if strings.EqualFold(policyState, "enabled") {
			policyModels = append(policyModels, model)
		}
	}
	selectedModels := pickerModels
	if len(selectedModels) == 0 && allowPolicyFallback {
		selectedModels = policyModels
	}
	models := make([]storedModel, 0, len(selectedModels))
	seen := make(map[string]struct{})
	for _, model := range selectedModels {
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
		levels := copilotReasoningLevels(model.ID, model.Capabilities.Supports.ReasoningEffort)
		models = append(models, storedModel{
			ID:                     model.ID,
			Name:                   valueOr(strings.TrimSpace(model.Name), model.ID),
			Version:                strings.TrimSpace(model.Version),
			Family:                 strings.TrimSpace(model.Capabilities.Family),
			Format:                 format,
			ContextWindow:          copilotContextWindow(model.ID, positiveInt64(model.Capabilities.Limits.MaxContextWindowTokens, model.Capabilities.Limits.MaxPromptTokens)),
			MaxPromptTokens:        maxInt64(model.Capabilities.Limits.MaxPromptTokens, 0),
			MaxOutputTokens:        maxInt64(model.Capabilities.Limits.MaxOutputTokens, 0),
			InputModalities:        modalities,
			ReasoningLevels:        levels,
			MinThinking:            max(model.Capabilities.Supports.MinThinkingBudget, 0),
			MaxThinking:            max(model.Capabilities.Supports.MaxThinkingBudget, 0),
			AdaptiveThinking:       model.Capabilities.Supports.AdaptiveThinking || forcesAnthropicAdaptiveThinking(model.ID),
			Streaming:              model.Capabilities.Supports.Streaming,
			SupportsToolSearch:     model.Capabilities.Supports.ToolSearch,
			SupportsContextEditing: model.Capabilities.Supports.ContextEditing,
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
	if isCopilotClaude(id) {
		return formatClaude
	}
	if id == "grok-4.5" || strings.HasPrefix(id, "gpt-5") || strings.HasPrefix(id, "oswe") || strings.HasPrefix(id, "mai-") {
		return formatOpenAIResponse
	}
	return formatOpenAI
}

func isCopilotClaude(modelID string) bool {
	id := strings.ToLower(strings.TrimSpace(modelID))
	for _, family := range []string{"haiku", "sonnet", "opus", "fable"} {
		version, found := strings.CutPrefix(id, "claude-"+family+"-")
		if !found || version == "" {
			continue
		}
		majorMatches := version[0] == '4' || version[0] == '5'
		return majorMatches && (len(version) == 1 || version[1] == '.' || version[1] == '-')
	}
	return false
}

func effectiveModelFormat(modelID, storedFormat string) string {
	if strings.EqualFold(strings.TrimSpace(modelID), "claude-fable-5") {
		return formatClaude
	}
	return storedFormat
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
		format := effectiveModelFormat(model.ID, model.Format)
		adaptiveThinking := model.AdaptiveThinking || compatibilityBool(model.ForceAdaptiveThinking, forcesAnthropicAdaptiveThinking(model.ID))
		reasoningLevels := copilotReasoningLevels(model.ID, model.ReasoningLevels)
		if model.ReasoningLevelsOverridden {
			reasoningLevels = cleanLevels(model.ReasoningLevels)
		}
		contextWindow := copilotContextWindow(model.ID, model.ContextWindow)
		if model.ContextWindowOverridden {
			contextWindow = model.ContextWindow
		}
		configurableThinking := format != formatOpenAI
		parameters := []string{"max_tokens"}
		if format != formatClaude || compatibilityBool(model.SupportsTemperature, supportsAnthropicTemperature(model.ID)) {
			parameters = append(parameters, "temperature")
		}
		parameters = append(parameters, "top_p", "tools", "tool_choice")
		if compatibilityBool(model.Streaming, false) {
			parameters = append(parameters, "stream")
		}
		var thinking *pluginapi.ThinkingSupport
		if configurableThinking && (len(reasoningLevels) > 0 || model.MaxThinking > 0 || adaptiveThinking) {
			thinking = &pluginapi.ThinkingSupport{
				Min:            model.MinThinking,
				Max:            model.MaxThinking,
				ZeroAllowed:    supportsReasoningOff(model.ID, format),
				DynamicAllowed: adaptiveThinking,
				Levels:         reasoningLevels,
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
			ContextLength:              contextWindow,
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
		route := routeForStoredModel(model)
		if model.ID != "" && route.Path != "" {
			s.routes[routeKey{AuthID: authID, ModelID: model.ID}] = route
		}
	}
	s.mu.Unlock()
}

func (s *pluginService) resolveModelRoute(authID, modelID string, storage copilotStorage) modelRoute {
	for _, model := range storage.Models {
		if model.ID == modelID {
			if s.loadedConfig().EnableRemoteCompatibility {
				if manifest, ok := cachedCompatibilityManifest(&storage); ok {
					model = applyCompatibilityManifest([]storedModel{model}, manifest)[0]
				}
			}
			return routeForStoredModel(model)
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
	return modelRoute{Format: format, Path: endpointPath(format), AdaptiveThinking: forcesAnthropicAdaptiveThinking(modelID)}
}

func routeForStoredModel(model storedModel) modelRoute {
	format := effectiveModelFormat(model.ID, model.Format)
	reasoningLevels := model.ReasoningLevels
	if model.ReasoningLevelsOverridden {
		reasoningLevels = cleanLevels(model.ReasoningLevels)
	} else {
		reasoningLevels = copilotReasoningLevels(model.ID, model.ReasoningLevels)
	}
	return modelRoute{
		Format:                          format,
		Path:                            endpointPath(format),
		Headers:                         cloneStringMap(model.CompatibilityHeaders),
		AdaptiveThinking:                model.AdaptiveThinking || compatibilityBool(model.ForceAdaptiveThinking, forcesAnthropicAdaptiveThinking(model.ID)),
		ForceAdaptiveThinking:           cloneBool(model.ForceAdaptiveThinking),
		SupportsTemperature:             cloneBool(model.SupportsTemperature),
		SupportsEagerToolInputStreaming: cloneBool(model.SupportsEagerToolInputStreaming),
		SupportsXHighEffort:             cloneBool(model.SupportsXHighEffort),
		Family:                          model.Family,
		MaxPromptTokens:                 model.MaxPromptTokens,
		MaxOutputTokens:                 model.MaxOutputTokens,
		Streaming:                       cloneBool(model.Streaming),
		Vision:                          hasModality(model.InputModalities, "image"),
		ReasoningLevels:                 reasoningLevels,
		SupportsToolSearch:              cloneBool(model.SupportsToolSearch),
		SupportsContextEditing:          cloneBool(model.SupportsContextEditing),
	}
}

func hasModality(modalities []string, want string) bool {
	for _, modality := range modalities {
		if strings.EqualFold(modality, want) {
			return true
		}
	}
	return false
}

func compatibilityBool(override *bool, fallback bool) bool {
	if override != nil {
		return *override
	}
	return fallback
}

func (s *pluginService) enableKnownModels(client hostClient, storage copilotStorage) {
	baseURL := apiBaseFromSessionToken(storage.CopilotSessionToken, storage.GitHubHost)
	if baseURL == "" {
		s.logEvent(client.callbackID, "warn", "models.policy_enable.skipped", map[string]any{"reason": "invalid_endpoint"})
		return
	}
	jobs := make(chan string)
	var wait sync.WaitGroup
	var enabled atomic.Int64
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
				resp, errHTTP := client.do(pluginapi.HTTPRequest{
					Method:  http.MethodPost,
					URL:     baseURL + "/models/" + modelID + "/policy",
					Headers: headers,
					Body:    []byte(`{"state":"enabled"}`),
				})
				if errHTTP == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
					enabled.Add(1)
				}
			}
		}()
	}
	for _, modelID := range knownCopilotModels {
		jobs <- modelID
	}
	close(jobs)
	wait.Wait()
	s.logEvent(client.callbackID, "debug", "models.policy_enable.completed", map[string]any{
		"attempted_count": len(knownCopilotModels),
		"enabled_count":   enabled.Load(),
		"failed_count":    int64(len(knownCopilotModels)) - enabled.Load(),
	})
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

func copilotReasoningLevels(model string, discovered []string) []string {
	levels := cleanLevels(discovered)
	id := strings.ToLower(normalizeModelID(model))
	var required []string
	switch {
	case forcesAnthropicAdaptiveThinking(id):
		required = []string{"minimal", "low", "medium", "high"}
		if supportsAnthropicXHighEffort(id) {
			required = append(required, "xhigh")
		}
		required = append(required, "max")
	case strings.HasPrefix(id, "gpt-5"):
		required = []string{"minimal", "low", "medium", "high"}
		if supportsCopilotGPTXHigh(id) {
			required = append(required, "xhigh")
		}
		if supportsCopilotGPTMax(id) {
			required = append(required, "max")
		}
	default:
		return levels
	}
	seen := make(map[string]struct{}, len(required)+len(levels))
	out := make([]string, 0, len(required)+len(levels))
	for _, level := range append(required, levels...) {
		if _, exists := seen[level]; exists {
			continue
		}
		seen[level] = struct{}{}
		out = append(out, level)
	}
	return out
}

func supportsCopilotGPTXHigh(model string) bool {
	switch strings.ToLower(normalizeModelID(model)) {
	case "gpt-5.2", "gpt-5.2-codex", "gpt-5.3-codex", "gpt-5.4", "gpt-5.4-mini", "gpt-5.4-nano",
		"gpt-5.5", "gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra":
		return true
	default:
		return false
	}
}

func supportsCopilotGPTMax(model string) bool {
	switch strings.ToLower(normalizeModelID(model)) {
	case "gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra":
		return true
	default:
		return false
	}
}

func supportsReasoningOff(model, format string) bool {
	return format != formatOpenAIResponse || !strings.HasPrefix(strings.ToLower(normalizeModelID(model)), "gpt-5")
}

func copilotContextWindow(model string, discovered int64) int64 {
	switch strings.ToLower(normalizeModelID(model)) {
	case "claude-fable-5", "claude-opus-4.6", "claude-opus-4.7", "claude-opus-4.8", "claude-opus-5",
		"claude-sonnet-4.6", "claude-sonnet-5", "gpt-5.3-codex", "gpt-5.4", "gpt-5.5":
		return maxInt64(discovered, 1_000_000)
	default:
		return discovered
	}
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
