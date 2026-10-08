package copilot

import (
	"strings"
)

func defaultSettings() settings {
	return settings{
		ClientID:                         defaultClientID,
		GitHubHost:                       defaultGitHubHost,
		EnableModels:                     true,
		ModelCacheTTL:                    300,
		MaxStreamBytes:                   4 << 20,
		WebSearchModel:                   "gpt-5.6-terra",
		EnableResponsesContextManagement: false,
	}
}

func (s *service) configureSettings(cfg settings) error {
	cfg.ClientID = strings.TrimSpace(cfg.ClientID)
	cfg.WebSearchModel = normalizeModelID(cfg.WebSearchModel)
	if cfg.ClientID == "" {
		return &serviceError{code: "invalid_config", message: "client_id is required"}
	}
	host, errHost := normalizeGitHubHost(cfg.GitHubHost)
	if errHost != nil {
		return &serviceError{code: "invalid_config", message: errHost.Error()}
	}
	cfg.GitHubHost = host
	if cfg.ModelCacheTTL < 0 {
		return &serviceError{code: "invalid_config", message: "model_cache_ttl_seconds must not be negative"}
	}
	if cfg.MaxStreamBytes < 64<<10 || cfg.MaxStreamBytes > 64<<20 {
		return &serviceError{code: "invalid_config", message: "max_stream_buffer_bytes must be between 65536 and 67108864"}
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
	s.logEvent("", "info", "copilot.configured", map[string]any{
		"github_host":                         cfg.GitHubHost,
		"enable_models":                       cfg.EnableModels,
		"model_cache_ttl_seconds":             cfg.ModelCacheTTL,
		"max_stream_buffer_bytes":             cfg.MaxStreamBytes,
		"web_search_model":                    cfg.WebSearchModel,
		"enable_responses_context_management": cfg.EnableResponsesContextManagement,
		"credential_identity_changed":         changedIdentity,
		"discarded_login_session_count":       len(staleSessions),
	})
	return nil
}

func (s *service) loadedConfig() settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config
}

func (s *service) shutdown() {
	s.stopStreamForwarding()
	s.logEvent("", "info", "copilot.shutdown", nil)
	s.mu.Lock()
	sessions := s.sessions
	s.sessions = make(map[string]*loginSession)
	s.routes = make(map[routeKey]modelRoute)
	s.mu.Unlock()
	for _, session := range sessions {
		clearLoginSessionSecrets(session)
	}
}
