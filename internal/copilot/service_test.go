package copilot

import "testing"

func TestConfigureAppliesDefaultsAndRejectsUnsafeHost(t *testing.T) {
	service := newService(nil)
	if errConfigure := service.configure(mustJSON(t, lifecycleRequest{ConfigYAML: []byte("enabled: true\npriority: 1\n")})); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	config := service.loadedConfig()
	if config.ClientID != defaultClientID || config.GitHubHost != "github.com" || !config.EnableModels ||
		config.WebSearchModel != "gpt-5.6-terra" || config.EnableResponsesContextManagement {
		t.Fatalf("default config = %#v", config)
	}
	for _, host := range []string{"http://github.com", "127.0.0.1", "https://github.com/path"} {
		errConfigure := service.configure(mustJSON(t, lifecycleRequest{ConfigYAML: []byte("github_host: \"" + host + "\"\n")}))
		if errConfigure == nil {
			t.Fatalf("unsafe host %q was accepted", host)
		}
	}
}
