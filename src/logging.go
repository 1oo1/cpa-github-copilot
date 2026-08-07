package main

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

type rpcHostLogRequest struct {
	HostCallbackID string         `json:"host_callback_id,omitempty"`
	Level          string         `json:"level,omitempty"`
	Message        string         `json:"message,omitempty"`
	Fields         map[string]any `json:"fields,omitempty"`
}

func (s *pluginService) logEvent(callbackID, level, event string, fields map[string]any) {
	if s == nil || s.bridge == nil {
		return
	}
	defer func() {
		_ = recover()
	}()

	event = normalizeLogEvent(event)
	safeFields := map[string]any{
		"event":           event,
		"plugin_provider": pluginIdentifier,
		"plugin_version":  pluginVersion,
	}
	for key, value := range fields {
		key = strings.TrimSpace(key)
		if key == "" || sensitiveLogKey(key) {
			continue
		}
		if safeValue, ok := safeLogValue(value); ok {
			safeFields[key] = safeValue
		}
	}
	_, _ = s.bridge.Call(pluginabi.MethodHostLog, rpcHostLogRequest{
		HostCallbackID: strings.TrimSpace(callbackID),
		Level:          normalizeLogLevel(level),
		Message:        "github-copilot: " + event,
		Fields:         safeFields,
	})
}

func (s *pluginService) logFailure(callbackID, event string, failure *pluginFailure, fields map[string]any) {
	out := make(map[string]any, len(fields)+3)
	for key, value := range fields {
		out[key] = value
	}
	if failure != nil {
		out["failure_code"] = failure.code
		out["http_status"] = failure.httpStatus
		out["retryable"] = failure.retryable
	}
	s.logEvent(callbackID, "warn", event, out)
}

func normalizeLogEvent(event string) string {
	event = strings.ToLower(strings.TrimSpace(event))
	var out strings.Builder
	for _, r := range event {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			out.WriteRune(r)
		}
		if out.Len() >= 80 {
			break
		}
	}
	if out.Len() == 0 {
		return "diagnostic"
	}
	return out.String()
}

func normalizeLogLevel(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "trace", "debug", "info", "warn", "error":
		return strings.ToLower(strings.TrimSpace(level))
	default:
		return "debug"
	}
}

func sensitiveLogKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	if sensitiveKey(key) {
		return true
	}
	for _, fragment := range []string{"body", "payload", "headers", "device_code", "user_code", "raw_request", "raw_response", "url", "query", "cookie", "content"} {
		if strings.Contains(key, fragment) {
			return true
		}
	}
	return false
}

func safeLogValue(value any) (any, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case int:
		return typed, true
	case int8:
		return typed, true
	case int16:
		return typed, true
	case int32:
		return typed, true
	case int64:
		return typed, true
	case uint:
		return typed, true
	case uint8:
		return typed, true
	case uint16:
		return typed, true
	case uint32:
		return typed, true
	case uint64:
		return typed, true
	case float32:
		return typed, true
	case float64:
		return typed, true
	case time.Time:
		if typed.IsZero() {
			return nil, false
		}
		return typed.UTC().Format(time.RFC3339), true
	case string:
		return safeLogString(typed)
	case []string:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			safeItem, ok := safeLogString(item)
			if !ok {
				continue
			}
			out = append(out, safeItem.(string))
		}
		if len(out) == 0 {
			return nil, false
		}
		return out, true
	default:
		return nil, false
	}
}

func safeLogString(value string) (any, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, false
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{"bearer ", "ghu_", "gho_", "ghp_", "github_pat_", "tid=", "sk-"} {
		if strings.Contains(lower, marker) {
			return nil, false
		}
	}
	if len(value) > 256 {
		value = value[:256]
	}
	return value, true
}

func callbackIDFromRequest(raw []byte) string {
	var request struct {
		HostCallbackID string `json:"host_callback_id,omitempty"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &request) != nil {
		return ""
	}
	return strings.TrimSpace(request.HostCallbackID)
}

func authLogFields(storage copilotStorage, now time.Time) map[string]any {
	fields := map[string]any{
		"github_host":            storage.GitHubHost,
		"long_term_auth_present": strings.TrimSpace(storage.GitHubAccessToken) != "",
		"session_auth_present":   strings.TrimSpace(storage.CopilotSessionToken) != "",
		"model_count":            len(storage.Models),
		"model_ids":              storedModelIDs(storage.Models),
	}
	if storage.RefreshAfter > 0 {
		refreshAt := time.UnixMilli(storage.RefreshAfter).UTC()
		fields["refresh_after"] = refreshAt
		fields["refresh_due"] = !now.UTC().Before(refreshAt)
	}
	if storage.ExpiresAt > 0 {
		expiresAt := time.UnixMilli(storage.ExpiresAt).UTC()
		fields["expires_at"] = expiresAt
		fields["session_expired"] = !now.UTC().Before(expiresAt)
	}
	return fields
}

func storedModelIDs(models []storedModel) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if id := strings.TrimSpace(model.ID); id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func safeLogFileName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "." {
		return ""
	}
	return name
}
