package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const (
	sessionAffinityTTL   = 30 * time.Minute
	maxSessionBindings   = 4096
	maxClaudeUserIDBytes = 4096
)

var (
	sessionUUID        = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	legacyClaudeUserID = regexp.MustCompile(`^user_[A-Za-z0-9-]+_account_[A-Za-z0-9-]*_session_([0-9a-fA-F-]{36})$`)
)

// apiRequestKey must use the same precedence for authentication and affinity.
// An irrelevant X-Api-Key must not split a session authenticated with Bearer.
func apiRequestKey(r *http.Request) string {
	if key := bearer(r); key != "" {
		return key
	}
	return r.Header.Get("X-Api-Key")
}

// requestSessionKey is local scheduling state, never an upstream identity. It
// hashes the authenticated key and session as separate fields; no raw API key,
// user/device/account metadata or session ID is retained in the affinity map.
func requestSessionKey(r *http.Request, body []byte, format string) string {
	session := r.Header.Get("X-Session-Id")
	if session != "" {
		if len(session) > 128 {
			return ""
		}
	} else if format == "claude" {
		userID := gjson.GetBytes(body, "metadata.user_id")
		if userID.Type != gjson.String {
			return ""
		}
		session = claudeCodeSessionID(userID.String())
	}
	key := apiRequestKey(r)
	if session == "" || key == "" {
		return ""
	}
	raw, _ := json.Marshal([2]string{key, session})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func claudeCodeSessionID(userID string) string {
	if len(userID) > maxClaudeUserIDBytes {
		return ""
	}
	var metadata struct {
		SessionID string `json:"session_id"`
	}
	session := ""
	if json.Unmarshal([]byte(userID), &metadata) == nil {
		session = metadata.SessionID
	} else if matches := legacyClaudeUserID.FindStringSubmatch(userID); len(matches) == 2 {
		session = matches[1]
	}
	// Do not bind all requests by user_id/device/account or guess from arbitrary
	// strings. Only the explicit Claude Code conversation UUID is a fallback.
	if !sessionUUID.MatchString(session) {
		return ""
	}
	return strings.ToLower(session)
}

func (s *Server) bindSession(session, id string) {
	if session == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.SessionAffinity {
		s.bindSessionLocked(session, id, time.Now())
	}
}

// Caller holds s.mu. Renewing a full cache must not erase every other session.
func (s *Server) bindSessionLocked(session, id string, now time.Time) {
	if _, exists := s.affinity[session]; !exists && len(s.affinity) >= maxSessionBindings {
		oldest := ""
		for key, bound := range s.affinity {
			if !bound.expires.After(now) {
				delete(s.affinity, key)
				continue
			}
			if oldest == "" || bound.expires.Before(s.affinity[oldest].expires) ||
				(bound.expires.Equal(s.affinity[oldest].expires) && key < oldest) {
				oldest = key
			}
		}
		if len(s.affinity) >= maxSessionBindings {
			delete(s.affinity, oldest)
		}
	}
	s.affinity[session] = binding{id, now.Add(sessionAffinityTTL)}
}
