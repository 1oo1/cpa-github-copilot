package main

import (
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		ClientID:       defaultClientID,
		GitHubHost:     defaultGitHubHost,
		EnableModels:   true,
		ModelCacheTTL:  300,
		MaxStreamBytes: 4 << 20,
	}
}

func (s *pluginService) configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return &pluginFailure{code: "invalid_config", message: "decode plugin configuration envelope"}
		}
	}
	cfg := defaultPluginConfig()
	if len(req.ConfigYAML) > 0 {
		if errUnmarshal := yaml.Unmarshal(req.ConfigYAML, &cfg); errUnmarshal != nil {
			return &pluginFailure{code: "invalid_config", message: "decode plugin configuration"}
		}
	}
	cfg.ClientID = strings.TrimSpace(cfg.ClientID)
	if cfg.ClientID == "" {
		return &pluginFailure{code: "invalid_config", message: "client_id is required"}
	}
	host, errHost := normalizeGitHubHost(cfg.GitHubHost)
	if errHost != nil {
		return &pluginFailure{code: "invalid_config", message: errHost.Error()}
	}
	cfg.GitHubHost = host
	if cfg.ModelCacheTTL < 0 {
		return &pluginFailure{code: "invalid_config", message: "model_cache_ttl_seconds must not be negative"}
	}
	if cfg.MaxStreamBytes < 64<<10 || cfg.MaxStreamBytes > 64<<20 {
		return &pluginFailure{code: "invalid_config", message: "max_stream_buffer_bytes must be between 65536 and 67108864"}
	}
	s.mu.Lock()
	changedIdentity := s.config.ClientID != cfg.ClientID || s.config.GitHubHost != cfg.GitHubHost
	var staleSessions map[string]*loginSession
	s.config = cfg
	if changedIdentity {
		staleSessions = s.sessions
		s.sessions = make(map[string]*loginSession)
		s.routes = make(map[routeKey]modelRoute)
	}
	s.mu.Unlock()
	for _, session := range staleSessions {
		clearLoginSessionSecrets(session)
	}
	s.logEvent("", "info", "plugin.configured", map[string]any{
		"github_host":                   cfg.GitHubHost,
		"enable_models":                 cfg.EnableModels,
		"model_cache_ttl_seconds":       cfg.ModelCacheTTL,
		"max_stream_buffer_bytes":       cfg.MaxStreamBytes,
		"credential_identity_changed":   changedIdentity,
		"discarded_login_session_count": len(staleSessions),
	})
	return nil
}

func (s *pluginService) loadedConfig() pluginConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config
}

func (s *pluginService) registration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "GitHub Copilot",
			Version:          pluginVersion,
			Author:           "cpa-github-copilot",
			GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "client_id", Type: pluginapi.ConfigFieldTypeString, Description: "GitHub OAuth public client ID used for device authorization."},
				{Name: "github_host", Type: pluginapi.ConfigFieldTypeString, Description: "GitHub.com or a trusted GitHub Enterprise hostname."},
				{Name: "enable_models", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Best-effort enable Copilot model policies after login."},
				{Name: "model_cache_ttl_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Lifetime of a non-empty account model catalog before rediscovery."},
				{Name: "max_stream_buffer_bytes", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum buffered partial SSE event size."},
			},
		},
		Capabilities: registrationCapabilities{
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeOAuth),
			ExecutorInputFormats:  []string{formatOpenAI, formatOpenAIResponse, formatClaude},
			ExecutorOutputFormats: []string{formatOpenAI, formatOpenAIResponse, formatClaude},
		},
	}
}

func (s *pluginService) dispatch(method string, request []byte) (response []byte, err error) {
	callbackID := callbackIDFromRequest(request)
	started := s.now().UTC()
	s.logEvent(callbackID, "debug", "rpc.started", map[string]any{
		"method":        method,
		"request_bytes": len(request),
	})
	defer func() {
		fields := map[string]any{
			"method":         method,
			"request_bytes":  len(request),
			"response_bytes": len(response),
			"duration_ms":    s.now().UTC().Sub(started).Milliseconds(),
		}
		if recovered := recover(); recovered != nil {
			fields["failure_code"] = "panic"
			s.logEvent(callbackID, "error", "rpc.panicked", fields)
			panic(recovered)
		}
		if err == nil {
			s.logEvent(callbackID, "debug", "rpc.completed", fields)
			return
		}
		if failure, ok := err.(*pluginFailure); ok && failure != nil {
			fields["failure_code"] = failure.code
			fields["http_status"] = failure.httpStatus
			fields["retryable"] = failure.retryable
		}
		s.logEvent(callbackID, "warn", "rpc.failed", fields)
	}()

	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := s.configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(s.registration())
	case pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: pluginIdentifier})
	case pluginabi.MethodAuthParse:
		return s.parseAuth(request)
	case pluginabi.MethodAuthLoginStart:
		return s.startLogin(request)
	case pluginabi.MethodAuthLoginPoll:
		return s.pollLogin(request)
	case pluginabi.MethodAuthRefresh:
		return s.refreshAuth(request)
	case pluginabi.MethodModelStatic:
		return okEnvelope(pluginapi.ModelResponse{Provider: pluginIdentifier})
	case pluginabi.MethodModelForAuth:
		return s.modelsForAuth(request)
	case pluginabi.MethodExecutorExecute:
		return s.execute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return s.executeStream(request)
	case pluginabi.MethodExecutorCountTokens:
		return nil, &pluginFailure{code: "not_supported", message: "GitHub Copilot does not expose token counting", httpStatus: 501}
	case pluginabi.MethodExecutorHTTPRequest:
		return s.executeHTTPRequest(request)
	default:
		return nil, &pluginFailure{code: "unknown_method", message: "unsupported plugin method"}
	}
}

func (s *pluginService) shutdown() {
	s.logEvent("", "info", "plugin.shutdown", nil)
	s.mu.Lock()
	sessions := s.sessions
	s.sessions = make(map[string]*loginSession)
	s.routes = make(map[routeKey]modelRoute)
	s.mu.Unlock()
	for _, session := range sessions {
		clearLoginSessionSecrets(session)
	}
}
