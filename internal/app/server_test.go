package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/1oo1/cpa-github-copilot/internal/contract"
	"github.com/1oo1/cpa-github-copilot/internal/copilot"
)

type fakeUpstream struct {
	mu            sync.Mutex
	counts        map[string]int
	used          map[string]string
	reset         string
	fail          bool
	gate, entered chan struct{}
	inference     []string
}

func (f *fakeUpstream) Do(ctx context.Context, req contract.HTTPRequest) (*http.Response, error) {
	if strings.HasSuffix(req.URL, "/copilot_internal/user") {
		token := strings.TrimPrefix(req.Headers.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		f.counts[token]++
		used := f.used[token]
		if used == "" {
			used = "0"
		}
		reset, fail, gate := f.reset, f.fail, f.gate
		if f.entered != nil {
			select {
			case f.entered <- struct{}{}:
			default:
			}
		}
		f.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if fail {
			return nil, fmt.Errorf("sensitive upstream details must not surface")
		}
		return fakeResponse(fmt.Sprintf(`{"copilot_plan":"individual_max","token_based_billing":true,"quota_reset_date_utc":%q,"quota_snapshots":{"premium_interactions":{"unlimited":false,"percent_remaining":96,"overage_count":%s,"overage_entitlement":100}}}`, reset, used)), nil
	}
	if strings.HasSuffix(req.URL, "/chat/completions") {
		f.mu.Lock()
		f.inference = append(f.inference, req.Headers.Get("Authorization"))
		f.mu.Unlock()
		if strings.Contains(string(req.Body), `"stream":true`) {
			return fakeResponse("data: {\"id\":\"chat-demo\",\"object\":\"chat.completion.chunk\",\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"), nil
		}
		return fakeResponse(`{"id":"chat-demo","object":"chat.completion","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`), nil
	}
	return nil, fmt.Errorf("unexpected upstream path")
}
func fakeResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}
func testServer(t *testing.T) (*Server, *fakeUpstream, Config, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := Config{ManagementKey: "management-test-key", APIKeys: []string{"api-test-key-012345"}, Routing: "fill-first", Copilot: copilot.DefaultSettings()}
	network := &fakeUpstream{counts: map[string]int{}, used: map[string]string{}, reset: time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)}
	s, err := New(cfg, path, network, []byte("<html>embedded management</html>"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, network, cfg, path
}
func credential(token string, priority int) []byte {
	return []byte(fmt.Sprintf(`{"type":"github-copilot","github_host":"github.com","github_access_token":%q,"copilot_session_token":%q,"priority":%d,"refresh_after":%d,"expires_at":%d,"models_fetched_at":%d,"models":[{"id":"test-model","name":"Test model","format":"openai","streaming":true}]}`, token, "tid="+token+";proxy-ep=proxy.individual.githubcopilot.com", priority, time.Now().Add(time.Hour).UnixMilli(), time.Now().Add(2*time.Hour).UnixMilli(), time.Now().UnixMilli()))
}
func request(s *Server, method, path, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	out := httptest.NewRecorder()
	s.ServeHTTP(out, req)
	return out
}
func TestStandaloneSecurityAndEmbeddedUI(t *testing.T) {
	s, _, cfg, _ := testServer(t)
	if out := request(s, "GET", "/", "", ""); out.Code != 200 || !strings.Contains(out.Body.String(), "embedded management") {
		t.Fatal(out.Code)
	}
	for _, path := range []string{"/v1/models", "/v0/management/accounts", "/v0/management/config"} {
		if out := request(s, "GET", path, "", ""); out.Code != 401 {
			t.Fatal(path, out.Code)
		}
	}
	if out := request(s, "GET", "/v0/management/codex-auth-url", cfg.ManagementKey, ""); out.Code != 404 {
		t.Fatal("unrelated OAuth endpoint remains")
	}
	if out := request(s, "GET", "/v0/management/config", cfg.ManagementKey, ""); strings.Contains(out.Body.String(), cfg.ManagementKey) || strings.Contains(out.Body.String(), cfg.APIKeys[0]) {
		t.Fatal("configuration leaked keys")
	}
}
func TestStandaloneImportAndInference(t *testing.T) {
	s, _, cfg, _ := testServer(t)
	if _, err := s.importAccount("test.json", credential("github-test", 1)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../escape.json", "x/y.json", ".hidden.json"} {
		if _, err := s.importAccount(name, credential("test", 0)); err == nil {
			t.Fatal("unsafe filename")
		}
	}
	if _, err := s.importAccount("codex.json", []byte(`{"type":"codex"}`)); err == nil {
		t.Fatal("accepted unrelated provider")
	}
	out := request(s, "POST", "/v1/chat/completions", cfg.APIKeys[0], `{"model":"test-model","messages":[{"role":"user","content":"PRIVATE_PROMPT"}]}`)
	if out.Code != 200 || !strings.Contains(out.Body.String(), "OK") {
		t.Fatalf("response %d %s", out.Code, out.Body.String())
	}
	if s.usage.Input != 3 || s.usage.Output != 2 {
		t.Fatalf("usage=%+v", s.usage)
	}
	out = request(s, "GET", "/v0/management/accounts", cfg.ManagementKey, "")
	if strings.Contains(out.Body.String(), "github-test") || strings.Contains(out.Body.String(), "copilot_session_token") {
		t.Fatal("credential leaked")
	}
	out = request(s, "GET", "/v0/management/logs", cfg.ManagementKey, "")
	if strings.Contains(out.Body.String(), "PRIVATE_PROMPT") || strings.Contains(out.Body.String(), "github-test") {
		t.Fatal("sensitive log")
	}
}
func TestBudgetThresholdFailoverAndRestart(t *testing.T) {
	s, f, cfg, path := testServer(t)
	_, _ = s.importAccount("a.json", credential("github-a", 10))
	_, _ = s.importAccount("b.json", credential("github-b", 1))
	f.used["github-a"] = "99.01"
	out := request(s, "POST", "/v1/chat/completions", cfg.APIKeys[0], `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	if out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	a := s.account("a.json")
	a.mu.Lock()
	blocked := a.guard.Blocked
	a.mu.Unlock()
	if !blocked || len(f.inference) != 1 || !strings.Contains(f.inference[0], "github-b") {
		t.Fatal("did not rotate blocked account")
	}
	next, err := New(cfg, path, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	a = next.account("a.json")
	if !a.guard.Blocked {
		t.Fatal("restart lost guard")
	}
	f.fail = true
	_ = next.checkQuota(context.Background(), a)
	if !a.guard.Blocked {
		t.Fatal("failure cleared guard")
	}
}
func TestBudgetStrictBoundaryAndVerifiedReset(t *testing.T) {
	s, f, _, _ := testServer(t)
	_, _ = s.importAccount("a.json", credential("a", 0))
	a := s.account("a.json")
	f.used["a"] = "99"
	_ = s.checkQuota(context.Background(), a)
	if a.guard.Blocked {
		t.Fatal("99% must not block")
	}
	a.nextQuota = time.Time{}
	f.used["a"] = "99.01"
	f.reset = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	_ = s.checkQuota(context.Background(), a)
	if !a.guard.Blocked {
		t.Fatal("99.01% must block")
	}
	a.nextQuota = time.Time{}
	f.used["a"] = "0"
	_ = s.checkQuota(context.Background(), a)
	if !a.guard.Blocked {
		t.Fatal("same period must not recover")
	}
	disabled := true
	_ = s.updateAccount("a.json", &disabled, nil)
	a.nextQuota = time.Time{}
	f.reset = time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	_ = s.checkQuota(context.Background(), a)
	if a.guard.Blocked || !a.disabled {
		t.Fatal("new cycle/manual disable handling incorrect")
	}
}
func TestBudgetChecksCoalesce(t *testing.T) {
	s, f, _, _ := testServer(t)
	_, _ = s.importAccount("a.json", credential("a", 0))
	a := s.account("a.json")
	f.gate = make(chan struct{})
	f.entered = make(chan struct{}, 1)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = s.checkQuota(context.Background(), a) }()
	}
	<-f.entered
	close(f.gate)
	wg.Wait()
	if f.counts["a"] != 1 {
		t.Fatalf("quota calls=%d", f.counts["a"])
	}
	if a.quota.Subscription == nil || a.quota.Subscription.Plan != "Copilot Max" {
		t.Fatal("quota not retained")
	}
}
func TestBudgetLateResponseDoesNotTouchReplacement(t *testing.T) {
	s, f, _, _ := testServer(t)
	_, _ = s.importAccount("a.json", credential("old", 0))
	old := s.account("a.json")
	f.used["old"] = "100"
	f.gate = make(chan struct{})
	f.entered = make(chan struct{}, 1)
	done := make(chan struct{})
	go func() { _ = s.checkQuota(context.Background(), old); close(done) }()
	<-f.entered
	if _, err := s.importAccount("a.json", credential("new", 0)); err != nil {
		t.Fatal(err)
	}
	close(f.gate)
	<-done
	if s.account("a.json").guard.Blocked {
		t.Fatal("late quota poisoned new credential")
	}
}
func TestStreamStandaloneSSE(t *testing.T) {
	s, _, cfg, _ := testServer(t)
	_, _ = s.importAccount("a.json", credential("a", 0))
	out := request(s, "POST", "/v1/chat/completions", cfg.APIKeys[0], `{"model":"test-model","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	if out.Code != 200 || !strings.Contains(out.Body.String(), "[DONE]") || out.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream %d %s", out.Code, out.Body.String())
	}
}
func TestStandaloneAccountStateAndKeyPersistence(t *testing.T) {
	s, _, cfg, path := testServer(t)
	_, _ = s.importAccount("a.json", credential("a", 0))
	priority := 50
	disabled := true
	if err := s.updateAccount("a.json", &disabled, &priority); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(s.authDir, "a.json"))
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	_ = json.Unmarshal(raw, &data)
	if data["disabled"] != true || data["priority"] != float64(50) {
		t.Fatal("settings not persisted")
	}
	info, _ := os.Stat(filepath.Join(s.authDir, "a.json"))
	if info.Mode().Perm()&0077 != 0 {
		t.Fatal("credential file is not private")
	}
	out := request(s, "POST", "/v0/management/api-keys", cfg.ManagementKey, "")
	if out.Code != 200 {
		t.Fatal(out.Body.String())
	}
	var key map[string]string
	_ = json.Unmarshal(out.Body.Bytes(), &key)
	if request(s, "GET", "/v1/models", key["key"], "").Code != 200 {
		t.Fatal("generated key rejected")
	}
	loaded, err := LoadConfig(path)
	if err != nil || len(loaded.APIKeys) != 2 {
		t.Fatal("keys not persisted", err)
	}
}
func TestStandaloneImportDirectoryPreservesSource(t *testing.T) {
	s, _, _, _ := testServer(t)
	dir := t.TempDir()
	raw := credential("imported", 0)
	_ = os.WriteFile(filepath.Join(dir, "copilot.json"), raw, 0600)
	_ = os.WriteFile(filepath.Join(dir, "other.json"), []byte(`{"type":"codex"}`), 0600)
	n, err := s.ImportDirectory(dir)
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "copilot.json"))
	if string(after) != string(raw) {
		t.Fatal("source changed")
	}
}
func TestBudgetMonitorWorksWithoutUI(t *testing.T) {
	s, f, _, _ := testServer(t)
	_, _ = s.importAccount("a.json", credential("a", 0))
	f.used["a"] = "100"
	f.entered = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	select {
	case <-f.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("monitor never checked")
	}
	a := s.account("a.json")
	_ = s.checkQuota(context.Background(), a)
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.guard.Blocked {
		t.Fatal("background did not block")
	}
}

func TestStandaloneAllClientProtocols(t *testing.T) {
	for _, tc := range []struct{ path, body, want string }{
		{"/v1/responses", `{"model":"test-model","input":[{"role":"user","content":"hello"}]}`, "response"},
		{"/v1/messages", `{"model":"test-model","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`, "message"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			s, _, cfg, _ := testServer(t)
			_, _ = s.importAccount("a.json", credential("a", 0))
			out := request(s, "POST", tc.path, cfg.APIKeys[0], tc.body)
			if out.Code != 200 || !strings.Contains(out.Body.String(), "OK") {
				t.Fatalf("%d %s", out.Code, out.Body.String())
			}
			body := strings.TrimSuffix(tc.body, "}") + `,"stream":true}`
			out = request(s, "POST", tc.path, cfg.APIKeys[0], body)
			if out.Code != 200 || !strings.Contains(out.Body.String(), tc.want) || !strings.Contains(out.Body.String(), "data:") {
				t.Fatalf("stream %d %s", out.Code, out.Body.String())
			}
		})
	}
}
func TestBudgetMigrationKeepsCPALatch(t *testing.T) {
	s, _, _, _ := testServer(t)
	dir := t.TempDir()
	raw := credential("old-cpa", 0)
	name := "copilot.json"
	_ = os.WriteFile(filepath.Join(dir, name), raw, 0600)
	auth, _ := s.engine.Parse(name, raw)
	metadata := map[string]any{}
	_ = json.Unmarshal(auth.StorageJSON, &metadata)
	values := []any{"github-copilot", name, metadata["github_access_token"], metadata["github_host"], metadata["account"]}
	encoded, _ := json.Marshal(values)
	sum := sha256.Sum256(encoded)
	state := fmt.Sprintf(`{"records":[{"auth_id":%q,"provider":"github-copilot","budget_guard":{"blocked":true,"reason":"additional_budget_exceeded","credential_identity":%q,"reset_at":"2026-10-01T00:00:00Z"}}]}`, name, hex.EncodeToString(sum[:]))
	_ = os.WriteFile(filepath.Join(dir, "copilot.cds"), []byte(state), 0600)
	if _, err := s.ImportDirectory(dir); err != nil {
		t.Fatal(err)
	}
	if !s.account(name).guard.Blocked {
		t.Fatal("old guard discarded")
	}
}

func TestStandaloneRejectsMalformedCredentialsAndControls(t *testing.T) {
	s, _, _, _ := testServer(t)
	for _, raw := range []string{`{"type":"github-copilot"}`, `{"type":"github-copilot","github_access_token":"synthetic","github_host":"evil.example"}`, `{"type":"github-copilot","github_access_token":"synthetic","disabled":"false"}`, `{"type":"github-copilot","github_access_token":"synthetic","priority":1.5}`} {
		if _, err := s.importAccount("invalid.json", []byte(raw)); err == nil {
			t.Fatalf("invalid credential accepted")
		}
	}
	if s.account("invalid.json") != nil {
		t.Fatal("invalid account was installed")
	}
}
func TestStandalonePriorityAffinityAndDisablement(t *testing.T) {
	s, _, _, _ := testServer(t)
	s.cfg.SessionAffinity = true
	_, _ = s.importAccount("a.json", credential("a", 20))
	_, _ = s.importAccount("b.json", credential("b", 1))
	if got := s.candidates(""); len(got) != 2 || got[0].id != "a.json" {
		t.Fatal("priority selection")
	}
	s.bindSession("synthetic-session", "b.json")
	if got := s.candidates("synthetic-session"); got[0].id != "b.json" {
		t.Fatal("affinity not retained")
	}
	b := s.account("b.json")
	b.mu.Lock()
	b.guard.Blocked = true
	b.mu.Unlock()
	if got := s.candidates("synthetic-session"); len(got) != 1 || got[0].id != "a.json" {
		t.Fatal("affinity bypassed guard")
	}
	disable := true
	_ = s.updateAccount("a.json", &disable, nil)
	if len(s.candidates("synthetic-session")) != 0 {
		t.Fatal("manual disable bypassed")
	}
}
func TestStandaloneConfigRejectsTrailingDocumentAndSharedKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := LoadConfig(path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append(raw, []byte("\n---\nlisten: 0.0.0.0:80\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("trailing YAML ignored")
	}
	_ = os.WriteFile(path, []byte("management-key: same-test-key-123456\napi-keys: [same-test-key-123456]\n"), 0600)
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("management and API keys not isolated")
	}
}

type runtimeTransportFunc func(context.Context, contract.HTTPRequest) (*http.Response, error)

func (f runtimeTransportFunc) Do(ctx context.Context, req contract.HTTPRequest) (*http.Response, error) {
	return f(ctx, req)
}
func TestStandaloneCredentialRefreshPreservesGuardAndControls(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{ManagementKey: "management-test-key", APIKeys: []string{"api-test-key-012345"}, Routing: "fill-first", Copilot: copilot.DefaultSettings()}
	network := runtimeTransportFunc(func(ctx context.Context, req contract.HTTPRequest) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL, "/copilot_internal/v2/token"):
			return fakeResponse(fmt.Sprintf(`{"token":"tid=renewed;proxy-ep=proxy.individual.githubcopilot.com","expires_at":%d}`, time.Now().Add(time.Hour).Unix())), nil
		case strings.HasSuffix(req.URL, "/models"):
			return fakeResponse(`{"data":[{"id":"updated-model","name":"Updated","model_picker_enabled":true,"supported_endpoints":["/chat/completions"],"capabilities":{"supports":{"streaming":true,"tool_calls":true}}}]}`), nil
		default:
			return nil, fmt.Errorf("unexpected test request")
		}
	})
	s, err := New(cfg, filepath.Join(dir, "config.yaml"), network, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.importAccount("a.json", credential("github-persistent", 41))
	if err != nil {
		t.Fatal(err)
	}
	a := s.account("a.json")
	a.guard = Guard{Blocked: true, Reason: "additional_budget_exceeded", ResetAt: time.Now().Add(24 * time.Hour)}
	disabled := true
	_ = s.updateAccount(a.id, &disabled, nil)
	if err = s.refresh(context.Background(), a, true); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.disabled || a.priority != 41 || !a.guard.Blocked {
		t.Fatal("refresh changed user controls or released guard")
	}
	if got := modelIDs(a.raw); len(got) != 1 || got[0] != "updated-model" {
		t.Fatalf("model catalogue update lost: %v", got)
	}
	if a.raw["github_access_token"] != "github-persistent" || !strings.Contains(string(a.auth.StorageJSON), "renewed") {
		t.Fatal("token refresh did not preserve long-term credential")
	}
}
func TestStandaloneQuotaWaiterCancellation(t *testing.T) {
	s, f, _, _ := testServer(t)
	_, _ = s.importAccount("a.json", credential("a", 0))
	a := s.account("a.json")
	f.gate = make(chan struct{})
	f.entered = make(chan struct{}, 1)
	done := make(chan struct{})
	go func() { _ = s.checkQuota(context.Background(), a); close(done) }()
	<-f.entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.checkQuota(ctx, a); err != context.Canceled {
		t.Fatalf("canceled waiter: %v", err)
	}
	close(f.gate)
	<-done
	if a.quota.Subscription == nil {
		t.Fatal("canceled waiter aborted another request")
	}
}

func TestStandaloneUsageAcceptsFormattedJSON(t *testing.T) {
	input, output := usageTokens([]byte("{\n  \"usage\": {\n    \"prompt_tokens\": 21,\n    \"completion_tokens\": 8\n  }\n}"))
	if input != 21 || output != 8 {
		t.Fatalf("formatted usage=%d/%d", input, output)
	}
	input, output = usageTokens([]byte("data: {\"message\":{\"usage\":{\"input_tokens\":21}}}\n\ndata: {\"usage\":{\"output_tokens\":8}}\n\n"))
	if input != 21 || output != 8 {
		t.Fatalf("stream usage=%d/%d", input, output)
	}
}
