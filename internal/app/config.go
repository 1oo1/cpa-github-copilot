package app

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/1oo1/cpa-github-copilot/internal/copilot"
	"gopkg.in/yaml.v3"
)

type Config struct {
	ProxyRedacted      bool              `yaml:"-" json:"proxy_redacted,omitempty"`
	Listen             string            `yaml:"listen" json:"listen"`
	AuthDir            string            `yaml:"auth-dir,omitempty" json:"-"`
	APIKeys            []string          `yaml:"api-keys" json:"-"`
	ManagementKey      string            `yaml:"management-key" json:"-"`
	ProxyURL           string            `yaml:"proxy-url,omitempty" json:"proxy_url"`
	Routing            string            `yaml:"routing-strategy" json:"routing"`
	SessionAffinity    bool              `yaml:"session-affinity" json:"session_affinity"`
	SessionAffinityTTL time.Duration     `yaml:"session-affinity-ttl" json:"session_affinity_ttl"`
	ModelAliases       map[string]string `yaml:"model-aliases,omitempty" json:"model_aliases"`
	ExcludedModels     []string          `yaml:"excluded-models,omitempty" json:"excluded_models"`
	TrustedProxies     []string          `yaml:"trusted-proxies,omitempty" json:"-"`
	Copilot            copilot.Settings  `yaml:"copilot" json:"-"`
}

func secret() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic("secure random unavailable")
	}
	return hex.EncodeToString(b)
}
func LoadConfig(path string) (Config, error) {
	cfg := Config{Listen: "127.0.0.1:8317", Routing: "round-robin", SessionAffinityTTL: sessionAffinityTTL, Copilot: copilot.DefaultSettings()}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		cfg.ManagementKey = secret()
		cfg.APIKeys = []string{secret()}
		raw, err = yaml.Marshal(cfg)
		if err == nil {
			err = atomicWrite(path, raw)
		}
		return cfg, err
	}
	if err != nil {
		return cfg, fmt.Errorf("cannot read configuration")
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err = dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("invalid configuration; use the standalone config format (see README)")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return cfg, fmt.Errorf("configuration must contain one YAML document")
	}
	if len(cfg.ManagementKey) < 16 || len(cfg.APIKeys) == 0 {
		return cfg, fmt.Errorf("management-key must have at least 16 characters and api-keys must not be empty")
	}
	for _, key := range cfg.APIKeys {
		if keyMatches(key, cfg.ManagementKey) {
			return cfg, fmt.Errorf("management-key must be distinct from API keys")
		}
		if len(key) < 16 {
			return cfg, fmt.Errorf("API keys must have at least 16 characters")
		}
	}
	if cfg.Routing != "round-robin" && cfg.Routing != "fill-first" {
		return cfg, fmt.Errorf("routing-strategy must be round-robin or fill-first")
	}
	if cfg.SessionAffinityTTL < 0 {
		return cfg, fmt.Errorf("session-affinity-ttl must not be negative")
	}
	if cfg.SessionAffinityTTL == 0 {
		cfg.SessionAffinityTTL = sessionAffinityTTL
	}
	if _, err = parseTrustedProxies(cfg.TrustedProxies); err != nil {
		return cfg, err
	}
	return cfg, nil
}
func keyMatches(given, expected string) bool {
	return subtle.ConstantTimeCompare([]byte(given), []byte(expected)) == 1
}
func atomicWrite(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("cannot create data directory")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return fmt.Errorf("cannot write data")
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("cannot persist data")
	}
	if err = os.Rename(name, path); err != nil {
		return fmt.Errorf("cannot replace data")
	}
	return nil
}
