package main

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRegistrationDeclaresProviderNativeCapabilities(t *testing.T) {
	service := newPluginService(nil)
	registration := service.registration()
	capabilities := registration.Capabilities
	if !capabilities.AuthProvider || !capabilities.ModelProvider || !capabilities.Executor {
		t.Fatalf("capabilities = %#v", capabilities)
	}
	if capabilities.ExecutorModelScope != string(pluginapi.ExecutorModelScopeOAuth) {
		t.Fatalf("executor scope = %q", capabilities.ExecutorModelScope)
	}
	wantFormats := strings.Join([]string{formatOpenAI, formatOpenAIResponse, formatClaude}, ",")
	if strings.Join(capabilities.ExecutorInputFormats, ",") != wantFormats || strings.Join(capabilities.ExecutorOutputFormats, ",") != wantFormats {
		t.Fatalf("executor formats = %#v -> %#v", capabilities.ExecutorInputFormats, capabilities.ExecutorOutputFormats)
	}
}

func TestConfigureAppliesDefaultsAndRejectsUnsafeHost(t *testing.T) {
	service := newPluginService(nil)
	if errConfigure := service.configure(mustJSON(t, lifecycleRequest{ConfigYAML: []byte("enabled: true\npriority: 1\n")})); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	config := service.loadedConfig()
	if config.ClientID != defaultClientID || config.GitHubHost != "github.com" || !config.EnableModels {
		t.Fatalf("default config = %#v", config)
	}
	for _, host := range []string{"http://github.com", "127.0.0.1", "https://github.com/path"} {
		errConfigure := service.configure(mustJSON(t, lifecycleRequest{ConfigYAML: []byte("github_host: \"" + host + "\"\n")}))
		if errConfigure == nil {
			t.Fatalf("unsafe host %q was accepted", host)
		}
	}
}
