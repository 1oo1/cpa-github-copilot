package main

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
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

func TestDispatchLogsLifecycleSuccessAndFailure(t *testing.T) {
	bridge := &fakeBridge{}
	service := newPluginService(bridge)
	raw, errRegister := service.dispatch(pluginabi.MethodPluginRegister, mustJSON(t, lifecycleRequest{
		ConfigYAML: []byte("client_id: CLIENT_ID_SENTINEL\ngithub_host: github.com\n"),
	}))
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	registered := decodePluginResult[registration](t, raw)
	if registered.Metadata.Version != pluginVersion {
		t.Fatalf("registered version = %q", registered.Metadata.Version)
	}
	_, errUnsupported := service.dispatch("unsupported.method", mustJSON(t, map[string]any{"host_callback_id": "callback-failure"}))
	if errUnsupported == nil || errUnsupported.(*pluginFailure).code != "unknown_method" {
		t.Fatalf("unsupported dispatch error = %#v", errUnsupported)
	}
	logs := bridge.snapshotLogs()
	if findLogEvent(t, logs, "plugin.configured").Level != "info" {
		t.Fatalf("configured log missing from %#v", logs)
	}
	foundFailure := false
	for _, entry := range logs {
		if entry.Fields["event"] == "rpc.failed" && entry.Fields["method"] == "unsupported.method" {
			foundFailure = entry.HostCallbackID == "callback-failure" && entry.Fields["failure_code"] == "unknown_method"
		}
	}
	if !foundFailure {
		t.Fatalf("dispatch failure log missing from %#v", logs)
	}
	service.shutdown()
	findLogEvent(t, bridge.snapshotLogs(), "plugin.shutdown")
	assertLogsExclude(t, bridge.snapshotLogs(), "CLIENT_ID_SENTINEL")
}
