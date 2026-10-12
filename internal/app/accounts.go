package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/1oo1/cpa-github-copilot/internal/contract"
	"github.com/1oo1/cpa-github-copilot/internal/copilot"
	"github.com/1oo1/cpa-github-copilot/internal/transport"
)

type Guard struct {
	Blocked bool      `json:"blocked"`
	Reason  string    `json:"reason,omitempty"`
	ResetAt time.Time `json:"reset_at,omitempty"`
}
type accountState struct {
	Identity string                      `json:"identity"`
	Guard    Guard                       `json:"guard"`
	Quota    contract.QuotaFetchResponse `json:"quota"`
}
type account struct {
	id                               string
	mu                               sync.Mutex
	auth                             contract.AuthData
	raw                              map[string]any
	epoch                            uint64
	deleted                          bool
	disabled                         bool
	priority                         int
	proxy                            string
	identity                         string
	guard                            Guard
	quota                            contract.QuotaFetchResponse
	nextQuota, timeRefresh, cooldown time.Time
	nextModels                       time.Time
	cooldownKind                     string
	quotaFlight, refreshFlight       chan struct{}
}
type accountView struct {
	ID       string                      `json:"id"`
	Name     string                      `json:"name"`
	Account  string                      `json:"account"`
	Disabled bool                        `json:"disabled"`
	Priority int                         `json:"priority"`
	Guard    Guard                       `json:"budget_guard"`
	Quota    contract.QuotaFetchResponse `json:"quota"`
	Models   []string                    `json:"models"`
	Cooldown time.Time                   `json:"cooldown_until,omitempty"`
}

func identity(raw map[string]any) string {
	token, _ := raw["github_access_token"].(string)
	if token == "" {
		token, _ = raw["refresh"].(string)
	}
	host, _ := raw["github_host"].(string)
	sum := sha256.Sum256([]byte(host + "\x00" + token))
	return hex.EncodeToString(sum[:])
}
func validName(name string) bool {
	return name != "" && len(name) < 160 && filepath.Base(name) == name && !strings.ContainsAny(name, "/\\\x00\r\n") && strings.HasSuffix(name, ".json") && !strings.HasPrefix(name, ".")
}
func (s *Server) importAccount(name string, body []byte) (string, error) {
	if !validName(name) {
		return "", fmt.Errorf("invalid credential filename")
	}
	auth, err := s.engine.Parse(name, body)
	if err != nil {
		return "", fmt.Errorf("invalid Copilot credential")
	}
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return "", fmt.Errorf("invalid credential")
	}
	if v, exists := raw["disabled"]; exists {
		if _, ok := v.(bool); !ok {
			return "", fmt.Errorf("disabled must be boolean")
		}
	}
	if v, exists := raw["proxy_url"]; exists {
		if _, ok := v.(string); !ok {
			return "", fmt.Errorf("proxy_url must be a string")
		}
	}
	if v, exists := raw["priority"]; exists {
		n, ok := v.(float64)
		if !ok || math.Trunc(n) != n || n < -1000000 || n > 1000000 {
			return "", fmt.Errorf("priority must be an integer between -1000000 and 1000000")
		}
	}
	normalized := map[string]any{}
	if json.Unmarshal(auth.StorageJSON, &normalized) != nil {
		return "", fmt.Errorf("invalid credential")
	}
	for _, key := range []string{"disabled", "proxy_url", "priority", "budget_guard_migration"} {
		if value, ok := raw[key]; ok {
			normalized[key] = value
		}
	}
	raw = normalized
	body, err = json.Marshal(raw)
	if err != nil {
		return "", fmt.Errorf("invalid credential")
	}
	// Parse once more so the engine and persisted account share normalized host/token identity.
	auth, err = s.engine.Parse(name, body)
	if err != nil {
		return "", err
	}
	auth.ID = name
	a := &account{id: name, auth: auth, raw: raw, epoch: 1, identity: identity(raw)}
	a.disabled, _ = raw["disabled"].(bool)
	a.proxy, _ = raw["proxy_url"].(string)
	if n, ok := raw["priority"].(float64); ok {
		a.priority = int(n)
	}
	if marker, ok := raw["budget_guard_migration"]; ok {
		encoded, _ := json.Marshal(marker)
		var migrated Guard
		if json.Unmarshal(encoded, &migrated) != nil {
			return "", fmt.Errorf("invalid imported budget state")
		}
		if migrated.Blocked {
			a.guard = migrated
		}
	}
	a.timeRefresh = auth.NextRefreshAfter
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	s.mu.Lock()
	previous := s.accounts[name]
	s.mu.Unlock()
	if previous != nil {
		previous.mu.Lock()
		a.epoch = previous.epoch + 1
		if a.identity == previous.identity {
			a.guard = previous.guard
			a.quota = previous.quota
		}
		previous.mu.Unlock()
	} else if rawState, readErr := os.ReadFile(s.statePath(name)); readErr == nil {
		var state accountState
		if json.Unmarshal(rawState, &state) != nil || len(state.Identity) != 64 {
			return "", fmt.Errorf("invalid budget state; repair state file before using this account")
		}
		if state.Identity == a.identity {
			a.guard = state.Guard
			a.quota = state.Quota
			a.quota.Stale = true
		}
	} else if !os.IsNotExist(readErr) {
		return "", fmt.Errorf("cannot read budget state")
	}
	if err = atomicWrite(filepath.Join(s.authDir, name), body); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accounts[name] != previous {
		return "", fmt.Errorf("account changed during import")
	}
	if previous != nil {
		previous.mu.Lock()
		previous.deleted = true
		previous.epoch++
		previous.mu.Unlock()
	}

	s.accounts[name] = a
	return name, nil
}
func (s *Server) statePath(id string) string { return filepath.Join(s.dataDir, "state", id) }
func (s *Server) saveStateSnapshot(id string, state accountState) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	return atomicWrite(s.statePath(id), raw)
}
func (s *Server) listAccounts() []accountView {
	s.mu.Lock()
	accounts := make([]*account, 0, len(s.accounts))
	for _, a := range s.accounts {
		accounts = append(accounts, a)
	}
	s.mu.Unlock()
	result := make([]accountView, 0, len(accounts))
	for _, a := range accounts {
		a.mu.Lock()
		name, _ := a.raw["account"].(string)
		quota := a.quota
		if time.Now().After(a.nextQuota) {
			quota.Stale = true
		}
		result = append(result, accountView{ID: a.auth.ID, Name: a.auth.FileName, Account: name, Disabled: a.disabled, Priority: a.priority, Guard: a.guard, Quota: quota, Models: modelIDs(a.raw), Cooldown: a.cooldown})
		a.mu.Unlock()
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
func modelIDs(raw map[string]any) []string {
	var out []string
	if models, ok := raw["models"].([]any); ok {
		for _, model := range models {
			if m, ok := model.(map[string]any); ok {
				if id, ok := m["id"].(string); ok && copilot.IsSupportedCopilotModelID(id) {
					out = append(out, id)
				}
			}
		}
	}
	return out
}
func (s *Server) account(id string) *account { s.mu.Lock(); defer s.mu.Unlock(); return s.accounts[id] }
func (s *Server) upstreamContext(ctx context.Context, a *account) context.Context {
	a.mu.Lock()
	proxy := a.proxy
	a.mu.Unlock()
	if proxy == "" {
		s.mu.Lock()
		proxy = s.cfg.ProxyURL
		s.mu.Unlock()
	}
	return transport.WithProxy(ctx, proxy)
}
func (s *Server) refresh(ctx context.Context, a *account, force bool) error {
	for {
		a.mu.Lock()
		if a.deleted {
			a.mu.Unlock()
			return fmt.Errorf("account removed")
		}
		if a.refreshFlight != nil {
			flight := a.refreshFlight
			a.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-flight:
				continue
			}
		}
		if !force && !a.timeRefresh.IsZero() && time.Now().Before(a.timeRefresh) {
			a.mu.Unlock()
			return nil
		}
		flight := make(chan struct{})
		a.refreshFlight = flight
		auth, epoch := a.auth, a.epoch
		a.mu.Unlock()
		refreshCtx, cancel := context.WithTimeout(s.upstreamContext(ctx, a), 60*time.Second)
		next, err := s.engine.Refresh(refreshCtx, auth)
		cancel()
		if err == nil {
			err = s.persistAuth(a, next)
		}
		a.mu.Lock()
		if !a.deleted && a.epoch == epoch {
			if err == nil {
				a.timeRefresh = next.NextRefreshAfter
				if a.timeRefresh.IsZero() {
					a.timeRefresh = time.Now().Add(15 * time.Minute)
				}
				if a.cooldownKind == "auth" {
					a.cooldown = time.Time{}
					a.cooldownKind = ""
				}
			}
			if err != nil {
				a.timeRefresh = time.Now().Add(time.Minute)
			}
		}
		a.refreshFlight = nil
		close(flight)
		a.mu.Unlock()
		return err
	}
}
func parseReset(value string) time.Time {
	date, err := time.Parse(time.RFC3339, value)
	if err != nil {
		date, _ = time.Parse("2006-01-02", value)
	}
	return date
}
func (s *Server) checkQuota(ctx context.Context, a *account) error {
	for {
		a.mu.Lock()
		if a.deleted {
			a.mu.Unlock()
			return fmt.Errorf("account removed")
		}
		if a.quotaFlight != nil {
			done := a.quotaFlight
			a.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-done:
				continue
			}
		}
		if time.Now().Before(a.nextQuota) {
			a.mu.Unlock()
			return nil
		}
		done := make(chan struct{})
		a.quotaFlight = done
		epoch, auth := a.epoch, a.auth
		a.mu.Unlock()
		quotaCtx, cancel := context.WithTimeout(s.upstreamContext(ctx, a), 15*time.Second)
		quota, err := s.engine.Quota(quotaCtx, auth)
		cancel()
		now := time.Now().UTC()
		var state accountState
		save := false
		a.mu.Lock()
		if !a.deleted && a.epoch == epoch {
			a.nextQuota = now.Add(time.Minute)
			if quota.RetryAfterSeconds > 60 {
				a.nextQuota = now.Add(time.Duration(quota.RetryAfterSeconds) * time.Second)
			}
			if err == nil && !quota.Stale {
				if obs := quota.Admission; obs != nil {
					reset := parseReset(obs.ResetTime)
					if obs.State == "block" {
						if !a.guard.Blocked || a.guard.ResetAt.IsZero() || (!now.Before(a.guard.ResetAt) && reset.After(a.guard.ResetAt)) {
							a.guard.ResetAt = reset
						}
						a.guard.Blocked = true
						a.guard.Reason = "additional_budget_exceeded"
					} else if obs.State == "allow" && a.guard.Blocked {
						if !a.guard.ResetAt.IsZero() && !now.Before(a.guard.ResetAt) && reset.After(a.guard.ResetAt) && reset.After(now) {
							a.guard = Guard{}
						} else if a.guard.ResetAt.IsZero() && !reset.IsZero() {
							a.guard.ResetAt = reset
						}
					}
				}
				quota.ObservedAt = now
				a.quota = quota
				state = accountState{Identity: a.identity, Guard: a.guard, Quota: a.quota}
				save = true
			} else {
				a.quota.Stale = true
			}
		}
		a.mu.Unlock()
		if save {
			if saveErr := s.saveStateSnapshot(a.id, state); saveErr != nil {
				a.mu.Lock()
				if !a.deleted && a.epoch == epoch {
					a.guard.Blocked = true
					a.guard.Reason = "budget_state_write_failed"
				}
				a.mu.Unlock()
				err = saveErr
			}
		}
		a.mu.Lock()
		a.quotaFlight = nil
		close(done)
		a.mu.Unlock()
		return err
	}
}
func (s *Server) monitor(ctx context.Context) {
	defer close(s.monitorDone)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var workers sync.WaitGroup
	defer workers.Wait()
	var mu sync.Mutex
	running := map[*account]bool{}
	for {
		s.flushUsage()
		s.mu.Lock()
		accounts := make([]*account, 0, len(s.accounts))
		for _, a := range s.accounts {
			accounts = append(accounts, a)
		}
		s.mu.Unlock()
		for _, a := range accounts {
			a.mu.Lock()
			due := !a.disabled && !a.deleted && (time.Now().After(a.nextQuota) || time.Now().After(a.timeRefresh) || time.Now().After(a.nextModels))
			a.mu.Unlock()
			if !due {
				continue
			}
			mu.Lock()
			busy := running[a]
			if !busy {
				running[a] = true
			}
			mu.Unlock()
			if busy {
				continue
			}
			workers.Add(1)
			go func(a *account) {
				defer workers.Done()
				defer func() { mu.Lock(); delete(running, a); mu.Unlock() }()
				_ = s.checkQuota(ctx, a)
				if s.refresh(ctx, a, false) == nil {
					_ = s.catalog(ctx, a, false)
				}
			}(a)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (s *Server) updateAccount(id string, disabled *bool, priority *int) error {
	a := s.account(id)
	if a == nil {
		return fmt.Errorf("account not found")
	}
	a.mu.Lock()
	if a.deleted {
		a.mu.Unlock()
		return fmt.Errorf("account removed")
	}
	epoch := a.epoch
	nextRaw := make(map[string]any, len(a.raw)+2)
	for key, value := range a.raw {
		nextRaw[key] = value
	}
	nextDisabled := a.disabled
	nextPriority := a.priority
	if disabled != nil {
		nextDisabled = *disabled
		nextRaw["disabled"] = *disabled
	}
	if priority != nil {
		nextPriority = *priority
		nextRaw["priority"] = *priority
	}
	a.mu.Unlock()
	body, err := json.Marshal(nextRaw)
	if err != nil {
		return fmt.Errorf("cannot encode account update")
	}
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	if err = atomicWrite(filepath.Join(s.authDir, id), body); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.deleted || a.epoch != epoch {
		return fmt.Errorf("account changed during update")
	}
	a.disabled = nextDisabled
	a.priority = nextPriority
	a.raw = nextRaw
	return nil
}
func (s *Server) deleteAccount(id string) error {
	if !validName(id) {
		return fmt.Errorf("invalid account")
	}
	s.mu.Lock()
	a := s.accounts[id]
	s.mu.Unlock()
	if a == nil {
		return fmt.Errorf("account not found")
	}
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	a.mu.Lock()
	if a.deleted {
		a.mu.Unlock()
		return fmt.Errorf("account removed")
	}
	epoch := a.epoch
	a.mu.Unlock()
	if err := os.Remove(filepath.Join(s.authDir, id)); err != nil {
		return fmt.Errorf("cannot delete credential")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if s.accounts[id] != a || a.deleted || a.epoch != epoch {
		return fmt.Errorf("account changed during delete")
	}
	a.deleted = true
	a.epoch++
	delete(s.accounts, id)
	_ = os.Remove(s.statePath(id))
	return nil
}
func (s *Server) loadAccounts() error {
	entries, err := os.ReadDir(s.authDir)
	if os.IsNotExist(err) {
		return os.MkdirAll(s.authDir, 0700)
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !validName(entry.Name()) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.authDir, entry.Name()))
		if err != nil {
			return fmt.Errorf("cannot read credential")
		}
		var marker struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &marker) != nil {
			return fmt.Errorf("invalid credential JSON: %s", entry.Name())
		}
		if !strings.EqualFold(marker.Type, "github-copilot") {
			continue
		}
		if _, err = s.importAccount(entry.Name(), raw); err != nil {
			return err
		}
	}
	return nil
}

// persistAuth commits provider updates outside account locks while preserving
// user-owned controls and rejecting credential replacement races.
func (s *Server) persistAuth(a *account, next contract.AuthData) error {
	a.mu.Lock()
	if a.deleted {
		a.mu.Unlock()
		return fmt.Errorf("account removed")
	}
	epoch := a.epoch
	raw := map[string]any{}
	if json.Unmarshal(next.StorageJSON, &raw) != nil {
		a.mu.Unlock()
		return fmt.Errorf("invalid credential update")
	}
	if identity(raw) != a.identity {
		a.mu.Unlock()
		return fmt.Errorf("credential identity changed unexpectedly")
	}
	for _, key := range []string{"priority", "disabled", "proxy_url", "budget_guard_migration"} {
		if value, ok := a.raw[key]; ok {
			raw[key] = value
		}
	}
	fileName := a.auth.FileName
	disabled := a.disabled
	a.mu.Unlock()
	body, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("cannot encode credential update")
	}
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	if err := atomicWrite(filepath.Join(s.authDir, a.id), body); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.deleted || a.epoch != epoch {
		return fmt.Errorf("account changed during credential update")
	}
	next.ID = a.id
	next.FileName = fileName
	next.Disabled = disabled
	a.auth = next
	a.raw = raw
	if fetched, ok := raw["models_fetched_at"].(float64); ok && fetched > 0 {
		a.nextModels = time.UnixMilli(int64(fetched)).Add(max(time.Minute, s.modelTTL))
	}
	return nil
}
func (s *Server) catalog(ctx context.Context, a *account, force bool) error {
	for {
		a.mu.Lock()
		if a.deleted {
			a.mu.Unlock()
			return fmt.Errorf("account removed")
		}
		if a.refreshFlight != nil {
			done := a.refreshFlight
			a.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-done:
				force = false
				continue
			}
		}
		// A fresh timestamp over a catalog made entirely from retired model IDs is
		// not a usable cache; fall through and discover a replacement immediately.
		if !force && !a.nextModels.IsZero() && time.Now().Before(a.nextModels) && len(modelIDs(a.raw)) > 0 {
			a.mu.Unlock()
			return nil
		}
		if !force {
			if fetched, ok := a.raw["models_fetched_at"].(float64); ok && len(modelIDs(a.raw)) > 0 && s.modelTTL > 0 {
				expires := time.UnixMilli(int64(fetched)).Add(s.modelTTL)
				if time.Now().Before(expires) {
					a.nextModels = expires
					a.mu.Unlock()
					return nil
				}
			}
		}
		done := make(chan struct{})
		a.refreshFlight = done
		epoch, auth := a.epoch, a.auth
		a.mu.Unlock()
		queryCtx, cancel := context.WithTimeout(s.upstreamContext(ctx, a), 30*time.Second)
		result, err := s.engine.Catalog(queryCtx, auth)
		cancel()
		if err == nil && len(result.AuthUpdate.StorageJSON) > 0 {
			err = s.persistAuth(a, result.AuthUpdate)
		}
		a.mu.Lock()
		if !a.deleted && a.epoch == epoch {
			a.nextModels = time.Now().Add(max(time.Minute, s.modelTTL))
		}
		a.refreshFlight = nil
		close(done)
		a.mu.Unlock()
		return err
	}
}
