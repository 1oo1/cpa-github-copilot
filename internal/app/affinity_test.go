package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/1oo1/cpa-github-copilot/internal/contract"
	"github.com/1oo1/cpa-github-copilot/internal/copilot"
)

const claudeSession = "11111111-2222-4333-8444-555555555555"

func claudeSessionBody(t *testing.T, session string, stream bool) string {
	t.Helper()
	userID, _ := json.Marshal(map[string]string{"device_id": "synthetic-device", "account_uuid": "synthetic-account", "session_id": session})
	body, _ := json.Marshal(map[string]any{"model": "test-model", "stream": stream, "metadata": map[string]string{"user_id": string(userID)}, "messages": []any{map[string]string{"role": "user", "content": "synthetic"}}})
	return string(body)
}

func TestStandaloneClaudeCodeSessionAffinity(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/enabled=%v", stream, enabled), func(t *testing.T) {
				s, f, cfg, _ := testServer(t)
				s.cfg.Routing = "round-robin"
				s.cfg.SessionAffinity = enabled
				for _, id := range []string{"a", "b"} {
					if _, err := s.importAccount(id+".json", credential(id, 0)); err != nil {
						t.Fatal(err)
					}
				}
				for i := 0; i < 3; i++ {
					req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(claudeSessionBody(t, claudeSession, stream)))
					req.Header.Set("X-Api-Key", cfg.APIKeys[0])
					out := httptest.NewRecorder()
					s.ServeHTTP(out, req)
					if out.Code != 200 {
						t.Fatalf("status=%d", out.Code)
					}
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				if len(f.inference) != 3 {
					t.Fatal("missing synthetic inference")
				}
				same := f.inference[0] == f.inference[1] && f.inference[0] == f.inference[2]
				if same != enabled {
					t.Fatalf("same account=%v, session-affinity=%v", same, enabled)
				}
			})
		}
	}
}

func TestClaudeCodeSessionID(t *testing.T) {
	for _, tc := range []struct{ name, userID, want string }{
		{"json", `{"device_id":"synthetic-device","account_uuid":"synthetic-account","session_id":"` + claudeSession + `"}`, claudeSession},
		{"legacy", "user_synthetic_account__session_" + claudeSession, claudeSession},
		{"legacy account", "user_synthetic_account_synthetic-account_session_" + claudeSession, claudeSession},
		{"uppercase", `{"session_id":"AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE"}`, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"},
		{"missing", `{"device_id":"synthetic-device"}`, ""},
		{"blank", `{"session_id":""}`, ""},
		{"number", `{"session_id":42}`, ""},
		{"array", `[{"session_id":"` + claudeSession + `"}]`, ""},
		{"malformed", `{"session_id":"` + claudeSession + `"`, ""},
		{"raw UUID", claudeSession, ""},
		{"arbitrary suffix", "something_session_" + claudeSession, ""},
		{"bad UUID", `{"session_id":"not-a-conversation-uuid"}`, ""},
		{"oversized", `{"device_id":"` + strings.Repeat("x", maxClaudeUserIDBytes) + `","session_id":"` + claudeSession + `"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeCodeSessionID(tc.userID); got != tc.want {
				t.Fatalf("session recognition mismatch for %s", tc.name)
			}
		})
	}
}

func TestRequestSessionKeyIsolationAndPrecedence(t *testing.T) {
	body := []byte(claudeSessionBody(t, claudeSession, false))
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("X-Api-Key", "synthetic-key-a")
	first := requestSessionKey(req, body, "claude")
	if len(first) != 64 || strings.Contains(first, claudeSession) {
		t.Fatal("missing or unhashed session key")
	}
	req.Header.Set("Authorization", "Bearer synthetic-key-a")
	req.Header.Set("X-Api-Key", "ignored-key")
	if requestSessionKey(req, body, "claude") != first {
		t.Fatal("irrelevant API key/header representation split the session")
	}
	req.Header.Set("Authorization", "Bearer synthetic-key-b")
	if requestSessionKey(req, body, "claude") == first {
		t.Fatal("API keys share an affinity binding")
	}
	req.Header.Set("Authorization", "Bearer synthetic-key-a")
	if requestSessionKey(req, []byte(claudeSessionBody(t, "66666666-7777-4888-8999-000000000000", false)), "claude") == first {
		t.Fatal("distinct sessions share a binding")
	}
	for _, format := range []string{"openai", "openai-response"} {
		fallback := requestSessionKey(req, body, format)
		if fallback == "" || fallback == first {
			t.Fatal("missing isolated request-hash fallback")
		}
		if fallback != requestSessionKey(req, body, format) {
			t.Fatal("request-hash fallback is not deterministic")
		}
	}
	req.Header.Set("X-Session-Id", "ignored-session")
	if requestSessionKey(req, body, "claude") != first {
		t.Fatal("X-Session-Id changed the affinity key")
	}
	req.Header.Set("X-Session-Id", strings.Repeat("x", 129))
	if requestSessionKey(req, body, "claude") != first {
		t.Fatal("oversized X-Session-Id changed the affinity key")
	}
	req.Header.Del("X-Session-Id")
	req.Header.Del("Authorization")
	req.Header.Del("X-Api-Key")
	if requestSessionKey(req, body, "claude") != "" {
		t.Fatal("unauthenticated session key generated")
	}
}

func TestSessionBindingCapacityAndExpiry(t *testing.T) {
	s, _, _, _ := testServer(t)
	s.cfg.SessionAffinity = true
	now := time.Now()
	for i := 0; i < maxSessionBindings; i++ {
		s.affinity[fmt.Sprintf("session-%04d", i)] = binding{"account", now.Add(time.Duration(i+1) * time.Minute)}
	}
	s.bindSession("session-0001", "renewed")
	if len(s.affinity) != maxSessionBindings || s.affinity["session-0002"].id != "account" {
		t.Fatal("renewal cleared unrelated bindings")
	}
	s.bindSession("new-session", "new-account")
	if len(s.affinity) != maxSessionBindings || s.affinity["new-session"].id != "new-account" || s.affinity["session-0002"].id != "account" {
		t.Fatal("inserting at capacity reset the binding cache")
	}
	if _, exists := s.affinity["session-0000"]; exists {
		t.Fatal("oldest binding was not evicted")
	}
	s.affinity["session-0002"] = binding{"account", now.Add(-time.Minute)}
	s.bindSession("another-session", "new-account")
	if _, exists := s.affinity["session-0002"]; exists {
		t.Fatal("expired binding was not reclaimed")
	}
	if len(s.affinity) > maxSessionBindings {
		t.Fatal("unbounded affinity cache")
	}
}

func TestConcurrentSessionSelectionReservesOneAccount(t *testing.T) {
	s, _, _, _ := testServer(t)
	s.cfg.SessionAffinity = true
	s.cfg.Routing = "round-robin"
	for _, id := range []string{"a", "b"} {
		if _, err := s.importAccount(id+".json", credential(id, 0)); err != nil {
			t.Fatal(err)
		}
	}
	choices := make(chan string, 20)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); choices <- s.candidates("new-concurrent-session")[0].id }()
	}
	wg.Wait()
	close(choices)
	first := ""
	for id := range choices {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatal("concurrent first requests selected different accounts")
		}
	}
}

func TestStandaloneClaudeSessionFailsOverUnavailableAccount(t *testing.T) {
	for _, reason := range []string{"disabled", "budget", "cooldown", "deleted"} {
		t.Run(reason, func(t *testing.T) {
			s, _, cfg, _ := testServer(t)
			s.cfg.SessionAffinity = true
			s.cfg.Routing = "round-robin"
			for _, id := range []string{"a", "b"} {
				if _, err := s.importAccount(id+".json", credential(id, 0)); err != nil {
					t.Fatal(err)
				}
			}
			body := claudeSessionBody(t, claudeSession, false)
			req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
			req.Header.Set("X-Api-Key", cfg.APIKeys[0])
			key := requestSessionKey(req, []byte(body), "claude")
			s.bindSession(key, "a.json")
			a := s.account("a.json")
			a.mu.Lock()
			switch reason {
			case "disabled":
				a.disabled = true
			case "budget":
				a.guard.Blocked = true
			case "cooldown":
				a.cooldown = time.Now().Add(time.Hour)
			case "deleted":
				a.deleted = true
			}
			a.mu.Unlock()
			out := httptest.NewRecorder()
			s.ServeHTTP(out, req)
			if out.Code != 200 {
				t.Fatalf("failover status=%d", out.Code)
			}
			s.mu.Lock()
			bound := s.affinity[key]
			s.mu.Unlock()
			if bound.id != "b.json" {
				t.Fatal("unavailable account retained affinity")
			}
		})
	}
}

func TestSessionBindingExpiryUsesNormalRouting(t *testing.T) {
	s, _, _, _ := testServer(t)
	s.cfg.SessionAffinity = true
	s.cfg.Routing = "fill-first"
	for _, id := range []string{"a", "b"} {
		if _, err := s.importAccount(id+".json", credential(id, 0)); err != nil {
			t.Fatal(err)
		}
	}
	s.affinity["expired"] = binding{"b.json", time.Now().Add(-time.Second)}
	if got := s.candidates("expired"); got[0].id != "a.json" {
		t.Fatal("expired affinity remained active")
	}
}

type affinityRetryTransport struct{ base *fakeUpstream }

func (f affinityRetryTransport) Do(ctx context.Context, req contract.HTTPRequest) (*http.Response, error) {
	if strings.HasSuffix(req.URL, "/chat/completions") && req.Headers.Get("Authorization") == "Bearer tid=a;proxy-ep=proxy.individual.githubcopilot.com" {
		response := fakeResponse(`{"error":{"type":"rate_limit_error"}}`)
		response.StatusCode = 429
		return response, nil
	}
	return f.base.Do(ctx, req)
}

func TestStandaloneClaudeSessionRebindsAfterUpstreamFailure(t *testing.T) {
	base := &fakeUpstream{counts: map[string]int{}, used: map[string]string{}, reset: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)}
	cfg := Config{ManagementKey: "synthetic-management-key", APIKeys: []string{"synthetic-api-key"}, Routing: "round-robin", SessionAffinity: true, Copilot: copilot.DefaultSettings()}
	s, err := New(cfg, filepath.Join(t.TempDir(), "config.yaml"), affinityRetryTransport{base}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"a", "b"} {
		if _, err := s.importAccount(id+".json", credential(id, 0)); err != nil {
			t.Fatal(err)
		}
	}
	body := claudeSessionBody(t, claudeSession, false)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("X-Api-Key", cfg.APIKeys[0])
	key := requestSessionKey(req, []byte(body), "claude")
	s.bindSession(key, "a.json")
	out := httptest.NewRecorder()
	s.ServeHTTP(out, req)
	if out.Code != 200 {
		t.Fatalf("retry status=%d", out.Code)
	}
	s.mu.Lock()
	bound := s.affinity[key]
	s.mu.Unlock()
	if bound.id != "b.json" {
		t.Fatal("successful retry did not rebind session")
	}
	if got := s.candidates(key); len(got) != 1 || got[0].id != "b.json" {
		t.Fatal("failed account cooldown bypassed")
	}
}

func TestRequestSessionKeyHashFallback(t *testing.T) {
	body := []byte(`{"model":"test-model","messages":[{"role":"user","content":"synthetic"}]}`)
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("X-Api-Key", "synthetic-key-a")
	key := requestSessionKey(req, body, "openai")
	if len(key) != 64 || key == requestSessionKey(req, []byte(`{"model":"test-model"}`), "openai") {
		t.Fatal("stable request signals were not hashed into the fallback key")
	}
	req.Header.Set("X-Api-Key", "synthetic-key-b")
	if requestSessionKey(req, body, "openai") == key {
		t.Fatal("fallback affinity leaked across API keys")
	}
	req.Header.Set("X-Api-Key", "synthetic-key-a")
	firstTurn := []byte(`{"model":"test-model","messages":[{"role":"user","content":"first prompt"}]}`)
	laterTurn := []byte(`{"model":"test-model","messages":[{"role":"user","content":"first prompt"},{"role":"assistant","content":"hi"},{"role":"user","content":"follow up"}]}`)
	if requestSessionKey(req, firstTurn, "openai") != requestSessionKey(req, laterTurn, "openai") {
		t.Fatal("stable request hash changed across conversation turns")
	}
	req.Header.Del("X-Api-Key")
	if requestSessionKey(req, body, "openai") != "" {
		t.Fatal("unauthenticated request received fallback affinity")
	}
}

func TestSessionBindingUsesConfiguredTTL(t *testing.T) {
	s, _, _, _ := testServer(t)
	s.cfg.SessionAffinity = true
	s.cfg.SessionAffinityTTL = 30 * time.Minute
	now := time.Now()
	s.mu.Lock()
	s.bindSessionLocked("configured-ttl", "account", now)
	bound := s.affinity["configured-ttl"]
	s.mu.Unlock()
	if !bound.expires.Equal(now.Add(30 * time.Minute)) {
		t.Fatalf("configured TTL not used: expires=%s", bound.expires)
	}
	s.cfg.SessionAffinityTTL = 0
	s.mu.Lock()
	s.bindSessionLocked("default-ttl", "account", now)
	bound = s.affinity["default-ttl"]
	s.mu.Unlock()
	if !bound.expires.Equal(now.Add(2 * time.Hour)) {
		t.Fatalf("default TTL mismatch: expires=%s", bound.expires)
	}
}

func TestSessionAffinityTTLConfiguration(t *testing.T) {
	s, _, cfg, path := testServer(t)
	if s.cfg.SessionAffinityTTL != 2*time.Hour {
		t.Fatalf("default TTL=%s", s.cfg.SessionAffinityTTL)
	}
	out := request(s, "PATCH", "/v0/management/config", cfg.ManagementKey, `{"session_affinity_ttl":"30m"}`)
	if out.Code != 200 || s.cfg.SessionAffinityTTL != 30*time.Minute {
		t.Fatalf("PATCH status=%d ttl=%s", out.Code, s.cfg.SessionAffinityTTL)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SessionAffinityTTL != 30*time.Minute {
		t.Fatalf("persisted TTL=%s", loaded.SessionAffinityTTL)
	}
	for _, body := range []string{
		`{"session_affinity_ttl":"0s"}`,
		`{"session_affinity_ttl_seconds":0}`,
		`{"session_affinity_ttl_seconds":-1}`,
		`{"session_affinity_ttl":"30m","session_affinity_ttl_seconds":1800}`,
	} {
		if out := request(s, "PATCH", "/v0/management/config", cfg.ManagementKey, body); out.Code != 400 {
			t.Fatalf("%s status=%d", body, out.Code)
		}
	}
}
