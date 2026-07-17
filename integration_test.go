package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestBuiltPluginLoadsInCLIProxyHost(t *testing.T) {
	binaryPath := os.Getenv("CPA_PLUGIN_INTEGRATION_BINARY")
	if binaryPath == "" {
		t.Skip("set CPA_PLUGIN_INTEGRATION_BINARY to a built c-shared plugin")
	}
	binary, errRead := os.ReadFile(binaryPath)
	if errRead != nil {
		t.Fatalf("read plugin binary: %v", errRead)
	}
	pluginsDir := t.TempDir()
	target := filepath.Join(pluginsDir, "github-copilot-go"+pluginhost.PluginExtension(runtime.GOOS))
	if errWrite := os.WriteFile(target, binary, 0o700); errWrite != nil {
		t.Fatalf("stage plugin binary: %v", errWrite)
	}
	rawConfig := fmt.Appendf(nil, `
auth-dir: %q
plugins:
  enabled: true
  dir: %q
  configs:
    github-copilot-go:
      enabled: true
      priority: 10
      enable_models: false
`, filepath.Join(pluginsDir, "auth"), pluginsDir)
	cfg, errConfig := config.ParseConfigBytes(rawConfig)
	if errConfig != nil {
		t.Fatalf("parse host config: %v", errConfig)
	}
	host := pluginhost.New()
	t.Cleanup(host.ShutdownAll)
	var logOutput bytes.Buffer
	logger := log.StandardLogger()
	originalOutput := logger.Out
	originalFormatter := logger.Formatter
	originalLevel := logger.Level
	log.SetOutput(&logOutput)
	log.SetFormatter(&log.TextFormatter{DisableColors: true, DisableTimestamp: true})
	log.SetLevel(log.DebugLevel)
	t.Cleanup(func() {
		log.SetOutput(originalOutput)
		log.SetFormatter(originalFormatter)
		log.SetLevel(originalLevel)
	})
	host.ApplyConfig(context.Background(), cfg)
	if !strings.Contains(logOutput.String(), "github-copilot: plugin.configured") {
		t.Fatalf("real host did not receive plugin diagnostic log: %s", logOutput.String())
	}
	if !host.PluginLoaded("github-copilot-go") || !host.PluginRegistered("github-copilot-go") {
		t.Fatalf("plugin load state: loaded=%v registered=%v", host.PluginLoaded("github-copilot-go"), host.PluginRegistered("github-copilot-go"))
	}
	registered := host.RegisteredPlugins()
	if len(registered) != 1 || !registered[0].SupportsOAuth || registered[0].OAuthProvider != pluginIdentifier {
		t.Fatalf("registered plugins = %#v", registered)
	}
	if !host.HasAuthProvider(pluginIdentifier) {
		t.Fatal("real host did not register the auth provider")
	}
	if format := host.PluginExecutorRequestToFormat("github-copilot-go", coreexecutor.Request{
		Model: "gpt-4.1", Format: sdktranslator.FormatOpenAI,
	}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}); format != sdktranslator.FormatOpenAI {
		t.Fatalf("real host executor request format = %q", format)
	}
	if auth, handled, errParse := host.ParseAuth(context.Background(), pluginapi.AuthParseRequest{
		FileName: "unrelated.json",
		RawJSON:  []byte(`{"type":"unrelated"}`),
	}); errParse != nil || handled || auth != nil {
		t.Fatalf("unrelated parse = auth %#v handled=%v error=%v", auth, handled, errParse)
	}
	now := time.Now().UTC()
	storage := executorStorage(now, storedModel{ID: "gpt-4.1", Name: "GPT 4.1", Format: formatOpenAI})
	storage.ModelsFetchedAt = now.UnixMilli()
	auth, handled, errParse := host.ParseAuth(context.Background(), pluginapi.AuthParseRequest{
		FileName: "github-copilot-test.json",
		RawJSON:  mustJSON(t, storage),
	})
	if errParse != nil || !handled || auth == nil || auth.Provider != pluginIdentifier {
		t.Fatalf("Copilot parse = auth %#v handled=%v error=%v", auth, handled, errParse)
	}
	discovered := host.ModelsForAuth(context.Background(), auth)
	if !discovered.Handled || discovered.Err != nil || discovered.Provider != pluginIdentifier || len(discovered.Models) != 1 || discovered.Models[0].ID != "gpt-4.1" {
		t.Fatalf("models for auth = %#v", discovered)
	}
}
