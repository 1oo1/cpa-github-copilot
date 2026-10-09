package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/1oo1/cpa-github-copilot/internal/contract"
)

func parseAffinityTTL(raw json.RawMessage) (time.Duration, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		ttl, err := time.ParseDuration(text)
		if err != nil || ttl <= 0 {
			return 0, fmt.Errorf("invalid duration")
		}
		return ttl, nil
	}
	var seconds float64
	if json.Unmarshal(raw, &seconds) != nil || seconds <= 0 || seconds > float64((1<<63-1)/time.Second) {
		return 0, fmt.Errorf("invalid duration")
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func (s *Server) managementRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/management/accounts", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, s.listAccounts()) })
	mux.HandleFunc("GET /v0/management/auth-files", func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]any{"files": s.listAccounts()})
	})
	mux.HandleFunc("POST /v0/management/accounts", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name       string          `json:"name"`
			Credential json.RawMessage `json:"credential"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		id, err := s.importAccount(req.Name, req.Credential)
		if err != nil {
			apiError(w, 400, "invalid_credential", err.Error())
			return
		}
		respond(w, 200, map[string]string{"id": id})
	})
	mux.HandleFunc("PATCH /v0/management/accounts", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID       string `json:"id"`
			Disabled *bool  `json:"disabled"`
			Priority *int   `json:"priority"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		if req.Priority != nil && (*req.Priority > 1000000 || *req.Priority < -1000000) {
			apiError(w, 400, "invalid_priority", "priority out of range")
			return
		}
		if err := s.updateAccount(req.ID, req.Disabled, req.Priority); err != nil {
			apiError(w, 400, "account_update_failed", err.Error())
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("DELETE /v0/management/accounts", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if err := s.deleteAccount(id); err != nil {
			apiError(w, 400, "delete_failed", err.Error())
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /v0/management/accounts/refresh", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID string `json:"id"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		a := s.account(req.ID)
		if a == nil {
			apiError(w, 404, "not_found", "account not found")
			return
		}
		if err := s.refresh(r.Context(), a, true); err != nil {
			apiError(w, 502, "refresh_failed", "Copilot credential refresh failed")
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	})
	quota := func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID        string `json:"id"`
			AuthIndex string `json:"auth_index"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = req.AuthIndex
		}
		a := s.account(req.ID)
		if a == nil {
			apiError(w, 404, "not_found", "account not found")
			return
		}
		err := s.checkQuota(r.Context(), a)
		a.mu.Lock()
		q := a.quota
		guard := a.guard
		a.mu.Unlock()
		if err != nil && q.ObservedAt.IsZero() {
			apiError(w, 502, "quota_failed", "Copilot quota is unavailable")
			return
		}
		if guard.Blocked {
			q.Admission = &contract.QuotaAdmission{State: "block", Reason: guard.Reason}
			if !guard.ResetAt.IsZero() {
				q.Admission.ResetTime = guard.ResetAt.Format(time.RFC3339)
			}
		}
		respond(w, 200, q)
	}
	mux.HandleFunc("POST /v0/management/quota/fetch", quota)
	login := func(w http.ResponseWriter, r *http.Request) {
		out, err := s.engine.LoginStart(r.Context())
		if err != nil {
			apiError(w, 502, "login_failed", "GitHub device authorization could not start")
			return
		}
		respond(w, 200, map[string]any{"url": out.URL, "state": out.State, "expires_at": out.ExpiresAt, "user_code": out.Metadata["user_code"], "interval": out.Metadata["interval"]})
	}
	mux.HandleFunc("POST /v0/management/login", login)
	mux.HandleFunc("GET /v0/management/github-copilot-auth-url", login)
	poll := func(w http.ResponseWriter, r *http.Request) {
		state := r.URL.Query().Get("state")
		if state == "" || len(state) > 256 {
			apiError(w, 400, "invalid_state", "invalid login state")
			return
		}
		out, err := s.engine.LoginPoll(r.Context(), state)
		if err != nil {
			apiError(w, 502, "login_failed", "GitHub authorization failed or expired")
			return
		}
		if out.Status == contract.AuthLoginStatusSuccess {
			body := out.Auth.StorageJSON
			name := out.Auth.FileName
			if previous := s.account(name); previous != nil {
				previous.mu.Lock()
				raw := map[string]any{}
				_ = json.Unmarshal(body, &raw)
				raw["priority"] = previous.priority
				raw["disabled"] = previous.disabled
				raw["proxy_url"] = previous.proxy
				body, _ = json.Marshal(raw)
				previous.mu.Unlock()
			}
			if _, err = s.importAccount(name, body); err != nil {
				apiError(w, 500, "save_failed", "cannot save authorized account")
				return
			}
		}
		respond(w, 200, map[string]any{"status": out.Status})
	}
	mux.HandleFunc("GET /v0/management/login/status", poll)
	mux.HandleFunc("GET /v0/management/get-auth-status", poll)
	mux.HandleFunc("GET /v0/management/usage", func(w http.ResponseWriter, r *http.Request) {
		s.usageMu.Lock()
		u := s.usage
		s.usageMu.Unlock()
		respond(w, 200, u)
	})
	mux.HandleFunc("GET /v0/management/logs", func(w http.ResponseWriter, r *http.Request) {
		s.usageMu.Lock()
		logs := append([]Log{}, s.logs...)
		s.usageMu.Unlock()
		respond(w, 200, logs)
	})
	mux.HandleFunc("GET /v0/management/config", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		cfg := s.cfg
		s.mu.Unlock()
		if u, err := url.Parse(cfg.ProxyURL); err == nil && u.User != nil {
			cfg.ProxyURL = ""
			cfg.ProxyRedacted = true
		}
		respond(w, 200, cfg)
	})
	mux.HandleFunc("PATCH /v0/management/config", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Proxy              *string         `json:"proxy_url"`
			Routing            *string         `json:"routing"`
			Affinity           *bool           `json:"session_affinity"`
			AffinityTTL        json.RawMessage `json:"session_affinity_ttl"`
			AffinityTTLSeconds *int64          `json:"session_affinity_ttl_seconds"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		if req.Routing != nil && *req.Routing != "round-robin" && *req.Routing != "fill-first" {
			apiError(w, 400, "invalid_config", "invalid routing strategy")
			return
		}
		ttl := time.Duration(0)
		if len(req.AffinityTTL) > 0 && string(req.AffinityTTL) != "null" {
			var err error
			ttl, err = parseAffinityTTL(req.AffinityTTL)
			if err != nil {
				apiError(w, 400, "invalid_config", "invalid session affinity TTL")
				return
			}
		}
		if req.AffinityTTLSeconds != nil {
			if ttl != 0 {
				apiError(w, 400, "invalid_config", "session affinity TTL must be specified once")
				return
			}
			if *req.AffinityTTLSeconds <= 0 || *req.AffinityTTLSeconds > int64((1<<63-1)/time.Second) {
				apiError(w, 400, "invalid_config", "session affinity TTL must be positive")
				return
			}
			ttl = time.Duration(*req.AffinityTTLSeconds) * time.Second
		}
		if req.Proxy != nil && *req.Proxy != "" {
			u, err := url.Parse(*req.Proxy)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
				apiError(w, 400, "invalid_config", "invalid proxy URL")
				return
			}
		}
		s.persistMu.Lock()
		defer s.persistMu.Unlock()
		s.mu.Lock()
		next := s.cfg
		s.mu.Unlock()
		if req.Proxy != nil {
			next.ProxyURL = *req.Proxy
		}
		if req.Routing != nil {
			next.Routing = *req.Routing
		}
		if req.Affinity != nil {
			next.SessionAffinity = *req.Affinity
		}
		if ttl != 0 {
			next.SessionAffinityTTL = ttl
		}
		if err := s.saveConfigSnapshot(next); err != nil {
			apiError(w, 500, "save_failed", "cannot save configuration")
			return
		}
		s.mu.Lock()
		s.cfg = next
		s.mu.Unlock()
		respond(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /v0/management/api-keys", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		var rows []map[string]string
		for _, key := range s.cfg.APIKeys {
			id := identity(map[string]any{"github_access_token": key})
			rows = append(rows, map[string]string{"id": id, "preview": key[:min(4, len(key))] + "…" + key[max(0, len(key)-4):]})
		}
		s.mu.Unlock()
		respond(w, 200, rows)
	})
	mux.HandleFunc("POST /v0/management/api-keys", func(w http.ResponseWriter, r *http.Request) {
		key := secret()
		s.persistMu.Lock()
		defer s.persistMu.Unlock()
		s.mu.Lock()
		next := append(append([]string(nil), s.cfg.APIKeys...), key)
		s.mu.Unlock()
		cfg := s.configSnapshotWithAPIKeys(next)
		if err := s.saveConfigSnapshot(cfg); err != nil {
			apiError(w, 500, "save_failed", "cannot save API key")
			return
		}
		s.mu.Lock()
		s.cfg = cfg
		s.mu.Unlock()
		respond(w, 200, map[string]string{"key": key})
	})
	mux.HandleFunc("DELETE /v0/management/api-keys", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		s.persistMu.Lock()
		defer s.persistMu.Unlock()
		s.mu.Lock()
		next := []string{}
		for _, key := range s.cfg.APIKeys {
			if identity(map[string]any{"github_access_token": key}) != id {
				next = append(next, key)
			}
		}
		if len(next) == 0 {
			s.mu.Unlock()
			apiError(w, 400, "last_key", "cannot delete the last API key")
			return
		}
		s.mu.Unlock()
		cfg := s.configSnapshotWithAPIKeys(next)
		if err := s.saveConfigSnapshot(cfg); err != nil {
			apiError(w, 500, "save_failed", "cannot save API keys")
			return
		}
		s.mu.Lock()
		s.cfg = cfg
		s.mu.Unlock()
		respond(w, 200, map[string]bool{"ok": true})
	})
	return mux
}

// ImportDirectory copies only Copilot credentials; the original CPA auth directory is never modified.
func (s *Server) ImportDirectory(dir string) (int, error) {
	sourcePath, _ := filepath.Abs(dir)
	targetPath, _ := filepath.Abs(s.authDir)
	if resolved, err := filepath.EvalSymlinks(sourcePath); err == nil {
		sourcePath = resolved
	}
	if resolved, err := filepath.EvalSymlinks(targetPath); err == nil {
		targetPath = resolved
	}
	if sourcePath == targetPath {
		return 0, fmt.Errorf("import source must differ from the new auth directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("cannot read import directory")
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return count, fmt.Errorf("cannot read import file")
		}
		var marker struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &marker) != nil || marker.Type != "github-copilot" {
			continue
		}
		guard, err := s.readCPAGuard(dir, entry.Name(), raw)
		if err != nil {
			return count, err
		}
		if guard.Blocked {
			var credential map[string]any
			_ = json.Unmarshal(raw, &credential)
			credential["budget_guard_migration"] = guard
			raw, _ = json.Marshal(credential)
		}
		if _, err = s.importAccount(entry.Name(), raw); err != nil {
			return count, err
		}
		if guard.Blocked {
			a := s.account(entry.Name())
			a.mu.Lock()
			a.guard = guard
			state := accountState{Identity: a.identity, Guard: a.guard, Quota: a.quota}
			a.mu.Unlock()
			err = s.saveStateSnapshot(entry.Name(), state)
			if err != nil {
				return count, err
			}
		}
		count++
	}
	return count, nil
}
