package app

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/1oo1/cpa-github-copilot/internal/copilot"
	"gopkg.in/yaml.v3"
)

// A newly added setting must be documented in the shipped deployment example.
// yaml:"-" response-only fields must not be confused with writable settings.
func assertExampleFields(t *testing.T, typ reflect.Type, mapping map[string]any, path string) {
	t.Helper()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		key := strings.Split(field.Tag.Get("yaml"), ",")[0]
		if key == "-" || !field.IsExported() {
			continue
		}
		if key == "" {
			key = strings.ToLower(field.Name)
		}
		value, ok := mapping[key]
		if !ok {
			t.Errorf("config.example.yaml is missing %s%s", path, key)
			continue
		}
		if field.Type.Kind() == reflect.Struct {
			nested, ok := value.(map[string]any)
			if !ok {
				t.Errorf("%s%s must be a mapping", path, key)
				continue
			}
			assertExampleFields(t, field.Type, nested, path+key+".")
		}
	}
}
func TestDeploymentExampleDocumentsAllConfigFields(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var mapping map[string]any
	if err = yaml.Unmarshal(raw, &mapping); err != nil {
		t.Fatal(err)
	}
	assertExampleFields(t, reflect.TypeOf(Config{}), mapping, "")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadConfig(path); err == nil {
		t.Fatal("public placeholder secrets must not start a deployment")
	}
	configured := strings.ReplaceAll(string(raw), "REPLACE_ADMIN", "test-admin-0123456789")
	configured = strings.ReplaceAll(configured, "REPLACE_CLIENT", "test-client-0123456789")
	if err = os.WriteFile(path, []byte(configured), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "0.0.0.0:8317" || cfg.AuthDir != "auths" {
		t.Fatal("example does not match container network/data layout")
	}
	if !reflect.DeepEqual(cfg.Copilot, copilot.DefaultSettings()) {
		t.Fatal("Copilot example defaults drifted from implementation")
	}
}
