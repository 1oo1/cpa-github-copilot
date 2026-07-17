package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type refreshProbeExecutor struct {
	once      sync.Once
	refreshed chan struct{}
}

func (e *refreshProbeExecutor) Identifier() string { return pluginIdentifier }

func (e *refreshProbeExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func (e *refreshProbeExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, nil
}

func (e *refreshProbeExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	e.once.Do(func() { close(e.refreshed) })
	updated := auth.Clone()
	now := time.Now().UTC()
	updated.Metadata["expires_at"] = now.Add(30 * time.Minute).UnixMilli()
	updated.NextRefreshAfter = now.Add(25 * time.Minute)
	return updated, nil
}

func (e *refreshProbeExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func (e *refreshProbeExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestAuthDataExposesSafeOAuthRefreshSchedulingMetadata(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	storage := copilotStorage{
		Type:                pluginIdentifier,
		GitHubAccessToken:   "ghu_REFRESH_SCHEDULING_SENTINEL",
		CopilotSessionToken: "tid=SESSION_SCHEDULING_SENTINEL;proxy-ep=proxy.individual.githubcopilot.com",
		RefreshAfter:        now.Add(25 * time.Minute).UnixMilli(),
		ExpiresAt:           now.Add(30 * time.Minute).UnixMilli(),
		GitHubHost:          "github.com",
	}
	auth := authDataFromStorage(storage, authDataDefaults{})
	if auth.Metadata["auth_kind"] != coreauth.AuthKindOAuth {
		t.Fatalf("auth_kind = %#v", auth.Metadata["auth_kind"])
	}
	if auth.Metadata["expires_at"] != storage.ExpiresAt {
		t.Fatalf("expires_at = %#v, want %d", auth.Metadata["expires_at"], storage.ExpiresAt)
	}
	if auth.Metadata["refresh_interval_seconds"] != int64(hostRefreshInterval/time.Second) {
		t.Fatalf("refresh_interval_seconds = %#v", auth.Metadata["refresh_interval_seconds"])
	}
	core := &coreauth.Auth{Metadata: auth.Metadata, Attributes: auth.Attributes}
	if core.AuthKind() != coreauth.AuthKindOAuth {
		t.Fatalf("host auth kind = %q", core.AuthKind())
	}
	expiresAt, ok := core.ExpirationTime()
	if !ok || !expiresAt.Equal(time.UnixMilli(storage.ExpiresAt)) {
		t.Fatalf("host expiration = %s, %t", expiresAt, ok)
	}
	metadata, errMarshal := json.Marshal(auth.Metadata)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if strings.Contains(string(metadata), "SENTINEL") {
		t.Fatalf("secret escaped into refresh metadata: %s", metadata)
	}
}

func TestExpiredPluginAuthIsImmediatelyAutoRefreshedByHost(t *testing.T) {
	now := time.Now().UTC()
	storage := copilotStorage{
		Type:                pluginIdentifier,
		GitHubAccessToken:   "ghu_AUTO_REFRESH_SENTINEL",
		CopilotSessionToken: "tid=AUTO_REFRESH_SENTINEL;proxy-ep=proxy.individual.githubcopilot.com",
		RefreshAfter:        now.Add(-10 * time.Minute).UnixMilli(),
		ExpiresAt:           now.Add(-5 * time.Minute).UnixMilli(),
		GitHubHost:          "github.com",
	}
	data := authDataFromStorage(storage, authDataDefaults{ID: "auth-1"})
	auth := &coreauth.Auth{
		ID:               data.ID,
		Provider:         data.Provider,
		Status:           coreauth.StatusActive,
		Metadata:         data.Metadata,
		Attributes:       data.Attributes,
		NextRefreshAfter: data.NextRefreshAfter,
	}
	manager := coreauth.NewManager(nil, nil, nil)
	probe := &refreshProbeExecutor{refreshed: make(chan struct{})}
	manager.RegisterExecutor(probe)
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer manager.StopAutoRefresh()
	manager.StartAutoRefresh(ctx, time.Hour)
	select {
	case <-probe.refreshed:
	case <-time.After(2 * time.Second):
		t.Fatal("host did not immediately refresh expired plugin auth")
	}
}

func TestAuthParseLogsExpiredCredentialStateWithoutSecrets(t *testing.T) {
	now := time.Unix(210_000, 0).UTC()
	bridge := &fakeBridge{}
	service := newPluginService(bridge)
	service.now = func() time.Time { return now }
	storage := copilotStorage{
		Type:                pluginIdentifier,
		GitHubAccessToken:   "ghu_PARSE_LOG_SENTINEL",
		CopilotSessionToken: "tid=PARSE_LOG_SENTINEL;proxy-ep=proxy.individual.githubcopilot.com",
		RefreshAfter:        now.Add(-10 * time.Minute).UnixMilli(),
		ExpiresAt:           now.Add(-5 * time.Minute).UnixMilli(),
		GitHubHost:          "github.com",
		Models:              []storedModel{{ID: "gpt-5.6-sol", Format: formatOpenAIResponse}},
	}
	if _, errParse := service.parseAuth(mustJSON(t, pluginapi.AuthParseRequest{
		FileName: "github-copilot-test.json",
		RawJSON:  mustJSON(t, storage),
	})); errParse != nil {
		t.Fatal(errParse)
	}
	logEntry := findLogEvent(t, bridge.snapshotLogs(), "auth.parsed")
	if logEntry.Fields["session_expired"] != true || logEntry.Fields["refresh_due"] != true || logEntry.Fields["disabled"] != false {
		t.Fatalf("auth parse log fields = %#v", logEntry.Fields)
	}
	assertLogsExclude(t, bridge.snapshotLogs(), "PARSE_LOG_SENTINEL", storage.GitHubAccessToken, storage.CopilotSessionToken)
}

func TestExpiredInferenceLogsStableFailureWithoutSecrets(t *testing.T) {
	now := time.Unix(220_000, 0).UTC()
	bridge := &fakeBridge{}
	service := newPluginService(bridge)
	service.now = func() time.Time { return now }
	storage := copilotStorage{
		Type:                pluginIdentifier,
		GitHubAccessToken:   "ghu_INFERENCE_LOG_SENTINEL",
		CopilotSessionToken: "tid=INFERENCE_LOG_SENTINEL;proxy-ep=proxy.individual.githubcopilot.com",
		RefreshAfter:        now.Add(-10 * time.Minute).UnixMilli(),
		ExpiresAt:           now.Add(-5 * time.Minute).UnixMilli(),
		GitHubHost:          "github.com",
		Models:              []storedModel{{ID: "claude-opus-4.8", Format: formatClaude}},
	}
	payload := []byte(`{"model":"claude-opus-4.8","messages":[{"role":"user","content":"PRIVATE_PROMPT_SENTINEL"}]}`)
	_, failure := service.execute(mustJSON(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID: "auth-1", Model: "claude-opus-4.8", Format: formatClaude, SourceFormat: formatClaude,
			Payload: payload, StorageJSON: mustJSON(t, storage),
		},
		HostCallbackID: "callback-inference",
	}))
	if failure == nil || failure.(*pluginFailure).code != "reauth_required" {
		t.Fatalf("failure = %#v", failure)
	}
	logEntry := findLogEvent(t, bridge.snapshotLogs(), "inference.rejected")
	if logEntry.Fields["failure_code"] != "reauth_required" || logEntry.Fields["session_expired"] != true {
		t.Fatalf("inference rejection log fields = %#v", logEntry.Fields)
	}
	assertLogsExclude(t, bridge.snapshotLogs(), "INFERENCE_LOG_SENTINEL", "PRIVATE_PROMPT_SENTINEL", storage.GitHubAccessToken, storage.CopilotSessionToken)
}

func TestLogEventFiltersSensitiveKeysAndValues(t *testing.T) {
	bridge := &fakeBridge{}
	service := newPluginService(bridge)
	service.logEvent("callback", "info", "test.security", map[string]any{
		"model":         "gpt-5.6-sol",
		"authorization": "Bearer AUTH_LOG_SENTINEL",
		"storage_json":  "STORAGE_LOG_SENTINEL",
		"safe_but_bad":  "tid=SESSION_LOG_SENTINEL",
	})
	entry := findLogEvent(t, bridge.snapshotLogs(), "test.security")
	if entry.Fields["model"] != "gpt-5.6-sol" {
		t.Fatalf("safe log field missing: %#v", entry.Fields)
	}
	assertLogsExclude(t, bridge.snapshotLogs(), "AUTH_LOG_SENTINEL", "STORAGE_LOG_SENTINEL", "SESSION_LOG_SENTINEL")
}

func findLogEvent(t *testing.T, logs []rpcHostLogRequest, event string) rpcHostLogRequest {
	t.Helper()
	for _, entry := range logs {
		if entry.Fields["event"] == event {
			return entry
		}
	}
	t.Fatalf("log event %q not found in %#v", event, logs)
	return rpcHostLogRequest{}
}

func assertLogsExclude(t *testing.T, logs []rpcHostLogRequest, forbidden ...string) {
	t.Helper()
	raw, errMarshal := json.Marshal(logs)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	for _, value := range forbidden {
		if value != "" && strings.Contains(string(raw), value) {
			t.Fatalf("diagnostic logs contain forbidden value %q", value)
		}
	}
}
