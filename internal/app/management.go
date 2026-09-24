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
			Proxy    *string `json:"proxy_url"`
			Routing  *string `json:"routing"`
			Affinity *bool   `json:"session_affinity"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		if req.Routing != nil && *req.Routing != "round-robin" && *req.Routing != "fill-first" {
			apiError(w, 400, "invalid_config", "invalid routing strategy")
			return
		}
		if req.Proxy != nil && *req.Proxy != "" {
			u, err := url.Parse(*req.Proxy)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
				apiError(w, 400, "invalid_config", "invalid proxy URL")
				return
			}
		}
		s.mu.Lock()
		old := s.cfg
		if req.Proxy != nil {
			s.cfg.ProxyURL = *req.Proxy
		}
		if req.Routing != nil {
			s.cfg.Routing = *req.Routing
		}
		if req.Affinity != nil {
			s.cfg.SessionAffinity = *req.Affinity
		}
		err := s.saveConfigLocked()
		if err != nil {
			s.cfg = old
		}
		s.mu.Unlock()
		if err != nil {
			apiError(w, 500, "save_failed", "cannot save configuration")
			return
		}
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
		s.mu.Lock()
		old := append([]string(nil), s.cfg.APIKeys...)
		s.cfg.APIKeys = append(s.cfg.APIKeys, key)
		err := s.saveConfigLocked()
		if err != nil {
			s.cfg.APIKeys = old
		}
		s.mu.Unlock()
		if err != nil {
			apiError(w, 500, "save_failed", "cannot save API key")
			return
		}
		respond(w, 200, map[string]string{"key": key})
	})
	mux.HandleFunc("DELETE /v0/management/api-keys", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		s.mu.Lock()
		old := s.cfg.APIKeys
		next := []string{}
		for _, key := range old {
			if identity(map[string]any{"github_access_token": key}) != id {
				next = append(next, key)
			}
		}
		if len(next) == 0 {
			s.mu.Unlock()
			apiError(w, 400, "last_key", "cannot delete the last API key")
			return
		}
		s.cfg.APIKeys = next
		err := s.saveConfigLocked()
		if err != nil {
			s.cfg.APIKeys = old
		}
		s.mu.Unlock()
		if err != nil {
			apiError(w, 500, "save_failed", "cannot save API keys")
			return
		}
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
			err = s.saveState(a)
			a.mu.Unlock()
			if err != nil {
				return count, err
			}
		}
		count++
	}
	return count, nil
}
