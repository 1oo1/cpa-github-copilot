package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	defaultPollInterval = 5 * time.Second
	minPollInterval     = time.Second
	refreshSafetyMargin = 5 * time.Minute
	hostRefreshInterval = 15 * time.Minute
)

type copilotStorage struct {
	Type                 string        `json:"type"`
	GitHubAccessToken    string        `json:"github_access_token"`
	CopilotSessionToken  string        `json:"copilot_session_token,omitempty"`
	RefreshAfter         int64         `json:"refresh_after,omitempty"`
	ExpiresAt            int64         `json:"expires_at,omitempty"`
	GitHubHost           string        `json:"github_host,omitempty"`
	APIBaseURL           string        `json:"api_base_url,omitempty"`
	Account              string        `json:"account,omitempty"`
	Models               []storedModel `json:"models,omitempty"`
	ModelsFetchedAt      int64         `json:"models_fetched_at,omitempty"`
	LegacyRefresh        string        `json:"refresh,omitempty"`
	LegacyAccess         string        `json:"access,omitempty"`
	LegacyExpires        int64         `json:"expires,omitempty"`
	LegacyEnterpriseURL  string        `json:"enterpriseUrl,omitempty"`
	LegacyAvailableModel []string      `json:"availableModelIds,omitempty"`
}

type deviceCodeResponse struct {
	DeviceCode      string  `json:"device_code"`
	UserCode        string  `json:"user_code"`
	VerificationURI string  `json:"verification_uri"`
	Interval        float64 `json:"interval,omitempty"`
	ExpiresIn       float64 `json:"expires_in"`
}

type deviceTokenResponse struct {
	AccessToken      string  `json:"access_token,omitempty"`
	Error            string  `json:"error,omitempty"`
	ErrorDescription string  `json:"error_description,omitempty"`
	Interval         float64 `json:"interval,omitempty"`
}

type copilotTokenResponse struct {
	Token     string  `json:"token"`
	ExpiresAt float64 `json:"expires_at"`
}

type githubUserResponse struct {
	Login string `json:"login"`
	ID    any    `json:"id"`
}

type loginPhase uint8

const (
	loginWaitingForUser loginPhase = iota
	loginExchangingSession
)

type loginSession struct {
	mu sync.Mutex

	provider          string
	clientID          string
	githubHost        string
	deviceCode        string
	interval          time.Duration
	nextPoll          time.Time
	expiresAt         time.Time
	githubAccessToken string
	phase             loginPhase
	polling           bool
}

func (s *pluginService) parseAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		s.logEvent("", "debug", "auth.parse.ignored", map[string]any{"reason": "invalid_envelope"})
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	var marker struct {
		Type string `json:"type"`
	}
	if errUnmarshal := json.Unmarshal(req.RawJSON, &marker); errUnmarshal != nil {
		s.logEvent("", "debug", "auth.parse.ignored", map[string]any{
			"file_name": safeLogFileName(req.FileName),
			"reason":    "invalid_json",
		})
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	if strings.ToLower(strings.TrimSpace(marker.Type)) != pluginIdentifier {
		s.logEvent("", "debug", "auth.parse.ignored", map[string]any{
			"file_name": safeLogFileName(req.FileName),
			"reason":    "provider_mismatch",
		})
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	storage, errStorage := decodeCopilotStorage(req.RawJSON)
	disabledReasons := make([]string, 0, 3)
	if errStorage != nil {
		disabledReasons = append(disabledReasons, "invalid_storage")
		storage = copilotStorage{Type: pluginIdentifier, GitHubHost: s.loadedConfig().GitHubHost}
	}
	disabled := errStorage != nil
	if strings.TrimSpace(storage.GitHubAccessToken) == "" {
		disabled = true
		disabledReasons = append(disabledReasons, "missing_long_term_auth")
	}
	if storage.GitHubHost == "" {
		storage.GitHubHost = s.loadedConfig().GitHubHost
	}
	if normalized, errHost := normalizeGitHubHost(storage.GitHubHost); errHost != nil || normalized != s.loadedConfig().GitHubHost {
		disabled = true
		disabledReasons = append(disabledReasons, "github_host_mismatch")
	}
	auth := authDataFromStorage(storage, authDataDefaults{
		ID:         req.FileName,
		FileName:   req.FileName,
		Metadata:   nil,
		Attributes: nil,
		Disabled:   disabled,
	})
	if len(auth.StorageJSON) == 0 {
		auth.StorageJSON = append([]byte(nil), req.RawJSON...)
	}
	fields := authLogFields(storage, s.now().UTC())
	fields["file_name"] = safeLogFileName(req.FileName)
	fields["storage_valid"] = errStorage == nil
	fields["disabled"] = disabled
	fields["disabled_reasons"] = disabledReasons
	level := "info"
	if disabled {
		level = "warn"
	}
	s.logEvent("", level, "auth.parsed", fields)
	return okEnvelope(pluginapi.AuthParseResponse{Handled: true, Auth: auth})
}

func decodeCopilotStorage(raw []byte) (copilotStorage, error) {
	var storage copilotStorage
	if errUnmarshal := json.Unmarshal(raw, &storage); errUnmarshal != nil {
		return copilotStorage{}, fmt.Errorf("decode GitHub Copilot credential")
	}
	storage.Type = strings.ToLower(strings.TrimSpace(storage.Type))
	if storage.Type != pluginIdentifier {
		return copilotStorage{}, fmt.Errorf("credential type is not GitHub Copilot")
	}
	if storage.GitHubAccessToken == "" {
		storage.GitHubAccessToken = storage.LegacyRefresh
	}
	if storage.CopilotSessionToken == "" {
		storage.CopilotSessionToken = storage.LegacyAccess
	}
	if storage.RefreshAfter == 0 {
		storage.RefreshAfter = storage.LegacyExpires
	}
	if storage.GitHubHost == "" && storage.LegacyEnterpriseURL != "" {
		if host, errHost := normalizeGitHubHost(storage.LegacyEnterpriseURL); errHost == nil {
			storage.GitHubHost = host
		}
	}
	if storage.GitHubHost == "" {
		storage.GitHubHost = defaultGitHubHost
	}
	if len(storage.Models) == 0 && len(storage.LegacyAvailableModel) > 0 {
		for _, id := range storage.LegacyAvailableModel {
			id = strings.TrimSpace(id)
			if id != "" {
				storage.Models = append(storage.Models, storedModel{ID: id, Name: id, Format: inferModelFormat(id)})
			}
		}
	}
	storage.LegacyRefresh = ""
	storage.LegacyAccess = ""
	storage.LegacyExpires = 0
	storage.LegacyEnterpriseURL = ""
	storage.LegacyAvailableModel = nil
	return storage, nil
}

type authDataDefaults struct {
	ID         string
	FileName   string
	Label      string
	Metadata   map[string]any
	Attributes map[string]string
	Disabled   bool
}

func authDataFromStorage(storage copilotStorage, defaults authDataDefaults) pluginapi.AuthData {
	storage.Type = pluginIdentifier
	storage.GitHubAccessToken = strings.TrimSpace(storage.GitHubAccessToken)
	storage.CopilotSessionToken = strings.TrimSpace(storage.CopilotSessionToken)
	storage.Account = normalizeAccount(storage.Account)
	storage.APIBaseURL = apiBaseFromSessionToken(storage.CopilotSessionToken, storage.GitHubHost)
	storageJSON, _ := json.Marshal(storage)
	fileName := safeAuthFileName(defaults.FileName, storage.Account)
	id := strings.TrimSpace(defaults.ID)
	if id == "" {
		id = fileName
	}
	label := strings.TrimSpace(defaults.Label)
	if label == "" {
		label = "GitHub Copilot"
		if storage.Account != "" {
			label += " (" + storage.Account + ")"
		}
	}
	metadata := safeAuthMetadata(defaults.Metadata)
	metadata["type"] = pluginIdentifier
	metadata["auth_kind"] = "oauth"
	metadata["refresh_interval_seconds"] = int64(hostRefreshInterval / time.Second)
	if storage.ExpiresAt > 0 {
		metadata["expires_at"] = storage.ExpiresAt
	} else {
		delete(metadata, "expires_at")
	}
	if storage.Account != "" {
		metadata["account"] = storage.Account
	}
	metadata["github_host"] = storage.GitHubHost
	auth := pluginapi.AuthData{
		Provider:    pluginIdentifier,
		ID:          id,
		FileName:    fileName,
		Label:       label,
		Disabled:    defaults.Disabled,
		StorageJSON: storageJSON,
		Metadata:    metadata,
		Attributes:  safeAuthAttributes(defaults.Attributes),
	}
	if storage.RefreshAfter > 0 {
		auth.NextRefreshAfter = time.UnixMilli(storage.RefreshAfter).UTC()
	}
	return auth
}

func safeAuthMetadata(input map[string]any) map[string]any {
	out := make(map[string]any)
	for key, value := range input {
		lower := strings.ToLower(strings.TrimSpace(key))
		if lower == "" || sensitiveKey(lower) {
			continue
		}
		out[key] = value
	}
	return out
}

func safeAuthAttributes(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	out := make(map[string]string)
	for key, value := range input {
		if !sensitiveKey(strings.ToLower(key)) {
			out[key] = value
		}
	}
	return out
}

func sensitiveKey(key string) bool {
	for _, fragment := range []string{"token", "authorization", "credential", "storage", "raw_json", "secret", "password"} {
		if strings.Contains(key, fragment) {
			return true
		}
	}
	return false
}

func safeAuthFileName(raw, account string) string {
	name := filepath.Base(strings.TrimSpace(raw))
	if name != "." && name != "" && strings.HasSuffix(strings.ToLower(name), ".json") {
		return name
	}
	suffix := normalizeAccount(account)
	if suffix == "" {
		suffix = "account"
	}
	return "github-copilot-" + suffix + ".json"
}

func normalizeAccount(raw string) string {
	raw = strings.TrimSpace(raw)
	var out strings.Builder
	for _, r := range raw {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '-', r == '_', r == '.':
			out.WriteRune(r)
		case out.Len() > 0:
			out.WriteByte('-')
		}
		if out.Len() >= 64 {
			break
		}
	}
	return strings.Trim(out.String(), "-._")
}

func (s *pluginService) startLogin(raw []byte) ([]byte, error) {
	var req rpcAuthLoginStartRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, &pluginFailure{code: "invalid_request", message: "decode login start request"}
	}
	cfg := s.loadedConfig()
	endpoints, errEndpoints := endpointsForHost(cfg.GitHubHost)
	if errEndpoints != nil {
		return nil, &pluginFailure{code: "invalid_config", message: "invalid GitHub endpoint configuration"}
	}
	form := url.Values{"client_id": {cfg.ClientID}, "scope": {"read:user"}}
	resp, errHTTP := (hostClient{bridge: s.bridge, callbackID: req.HostCallbackID}).do(pluginapi.HTTPRequest{
		Method: http.MethodPost,
		URL:    endpoints.DeviceCodeURL,
		Headers: http.Header{
			"Accept":       []string{"application/json"},
			"Content-Type": []string{"application/x-www-form-urlencoded"},
			"User-Agent":   []string{copilotUserAgent},
		},
		Body: []byte(form.Encode()),
	})
	if errHTTP != nil {
		return nil, &pluginFailure{code: "device_flow_network_error", message: "failed to start GitHub device authorization", retryable: true, httpStatus: http.StatusBadGateway}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, upstreamFailure("device_flow_http_error", "GitHub device authorization failed", resp.StatusCode)
	}
	var device deviceCodeResponse
	if errDecode := json.Unmarshal(resp.Body, &device); errDecode != nil || strings.TrimSpace(device.DeviceCode) == "" ||
		strings.TrimSpace(device.UserCode) == "" || device.ExpiresIn <= 0 || device.ExpiresIn > 24*60*60 ||
		device.Interval < 0 || device.Interval > 5*60 || math.IsNaN(device.ExpiresIn) || math.IsInf(device.ExpiresIn, 0) {
		return nil, &pluginFailure{code: "device_flow_invalid_response", message: "GitHub device authorization returned an invalid response", httpStatus: http.StatusBadGateway}
	}
	verificationURL, errVerification := validateVerificationURL(device.VerificationURI, endpoints.GitHubHost)
	if errVerification != nil {
		return nil, &pluginFailure{code: "device_flow_invalid_response", message: errVerification.Error(), httpStatus: http.StatusBadGateway}
	}
	state, errState := randomState(s.random)
	if errState != nil {
		return nil, &pluginFailure{code: "internal_error", message: "failed to create device authorization state"}
	}
	interval := durationSeconds(device.Interval, defaultPollInterval)
	now := s.now().UTC()
	if !s.reserveLoginSessionCapacity(now) {
		return nil, &pluginFailure{code: "too_many_login_sessions", message: "too many GitHub device authorization sessions are pending", retryable: true, httpStatus: http.StatusTooManyRequests}
	}
	expiresAt := now.Add(durationSeconds(device.ExpiresIn, 15*time.Minute))
	session := &loginSession{
		provider:   pluginIdentifier,
		clientID:   cfg.ClientID,
		githubHost: endpoints.GitHubHost,
		deviceCode: strings.TrimSpace(device.DeviceCode),
		interval:   interval,
		nextPoll:   now.Add(interval),
		expiresAt:  expiresAt,
		phase:      loginWaitingForUser,
	}
	s.mu.Lock()
	s.sessions[state] = session
	s.mu.Unlock()
	s.logEvent(req.HostCallbackID, "info", "auth.login.started", map[string]any{
		"github_host":           endpoints.GitHubHost,
		"poll_interval_seconds": int(interval / time.Second),
		"expires_at":            expiresAt,
	})
	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  pluginIdentifier,
		URL:       verificationURLWithCode(verificationURL, strings.TrimSpace(device.UserCode)),
		State:     state,
		ExpiresAt: expiresAt,
		Metadata: map[string]any{
			"user_code":  strings.TrimSpace(device.UserCode),
			"interval":   int(interval / time.Second),
			"expires_at": expiresAt.Format(time.RFC3339),
		},
	})
}

func randomState(reader io.Reader) (string, error) {
	if reader == nil {
		return "", fmt.Errorf("random source is unavailable")
	}
	buf := make([]byte, 32)
	if _, errRead := io.ReadFull(reader, buf); errRead != nil {
		return "", errRead
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func durationSeconds(raw float64, fallback time.Duration) time.Duration {
	if raw <= 0 {
		return fallback
	}
	duration := time.Duration(raw * float64(time.Second))
	if duration < minPollInterval {
		return minPollInterval
	}
	return duration
}

func (s *pluginService) pollLogin(raw []byte) ([]byte, error) {
	var req rpcAuthLoginPollRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, &pluginFailure{code: "invalid_request", message: "decode login poll request"}
	}
	state := strings.TrimSpace(req.State)
	s.mu.RLock()
	session := s.sessions[state]
	s.mu.RUnlock()
	if session == nil {
		s.logEvent(req.HostCallbackID, "warn", "auth.login.failed", map[string]any{"reason": "unknown_session"})
		return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "Device authorization session is unknown or expired"})
	}
	now := s.now().UTC()
	session.mu.Lock()
	if !now.Before(session.expiresAt) {
		session.mu.Unlock()
		s.deleteSession(state)
		s.logEvent(req.HostCallbackID, "warn", "auth.login.failed", map[string]any{"reason": "expired"})
		return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "GitHub device authorization expired"})
	}
	if session.polling || now.Before(session.nextPoll) {
		nextPoll := session.nextPoll
		polling := session.polling
		session.mu.Unlock()
		s.logEvent(req.HostCallbackID, "debug", "auth.login.pending", map[string]any{
			"poll_in_progress": polling,
			"next_poll_at":     nextPoll,
		})
		return pendingLoginResponse()
	}
	session.polling = true
	phase := session.phase
	session.mu.Unlock()
	defer func() {
		session.mu.Lock()
		session.polling = false
		session.mu.Unlock()
	}()
	client := hostClient{bridge: s.bridge, callbackID: req.HostCallbackID}
	if phase == loginWaitingForUser {
		outcome, errPoll := s.pollGitHubToken(client, session)
		if errPoll != nil {
			if errPoll.retryable {
				s.reschedule(session, 0)
				s.logFailure(req.HostCallbackID, "auth.login.retry_scheduled", errPoll, nil)
				return pendingLoginResponse()
			}
			s.deleteSession(state)
			s.logFailure(req.HostCallbackID, "auth.login.failed", errPoll, nil)
			return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: errPoll.message})
		}
		if !outcome {
			s.logEvent(req.HostCallbackID, "debug", "auth.login.pending", map[string]any{"reason": "awaiting_authorization"})
			return pendingLoginResponse()
		}
	}
	storage, failure := s.completeLogin(client, session)
	if failure != nil {
		if failure.retryable {
			s.reschedule(session, 0)
			s.logFailure(req.HostCallbackID, "auth.login.retry_scheduled", failure, nil)
			return pendingLoginResponse()
		}
		s.deleteSession(state)
		s.logFailure(req.HostCallbackID, "auth.login.failed", failure, nil)
		return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: failure.message})
	}
	s.deleteSession(state)
	s.logEvent(req.HostCallbackID, "info", "auth.login.completed", authLogFields(storage, now))
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "GitHub Copilot authentication completed",
		Auth:    authDataFromStorage(storage, authDataDefaults{}),
	})
}

func pendingLoginResponse() ([]byte, error) {
	return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending, Message: "Waiting for GitHub device authorization"})
}

func (s *pluginService) pollGitHubToken(client hostClient, session *loginSession) (bool, *pluginFailure) {
	endpoints, _ := endpointsForHost(session.githubHost)
	form := url.Values{
		"client_id":   {session.clientID},
		"device_code": {session.deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	resp, errHTTP := client.do(pluginapi.HTTPRequest{
		Method: http.MethodPost,
		URL:    endpoints.AccessTokenURL,
		Headers: http.Header{
			"Accept":       []string{"application/json"},
			"Content-Type": []string{"application/x-www-form-urlencoded"},
			"User-Agent":   []string{copilotUserAgent},
		},
		Body: []byte(form.Encode()),
	})
	if errHTTP != nil {
		return false, &pluginFailure{code: "token_network_error", message: "GitHub device authorization is temporarily unavailable", retryable: true}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		failure := upstreamFailure("token_http_error", "GitHub device authorization failed", resp.StatusCode)
		return false, failure
	}
	var tokenResp deviceTokenResponse
	if errDecode := json.Unmarshal(resp.Body, &tokenResp); errDecode != nil {
		return false, &pluginFailure{code: "token_invalid_response", message: "GitHub device authorization returned an invalid response"}
	}
	if token := strings.TrimSpace(tokenResp.AccessToken); token != "" {
		session.mu.Lock()
		session.githubAccessToken = token
		session.deviceCode = ""
		session.phase = loginExchangingSession
		session.mu.Unlock()
		return true, nil
	}
	switch strings.TrimSpace(tokenResp.Error) {
	case "authorization_pending", "":
		s.reschedule(session, 0)
		return false, nil
	case "slow_down":
		interval := time.Duration(0)
		if tokenResp.Interval > 0 {
			interval = durationSeconds(tokenResp.Interval, 0)
		} else {
			session.mu.Lock()
			interval = session.interval + 5*time.Second
			session.mu.Unlock()
		}
		s.reschedule(session, interval)
		return false, nil
	case "access_denied":
		return false, &pluginFailure{code: "github_user_error", message: "GitHub device authorization was denied"}
	case "expired_token":
		return false, &pluginFailure{code: "github_user_error", message: "GitHub device authorization expired"}
	default:
		return false, &pluginFailure{code: "github_user_error", message: "GitHub device authorization failed"}
	}
}

func (s *pluginService) reschedule(session *loginSession, interval time.Duration) {
	session.mu.Lock()
	if interval > 0 {
		session.interval = interval
	}
	session.nextPoll = s.now().UTC().Add(session.interval)
	session.mu.Unlock()
}

func (s *pluginService) deleteSession(state string) {
	s.mu.Lock()
	session := s.sessions[state]
	delete(s.sessions, state)
	s.mu.Unlock()
	clearLoginSessionSecrets(session)
}

func (s *pluginService) reserveLoginSessionCapacity(now time.Time) bool {
	var expired []*loginSession
	s.mu.Lock()
	for state, session := range s.sessions {
		if session == nil {
			delete(s.sessions, state)
			continue
		}
		session.mu.Lock()
		isExpired := !now.Before(session.expiresAt)
		session.mu.Unlock()
		if isExpired {
			delete(s.sessions, state)
			expired = append(expired, session)
		}
	}
	available := len(s.sessions) < 256
	s.mu.Unlock()
	for _, session := range expired {
		clearLoginSessionSecrets(session)
	}
	return available
}

func clearLoginSessionSecrets(session *loginSession) {
	if session == nil {
		return
	}
	session.mu.Lock()
	session.deviceCode = ""
	session.githubAccessToken = ""
	session.mu.Unlock()
}

func (s *pluginService) completeLogin(client hostClient, session *loginSession) (copilotStorage, *pluginFailure) {
	session.mu.Lock()
	githubToken := session.githubAccessToken
	githubHost := session.githubHost
	session.mu.Unlock()
	storage, failure := s.exchangeSession(client, githubToken, githubHost)
	if failure != nil {
		return copilotStorage{}, failure
	}
	if s.loadedConfig().EnableModels {
		s.enableKnownModels(client, storage)
	}
	models, failure := s.discoverModels(client, storage)
	if failure != nil {
		return copilotStorage{}, failure
	}
	storage.Models = models
	storage.ModelsFetchedAt = s.now().UTC().UnixMilli()
	storage.Account = s.fetchGitHubAccount(client, githubToken, githubHost)
	return storage, nil
}

func (s *pluginService) exchangeSession(client hostClient, githubToken, githubHost string) (copilotStorage, *pluginFailure) {
	githubToken = strings.TrimSpace(githubToken)
	if githubToken == "" {
		return copilotStorage{}, &pluginFailure{code: "reauth_required", message: "GitHub Copilot credential requires login", httpStatus: http.StatusUnauthorized}
	}
	endpoints, errEndpoints := endpointsForHost(githubHost)
	if errEndpoints != nil || endpoints.GitHubHost != s.loadedConfig().GitHubHost {
		return copilotStorage{}, &pluginFailure{code: "invalid_auth", message: "GitHub Copilot credential host does not match plugin configuration"}
	}
	resp, errHTTP := client.do(pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     endpoints.CopilotTokenURL,
		Headers: brokerHeaders(githubToken),
	})
	if errHTTP != nil {
		return copilotStorage{}, &pluginFailure{code: "token_network_error", message: "GitHub Copilot token service is temporarily unavailable", retryable: true}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return copilotStorage{}, upstreamFailure("token_http_error", "GitHub Copilot token exchange failed", resp.StatusCode)
	}
	var tokenResp copilotTokenResponse
	if errDecode := json.Unmarshal(resp.Body, &tokenResp); errDecode != nil || strings.TrimSpace(tokenResp.Token) == "" ||
		tokenResp.ExpiresAt <= 0 || math.IsNaN(tokenResp.ExpiresAt) || math.IsInf(tokenResp.ExpiresAt, 0) {
		return copilotStorage{}, &pluginFailure{code: "token_invalid_response", message: "GitHub Copilot token service returned an invalid response", httpStatus: http.StatusBadGateway}
	}
	now := s.now().UTC()
	if tokenResp.ExpiresAt <= float64(now.Unix()) || tokenResp.ExpiresAt > float64(now.Add(7*24*time.Hour).Unix()) {
		return copilotStorage{}, &pluginFailure{code: "token_invalid_response", message: "GitHub Copilot token service returned an invalid expiry", httpStatus: http.StatusBadGateway}
	}
	expiresAt := time.Unix(int64(tokenResp.ExpiresAt), 0).UTC()
	if !expiresAt.After(now) {
		return copilotStorage{}, &pluginFailure{code: "token_invalid_response", message: "GitHub Copilot token service returned an expired session", httpStatus: http.StatusBadGateway}
	}
	refreshAt := expiresAt.Add(-refreshSafetyMargin)
	if refreshAt.Before(now.Add(30 * time.Second)) {
		refreshAt = now.Add(30 * time.Second)
		if !refreshAt.Before(expiresAt) {
			refreshAt = now
		}
	}
	token := strings.TrimSpace(tokenResp.Token)
	storage := copilotStorage{
		Type:                pluginIdentifier,
		GitHubAccessToken:   githubToken,
		CopilotSessionToken: token,
		RefreshAfter:        refreshAt.UnixMilli(),
		ExpiresAt:           expiresAt.UnixMilli(),
		GitHubHost:          endpoints.GitHubHost,
		APIBaseURL:          apiBaseFromSessionToken(token, endpoints.GitHubHost),
	}
	s.logEvent(client.callbackID, "info", "auth.session.exchanged", authLogFields(storage, now))
	return storage, nil
}

func (s *pluginService) fetchGitHubAccount(client hostClient, githubToken, githubHost string) string {
	endpoints, errEndpoints := endpointsForHost(githubHost)
	if errEndpoints != nil {
		return ""
	}
	resp, errHTTP := client.do(pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    endpoints.GitHubUserURL,
		Headers: http.Header{
			"Accept":        []string{"application/vnd.github+json"},
			"Authorization": []string{"Bearer " + githubToken},
			"User-Agent":    []string{copilotUserAgent},
		},
	})
	if errHTTP != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	var user githubUserResponse
	if json.Unmarshal(resp.Body, &user) != nil {
		return ""
	}
	if login := normalizeAccount(user.Login); login != "" {
		return login
	}
	switch id := user.ID.(type) {
	case float64:
		return "user-" + strconv.FormatInt(int64(id), 10)
	case string:
		return "user-" + normalizeAccount(id)
	default:
		return ""
	}
}

func (s *pluginService) refreshAuth(raw []byte) ([]byte, error) {
	var req rpcAuthRefreshRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, &pluginFailure{code: "invalid_request", message: "decode auth refresh request"}
	}
	previous, errStorage := decodeCopilotStorage(req.StorageJSON)
	if errStorage != nil {
		s.logFailure(req.HostCallbackID, "auth.refresh.failed", &pluginFailure{code: "invalid_auth", httpStatus: http.StatusUnauthorized}, map[string]any{
			"auth_id": req.AuthID,
		})
		return nil, &pluginFailure{code: "invalid_auth", message: "GitHub Copilot credential is invalid", httpStatus: http.StatusUnauthorized}
	}
	startFields := authLogFields(previous, s.now().UTC())
	startFields["auth_id"] = req.AuthID
	s.logEvent(req.HostCallbackID, "info", "auth.refresh.started", startFields)
	client := hostClient{bridge: s.bridge, callbackID: req.HostCallbackID}
	next, failure := s.exchangeSession(client, previous.GitHubAccessToken, previous.GitHubHost)
	if failure != nil {
		s.logFailure(req.HostCallbackID, "auth.refresh.failed", failure, map[string]any{"auth_id": req.AuthID, "stage": "session_exchange"})
		return nil, failure
	}
	models, failure := s.discoverModels(client, next)
	if failure != nil {
		s.logFailure(req.HostCallbackID, "auth.refresh.failed", failure, map[string]any{"auth_id": req.AuthID, "stage": "model_discovery"})
		return nil, failure
	}
	next.Models = models
	next.ModelsFetchedAt = s.now().UTC().UnixMilli()
	next.Account = previous.Account
	auth := authDataFromStorage(next, authDataDefaults{
		ID:         req.AuthID,
		FileName:   fileNameFromAttributes(req.Attributes, req.AuthID),
		Metadata:   req.Metadata,
		Attributes: req.Attributes,
	})
	completedFields := authLogFields(next, s.now().UTC())
	completedFields["auth_id"] = req.AuthID
	s.logEvent(req.HostCallbackID, "info", "auth.refresh.completed", completedFields)
	return okEnvelope(pluginapi.AuthRefreshResponse{Auth: auth, NextRefreshAfter: auth.NextRefreshAfter})
}

func fileNameFromAttributes(attributes map[string]string, fallback string) string {
	if path := strings.TrimSpace(attributes["path"]); path != "" {
		return filepath.Base(path)
	}
	return filepath.Base(strings.TrimSpace(fallback))
}

func upstreamFailure(code, message string, status int) *pluginFailure {
	retryable := status == http.StatusTooManyRequests || status >= 500
	if status == 0 {
		status = http.StatusBadGateway
	}
	return &pluginFailure{code: code, message: message + " with status " + strconv.Itoa(status), retryable: retryable, httpStatus: status}
}

func drainAndClose(_ io.Reader) {}
