package app

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func prKeyIDs(t *testing.T, s *Server) []map[string]string {
	t.Helper()
	out := prGuardedGet(t, s, "/v0/management/api-keys")
	var rows []map[string]string
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func prGuardedGet(t *testing.T, s *Server, path string) []byte {
	t.Helper()
	ch := make(chan []byte, 1)
	go func() { ch <- request(s, "GET", path, "management-test-key", "").Body.Bytes() }()
	select {
	case raw := <-ch:
		return raw
	case <-time.After(prTimeout):
		t.Fatalf("GET %s did not finish (deadlock?)\n%s", path, prStacks())
		return nil
	}
}

func prRequest(t *testing.T, s *Server, method, path, body string) (int, string) {
	t.Helper()
	type result struct {
		code int
		body string
	}
	ch := make(chan result, 1)
	go func() {
		out := request(s, method, path, "management-test-key", body)
		ch <- result{out.Code, out.Body.String()}
	}()
	select {
	case r := <-ch:
		return r.code, r.body
	case <-time.After(prTimeout):
		t.Fatalf("%s %s did not finish (deadlock?)\n%s", method, path, prStacks())
		return 0, ""
	}
}

func TestCheckQuotaStateWriteFailureLatchesBudgetGuard(t *testing.T) {
	s, up, _, _ := prNewServer(t)
	a := prImport(t, s, "a.json", prCred(t, "quota-latch", 0, nil))
	up.quota.used["quota-latch"] = "0"
	restore := prBreakState(t, s)
	if err := s.checkQuota(context.Background(), a); err == nil {
		t.Fatal("state write failure must be reported")
	}
	a.mu.Lock()
	blocked, reason := a.guard.Blocked, a.guard.Reason
	a.mu.Unlock()
	if !blocked || reason != "budget_state_write_failed" {
		t.Fatalf("guard=%v reason=%q", blocked, reason)
	}
	if len(s.candidates("")) != 0 {
		t.Fatal("latched account must not be schedulable")
	}

	restore()
	a.mu.Lock()
	a.nextQuota = time.Time{}
	a.mu.Unlock()
	if err := s.checkQuota(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	still := a.guard.Blocked
	a.mu.Unlock()
	if !still {
		t.Fatal("an allow observation without a verified new period must not release the latch")
	}
	raw, err := os.ReadFile(s.statePath("a.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"blocked":true`) {
		t.Fatalf("latched state was not persisted after recovery of the disk: %s", raw)
	}
	prNoTempFiles(t, s.dataDir)
}

func TestCheckQuotaStateWriteFailureIgnoresReplacedCredential(t *testing.T) {
	s, up, _, _ := prNewServer(t)
	old := prImport(t, s, "a.json", prCred(t, "old-quota", 0, nil))
	up.quota.used["old-quota"] = "0"
	restore := prBreakState(t, s)
	defer restore()
	up.quota.gate = make(chan struct{})
	up.quota.entered = make(chan struct{}, 1)
	done := prAsync(func() error { return s.checkQuota(context.Background(), old) })
	select {
	case <-up.quota.entered:
	case <-time.After(prTimeout):
		t.Fatal("quota request never started")
	}
	prImport(t, s, "a.json", prCred(t, "new-quota", 0, nil))
	close(up.quota.gate)
	prWaitErr(t, done, "stale quota check")
	if s.account("a.json").guard.Blocked {
		t.Fatal("late state-write failure latched the replacement credential")
	}
}

func TestPatchConfigPersistsAndAppliesTogether(t *testing.T) {
	s, _, _, path := prNewServer(t)
	code, body := prRequest(t, s, "PATCH", "/v0/management/config", `{"routing":"round-robin","session_affinity":true,"session_affinity_ttl_seconds":90,"proxy_url":"http://127.0.0.1:3128"}`)
	if code != 200 {
		t.Fatal(code, body)
	}
	got := prConfig(s)
	if got.Routing != "round-robin" || !got.SessionAffinity || got.SessionAffinityTTL != 90*time.Second || got.ProxyURL != "http://127.0.0.1:3128" {
		t.Fatalf("memory=%+v", got)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Routing != got.Routing || loaded.SessionAffinity != got.SessionAffinity || loaded.SessionAffinityTTL != got.SessionAffinityTTL || loaded.ProxyURL != got.ProxyURL {
		t.Fatalf("disk=%+v memory=%+v", loaded, got)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatal("configuration file is not private", err)
	}
	prNoTempFiles(t, s.dataDir)
}

func TestPatchConfigWriteFailureKeepsMemoryAndReleasesLocks(t *testing.T) {
	s, _, _, _ := prNewServer(t)
	before := prConfig(s)
	restore := prBreakConfig(t, s)
	code, body := prRequest(t, s, "PATCH", "/v0/management/config", `{"routing":"round-robin","session_affinity":true}`)
	if code != 500 || strings.Contains(body, s.configPath) {
		t.Fatalf("code=%d body=%s", code, body)
	}
	after := prConfig(s)
	if after.Routing != before.Routing || after.SessionAffinity != before.SessionAffinity {
		t.Fatalf("failed save leaked into memory: %+v", after)
	}
	restore()
	if code, body = prRequest(t, s, "PATCH", "/v0/management/config", `{"routing":"round-robin"}`); code != 200 {
		t.Fatalf("server did not recover after a failed save: %d %s", code, body)
	}
}

func TestPatchConfigRejectsInvalidInputWithoutChange(t *testing.T) {
	s, _, _, path := prNewServer(t)
	before := prConfig(s)
	for _, body := range []string{
		`{"routing":"random"}`,
		`{"proxy_url":"ftp://example.invalid"}`,
		`{"session_affinity_ttl_seconds":-1}`,
		`{"session_affinity_ttl":"5s","session_affinity_ttl_seconds":5}`,
		`{"routing":"fill-first"} {}`,
	} {
		if code, out := prRequest(t, s, "PATCH", "/v0/management/config", body); code != 400 {
			t.Fatalf("%s accepted: %d %s", body, code, out)
		}
	}
	if after := prConfig(s); after.Routing != before.Routing || after.ProxyURL != before.ProxyURL || after.SessionAffinityTTL != before.SessionAffinityTTL {
		t.Fatalf("rejected request changed configuration: %+v", after)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("rejected request wrote the configuration file")
	}
}

func TestAPIKeyCreateAndDeletePersist(t *testing.T) {
	s, _, cfg, path := prNewServer(t)
	code, body := prRequest(t, s, "POST", "/v0/management/api-keys", "")
	if code != 200 {
		t.Fatal(code, body)
	}
	var created map[string]string
	if err := json.Unmarshal([]byte(body), &created); err != nil || len(created["key"]) < 16 {
		t.Fatal("generated key missing", err)
	}
	if got := prConfig(s).APIKeys; len(got) != 2 || got[0] != cfg.APIKeys[0] || got[1] != created["key"] {
		t.Fatalf("memory keys=%v", got)
	}
	loaded, err := LoadConfig(path)
	if err != nil || len(loaded.APIKeys) != 2 || loaded.APIKeys[1] != created["key"] {
		t.Fatal("created key not persisted", err)
	}
	rows := prKeyIDs(t, s)
	if len(rows) != 2 || strings.Contains(string(prMustMarshal(t, rows)), created["key"]) {
		t.Fatalf("listing must hide full keys: %v", rows)
	}
	code, body = prRequest(t, s, "DELETE", "/v0/management/api-keys?id="+rows[1]["id"], "")
	if code != 200 {
		t.Fatal(code, body)
	}
	if got := prConfig(s).APIKeys; len(got) != 1 || got[0] != cfg.APIKeys[0] {
		t.Fatalf("memory keys after delete=%v", got)
	}
	if loaded, err = LoadConfig(path); err != nil || len(loaded.APIKeys) != 1 || loaded.APIKeys[0] != cfg.APIKeys[0] {
		t.Fatal("delete not persisted", err)
	}
	if request(s, "GET", "/v1/models", created["key"], "").Code != 401 {
		t.Fatal("deleted key still authenticates")
	}
	prNoTempFiles(t, s.dataDir)
}

func prMustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestAPIKeyWriteFailuresAndLastKeyKeepState(t *testing.T) {
	s, _, cfg, _ := prNewServer(t)
	restore := prBreakConfig(t, s)
	if code, body := prRequest(t, s, "POST", "/v0/management/api-keys", ""); code != 500 {
		t.Fatalf("create under write failure: %d %s", code, body)
	}
	if got := prConfig(s).APIKeys; len(got) != 1 {
		t.Fatalf("failed create leaked key into memory: %v", got)
	}
	restore()

	rows := prKeyIDs(t, s)
	if code, body := prRequest(t, s, "DELETE", "/v0/management/api-keys?id="+rows[0]["id"], ""); code != 400 {
		t.Fatalf("last key deleted: %d %s", code, body)
	}
	if code, _ := prRequest(t, s, "POST", "/v0/management/api-keys", ""); code != 200 {
		t.Fatal("server did not recover after failed create")
	}
	rows = prKeyIDs(t, s)
	restore = prBreakConfig(t, s)
	if code, body := prRequest(t, s, "DELETE", "/v0/management/api-keys?id="+rows[0]["id"], ""); code != 500 {
		t.Fatalf("delete under write failure: %d %s", code, body)
	}
	if got := prConfig(s).APIKeys; len(got) != 2 || got[0] != cfg.APIKeys[0] {
		t.Fatalf("failed delete changed memory: %v", got)
	}
	restore()
}
