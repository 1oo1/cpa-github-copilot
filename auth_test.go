package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestParseAuthHandledContract(t *testing.T) {
	service := newPluginService(nil)
	for _, test := range []struct {
		name         string
		raw          string
		wantHandled  bool
		wantDisabled bool
	}{
		{name: "malformed", raw: `{`, wantHandled: false},
		{name: "unrelated", raw: `{"type":"other","token":"x"}`, wantHandled: false},
		{name: "recognized incomplete", raw: `{"type":"github-copilot"}`, wantHandled: true, wantDisabled: true},
		{name: "recognized valid", raw: `{"type":"github-copilot","github_access_token":"github-secret","github_host":"github.com"}`, wantHandled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, errParse := service.parseAuth(mustJSON(t, pluginapi.AuthParseRequest{FileName: "copilot.json", RawJSON: []byte(test.raw)}))
			if errParse != nil {
				t.Fatalf("parseAuth error: %v", errParse)
			}
			result := decodePluginResult[pluginapi.AuthParseResponse](t, raw)
			if result.Handled != test.wantHandled {
				t.Fatalf("Handled = %v, want %v", result.Handled, test.wantHandled)
			}
			if result.Handled && result.Auth.Disabled != test.wantDisabled {
				t.Fatalf("Disabled = %v, want %v", result.Auth.Disabled, test.wantDisabled)
			}
		})
	}
}

func TestStartLoginRejectsUntrustedVerificationURL(t *testing.T) {
	bridge := &fakeBridge{handler: func(_ string, _ any) (any, error) {
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{
			"device_code":"device-secret",
			"user_code":"CODE",
			"verification_uri":"https://attacker.example/device",
			"interval":5,
			"expires_in":900
		}`)}, nil
	}}
	service := newPluginService(bridge)
	_, failure := service.startLogin(mustJSON(t, rpcAuthLoginStartRequest{HostCallbackID: "callback"}))
	if failure == nil || !strings.Contains(failure.Error(), "untrusted") {
		t.Fatalf("failure = %v", failure)
	}
}

func TestParseAuthKeepsSecretsOnlyInStorageJSON(t *testing.T) {
	const githubSecret = "ghu_SENTINEL_GITHUB_SECRET"
	const sessionSecret = "tid=SENTINEL_SESSION_SECRET;proxy-ep=proxy.individual.githubcopilot.com"
	service := newPluginService(nil)
	raw, errParse := service.parseAuth(mustJSON(t, pluginapi.AuthParseRequest{
		FileName: "copilot.json",
		RawJSON: mustJSON(t, map[string]any{
			"type":                  pluginIdentifier,
			"github_access_token":   githubSecret,
			"copilot_session_token": sessionSecret,
			"github_host":           "github.com",
			"refresh_after":         time.Now().Add(time.Hour).UnixMilli(),
		}),
	}))
	if errParse != nil {
		t.Fatal(errParse)
	}
	result := decodePluginResult[pluginapi.AuthParseResponse](t, raw)
	metadata := string(mustJSON(t, result.Auth.Metadata))
	attributes := string(mustJSON(t, result.Auth.Attributes))
	if strings.Contains(metadata, "SENTINEL") || strings.Contains(attributes, "SENTINEL") || strings.Contains(result.Auth.Label, "SENTINEL") {
		t.Fatalf("secret escaped StorageJSON: metadata=%s attributes=%s label=%s", metadata, attributes, result.Auth.Label)
	}
	if !bytes.Contains(result.Auth.StorageJSON, []byte(githubSecret)) || !bytes.Contains(result.Auth.StorageJSON, []byte(sessionSecret)) {
		t.Fatal("normalized StorageJSON did not retain required credentials")
	}
}

func TestDecodeLegacyPiCredential(t *testing.T) {
	storage, errDecode := decodeCopilotStorage([]byte(`{
		"type":"github-copilot",
		"refresh":"ghu_legacy",
		"access":"tid=legacy;proxy-ep=proxy.business.githubcopilot.com",
		"expires":1999999999000,
		"availableModelIds":["gpt-5.4","claude-sonnet-4.6"]
	}`))
	if errDecode != nil {
		t.Fatal(errDecode)
	}
	if storage.GitHubAccessToken != "ghu_legacy" || !strings.HasPrefix(storage.CopilotSessionToken, "tid=legacy") || storage.RefreshAfter != 1999999999000 {
		t.Fatalf("legacy credential not normalized: %#v", storage)
	}
	if len(storage.Models) != 2 || storage.Models[0].Format != formatOpenAIResponse || storage.Models[1].Format != formatClaude {
		t.Fatalf("legacy models not normalized: %#v", storage.Models)
	}
}

func TestStartLoginUsesHostBridgeAndValidatesResponse(t *testing.T) {
	bridge := &fakeBridge{}
	bridge.handler = func(method string, payload any) (any, error) {
		if method != pluginabi.MethodHostHTTPDo {
			t.Fatalf("method = %s", method)
		}
		req := payload.(rpcHostHTTPRequest)
		if req.URL != "https://github.com/login/device/code" || req.HostCallbackID != "callback-1" {
			t.Fatalf("host request = %#v", req)
		}
		values, errParse := url.ParseQuery(string(req.Body))
		if errParse != nil || values.Get("client_id") != defaultClientID || values.Get("scope") != "read:user" {
			t.Fatalf("device form = %q, %v", req.Body, errParse)
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{
			"device_code":"device-secret",
			"user_code":"ABCD-EFGH",
			"verification_uri":"https://github.com/login/device",
			"interval":5,
			"expires_in":900
		}`)}, nil
	}
	service := newPluginService(bridge)
	service.now = func() time.Time { return time.Unix(1000, 0).UTC() }
	service.random = bytes.NewReader(bytes.Repeat([]byte{7}, 32))
	raw, errStart := service.startLogin(mustJSON(t, rpcAuthLoginStartRequest{HostCallbackID: "callback-1"}))
	if errStart != nil {
		t.Fatal(errStart)
	}
	result := decodePluginResult[pluginapi.AuthLoginStartResponse](t, raw)
	if result.State == "" || !strings.Contains(result.URL, "user_code=ABCD-EFGH") {
		t.Fatalf("login response = %#v", result)
	}
	if strings.Contains(result.State, "device-secret") {
		t.Fatal("device code leaked into OAuth state")
	}
}

func TestDeviceFlowPollTimingSlowDownAndSuccess(t *testing.T) {
	now := time.Unix(10_000, 0).UTC()
	accessResponses := [][]byte{
		[]byte(`{"error":"authorization_pending"}`),
		[]byte(`{"error":"slow_down","interval":7}`),
		[]byte(`{"access_token":"ghu_LONG_TERM_SENTINEL"}`),
	}
	accessCalls := 0
	bridge := &fakeBridge{}
	bridge.handler = func(method string, payload any) (any, error) {
		if method != pluginabi.MethodHostHTTPDo {
			t.Fatalf("method = %s", method)
		}
		req := payload.(rpcHostHTTPRequest)
		switch {
		case strings.HasSuffix(req.URL, "/login/device/code"):
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"device_code":"device","user_code":"CODE","verification_uri":"https://github.com/login/device","interval":5,"expires_in":900}`)}, nil
		case strings.HasSuffix(req.URL, "/login/oauth/access_token"):
			body := accessResponses[accessCalls]
			accessCalls++
			return pluginapi.HTTPResponse{StatusCode: 200, Body: body}, nil
		case strings.Contains(req.URL, "/copilot_internal/v2/token"):
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"token":"tid=SESSION_SENTINEL;proxy-ep=proxy.individual.githubcopilot.com","expires_at":20000}`)}, nil
		case strings.HasSuffix(req.URL, "/models"):
			return pluginapi.HTTPResponse{StatusCode: 200, Body: selectableModelsJSON("gpt-5.4", []string{"/responses"})}, nil
		case strings.HasSuffix(req.URL, "/user"):
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"login":"octocat","id":1}`)}, nil
		default:
			t.Fatalf("unexpected URL %s", req.URL)
			return nil, nil
		}
	}
	service := newPluginService(bridge)
	service.now = func() time.Time { return now }
	service.random = bytes.NewReader(bytes.Repeat([]byte{9}, 32))
	service.mu.Lock()
	service.config.EnableModels = false
	service.mu.Unlock()
	startRaw, errStart := service.startLogin(mustJSON(t, rpcAuthLoginStartRequest{HostCallbackID: "start"}))
	if errStart != nil {
		t.Fatal(errStart)
	}
	state := decodePluginResult[pluginapi.AuthLoginStartResponse](t, startRaw).State
	poll := func() pluginapi.AuthLoginPollResponse {
		raw, errPoll := service.pollLogin(mustJSON(t, rpcAuthLoginPollRequest{
			AuthLoginPollRequest: pluginapi.AuthLoginPollRequest{State: state},
			HostCallbackID:       "poll",
		}))
		if errPoll != nil {
			t.Fatal(errPoll)
		}
		return decodePluginResult[pluginapi.AuthLoginPollResponse](t, raw)
	}
	if got := poll(); got.Status != pluginapi.AuthLoginStatusPending || accessCalls != 0 {
		t.Fatalf("immediate poll = %#v, access calls=%d", got, accessCalls)
	}
	now = now.Add(5 * time.Second)
	if got := poll(); got.Status != pluginapi.AuthLoginStatusPending || accessCalls != 1 {
		t.Fatalf("first poll = %#v, access calls=%d", got, accessCalls)
	}
	now = now.Add(5 * time.Second)
	if got := poll(); got.Status != pluginapi.AuthLoginStatusPending || accessCalls != 2 {
		t.Fatalf("slow-down poll = %#v, access calls=%d", got, accessCalls)
	}
	now = now.Add(6 * time.Second)
	if got := poll(); got.Status != pluginapi.AuthLoginStatusPending || accessCalls != 2 {
		t.Fatalf("early slow-down poll = %#v, access calls=%d", got, accessCalls)
	}
	now = now.Add(time.Second)
	got := poll()
	if got.Status != pluginapi.AuthLoginStatusSuccess || accessCalls != 3 || got.Auth.Metadata["account"] != "octocat" {
		t.Fatalf("completed poll = %#v, access calls=%d", got, accessCalls)
	}
	storage, errStorage := decodeCopilotStorage(got.Auth.StorageJSON)
	if errStorage != nil || storage.GitHubAccessToken != "ghu_LONG_TERM_SENTINEL" || len(storage.Models) != 1 {
		t.Fatalf("completed storage = %#v, %v", storage, errStorage)
	}
	if strings.Contains(string(mustJSON(t, got.Auth.Metadata)), "SENTINEL") {
		t.Fatal("token leaked into auth metadata")
	}
}

func TestSlowDownWithoutIntervalAddsFiveSeconds(t *testing.T) {
	now := time.Unix(15_000, 0).UTC()
	bridge := &fakeBridge{handler: func(_ string, _ any) (any, error) {
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"error":"slow_down"}`)}, nil
	}}
	service := newPluginService(bridge)
	service.now = func() time.Time { return now }
	session := &loginSession{clientID: defaultClientID, githubHost: "github.com", deviceCode: "device", interval: 5 * time.Second}
	complete, failure := service.pollGitHubToken(hostClient{bridge: bridge}, session)
	if complete || failure != nil {
		t.Fatalf("complete=%v failure=%v", complete, failure)
	}
	session.mu.Lock()
	interval, nextPoll := session.interval, session.nextPoll
	session.mu.Unlock()
	if interval != 10*time.Second || !nextPoll.Equal(now.Add(10*time.Second)) {
		t.Fatalf("interval=%s next=%s", interval, nextPoll)
	}
}

func TestConcurrentLoginPollIsDeduplicated(t *testing.T) {
	now := time.Unix(16_000, 0).UTC()
	entered := make(chan struct{})
	release := make(chan struct{})
	bridge := &fakeBridge{handler: func(_ string, _ any) (any, error) {
		close(entered)
		<-release
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"error":"authorization_pending"}`)}, nil
	}}
	service := newPluginService(bridge)
	service.now = func() time.Time { return now }
	state := "deduplicated-state"
	service.sessions[state] = &loginSession{
		provider: pluginIdentifier, clientID: defaultClientID, githubHost: "github.com", deviceCode: "device",
		interval: time.Second, nextPoll: now, expiresAt: now.Add(time.Minute),
	}
	firstDone := make(chan error, 1)
	go func() {
		_, errPoll := service.pollLogin(mustJSON(t, rpcAuthLoginPollRequest{AuthLoginPollRequest: pluginapi.AuthLoginPollRequest{State: state}}))
		firstDone <- errPoll
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first poll did not enter host bridge")
	}
	raw, errSecond := service.pollLogin(mustJSON(t, rpcAuthLoginPollRequest{AuthLoginPollRequest: pluginapi.AuthLoginPollRequest{State: state}}))
	if errSecond != nil || decodePluginResult[pluginapi.AuthLoginPollResponse](t, raw).Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("second poll error=%v raw=%s", errSecond, raw)
	}
	if len(bridge.snapshot()) != 1 {
		t.Fatalf("host calls = %d, want one", len(bridge.snapshot()))
	}
	close(release)
	if errFirst := <-firstDone; errFirst != nil {
		t.Fatal(errFirst)
	}
}

func TestLoginPollExpiryClearsTemporarySecrets(t *testing.T) {
	now := time.Unix(17_000, 0).UTC()
	service := newPluginService(nil)
	service.now = func() time.Time { return now }
	session := &loginSession{
		deviceCode: "device-secret", githubAccessToken: "github-secret", expiresAt: now,
	}
	service.sessions["expired"] = session
	raw, errPoll := service.pollLogin(mustJSON(t, rpcAuthLoginPollRequest{AuthLoginPollRequest: pluginapi.AuthLoginPollRequest{State: "expired"}}))
	if errPoll != nil {
		t.Fatal(errPoll)
	}
	result := decodePluginResult[pluginapi.AuthLoginPollResponse](t, raw)
	if result.Status != pluginapi.AuthLoginStatusError || !strings.Contains(strings.ToLower(result.Message), "expired") {
		t.Fatalf("poll result = %#v", result)
	}
	session.mu.Lock()
	deviceCode, githubToken := session.deviceCode, session.githubAccessToken
	session.mu.Unlock()
	if deviceCode != "" || githubToken != "" {
		t.Fatal("expired login session retained temporary credentials")
	}
}

func TestLoginPollDenialIsTerminal(t *testing.T) {
	now := time.Unix(18_000, 0).UTC()
	bridge := &fakeBridge{handler: func(_ string, _ any) (any, error) {
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"error":"access_denied","error_description":"private detail"}`)}, nil
	}}
	service := newPluginService(bridge)
	service.now = func() time.Time { return now }
	service.sessions["denied"] = &loginSession{
		clientID: defaultClientID, githubHost: "github.com", deviceCode: "device",
		interval: time.Second, nextPoll: now, expiresAt: now.Add(time.Minute),
	}
	raw, errPoll := service.pollLogin(mustJSON(t, rpcAuthLoginPollRequest{AuthLoginPollRequest: pluginapi.AuthLoginPollRequest{State: "denied"}}))
	if errPoll != nil {
		t.Fatal(errPoll)
	}
	result := decodePluginResult[pluginapi.AuthLoginPollResponse](t, raw)
	if result.Status != pluginapi.AuthLoginStatusError || !strings.Contains(strings.ToLower(result.Message), "denied") || strings.Contains(result.Message, "private detail") {
		t.Fatalf("poll result = %#v", result)
	}
	service.mu.RLock()
	_, exists := service.sessions["denied"]
	service.mu.RUnlock()
	if exists {
		t.Fatal("denied session was not removed")
	}
}

func TestLoginPollRetriesHostBridgeFailure(t *testing.T) {
	now := time.Unix(19_000, 0).UTC()
	bridge := &fakeBridge{handler: func(_ string, _ any) (any, error) {
		return nil, fmt.Errorf("temporary transport failure")
	}}
	service := newPluginService(bridge)
	service.now = func() time.Time { return now }
	service.sessions["retry"] = &loginSession{
		clientID: defaultClientID, githubHost: "github.com", deviceCode: "device",
		interval: 5 * time.Second, nextPoll: now, expiresAt: now.Add(time.Minute),
	}
	raw, errPoll := service.pollLogin(mustJSON(t, rpcAuthLoginPollRequest{AuthLoginPollRequest: pluginapi.AuthLoginPollRequest{State: "retry"}}))
	if errPoll != nil {
		t.Fatal(errPoll)
	}
	result := decodePluginResult[pluginapi.AuthLoginPollResponse](t, raw)
	if result.Status != pluginapi.AuthLoginStatusPending || strings.Contains(result.Message, "transport") {
		t.Fatalf("poll result = %#v", result)
	}
}

func TestRefreshAuthReplacesOnlyCompleteSession(t *testing.T) {
	now := time.Unix(20_000, 0).UTC()
	bridge := &fakeBridge{}
	bridge.handler = func(_ string, payload any) (any, error) {
		req := payload.(rpcHostHTTPRequest)
		switch {
		case strings.Contains(req.URL, "/copilot_internal/v2/token"):
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"token":"tid=NEW_SESSION;proxy-ep=proxy.business.githubcopilot.com","expires_at":30000}`)}, nil
		case strings.HasSuffix(req.URL, "/models"):
			return pluginapi.HTTPResponse{StatusCode: 200, Body: selectableModelsJSON("claude-sonnet-4.6", []string{"/v1/messages"})}, nil
		default:
			t.Fatalf("unexpected URL %s", req.URL)
			return nil, nil
		}
	}
	service := newPluginService(bridge)
	service.now = func() time.Time { return now }
	previous := copilotStorage{Type: pluginIdentifier, GitHubAccessToken: "ghu_OLD", CopilotSessionToken: "old", GitHubHost: "github.com", Account: "octocat"}
	raw, errRefresh := service.refreshAuth(mustJSON(t, rpcAuthRefreshRequest{
		AuthRefreshRequest: pluginapi.AuthRefreshRequest{
			AuthID:      "copilot.json",
			StorageJSON: mustJSON(t, previous),
			Metadata:    map[string]any{"note": "keep", "access_token": "MUST_DROP"},
		},
		HostCallbackID: "refresh",
	}))
	if errRefresh != nil {
		t.Fatal(errRefresh)
	}
	result := decodePluginResult[pluginapi.AuthRefreshResponse](t, raw)
	next, errStorage := decodeCopilotStorage(result.Auth.StorageJSON)
	if errStorage != nil || !strings.Contains(next.CopilotSessionToken, "NEW_SESSION") || next.Account != "octocat" || len(next.Models) != 1 {
		t.Fatalf("refreshed storage = %#v, %v", next, errStorage)
	}
	if _, leaked := result.Auth.Metadata["access_token"]; leaked || result.Auth.Metadata["note"] != "keep" {
		t.Fatalf("refreshed metadata = %#v", result.Auth.Metadata)
	}
}

func TestRefreshAuthReturnsNoPartialCredentialWhenDiscoveryFails(t *testing.T) {
	now := time.Unix(24_000, 0).UTC()
	bridge := &fakeBridge{handler: func(_ string, payload any) (any, error) {
		req := payload.(rpcHostHTTPRequest)
		switch {
		case strings.Contains(req.URL, "/copilot_internal/v2/token"):
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"token":"tid=NEW;proxy-ep=proxy.individual.githubcopilot.com","expires_at":25000}`)}, nil
		case strings.HasSuffix(req.URL, "/models"):
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"unexpected":[]}`)}, nil
		default:
			return nil, fmt.Errorf("unexpected URL")
		}
	}}
	service := newPluginService(bridge)
	service.now = func() time.Time { return now }
	previous := copilotStorage{
		Type: pluginIdentifier, GitHubAccessToken: "ghu_OLD", CopilotSessionToken: "tid=OLD",
		GitHubHost: "github.com", Models: []storedModel{{ID: "gpt-4.1", Format: formatOpenAI}},
	}
	response, failure := service.refreshAuth(mustJSON(t, rpcAuthRefreshRequest{
		AuthRefreshRequest: pluginapi.AuthRefreshRequest{AuthID: "auth", StorageJSON: mustJSON(t, previous)},
		HostCallbackID:     "refresh",
	}))
	if failure == nil || response != nil || !strings.Contains(failure.Error(), "invalid model catalog") {
		t.Fatalf("response=%s failure=%v", response, failure)
	}
	if previous.CopilotSessionToken != "tid=OLD" || len(previous.Models) != 1 {
		t.Fatalf("previous credential was mutated: %#v", previous)
	}
}

func TestExchangeSessionExpiryMarginAndMissingFields(t *testing.T) {
	now := time.Unix(25_000, 0).UTC()
	responses := [][]byte{
		[]byte(`{"expires_at":26000}`),
		[]byte(`{"token":"tid=x;proxy-ep=proxy.individual.githubcopilot.com","expires_at":26000}`),
	}
	bridge := &fakeBridge{handler: func(_ string, _ any) (any, error) {
		body := responses[0]
		responses = responses[1:]
		return pluginapi.HTTPResponse{StatusCode: 200, Body: body}, nil
	}}
	service := newPluginService(bridge)
	service.now = func() time.Time { return now }
	if _, failure := service.exchangeSession(hostClient{bridge: bridge}, "ghu", "github.com"); failure == nil || strings.Contains(failure.Error(), "ghu") {
		t.Fatalf("missing-token failure = %v", failure)
	}
	storage, failure := service.exchangeSession(hostClient{bridge: bridge}, "ghu", "github.com")
	if failure != nil {
		t.Fatal(failure)
	}
	wantRefresh := time.Unix(26000, 0).Add(-refreshSafetyMargin).UnixMilli()
	if storage.RefreshAfter != wantRefresh {
		t.Fatalf("refresh_after = %d, want %d", storage.RefreshAfter, wantRefresh)
	}
}

func TestTokenErrorsDoNotContainResponseBodyOrCredentials(t *testing.T) {
	const sentinel = "SENTINEL_SHOULD_NEVER_APPEAR"
	bridge := &fakeBridge{handler: func(_ string, _ any) (any, error) {
		return pluginapi.HTTPResponse{StatusCode: http.StatusUnauthorized, Body: []byte(`{"token":"` + sentinel + `"}`)}, nil
	}}
	service := newPluginService(bridge)
	_, failure := service.exchangeSession(hostClient{bridge: bridge}, "ghu_"+sentinel, "github.com")
	if failure == nil || strings.Contains(failure.Error(), sentinel) {
		t.Fatalf("failure = %v", failure)
	}
}

func selectableModelsJSON(id string, endpoints []string) []byte {
	return mustJSONWithoutTest(map[string]any{"data": []any{map[string]any{
		"id":                   id,
		"name":                 id,
		"version":              id + "-2026-01-01",
		"model_picker_enabled": true,
		"supported_endpoints":  endpoints,
		"capabilities": map[string]any{
			"family":   "test",
			"limits":   map[string]any{"max_context_window_tokens": 100000, "max_prompt_tokens": 90000, "max_output_tokens": 10000},
			"supports": map[string]any{"tool_calls": true, "streaming": true},
		},
	}}})
}

func mustJSONWithoutTest(value any) []byte {
	raw, _ := json.Marshal(value)
	return raw
}
