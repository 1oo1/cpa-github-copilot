package main

import (
	"os"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginstore"
)

func TestPluginStoreRegistry(t *testing.T) {
	raw, errRead := os.ReadFile("../registry.json")
	if errRead != nil {
		t.Fatalf("read registry: %v", errRead)
	}
	registry, errParse := pluginstore.ParseRegistry(raw)
	if errParse != nil {
		t.Fatalf("parse registry: %v", errParse)
	}
	if len(registry.Plugins) != 1 {
		t.Fatalf("plugins = %d, want 1", len(registry.Plugins))
	}
	plugin := registry.Plugins[0]
	if plugin.ID != "github-copilot-go" || plugin.Repository != "https://github.com/1oo1/cpa-github-copilot" {
		t.Fatalf("plugin = %#v", plugin)
	}
	if plugin.Version != "" || pluginstore.PluginInstallType(plugin) != pluginstore.InstallTypeGitHubRelease {
		t.Fatalf("plugin release resolution = version %q, install type %q", plugin.Version, pluginstore.PluginInstallType(plugin))
	}
}
