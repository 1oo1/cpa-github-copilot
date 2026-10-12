package copilot

import (
	"context"
	"encoding/json"

	"strings"

	"testing"
	"time"

	contract "github.com/1oo1/cpa-github-copilot/internal/contract"
)

func TestAuthParseLogsExpiredCredentialStateWithoutSecrets(t *testing.T) {
	now := time.Unix(210_000, 0).UTC()
	fake := newFakeHost(t, nil)
	service := newService(fake)
	service.now = func() time.Time { return now }
	storage := copilotStorage{
		Type:                providerIdentifier,
		GitHubAccessToken:   "ghu_PARSE_LOG_SENTINEL",
		CopilotSessionToken: "tid=PARSE_LOG_SENTINEL;proxy-ep=proxy.individual.githubcopilot.com",
		RefreshAfter:        now.Add(-10 * time.Minute).UnixMilli(),
		ExpiresAt:           now.Add(-5 * time.Minute).UnixMilli(),
		GitHubHost:          "github.com",
		Models:              []storedModel{{ID: "gpt-6", Format: formatOpenAIResponse}},
	}
	if _, errParse := service.parseAuthRequest(contract.AuthParseRequest{
		FileName: "github-copilot-test.json",
		RawJSON:  mustJSON(t, storage),
	}); errParse != nil {
		t.Fatal(errParse)
	}
	logEntry := findLogEvent(t, fake.snapshotLogs(), "auth.parsed")
	if logEntry.Fields["session_expired"] != true || logEntry.Fields["refresh_due"] != true || logEntry.Fields["disabled"] != false {
		t.Fatalf("auth parse log fields = %#v", logEntry.Fields)
	}
	assertLogsExclude(t, fake.snapshotLogs(), "PARSE_LOG_SENTINEL", storage.GitHubAccessToken, storage.CopilotSessionToken)
}

func TestExpiredInferenceLogsStableFailureWithoutSecrets(t *testing.T) {
	now := time.Unix(220_000, 0).UTC()
	fake := newFakeHost(t, nil)
	service := newService(fake)
	service.now = func() time.Time { return now }
	storage := copilotStorage{
		Type:                providerIdentifier,
		GitHubAccessToken:   "ghu_INFERENCE_LOG_SENTINEL",
		CopilotSessionToken: "tid=INFERENCE_LOG_SENTINEL;proxy-ep=proxy.individual.githubcopilot.com",
		RefreshAfter:        now.Add(-10 * time.Minute).UnixMilli(),
		ExpiresAt:           now.Add(-5 * time.Minute).UnixMilli(),
		GitHubHost:          "github.com",
		Models:              []storedModel{{ID: "claude-opus-5-1", Format: formatClaude}},
	}
	payload := []byte(`{"model":"claude-opus-5-1","messages":[{"role":"user","content":"PRIVATE_PROMPT_SENTINEL"}]}`)
	_, failure := service.executeRequest(context.Background(), "callback-inference", contract.ExecutorRequest{
		AuthID: "auth-1", Model: "claude-opus-5-1", Format: formatClaude, SourceFormat: formatClaude,
		Payload: payload, StorageJSON: mustJSON(t, storage),
	})
	if failure == nil || failure.(*serviceError).code != "reauth_required" {
		t.Fatalf("failure = %#v", failure)
	}
	logEntry := findLogEvent(t, fake.snapshotLogs(), "inference.rejected")
	if logEntry.Fields["failure_code"] != "reauth_required" || logEntry.Fields["session_expired"] != true {
		t.Fatalf("inference rejection log fields = %#v", logEntry.Fields)
	}
	assertLogsExclude(t, fake.snapshotLogs(), "INFERENCE_LOG_SENTINEL", "PRIVATE_PROMPT_SENTINEL", storage.GitHubAccessToken, storage.CopilotSessionToken)
}

func TestLogEventFiltersSensitiveKeysAndValues(t *testing.T) {
	fake := newFakeHost(t, nil)
	service := newService(fake)
	service.logEvent("callback", "info", "test.security", map[string]any{
		"model":         "gpt-6",
		"authorization": "Bearer AUTH_LOG_SENTINEL",
		"storage_json":  "STORAGE_LOG_SENTINEL",
		"safe_but_bad":  "tid=SESSION_LOG_SENTINEL",
	})
	entry := findLogEvent(t, fake.snapshotLogs(), "test.security")
	if entry.Fields["model"] != "gpt-6" {
		t.Fatalf("safe log field missing: %#v", entry.Fields)
	}
	assertLogsExclude(t, fake.snapshotLogs(), "AUTH_LOG_SENTINEL", "STORAGE_LOG_SENTINEL", "SESSION_LOG_SENTINEL")
}

func findLogEvent(t *testing.T, logs []hostLogEntry, event string) hostLogEntry {
	t.Helper()
	for _, entry := range logs {
		if entry.Fields["event"] == event {
			return entry
		}
	}
	t.Fatalf("log event %q not found in %#v", event, logs)
	return hostLogEntry{}
}

func assertLogsExclude(t *testing.T, logs []hostLogEntry, forbidden ...string) {
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
