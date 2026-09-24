package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// readCPAGuard imports only the former Copilot budget latch, never unrelated
// provider cooldowns. The persisted digest must match this credential.
func (s *Server) readCPAGuard(dir, name string, credential []byte) (Guard, error) {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	base = regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(base, "_")
	base = strings.Trim(base, "._-")
	raw, err := os.ReadFile(filepath.Join(dir, base+".cds"))
	if os.IsNotExist(err) {
		return Guard{}, nil
	}
	if err != nil {
		return Guard{}, fmt.Errorf("cannot read CPA budget state")
	}
	var file struct {
		Records []struct {
			AuthID   string `json:"auth_id"`
			Provider string `json:"provider"`
			Budget   struct {
				Guard
				Identity string `json:"credential_identity"`
			} `json:"budget_guard"`
		} `json:"records"`
	}
	if json.Unmarshal(raw, &file) != nil {
		return Guard{}, fmt.Errorf("invalid CPA budget state")
	}
	auth, err := s.engine.Parse(name, credential)
	if err != nil {
		return Guard{}, err
	}
	var metadata map[string]any
	_ = json.Unmarshal(auth.StorageJSON, &metadata)
	for _, record := range file.Records {
		if !record.Budget.Blocked || record.Provider != "github-copilot" {
			continue
		}
		values := []any{record.Provider, record.AuthID, metadata["github_access_token"], metadata["github_host"], metadata["account"]}
		encoded, _ := json.Marshal(values)
		sum := sha256.Sum256(encoded)
		if record.Budget.Identity != hex.EncodeToString(sum[:]) {
			return Guard{}, fmt.Errorf("CPA budget state does not match credential")
		}
		return record.Budget.Guard, nil
	}
	return Guard{}, nil
}
