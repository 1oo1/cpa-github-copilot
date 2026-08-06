package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	compatibilitySchemaVersion = 1
	maxCompatibilityBytes      = 1 << 20
	remoteCompatibilityURL     = "https://raw.githubusercontent.com/1oo1/cpa-github-copilot/main/compatibility.json"
)

//go:embed compatibility.json
var builtinCompatibilityRaw []byte

type compatibilityManifest struct {
	SchemaVersion int                           `json:"schema_version"`
	GeneratedAt   time.Time                     `json:"generated_at"`
	Models        map[string]compatibilityModel `json:"models"`
}

type compatibilityModel struct {
	ID                               string             `json:"id,omitempty"`
	Name                             string             `json:"name,omitempty"`
	API                              string             `json:"api,omitempty"`
	Provider                         string             `json:"provider,omitempty"`
	BaseURL                          string             `json:"baseUrl,omitempty"`
	Reasoning                        bool               `json:"reasoning,omitempty"`
	Input                            []string           `json:"input,omitempty"`
	Cost                             *compatibilityCost `json:"cost,omitempty"`
	ContextWindow                    *int64             `json:"contextWindow,omitempty"`
	MaxOutputTokens                  *int64             `json:"maxTokens,omitempty"`
	Headers                          map[string]string  `json:"headers,omitempty"`
	Compat                           compatibilityFlags `json:"compat,omitempty"`
	ThinkingLevelMap                 map[string]*string `json:"thinkingLevelMap,omitempty"`
	LegacyFormat                     string             `json:"format,omitempty"`
	LegacyContextWindow              *int64             `json:"context_window,omitempty"`
	LegacyMaxOutputTokens            *int64             `json:"max_output_tokens,omitempty"`
	LegacyReasoningLevels            []string           `json:"reasoning_levels,omitempty"`
	LegacyForceAdaptiveThinking      *bool              `json:"force_adaptive_thinking,omitempty"`
	LegacySupportsTemperature        *bool              `json:"supports_temperature,omitempty"`
	LegacySupportsEagerToolStreaming *bool              `json:"supports_eager_tool_input_streaming,omitempty"`
	LegacySupportsXHighEffort        *bool              `json:"supports_xhigh_effort,omitempty"`
}

type compatibilityFlags struct {
	ForceAdaptiveThinking           *bool `json:"forceAdaptiveThinking,omitempty"`
	SupportsDeveloperRole           *bool `json:"supportsDeveloperRole,omitempty"`
	SupportsEagerToolInputStreaming *bool `json:"supportsEagerToolInputStreaming,omitempty"`
	SupportsOpenAIGrammarTools      *bool `json:"supportsOpenAIGrammarTools,omitempty"`
	SupportsReasoningEffort         *bool `json:"supportsReasoningEffort,omitempty"`
	SupportsStore                   *bool `json:"supportsStore,omitempty"`
	SupportsTemperature             *bool `json:"supportsTemperature,omitempty"`
}

type compatibilityCost struct {
	Input      float64                 `json:"input"`
	Output     float64                 `json:"output"`
	CacheRead  float64                 `json:"cacheRead"`
	CacheWrite float64                 `json:"cacheWrite"`
	Tiers      []compatibilityCostTier `json:"tiers,omitempty"`
}

type compatibilityCostTier struct {
	InputTokensAbove int64   `json:"inputTokensAbove"`
	Input            float64 `json:"input"`
	Output           float64 `json:"output"`
	CacheRead        float64 `json:"cacheRead"`
	CacheWrite       float64 `json:"cacheWrite"`
}

var builtinCompatibility = mustParseCompatibilityManifest(builtinCompatibilityRaw)

func mustParseCompatibilityManifest(raw []byte) compatibilityManifest {
	manifest, errParse := parseCompatibilityManifest(raw)
	if errParse != nil {
		panic(errParse)
	}
	return manifest
}

func parseCompatibilityManifest(raw []byte) (compatibilityManifest, error) {
	if len(raw) == 0 || len(raw) > maxCompatibilityBytes {
		return compatibilityManifest{}, fmt.Errorf("compatibility manifest size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var manifest compatibilityManifest
	if errDecode := decoder.Decode(&manifest); errDecode != nil {
		return compatibilityManifest{}, fmt.Errorf("decode compatibility manifest: %w", errDecode)
	}
	if errTrailing := decoder.Decode(&struct{}{}); errTrailing != io.EOF {
		return compatibilityManifest{}, fmt.Errorf("compatibility manifest contains trailing data")
	}
	if manifest.SchemaVersion != compatibilitySchemaVersion {
		return compatibilityManifest{}, fmt.Errorf("unsupported compatibility schema version")
	}
	if manifest.GeneratedAt.IsZero() {
		return compatibilityManifest{}, fmt.Errorf("compatibility manifest generated_at is required")
	}
	if len(manifest.Models) > 512 {
		return compatibilityManifest{}, fmt.Errorf("compatibility manifest contains too many models")
	}
	for id, model := range manifest.Models {
		if errValidate := validateCompatibilityModel(id, model); errValidate != nil {
			return compatibilityManifest{}, errValidate
		}
	}
	return manifest, nil
}

func validateCompatibilityModel(id string, model compatibilityModel) error {
	if id == "" || len(id) > 128 || normalizeModelID(id) != id {
		return fmt.Errorf("compatibility manifest contains invalid model id")
	}
	if model.ID != "" && model.ID != id {
		return fmt.Errorf("compatibility manifest model id does not match its key")
	}
	if model.Provider != "" && model.Provider != pluginIdentifier {
		return fmt.Errorf("compatibility manifest contains invalid provider")
	}
	format := compatibilityModelFormat(model)
	if format == "invalid" || (model.LegacyFormat != "" && model.API != "") {
		return fmt.Errorf("compatibility manifest contains invalid model format")
	}
	contextWindow := firstInt64(model.ContextWindow, model.LegacyContextWindow)
	if contextWindow != nil && (*contextWindow < 1024 || *contextWindow > 10_000_000) {
		return fmt.Errorf("compatibility manifest context window is out of range")
	}
	maxOutputTokens := firstInt64(model.MaxOutputTokens, model.LegacyMaxOutputTokens)
	if maxOutputTokens != nil && (*maxOutputTokens < 1 || *maxOutputTokens > 1_000_000) {
		return fmt.Errorf("compatibility manifest max output tokens is out of range")
	}
	if len(model.LegacyReasoningLevels) > 8 {
		return fmt.Errorf("compatibility manifest contains too many reasoning levels")
	}
	seen := make(map[string]struct{}, len(model.LegacyReasoningLevels))
	for _, level := range model.LegacyReasoningLevels {
		level = strings.ToLower(strings.TrimSpace(level))
		switch level {
		case "minimal", "low", "medium", "high", "xhigh", "max":
		default:
			return fmt.Errorf("compatibility manifest contains invalid reasoning level")
		}
		if _, exists := seen[level]; exists {
			return fmt.Errorf("compatibility manifest contains duplicate reasoning level")
		}
		seen[level] = struct{}{}
	}
	for level, mapped := range model.ThinkingLevelMap {
		switch level {
		case "off", "minimal", "low", "medium", "high", "xhigh", "max":
		default:
			return fmt.Errorf("compatibility manifest contains invalid thinking level")
		}
		if mapped == nil {
			continue
		}
		switch *mapped {
		case "minimal", "low", "medium", "high", "xhigh", "max":
		default:
			return fmt.Errorf("compatibility manifest contains invalid thinking level mapping")
		}
	}
	if errHeaders := validateCompatibilityHeaders(model.Headers); errHeaders != nil {
		return errHeaders
	}
	return nil
}

func compatibilityModelFormat(model compatibilityModel) string {
	if model.LegacyFormat != "" {
		if endpointPath(model.LegacyFormat) != "" {
			return model.LegacyFormat
		}
		return "invalid"
	}
	switch model.API {
	case "":
		return ""
	case "anthropic-messages":
		return formatClaude
	case "openai-completions":
		return formatOpenAI
	case "openai-responses":
		return formatOpenAIResponse
	default:
		return "invalid"
	}
}

func validateCompatibilityHeaders(headers map[string]string) error {
	for name, value := range headers {
		switch http.CanonicalHeaderKey(strings.TrimSpace(name)) {
		case "User-Agent", "Editor-Version", "Editor-Plugin-Version", "Copilot-Integration-Id":
		default:
			return fmt.Errorf("compatibility manifest contains unsafe header")
		}
		if value == "" || len(value) > 256 || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("compatibility manifest contains invalid header value")
		}
	}
	return nil
}

func firstInt64(primary, fallback *int64) *int64 {
	if primary != nil {
		return primary
	}
	return fallback
}

func compatibilityManifestIsUsable(manifest compatibilityManifest, minimumGeneratedAt time.Time) bool {
	return !manifest.GeneratedAt.Before(minimumGeneratedAt)
}

func applyCompatibilityManifest(models []storedModel, manifest compatibilityManifest) []storedModel {
	out := append([]storedModel(nil), models...)
	for index := range out {
		override, exists := manifest.Models[out[index].ID]
		if !exists {
			continue
		}
		if format := compatibilityModelFormat(override); format != "" {
			out[index].Format = format
		}
		if contextWindow := firstInt64(override.ContextWindow, override.LegacyContextWindow); contextWindow != nil {
			out[index].ContextWindow = *contextWindow
			out[index].ContextWindowOverridden = true
		}
		if maxOutputTokens := firstInt64(override.MaxOutputTokens, override.LegacyMaxOutputTokens); maxOutputTokens != nil {
			out[index].MaxOutputTokens = *maxOutputTokens
		}
		if override.LegacyReasoningLevels != nil {
			out[index].ReasoningLevels = append([]string(nil), override.LegacyReasoningLevels...)
			out[index].ReasoningLevelsOverridden = true
		}
		out[index].ForceAdaptiveThinking = cloneBool(firstBool(override.Compat.ForceAdaptiveThinking, override.LegacyForceAdaptiveThinking))
		out[index].SupportsTemperature = cloneBool(firstBool(override.Compat.SupportsTemperature, override.LegacySupportsTemperature))
		out[index].SupportsEagerToolInputStreaming = cloneBool(firstBool(override.Compat.SupportsEagerToolInputStreaming, override.LegacySupportsEagerToolStreaming))
		out[index].SupportsXHighEffort = cloneBool(compatibilityXHighSupport(override))
		out[index].CompatibilityHeaders = cloneStringMap(override.Headers)
	}
	return out
}

func firstBool(primary, fallback *bool) *bool {
	if primary != nil {
		return primary
	}
	return fallback
}

func compatibilityXHighSupport(model compatibilityModel) *bool {
	if model.LegacySupportsXHighEffort != nil {
		return model.LegacySupportsXHighEffort
	}
	mapped, exists := model.ThinkingLevelMap["xhigh"]
	if !exists {
		return nil
	}
	supported := mapped != nil
	return &supported
}

func cloneStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[http.CanonicalHeaderKey(key)] = value
	}
	return out
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func (s *pluginService) loadCompatibilityManifest(client hostClient, storage *copilotStorage) (compatibilityManifest, bool) {
	config := s.loadedConfig()
	if !config.EnableRemoteCompatibility {
		return builtinCompatibility, false
	}
	cached, hasCached := cachedCompatibilityManifest(storage)
	if compatibilityCacheFresh(storage, config.RemoteCompatibilityCacheTTL, s.now().UTC()) {
		if hasCached {
			return cached, false
		}
		return builtinCompatibility, false
	}

	headers := http.Header{
		"Accept":     []string{"application/json"},
		"User-Agent": []string{"cpa-github-copilot/" + pluginVersion},
	}
	if etag := safeCompatibilityETag(storage.CompatibilityETag); hasCached && etag != "" {
		headers.Set("If-None-Match", etag)
	}
	resp, errHTTP := client.do(pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     remoteCompatibilityURL,
		Headers: headers,
	})
	if errHTTP != nil {
		s.logEvent(client.callbackID, "warn", "compatibility.refresh.failed", map[string]any{"reason": "host_http"})
		return compatibilityFallback(cached, hasCached), false
	}
	now := s.now().UTC()
	if resp.StatusCode == http.StatusNotModified && hasCached {
		storage.CompatibilityCheckedAt = now.UnixMilli()
		s.logEvent(client.callbackID, "debug", "compatibility.refresh.not_modified", nil)
		return cached, true
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.logEvent(client.callbackID, "warn", "compatibility.refresh.failed", map[string]any{
			"reason":      "upstream_http",
			"http_status": resp.StatusCode,
		})
		return compatibilityFallback(cached, hasCached), false
	}
	manifest, errParse := parseCompatibilityManifest(resp.Body)
	if errParse != nil || !compatibilityManifestIsUsable(manifest, builtinCompatibility.GeneratedAt) {
		storage.CompatibilityCheckedAt = now.UnixMilli()
		reason := "invalid_manifest"
		if errParse == nil {
			reason = "older_than_builtin"
		}
		s.logEvent(client.callbackID, "warn", "compatibility.refresh.failed", map[string]any{"reason": reason})
		return compatibilityFallback(cached, hasCached), true
	}
	storage.CompatibilityManifest = append(storage.CompatibilityManifest[:0], resp.Body...)
	storage.CompatibilityCheckedAt = now.UnixMilli()
	storage.CompatibilityETag = safeCompatibilityETag(resp.Headers.Get("Etag"))
	s.logEvent(client.callbackID, "info", "compatibility.refresh.completed", map[string]any{
		"generated_at": manifest.GeneratedAt,
		"model_count":  len(manifest.Models),
	})
	return manifest, true
}

func cachedCompatibilityManifest(storage *copilotStorage) (compatibilityManifest, bool) {
	if storage == nil || len(storage.CompatibilityManifest) == 0 {
		return compatibilityManifest{}, false
	}
	manifest, errParse := parseCompatibilityManifest(storage.CompatibilityManifest)
	if errParse != nil || !compatibilityManifestIsUsable(manifest, builtinCompatibility.GeneratedAt) {
		return compatibilityManifest{}, false
	}
	return manifest, true
}

func compatibilityCacheFresh(storage *copilotStorage, ttlSeconds int, now time.Time) bool {
	if storage == nil || storage.CompatibilityCheckedAt <= 0 || ttlSeconds == 0 {
		return false
	}
	return now.Before(time.UnixMilli(storage.CompatibilityCheckedAt).Add(time.Duration(ttlSeconds) * time.Second))
}

func compatibilityFallback(cached compatibilityManifest, hasCached bool) compatibilityManifest {
	if hasCached {
		return cached
	}
	return builtinCompatibility
}

func safeCompatibilityETag(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 256 {
		return ""
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return ""
		}
	}
	return value
}
