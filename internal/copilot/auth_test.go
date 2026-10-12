package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	contract "github.com/1oo1/cpa-github-copilot/internal/contract"
)

func TestParseAuthHandledContract(t *testing.T) {
	service := newService(nil)
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
			result, errParse := service.parseAuthRequest(contract.AuthParseRequest{FileName: "copilot.json", RawJSON: []byte(test.raw)})
			if errParse != nil {
				t.Fatalf("parseAuth error: %v", errParse)
			}
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
	fake := newFakeHost(t, func(hostHTTPCall) (contract.HTTPResponse, error) {
		return contract.HTTPResponse{StatusCode: 200, Body: []byte(`{
			"device_code":"device-secret",
			"user_code":"CODE",
			"verification_uri":"https://attacker.example/device",
			"interval":5,
			"expires_in":900
		}`)}, nil
	})
	service := newService(fake)
	_, failure := service.startLoginRequest(context.Background(), "callback", contract.AuthLoginStartRequest{})
	if failure == nil || !strings.Contains(failure.Error(), "untrusted") {
		t.Fatalf("failure = %v", failure)
	}
}

func TestParseAuthKeepsSecretsOnlyInStorageJSON(t *testing.T) {
	const githubSecret = "ghu_SENTINEL_GITHUB_SECRET"
	const sessionSecret = "tid=SENTINEL_SESSION_SECRET;proxy-ep=proxy.individual.githubcopilot.com"
	service := newService(nil)
	result, errParse := service.parseAuthRequest(contract.AuthParseRequest{
		FileName: "copilot.json",
		RawJSON: mustJSON(t, map[string]any{
			"type":                  providerIdentifier,
			"github_access_token":   githubSecret,
			"copilot_session_token": sessionSecret,
			"github_host":           "github.com",
			"refresh_after":         time.Now().Add(time.Hour).UnixMilli(),
		}),
	})
	if errParse != nil {
		t.Fatal(errParse)
	}
	metadata := string(mustJSON(t, result.Auth.Metadata))
	attributes := string(mustJSON(t, result.Auth.Attributes))
	if strings.Contains(metadata, "SENTINEL") || strings.Contains(attributes, "SENTINEL") || strings.Contains(result.Auth.Label, "SENTINEL") {
		t.Fatalf("secret escaped StorageJSON: metadata=%s attributes=%s label=%s", metadata, attributes, result.Auth.Label)
	}
	if !bytes.Contains(result.Auth.StorageJSON, []byte(githubSecret)) || !bytes.Contains(result.Auth.StorageJSON, []byte(sessionSecret)) {
		t.Fatal("normalized StorageJSON did not retain required credentials")
	}
}

func TestParseAuthExposesPersistedPriorityToScheduler(t *testing.T) {
	service := newService(nil)
	result, errParse := service.parseAuthRequest(contract.AuthParseRequest{
		FileName: "copilot.json",
		RawJSON: mustJSON(t, map[string]any{
			"type":                providerIdentifier,
			"priority":            7,
			"github_access_token": "ghu_test",
			"github_host":         "github.com",
		}),
	})
	if errParse != nil {
		t.Fatal(errParse)
	}
	if result.Auth.Attributes["priority"] != "7" {
		t.Fatalf("priority attribute = %q, want 7", result.Auth.Attributes["priority"])
	}
	var persisted struct {
		Priority int `json:"priority"`
	}
	if errUnmarshal := json.Unmarshal(result.Auth.StorageJSON, &persisted); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if persisted.Priority != 7 {
		t.Fatalf("persisted priority = %d, want 7", persisted.Priority)
	}
}

func TestDecodeLegacyPiCredential(t *testing.T) {
	storage, errDecode := decodeCopilotStorage([]byte(`{
		"type":"github-copilot",
		"refresh":"ghu_legacy",
		"access":"tid=legacy;proxy-ep=proxy.business.githubcopilot.com",
		"expires":1999999999000,
		"availableModelIds":["gpt-6","claude-sonnet-5-1"]
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

func TestStartLoginUsesHostRuntimeAndValidatesResponse(t *testing.T) {
	fake := newFakeHost(t, func(req hostHTTPCall) (contract.HTTPResponse, error) {
		if req.URL != "https://github.com/login/device/code" || req.CallbackID != "callback-1" {
			t.Fatalf("host request = %#v", req)
		}
		values, errParse := url.ParseQuery(string(req.Body))
		if errParse != nil || values.Get("client_id") != defaultClientID || values.Get("scope") != "read:user" {
			t.Fatalf("device form = %q, %v", req.Body, errParse)
		}
		return contract.HTTPResponse{StatusCode: 200, Body: []byte(`{
			"device_code":"device-secret",
			"user_code":"ABCD-EFGH",
			"verification_uri":"https://github.com/login/device",
			"interval":5,
			"expires_in":900
		}`)}, nil
	})
	service := newService(fake)
	service.now = func() time.Time { return time.Unix(1000, 0).UTC() }
	service.random = bytes.NewReader(bytes.Repeat([]byte{7}, 32))
	result, errStart := service.startLoginRequest(context.Background(), "callback-1", contract.AuthLoginStartRequest{})
	if errStart != nil {
		t.Fatal(errStart)
	}
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
	fake := newFakeHost(t, func(req hostHTTPCall) (contract.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(req.URL, "/login/device/code"):
			return contract.HTTPResponse{StatusCode: 200, Body: []byte(`{"device_code":"device","user_code":"CODE","verification_uri":"https://github.com/login/device","interval":5,"expires_in":900}`)}, nil
		case strings.HasSuffix(req.URL, "/login/oauth/access_token"):
			body := accessResponses[accessCalls]
			accessCalls++
			return contract.HTTPResponse{StatusCode: 200, Body: body}, nil
		case strings.Contains(req.URL, "/copilot_internal/v2/token"):
			return contract.HTTPResponse{StatusCode: 200, Body: []byte(`{"token":"tid=SESSION_SENTINEL;proxy-ep=proxy.individual.githubcopilot.com","expires_at":20000}`)}, nil
		case strings.HasSuffix(req.URL, "/models"):
			return contract.HTTPResponse{StatusCode: 200, Body: selectableModelsJSON("gpt-6", []string{"/responses"})}, nil
		case strings.HasSuffix(req.URL, "/user"):
			return contract.HTTPResponse{StatusCode: 200, Body: []byte(`{"login":"octocat","id":1}`)}, nil
		default:
			t.Fatalf("unexpected URL %s", req.URL)
			return contract.HTTPResponse{}, nil
		}
	})
	service := newService(fake)
	service.now = func() time.Time { return now }
	service.random = bytes.NewReader(bytes.Repeat([]byte{9}, 32))
	started, errStart := service.startLoginRequest(context.Background(), "start", contract.AuthLoginStartRequest{})
	if errStart != nil {
		t.Fatal(errStart)
	}
	state := started.State
	poll := func() contract.AuthLoginPollResponse {
		result, errPoll := service.pollLoginRequest(context.Background(), "poll", contract.AuthLoginPollRequest{State: state})
		if errPoll != nil {
			t.Fatal(errPoll)
		}
		return result
	}
	if got := poll(); got.Status != contract.AuthLoginStatusPending || accessCalls != 0 {
		t.Fatalf("immediate poll = %#v, access calls=%d", got, accessCalls)
	}
	now = now.Add(5 * time.Second)
	if got := poll(); got.Status != contract.AuthLoginStatusPending || accessCalls != 1 {
		t.Fatalf("first poll = %#v, access calls=%d", got, accessCalls)
	}
	now = now.Add(5 * time.Second)
	if got := poll(); got.Status != contract.AuthLoginStatusPending || accessCalls != 2 {
		t.Fatalf("slow-down poll = %#v, access calls=%d", got, accessCalls)
	}
	now = now.Add(6 * time.Second)
	if got := poll(); got.Status != contract.AuthLoginStatusPending || accessCalls != 2 {
		t.Fatalf("early slow-down poll = %#v, access calls=%d", got, accessCalls)
	}
	now = now.Add(time.Second)
	got := poll()
	if got.Status != contract.AuthLoginStatusSuccess || accessCalls != 3 || got.Auth.Metadata["account"] != "octocat" {
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
	fake := newFakeHost(t, func(hostHTTPCall) (contract.HTTPResponse, error) {
		return contract.HTTPResponse{StatusCode: 200, Body: []byte(`{"error":"slow_down"}`)}, nil
	})
	service := newService(fake)
	service.now = func() time.Time { return now }
	session := &loginSession{clientID: defaultClientID, githubHost: "github.com", deviceCode: "device", interval: 5 * time.Second}
	complete, failure := service.pollGitHubToken(testHostClient(fake), session)
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
	fake := newFakeHost(t, func(hostHTTPCall) (contract.HTTPResponse, error) {
		close(entered)
		<-release
		return contract.HTTPResponse{StatusCode: 200, Body: []byte(`{"error":"authorization_pending"}`)}, nil
	})
	service := newService(fake)
	service.now = func() time.Time { return now }
	state := "deduplicated-state"
	service.sessions[state] = &loginSession{
		provider: providerIdentifier, clientID: defaultClientID, githubHost: "github.com", deviceCode: "device",
		interval: time.Second, nextPoll: now, expiresAt: now.Add(time.Minute),
	}
	firstDone := make(chan error, 1)
	go func() {
		_, errPoll := service.pollLoginRequest(context.Background(), "", contract.AuthLoginPollRequest{State: state})
		firstDone <- errPoll
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first poll did not enter host runtime")
	}
	result, errSecond := service.pollLoginRequest(context.Background(), "", contract.AuthLoginPollRequest{State: state})
	if errSecond != nil || result.Status != contract.AuthLoginStatusPending {
		t.Fatalf("second poll error=%v result=%#v", errSecond, result)
	}
	if len(fake.snapshot()) != 1 {
		t.Fatalf("host calls = %d, want one", len(fake.snapshot()))
	}
	close(release)
	if errFirst := <-firstDone; errFirst != nil {
		t.Fatal(errFirst)
	}
}

func TestLoginPollExpiryClearsTemporarySecrets(t *testing.T) {
	now := time.Unix(17_000, 0).UTC()
	service := newService(nil)
	service.now = func() time.Time { return now }
	session := &loginSession{
		deviceCode: "device-secret", githubAccessToken: "github-secret", expiresAt: now,
	}
	service.sessions["expired"] = session
	result, errPoll := service.pollLoginRequest(context.Background(), "", contract.AuthLoginPollRequest{State: "expired"})
	if errPoll != nil {
		t.Fatal(errPoll)
	}
	if result.Status != contract.AuthLoginStatusError || !strings.Contains(strings.ToLower(result.Message), "expired") {
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
	fake := newFakeHost(t, func(hostHTTPCall) (contract.HTTPResponse, error) {
		return contract.HTTPResponse{StatusCode: 200, Body: []byte(`{"error":"access_denied","error_description":"private detail"}`)}, nil
	})
	service := newService(fake)
	service.now = func() time.Time { return now }
	service.sessions["denied"] = &loginSession{
		clientID: defaultClientID, githubHost: "github.com", deviceCode: "device",
		interval: time.Second, nextPoll: now, expiresAt: now.Add(time.Minute),
	}
	result, errPoll := service.pollLoginRequest(context.Background(), "", contract.AuthLoginPollRequest{State: "denied"})
	if errPoll != nil {
		t.Fatal(errPoll)
	}
	if result.Status != contract.AuthLoginStatusError || !strings.Contains(strings.ToLower(result.Message), "denied") || strings.Contains(result.Message, "private detail") {
		t.Fatalf("poll result = %#v", result)
	}
	service.mu.RLock()
	_, exists := service.sessions["denied"]
	service.mu.RUnlock()
	if exists {
		t.Fatal("denied session was not removed")
	}
}

func TestLoginPollRetriesHostRuntimeFailure(t *testing.T) {
	now := time.Unix(19_000, 0).UTC()
	fake := newFakeHost(t, func(hostHTTPCall) (contract.HTTPResponse, error) {
		return contract.HTTPResponse{}, fmt.Errorf("temporary transport failure")
	})
	service := newService(fake)
	service.now = func() time.Time { return now }
	service.sessions["retry"] = &loginSession{
		clientID: defaultClientID, githubHost: "github.com", deviceCode: "device",
		interval: 5 * time.Second, nextPoll: now, expiresAt: now.Add(time.Minute),
	}
	result, errPoll := service.pollLoginRequest(context.Background(), "", contract.AuthLoginPollRequest{State: "retry"})
	if errPoll != nil {
		t.Fatal(errPoll)
	}
	if result.Status != contract.AuthLoginStatusPending || strings.Contains(result.Message, "transport") {
		t.Fatalf("poll result = %#v", result)
	}
}

func TestRefreshAuthReplacesOnlyCompleteSession(t *testing.T) {
	now := time.Unix(20_000, 0).UTC()
	fake := newFakeHost(t, func(req hostHTTPCall) (contract.HTTPResponse, error) {
		switch {
		case strings.Contains(req.URL, "/copilot_internal/v2/token"):
			return contract.HTTPResponse{StatusCode: 200, Body: []byte(`{"token":"tid=NEW_SESSION;proxy-ep=proxy.business.githubcopilot.com","expires_at":30000}`)}, nil
		case strings.HasSuffix(req.URL, "/models"):
			return contract.HTTPResponse{StatusCode: 200, Body: selectableModelsJSON("claude-sonnet-5-1", []string{"/v1/messages"})}, nil
		default:
			t.Fatalf("unexpected URL %s", req.URL)
			return contract.HTTPResponse{}, nil
		}
	})
	service := newService(fake)
	service.now = func() time.Time { return now }
	previous := copilotStorage{
		Type: providerIdentifier, Priority: 7, GitHubAccessToken: "ghu_OLD", CopilotSessionToken: "old", GitHubHost: "github.com", Account: "octocat",
	}
	result, errRefresh := service.refreshAuthRequest(context.Background(), "refresh", contract.AuthRefreshRequest{
		AuthID:      "copilot.json",
		StorageJSON: mustJSON(t, previous),
		Metadata:    map[string]any{"note": "keep", "access_token": "MUST_DROP"},
	})
	if errRefresh != nil {
		t.Fatal(errRefresh)
	}
	next, errStorage := decodeCopilotStorage(result.Auth.StorageJSON)
	if errStorage != nil || !strings.Contains(next.CopilotSessionToken, "NEW_SESSION") || next.Account != "octocat" || len(next.Models) != 1 {
		t.Fatalf("refreshed storage = %#v, %v", next, errStorage)
	}
	route := service.resolveModelRoute("copilot.json", "claude-sonnet-5-1", next)
	if route.Format != formatClaude || route.AdaptiveThinking || len(route.ReasoningLevels) != 0 {
		t.Fatalf("discovered route = %#v", route)
	}
	if _, leaked := result.Auth.Metadata["access_token"]; leaked || result.Auth.Metadata["note"] != "keep" {
		t.Fatalf("refreshed metadata = %#v", result.Auth.Metadata)
	}
	if result.Auth.Attributes["priority"] != "7" || next.Priority != 7 {
		t.Fatalf("refreshed priority = attributes=%q storage=%d, want 7", result.Auth.Attributes["priority"], next.Priority)
	}
}

func TestRefreshAuthKeepsNewSessionAndCachedModelsWhenDiscoveryFails(t *testing.T) {
	now := time.Unix(24_000, 0).UTC()
	fake := newFakeHost(t, func(req hostHTTPCall) (contract.HTTPResponse, error) {
		switch {
		case strings.Contains(req.URL, "/copilot_internal/v2/token"):
			return contract.HTTPResponse{StatusCode: 200, Body: []byte(`{"token":"tid=NEW;proxy-ep=proxy.individual.githubcopilot.com","expires_at":25000}`)}, nil
		case strings.HasSuffix(req.URL, "/models"):
			return contract.HTTPResponse{StatusCode: 200, Body: []byte(`{"unexpected":[]}`)}, nil
		default:
			return contract.HTTPResponse{}, fmt.Errorf("unexpected URL")
		}
	})
	service := newService(fake)
	service.now = func() time.Time { return now }
	previous := copilotStorage{
		Type: providerIdentifier, GitHubAccessToken: "ghu_OLD", CopilotSessionToken: "tid=OLD",
		GitHubHost: "github.com", ModelsFetchedAt: now.Add(-time.Hour).UnixMilli(),
		Models: []storedModel{{ID: "gpt-6", Format: formatOpenAI}},
	}
	result, failure := service.refreshAuthRequest(context.Background(), "refresh", contract.AuthRefreshRequest{AuthID: "auth", StorageJSON: mustJSON(t, previous)})
	if failure != nil {
		t.Fatalf("response=%#v failure=%v", result, failure)
	}
	next, errStorage := decodeCopilotStorage(result.Auth.StorageJSON)
	if errStorage != nil || !strings.HasPrefix(next.CopilotSessionToken, "tid=NEW;") || next.ExpiresAt != time.Unix(25000, 0).UnixMilli() {
		t.Fatalf("refreshed session = %#v, %v", next, errStorage)
	}
	if len(next.Models) != 1 || next.Models[0].ID != "gpt-6" || next.ModelsFetchedAt != previous.ModelsFetchedAt {
		t.Fatalf("cached models were not preserved: %#v", next)
	}
}

func TestExchangeSessionExpiryMarginAndMissingFields(t *testing.T) {
	now := time.Unix(25_000, 0).UTC()
	responses := [][]byte{
		[]byte(`{"expires_at":26000}`),
		[]byte(`{"token":"tid=x;proxy-ep=proxy.individual.githubcopilot.com","expires_at":26000}`),
	}
	fake := newFakeHost(t, func(hostHTTPCall) (contract.HTTPResponse, error) {
		body := responses[0]
		responses = responses[1:]
		return contract.HTTPResponse{StatusCode: 200, Body: body}, nil
	})
	service := newService(fake)
	service.now = func() time.Time { return now }
	if _, failure := service.exchangeSession(testHostClient(fake), "ghu", "github.com"); failure == nil || strings.Contains(failure.Error(), "ghu") {
		t.Fatalf("missing-token failure = %v", failure)
	}
	storage, failure := service.exchangeSession(testHostClient(fake), "ghu", "github.com")
	if failure != nil {
		t.Fatal(failure)
	}
	wantRefresh := time.Unix(26000, 0).Add(-10 * time.Minute).UnixMilli()
	if storage.RefreshAfter != wantRefresh {
		t.Fatalf("refresh_after = %d, want %d", storage.RefreshAfter, wantRefresh)
	}
}

func TestTokenErrorsDoNotContainResponseBodyOrCredentials(t *testing.T) {
	const sentinel = "SENTINEL_SHOULD_NEVER_APPEAR"
	fake := newFakeHost(t, func(hostHTTPCall) (contract.HTTPResponse, error) {
		return contract.HTTPResponse{StatusCode: http.StatusUnauthorized, Body: []byte(`{"token":"` + sentinel + `"}`)}, nil
	})
	service := newService(fake)
	_, failure := service.exchangeSession(testHostClient(fake), "ghu_"+sentinel, "github.com")
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
