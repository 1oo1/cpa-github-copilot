package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/1oo1/cpa-github-copilot/internal/contract"
)

type prLock struct {
	name string
	mu   *sync.Mutex
}

// prWriteWhileHeld proves the disk write needs neither s.mu nor a.mu. The operation is parked
// just before its write (on persistMu, which the test owns), both mutexes are verified free,
// then the test takes the listed ones itself and lets the write proceed: the file must change
// while they are held. Only the final in-memory commit may wait for them.
func prWriteWhileHeld(t *testing.T, s *Server, frame string, free, hold []prLock, path string, wrote func([]byte, error) bool, op func() error) error {
	t.Helper()
	release := prHoldPersist(t, s)
	done := prAsync(op)
	prWaitParked(t, frame)
	for _, l := range free {
		prFree(t, l.name, l.mu)
	}
	for _, l := range hold {
		l.mu.Lock()
	}
	var once sync.Once
	unlock := func() {
		once.Do(func() {
			for i := len(hold) - 1; i >= 0; i-- {
				hold[i].mu.Unlock()
			}
		})
	}
	defer unlock()
	release()
	prWaitFile(t, path, wrote)
	unlock()
	return prWaitErr(t, done, frame)
}

func prContains(sub string) func([]byte, error) bool {
	return func(raw []byte, err error) bool { return err == nil && strings.Contains(string(raw), sub) }
}

func TestPersistRegNoDiskWriteWhileStateMutexesHeld(t *testing.T) {
	t.Run("updateAccount", func(t *testing.T) {
		s, _, _, _ := prNewServer(t)
		a := prImport(t, s, "a.json", prCred(t, "u", 1, nil))
		locks := []prLock{{"s.mu", &s.mu}, {"a.mu", &a.mu}}
		priority := 77
		err := prWriteWhileHeld(t, s, "(*Server).updateAccount", locks, locks, prAuthFile(s, "a.json"), prContains(`"priority":77`), func() error { return s.updateAccount("a.json", nil, &priority) })
		if err != nil {
			t.Fatal(err)
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.priority != 77 {
			t.Fatal("commit missing after write")
		}
	})
	t.Run("refresh persistAuth", func(t *testing.T) {
		s, _, _, _ := prNewServer(t)
		a := prImport(t, s, "a.json", prCred(t, "r", 1, nil))
		locks := []prLock{{"s.mu", &s.mu}, {"a.mu", &a.mu}}
		err := prWriteWhileHeld(t, s, "(*Server).persistAuth", locks, locks, prAuthFile(s, "a.json"), prContains("renewed"), func() error { return s.refresh(context.Background(), a, true) })
		if err != nil {
			t.Fatal(err)
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if !strings.Contains(string(a.auth.StorageJSON), "renewed") {
			t.Fatal("commit missing after write")
		}
	})
	t.Run("catalog persistAuth", func(t *testing.T) {
		s, _, _, _ := prNewServer(t)
		a := prImport(t, s, "a.json", prStaleCred(t, "c"))
		locks := []prLock{{"s.mu", &s.mu}, {"a.mu", &a.mu}}
		err := prWriteWhileHeld(t, s, "(*Server).persistAuth", locks, locks, prAuthFile(s, "a.json"), prContains("updated-model"), func() error { return s.catalog(context.Background(), a, true) })
		if err != nil {
			t.Fatal(err)
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if ids := modelIDs(a.raw); len(ids) != 1 || ids[0] != "updated-model" {
			t.Fatalf("models=%v", ids)
		}
	})
	t.Run("checkQuota state snapshot", func(t *testing.T) {
		s, _, _, _ := prNewServer(t)
		a := prImport(t, s, "a.json", prCred(t, "q", 1, nil))
		locks := []prLock{{"s.mu", &s.mu}, {"a.mu", &a.mu}}
		err := prWriteWhileHeld(t, s, "(*Server).saveStateSnapshot", locks, locks, s.statePath("a.json"), prContains(a.identity), func() error { return s.checkQuota(context.Background(), a) })
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("deleteAccount", func(t *testing.T) {
		s, _, _, _ := prNewServer(t)
		a := prImport(t, s, "a.json", prCred(t, "d", 1, nil))
		// delete reads a.mu after taking persistMu, so only s.mu can be held across its removal.
		free := []prLock{{"s.mu", &s.mu}, {"a.mu", &a.mu}}
		hold := []prLock{{"s.mu", &s.mu}}
		gone := func(_ []byte, err error) bool { return os.IsNotExist(err) }
		if err := prWriteWhileHeld(t, s, "(*Server).deleteAccount", free, hold, prAuthFile(s, "a.json"), gone, func() error { return s.deleteAccount("a.json") }); err != nil {
			t.Fatal(err)
		}
		if s.account("a.json") != nil {
			t.Fatal("account still routable")
		}
	})
}

// Operations whose read phase takes s.mu after persistMu cannot be observed mid-write, so their
// proof is that every mutex is free while they wait for persistence and nothing was committed.
func TestPersistRegNoMutexHeldWhileWaitingForPersistence(t *testing.T) {
	park := func(t *testing.T, s *Server, frame string, op func()) (release func(), finished func()) {
		t.Helper()
		rel := prHoldPersist(t, s)
		done := prAsync(func() error { op(); return nil })
		prWaitParked(t, frame)
		return rel, func() { rel(); prWaitErr(t, done, frame) }
	}
	t.Run("importAccount", func(t *testing.T) {
		s, _, _, _ := prNewServer(t)
		old := prImport(t, s, "a.json", prCred(t, "same", 1, nil))
		_, finish := park(t, s, "(*Server).importAccount", func() { _, _ = s.importAccount("a.json", prCred(t, "same", 9, nil)) })
		prFree(t, "s.mu", &s.mu)
		prFree(t, "previous a.mu", &old.mu)
		if s.account("a.json") != old {
			t.Fatal("replacement installed before the credential write")
		}
		if got := prReadJSON(t, prAuthFile(s, "a.json"))["priority"]; got != float64(1) {
			t.Fatalf("credential rewritten early: %v", got)
		}
		finish()
		if s.account("a.json") == old {
			t.Fatal("replacement missing")
		}
	})
	t.Run("PATCH config", func(t *testing.T) {
		s, _, _, _ := prNewServer(t)
		_, finish := park(t, s, "managementRoutes", func() {
			prRequest(t, s, "PATCH", "/v0/management/config", `{"routing":"round-robin"}`)
		})
		prFree(t, "s.mu", &s.mu)
		if prConfig(s).Routing != "fill-first" {
			t.Fatal("configuration committed before the file write")
		}
		if _, err := os.Stat(s.configPath); err == nil {
			t.Fatal("configuration written before persistence gate opened")
		}
		finish()
		if prConfig(s).Routing != "round-robin" {
			t.Fatal("commit missing")
		}
	})
	t.Run("API key create", func(t *testing.T) {
		s, _, _, _ := prNewServer(t)
		_, finish := park(t, s, "managementRoutes", func() { prRequest(t, s, "POST", "/v0/management/api-keys", "") })
		prFree(t, "s.mu", &s.mu)
		if len(prConfig(s).APIKeys) != 1 {
			t.Fatal("key committed before the file write")
		}
		finish()
		if len(prConfig(s).APIKeys) != 2 {
			t.Fatal("commit missing")
		}
	})
	t.Run("API key delete", func(t *testing.T) {
		s, _, cfg, _ := prNewServer(t)
		if code, body := prRequest(t, s, "POST", "/v0/management/api-keys", ""); code != 200 {
			t.Fatal(code, body)
		}
		id := identity(map[string]any{"github_access_token": cfg.APIKeys[0]})
		_, finish := park(t, s, "managementRoutes", func() { prRequest(t, s, "DELETE", "/v0/management/api-keys?id="+id, "") })
		prFree(t, "s.mu", &s.mu)
		if len(prConfig(s).APIKeys) != 2 {
			t.Fatal("key removed before the file write")
		}
		finish()
		if len(prConfig(s).APIKeys) != 1 {
			t.Fatal("commit missing")
		}
	})
}

func TestPersistRegImportReplacementSemantics(t *testing.T) {
	s, _, _, _ := prNewServer(t)
	old := prImport(t, s, "a.json", prCred(t, "same-identity", 1, nil))
	old.mu.Lock()
	old.guard = Guard{Blocked: true, Reason: "additional_budget_exceeded", ResetAt: time.Now().Add(24 * time.Hour)}
	old.quota.Subscription = &contract.QuotaSubscription{Plan: "latched-plan"}
	oldEpoch := old.epoch
	old.mu.Unlock()

	next := prImport(t, s, "a.json", prCred(t, "same-identity", 9, nil))
	if next == old {
		t.Fatal("account object was not replaced")
	}
	next.mu.Lock()
	inherited := next.guard.Blocked && next.quota.Subscription != nil && next.quota.Subscription.Plan == "latched-plan"
	priority, epoch := next.priority, next.epoch
	next.mu.Unlock()
	old.mu.Lock()
	deleted, oldNow := old.deleted, old.epoch
	old.mu.Unlock()
	if !inherited || priority != 9 || epoch <= oldEpoch || !deleted || oldNow <= oldEpoch {
		t.Fatalf("inherited=%v priority=%d epoch=%d deleted=%v oldEpoch=%d", inherited, priority, epoch, deleted, oldNow)
	}
	if got := prReadJSON(t, prAuthFile(s, "a.json"))["priority"]; got != float64(9) {
		t.Fatalf("disk priority=%v", got)
	}
	if err := s.updateAccount("a.json", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.refresh(context.Background(), old, true); err == nil {
		t.Fatal("replaced account must not refresh")
	}

	other := prImport(t, s, "a.json", prCred(t, "different-identity", 2, nil))
	other.mu.Lock()
	blocked := other.guard.Blocked
	other.mu.Unlock()
	if blocked || len(s.listAccounts()) != 1 {
		t.Fatal("guard leaked across a different credential, or duplicate listing")
	}
	prNoTempFiles(t, s.dataDir)
}

func TestPersistRegImportWriteFailureKeepsPreviousAccount(t *testing.T) {
	s, _, _, _ := prNewServer(t)
	old := prImport(t, s, "a.json", prCred(t, "keep", 1, nil))
	before := prRead(t, prAuthFile(s, "a.json"))
	restore := prBreakAuthDir(t, s)
	if _, err := s.importAccount("a.json", prCred(t, "keep", 5, nil)); err == nil {
		t.Fatal("write failure accepted")
	}
	restore()
	if s.account("a.json") != old {
		t.Fatal("failed import replaced the account")
	}
	old.mu.Lock()
	deleted, priority := old.deleted, old.priority
	old.mu.Unlock()
	if deleted || priority != 1 || string(prRead(t, prAuthFile(s, "a.json"))) != string(before) {
		t.Fatal("failed import changed memory or disk")
	}
}

func TestPersistRegRefreshAndCatalogPersistAuth(t *testing.T) {
	s, up, _, _ := prNewServer(t)
	a := prImport(t, s, "a.json", prCred(t, "long-term", 41, map[string]any{"proxy_url": "http://127.0.0.1:3128"}))
	disabled := true
	if err := s.updateAccount("a.json", &disabled, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.refresh(context.Background(), a, true); err != nil {
		t.Fatal(err)
	}
	disk := prReadJSON(t, prAuthFile(s, "a.json"))
	if disk["github_access_token"] != "long-term" || disk["priority"] != float64(41) || disk["disabled"] != true || disk["proxy_url"] != "http://127.0.0.1:3128" {
		t.Fatalf("refresh lost long-term credential or controls: %v", disk)
	}
	if !strings.Contains(fmt.Sprint(disk["copilot_session_token"]), "renewed") {
		t.Fatal("renewed session token not persisted")
	}
	a.mu.Lock()
	memToken := string(a.auth.StorageJSON)
	a.mu.Unlock()
	if !strings.Contains(memToken, "renewed") {
		t.Fatal("memory not updated after persistence")
	}

	// Write failure: the error surfaces, memory and disk stay on the old credential, retry is scheduled soon.
	before := prRead(t, prAuthFile(s, "a.json"))
	restore := prBreakAuthDir(t, s)
	err := s.refresh(context.Background(), a, true)
	restore()
	if err == nil {
		t.Fatal("refresh hid a persistence failure")
	}
	a.mu.Lock()
	retry := time.Until(a.timeRefresh)
	a.mu.Unlock()
	if retry <= 0 || retry > 2*time.Minute || string(prRead(t, prAuthFile(s, "a.json"))) != string(before) {
		t.Fatalf("retry=%v or disk changed", retry)
	}

	// Catalog: stale cache is rediscovered and persisted; a failed write keeps the cached models.
	stale := prImport(t, s, "b.json", prStaleCred(t, "catalog"))
	restore = prBreakAuthDir(t, s)
	if err = s.catalog(context.Background(), stale, true); err == nil {
		t.Fatal("catalog hid a persistence failure")
	}
	restore()
	stale.mu.Lock()
	ids := modelIDs(stale.raw)
	stale.mu.Unlock()
	if len(ids) != 1 || ids[0] != "test-model" || strings.Contains(string(prRead(t, prAuthFile(s, "b.json"))), "updated-model") {
		t.Fatalf("failed catalog persistence leaked: %v", ids)
	}
	if err = s.catalog(context.Background(), stale, true); err != nil {
		t.Fatal(err)
	}
	stale.mu.Lock()
	ids = modelIDs(stale.raw)
	stale.mu.Unlock()
	if len(ids) != 1 || ids[0] != "updated-model" || !strings.Contains(string(prRead(t, prAuthFile(s, "b.json"))), "updated-model") {
		t.Fatalf("catalog not persisted: %v", ids)
	}

	// Upstream failure never touches the stored credential.
	up.mu.Lock()
	up.failToken = true
	up.mu.Unlock()
	before = prRead(t, prAuthFile(s, "a.json"))
	if err = s.refresh(context.Background(), a, true); err == nil || string(prRead(t, prAuthFile(s, "a.json"))) != string(before) {
		t.Fatal("upstream refresh failure changed the stored credential")
	}
	prNoTempFiles(t, s.dataDir)
}

func TestPersistRegConcurrentOperationsDoNotDeadlock(t *testing.T) {
	s, _, cfg, path := prNewServer(t)
	names := []string{"a.json", "b.json", "c.json"}
	for i, name := range names {
		prImport(t, s, name, prStaleCred(t, fmt.Sprintf("tok-%d", i)))
	}
	prImport(t, s, "victim.json", prCred(t, "victim", 0, nil))

	const rounds = 12
	var wg sync.WaitGroup
	spawn := func(fn func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range rounds {
				fn(i)
			}
		}()
	}
	for _, name := range names {
		spawn(func(i int) {
			disabled, priority := i%2 == 1, i
			_ = s.updateAccount(name, &disabled, &priority)
		})
		spawn(func(i int) {
			a := s.account(name)
			if a == nil {
				return
			}
			_ = s.checkQuota(context.Background(), a)
			_ = s.refresh(context.Background(), a, i%3 == 0)
			_ = s.catalog(context.Background(), a, i%2 == 0)
		})
		spawn(func(i int) {
			_, _ = s.importAccount(name, prStaleCred(t, "tok-"+name))
		})
	}
	spawn(func(int) { _ = s.listAccounts(); _ = s.candidates("session") })
	spawn(func(int) { request(s, "GET", "/v0/management/accounts", cfg.ManagementKey, "") })
	spawn(func(i int) {
		if out := request(s, "PATCH", "/v0/management/config", cfg.ManagementKey, fmt.Sprintf(`{"session_affinity_ttl_seconds":%d}`, 60+i)); out.Code != 200 {
			t.Errorf("PATCH config: %d", out.Code)
		}
	})
	spawn(func(int) {
		out := request(s, "POST", "/v0/management/api-keys", cfg.ManagementKey, "")
		var created map[string]string
		if json.Unmarshal(out.Body.Bytes(), &created) != nil || out.Code != 200 {
			t.Errorf("create key: %d", out.Code)
			return
		}
		id := identity(map[string]any{"github_access_token": created["key"]})
		if out = request(s, "DELETE", "/v0/management/api-keys?id="+id, cfg.ManagementKey, ""); out.Code != 200 {
			t.Errorf("delete key: %d", out.Code)
		}
	})
	spawn(func(int) {
		request(s, "POST", "/v1/chat/completions", cfg.APIKeys[0], `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	})
	wg.Add(1)
	go func() { defer wg.Done(); _ = s.deleteAccount("victim.json") }()

	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(2 * prTimeout):
		t.Fatalf("concurrent persistence operations deadlocked\n%s", prStacks())
	}

	if s.account("victim.json") != nil {
		t.Fatal("deleted account reappeared")
	}
	for _, name := range names {
		a := s.account(name)
		if a == nil {
			t.Fatalf("%s lost", name)
		}
		var raw map[string]any
		if err := json.Unmarshal(prRead(t, prAuthFile(s, name)), &raw); err != nil || identity(raw) != a.identity {
			t.Fatalf("%s on disk is corrupt or foreign: %v", name, err)
		}
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := prConfig(s); len(loaded.APIKeys) != len(got.APIKeys) || loaded.SessionAffinityTTL != got.SessionAffinityTTL {
		t.Fatalf("disk and memory configuration diverged: %+v vs %+v", loaded, got)
	}
	prNoTempFiles(t, s.dataDir)
}
