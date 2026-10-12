package copilot

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestConfigureAppliesDefaultsAndRejectsUnsafeHost(t *testing.T) {
	service := newService(nil)
	if errConfigure := service.configureSettings(defaultSettings()); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	config := service.loadedConfig()
	if config.ClientID != defaultClientID || config.GitHubHost != "github.com" ||
		config.WebSearchModel != "gpt-6" || config.EnableResponsesContextManagement {
		t.Fatalf("default config = %#v", config)
	}
	legacyWebSearch := defaultSettings()
	legacyWebSearch.WebSearchModel = "gpt-5.6-terra"
	if errConfigure := service.configureSettings(legacyWebSearch); errConfigure == nil {
		t.Fatal("retired GPT web_search_model was accepted")
	}
	for _, host := range []string{"http://github.com", "127.0.0.1", "https://github.com/path"} {
		cfg := defaultSettings()
		cfg.GitHubHost = host
		errConfigure := service.configureSettings(cfg)
		if errConfigure == nil {
			t.Fatalf("unsafe host %q was accepted", host)
		}
	}
}

func TestSettingsYAMLKeysDecodeOverDefaults(t *testing.T) {
	cfg := defaultSettings()
	raw := "client_id: synthetic-client\n" +
		"github_host: ghe.example.com\n" +
		"model_cache_ttl_seconds: 42\n" +
		"max_stream_buffer_bytes: 131072\n" +
		"web_search_model: \"\"\n" +
		"enable_responses_context_management: true\n"
	if errDecode := yaml.Unmarshal([]byte(raw), &cfg); errDecode != nil {
		t.Fatal(errDecode)
	}
	want := settings{
		ClientID: "synthetic-client", GitHubHost: "ghe.example.com", ModelCacheTTL: 42,
		MaxStreamBytes: 131072, WebSearchModel: "", EnableResponsesContextManagement: true,
	}
	if cfg != want {
		t.Fatalf("decoded settings = %#v, want %#v", cfg, want)
	}
	service := newService(nil)
	if errConfigure := service.configureSettings(cfg); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	if got := service.loadedConfig(); got.WebSearchModel != "" || got.GitHubHost != "ghe.example.com" || got.ModelCacheTTL != 42 {
		t.Fatalf("configured settings = %#v", got)
	}
}
