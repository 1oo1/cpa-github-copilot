package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requestFrom(s *Server, remote, forwarded, path, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.RemoteAddr = remote
	if forwarded != "" {
		req.Header.Set("X-Forwarded-For", forwarded)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	out := httptest.NewRecorder()
	s.ServeHTTP(out, req)
	return out
}

func TestSecurityHeadersAndUIPolicy(t *testing.T) {
	s, _, _, _ := testServer(t)
	out := request(s, "GET", "/", "", "")
	csp := out.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "connect-src 'self'") {
		t.Fatalf("csp=%q", csp)
	}
	if out.Header().Get("X-Content-Type-Options") != "nosniff" || out.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("missing UI hardening headers")
	}
	if request(s, "GET", "/v1/models", "", "").Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("API responses must disable MIME sniffing")
	}
	policy := uiContentSecurityPolicy([]byte("<script type=module>alert(1)</script><SCRIPT>b()</SCRIPT >"))
	if strings.Count(policy, "'sha256-") != 2 || strings.Contains(policy, "unsafe-eval") {
		t.Fatalf("inline scripts not pinned: %q", policy)
	}
}

func TestAuthFailuresAreRateLimitedPerScopeAndClient(t *testing.T) {
	s, _, cfg, _ := testServer(t)
	attacker := "198.51.100.7:5000"
	for range authFailureLimit {
		if out := requestFrom(s, attacker, "", "/v0/management/usage", "wrong-management-key"); out.Code != 401 {
			t.Fatal(out.Code)
		}
	}
	out := requestFrom(s, attacker, "", "/v0/management/usage", cfg.ManagementKey)
	if out.Code != 429 || out.Header().Get("Retry-After") == "" {
		t.Fatalf("blocked client accepted a fresh guess: %d", out.Code)
	}
	if out := requestFrom(s, "198.51.100.8:5000", "", "/v0/management/usage", cfg.ManagementKey); out.Code != 200 {
		t.Fatal("other clients must not be blocked", out.Code)
	}
	if out := requestFrom(s, attacker, "", "/v0/management/usage", cfg.ManagementKey); out.Code != 200 {
		t.Fatal("a key already proven elsewhere must keep working", out.Code)
	}
	if out := requestFrom(s, attacker, "", "/v1/models", cfg.APIKeys[0]); out.Code != 200 {
		t.Fatal("management failures must not block API keys", out.Code)
	}
	for range authFailureLimit * 2 {
		requestFrom(s, "198.51.100.9:5000", "", "/v0/management/usage", "")
	}
	if out := requestFrom(s, "198.51.100.9:5000", "", "/v0/management/usage", cfg.ManagementKey); out.Code != 200 {
		t.Fatal("requests without credentials must not count", out.Code)
	}
	var logs []Log
	_ = json.Unmarshal(request(s, "GET", "/v0/management/logs", cfg.ManagementKey, "").Body.Bytes(), &logs)
	found := false
	for _, log := range logs {
		found = found || (log.Event == "auth.management.client_blocked" && log.Client == "198.51.100.7")
	}
	if !found {
		t.Fatal("block not recorded")
	}
}

func TestForwardedForTrustedOnlyFromConfiguredProxies(t *testing.T) {
	s, _, _, _ := testServer(t)
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.5:1234"
	req.Header.Set("X-Forwarded-For", "192.0.2.1")
	if got := s.clientKey(req).String(); got != "203.0.113.5" {
		t.Fatalf("untrusted peer spoofed its address: %s", got)
	}
	s.trustedProxies, _ = parseTrustedProxies([]string{"10.0.0.0/8"})
	req.RemoteAddr = "10.1.2.3:1234"
	req.Header.Set("X-Forwarded-For", "192.0.2.1, 198.51.100.4, 10.9.9.9")
	if got := s.clientKey(req).String(); got != "198.51.100.4" {
		t.Fatalf("rightmost untrusted hop expected: %s", got)
	}
	req.RemoteAddr = "[2001:db8:1:2:3:4:5:6]:1"
	req.Header.Del("X-Forwarded-For")
	if got := s.clientKey(req).String(); got != "2001:db8:1:2::" {
		t.Fatalf("IPv6 clients must be grouped by /64: %s", got)
	}
	if _, err := parseTrustedProxies([]string{"proxy.example"}); err == nil {
		t.Fatal("hostnames are not trusted proxies")
	}
}

func TestInferenceBodyLimitAndMemoryGate(t *testing.T) {
	s, _, cfg, _ := testServer(t)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(strings.Repeat(" ", maxInferenceBody+1)))
	req.Header.Set("Authorization", "Bearer "+cfg.APIKeys[0])
	out := httptest.NewRecorder()
	s.ServeHTTP(out, req)
	if out.Code != http.StatusRequestEntityTooLarge {
		t.Fatal(out.Code)
	}

	gate := newMemoryGate(100)
	first, ok := gate.acquire(context.Background(), 1000)
	if !ok {
		t.Fatal("an idle gate must admit an oversized request")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, ok = gate.acquire(ctx, 10); ok {
		t.Fatal("full gate admitted a request")
	}
	admitted := make(chan *memoryLease)
	go func() { lease, _ := gate.acquire(context.Background(), 60); admitted <- lease }()
	first.resize(40)
	second := <-admitted
	if gate.used != 100 {
		t.Fatalf("used=%d", gate.used)
	}
	first.release()
	second.release()
	second.release()
	if gate.used != 0 {
		t.Fatalf("release must be idempotent: used=%d", gate.used)
	}
}

func TestUsageIsPersistedOutsideTheRequestPath(t *testing.T) {
	s, _, _, path := testServer(t)
	usagePath := filepath.Join(filepath.Dir(path), "usage.json")
	s.recordUsage("a.json", "test-model", 200, time.Now(), 3, 2)
	if _, err := os.Stat(usagePath); !os.IsNotExist(err) {
		t.Fatal("request completion must not write usage synchronously")
	}
	s.Close()
	raw, err := os.ReadFile(usagePath)
	var usage Usage
	if err != nil || json.Unmarshal(raw, &usage) != nil || usage.Requests != 1 || usage.Input != 3 {
		t.Fatalf("usage not flushed on close: %s %v", raw, err)
	}
}
