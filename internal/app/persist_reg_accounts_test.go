package app

import (
	"os"
	"strings"
	"testing"
)

func TestPersistRegUpdateAccountSuccessAndWriteFailureRollback(t *testing.T) {
	s, _, _, _ := prNewServer(t)
	a := prImport(t, s, "a.json", prCred(t, "update", 3, map[string]any{"proxy_url": "http://127.0.0.1:3128"}))
	disabled, priority := true, 55
	if err := s.updateAccount("a.json", &disabled, &priority); err != nil {
		t.Fatal(err)
	}
	disk := prReadJSON(t, prAuthFile(s, "a.json"))
	if disk["disabled"] != true || disk["priority"] != float64(55) || disk["github_access_token"] != "update" || disk["proxy_url"] != "http://127.0.0.1:3128" {
		t.Fatalf("disk=%v", disk)
	}
	info, err := os.Stat(prAuthFile(s, "a.json"))
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatal("credential file is not private", err)
	}
	a.mu.Lock()
	memDisabled, memPriority, rawPriority := a.disabled, a.priority, a.raw["priority"]
	a.mu.Unlock()
	if !memDisabled || memPriority != 55 || rawPriority != 55 {
		t.Fatalf("memory=%v/%d/%v", memDisabled, memPriority, rawPriority)
	}

	before := prRead(t, prAuthFile(s, "a.json"))
	restore := prBreakAuthDir(t, s)
	enabled, other := false, 7
	if err = s.updateAccount("a.json", &enabled, &other); err == nil {
		t.Fatal("write failure accepted")
	}
	restore()
	a.mu.Lock()
	memDisabled, memPriority, rawPriority, rawDisabled := a.disabled, a.priority, a.raw["priority"], a.raw["disabled"]
	a.mu.Unlock()
	if !memDisabled || memPriority != 55 || rawPriority != 55 || rawDisabled != true {
		t.Fatalf("rollback failed: %v/%d/%v/%v", memDisabled, memPriority, rawPriority, rawDisabled)
	}
	if string(prRead(t, prAuthFile(s, "a.json"))) != string(before) {
		t.Fatal("failed update changed the credential file")
	}
	if len(s.candidates("")) != 0 {
		t.Fatal("rolled-back disablement must keep the account unschedulable")
	}
	for _, err := range []error{s.updateAccount("missing.json", &enabled, nil)} {
		if err == nil {
			t.Fatal("missing account accepted")
		}
	}
	prNoTempFiles(t, s.dataDir)
}

func TestPersistRegDeleteAccount(t *testing.T) {
	s, up, _, _ := prNewServer(t)
	a := prImport(t, s, "a.json", prCred(t, "delete", 1, nil))
	if err := s.checkQuota(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.statePath("a.json")); err != nil {
		t.Fatal("state snapshot missing before delete", err)
	}
	for _, id := range []string{"../a.json", "", "a"} {
		if err := s.deleteAccount(id); err == nil {
			t.Fatalf("invalid id %q accepted", id)
		}
	}
	if err := s.deleteAccount("missing.json"); err == nil {
		t.Fatal("missing account accepted")
	}

	// A failing credential removal leaves the account fully intact.
	file := prAuthFile(s, "a.json")
	saved := prRead(t, file)
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := s.deleteAccount("a.json"); err == nil {
		t.Fatal("deletion of an unremovable credential reported success")
	}
	if err := os.WriteFile(file, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	intact := !a.deleted
	a.mu.Unlock()
	if s.account("a.json") != a || !intact {
		t.Fatal("failed delete changed memory")
	}

	if err := s.deleteAccount("a.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("credential file remains")
	}
	if _, err := os.Stat(s.statePath("a.json")); !os.IsNotExist(err) {
		t.Fatal("state file remains")
	}
	a.mu.Lock()
	deleted := a.deleted
	a.mu.Unlock()
	if !deleted || s.account("a.json") != nil || len(s.listAccounts()) != 0 || len(s.candidates("")) != 0 {
		t.Fatal("deleted account still routable")
	}
	if err := s.deleteAccount("a.json"); err == nil {
		t.Fatal("second delete accepted")
	}
	if err := s.updateAccount("a.json", nil, nil); err == nil {
		t.Fatal("update of a deleted account accepted")
	}
	if err := s.checkQuota(t.Context(), a); err == nil {
		t.Fatal("deleted account performed a quota check")
	}
	if up.quota.counts["delete"] != 1 {
		t.Fatalf("quota calls=%d", up.quota.counts["delete"])
	}

	// A recreated account of the same name starts clean, with no state inherited from the removed one.
	again := prImport(t, s, "a.json", prCred(t, "delete", 1, nil))
	again.mu.Lock()
	clean := again.quota.Subscription == nil && !again.guard.Blocked
	again.mu.Unlock()
	if !clean || strings.Contains(string(prRead(t, prAuthFile(s, "a.json"))), "budget_guard") {
		t.Fatal("recreated account inherited removed state")
	}
	prNoTempFiles(t, s.dataDir)
}
