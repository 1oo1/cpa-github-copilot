package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type integrationRoundTripper func(*http.Request) (*http.Response, error)

func (f integrationRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestBuiltPluginRunsInCLIProxyHost(t *testing.T) {
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
	if expectedVersion := os.Getenv("CPA_PLUGIN_INTEGRATION_VERSION"); expectedVersion != "" && registered[0].Metadata.Version != expectedVersion {
		t.Fatalf("registered version = %q, want %q", registered[0].Metadata.Version, expectedVersion)
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
	storage := executorStorage(now,
		storedModel{ID: "gpt-4.1", Name: "GPT 4.1", Format: formatOpenAI},
		storedModel{ID: "gpt-5.4", Name: "GPT 5.4", Format: formatOpenAIResponse},
	)
	storage.ModelsFetchedAt = now.UnixMilli()
	auth, handled, errParse := host.ParseAuth(context.Background(), pluginapi.AuthParseRequest{
		FileName: "github-copilot-test.json",
		RawJSON:  mustJSON(t, storage),
	})
	if errParse != nil || !handled || auth == nil || auth.Provider != pluginIdentifier {
		t.Fatalf("Copilot parse = auth %#v handled=%v error=%v", auth, handled, errParse)
	}
	discovered := host.ModelsForAuth(context.Background(), auth)
	if !discovered.Handled || discovered.Err != nil || discovered.Provider != pluginIdentifier || len(discovered.Models) != 2 ||
		discovered.Models[0].ID != "gpt-4.1" || discovered.Models[1].ID != "gpt-5.4" {
		t.Fatalf("models for auth = %#v", discovered)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	host.RegisterExecutors(manager, nil)
	executor, registeredExecutor := manager.Executor(pluginIdentifier)
	if !registeredExecutor || executor == nil || !host.OwnsExecutor(executor) {
		t.Fatalf("real host executor registration = registered=%v executor=%T", registeredExecutor, executor)
	}

	var requestMu sync.Mutex
	requestModes := make([]bool, 0, 2)
	rawResponsesRequests := 0
	transport := integrationRoundTripper(func(req *http.Request) (*http.Response, error) {
		body, errReadBody := io.ReadAll(req.Body)
		if errReadBody != nil {
			return nil, fmt.Errorf("read integration upstream request: %w", errReadBody)
		}
		var payload map[string]any
		if errDecode := json.Unmarshal(body, &payload); errDecode != nil {
			return nil, fmt.Errorf("decode integration upstream request: %w", errDecode)
		}
		if req.Header.Get("Authorization") != "Bearer "+storage.CopilotSessionToken ||
			req.Header.Get("User-Agent") != copilotUserAgent || req.Header.Get("Editor-Version") != copilotEditorVersion ||
			req.Header.Get("Editor-Plugin-Version") != copilotPluginVersion {
			return nil, fmt.Errorf("unexpected integration identity headers")
		}
		if req.URL.String() == individualCopilotAPIURL+"/responses" {
			if payload["model"] != "gpt-5.4" || payload["input"] == nil || payload["stream"] != false {
				return nil, fmt.Errorf("unexpected integration raw Responses payload %s", body)
			}
			requestMu.Lock()
			rawResponsesRequests++
			requestMu.Unlock()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{
					"type":"response.completed",
					"response":{"id":"resp-reusable","object":"response","status":"in_progress","status":"completed",
						"error":{"message":"PRIVATE_RAW_TERMINAL_SENTINEL"},"error":null,"output":[]}
				}`)),
				Request: req,
			}, nil
		}
		stream, _ := payload["stream"].(bool)
		requestMu.Lock()
		requestModes = append(requestModes, stream)
		requestMu.Unlock()
		if req.Method != http.MethodPost || req.URL.String() != individualCopilotAPIURL+"/chat/completions" {
			return nil, fmt.Errorf("unexpected integration upstream target %s %s", req.Method, req.URL)
		}
		if payload["model"] != "gpt-4.1" || payload["messages"] == nil || payload["input"] != nil {
			return nil, fmt.Errorf("unexpected integration upstream payload %s", body)
		}
		responseBody := `{"id":"chatcmpl-integration","object":"chat.completion","created":1,"model":"gpt-4.1","choices":[{"index":0,"message":{"role":"assistant","content":"integration hello"},"finish_reason":"length"}]}`
		contentType := "application/json"
		if stream {
			responseBody = strings.Join([]string{
				`data: {"id":"chatcmpl-integration","object":"chat.completion.chunk","created":1,"model":"gpt-4.1","choices":[{"index":0,"delta":{"role":"assistant","content":"integration hello"},"finish_reason":null}]}`,
				`data: {"id":"chatcmpl-integration","object":"chat.completion.chunk","created":1,"model":"gpt-4.1","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
				`data: [DONE]`,
			}, "\n\n") + "\n\n"
			contentType = "text/event-stream"
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{contentType}, "X-GitHub-Request-Id": []string{"real-host"}},
			Body:       io.NopCloser(strings.NewReader(responseBody)),
			Request:    req,
		}, nil
	})
	baseContext := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	executionContext, cancelExecution := context.WithTimeout(baseContext, 5*time.Second)
	defer cancelExecution()
	responsesPayload := []byte(`{"model":"gpt-4.1","input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	request := coreexecutor.Request{Model: "gpt-4.1", Format: sdktranslator.FormatOpenAIResponse, Payload: responsesPayload}
	options := coreexecutor.Options{
		OriginalRequest: responsesPayload,
		SourceFormat:    sdktranslator.FormatOpenAIResponse,
		ResponseFormat:  sdktranslator.FormatOpenAIResponse,
	}
	response, errExecute := executor.Execute(executionContext, auth, request, options)
	if errExecute != nil {
		t.Fatalf("real host execute: %v", errExecute)
	}
	var responsePayload map[string]any
	if errDecode := json.Unmarshal(response.Payload, &responsePayload); errDecode != nil {
		t.Fatalf("decode real host execute response: %v; payload=%s", errDecode, response.Payload)
	}
	details, _ := responsePayload["incomplete_details"].(map[string]any)
	if responsePayload["status"] != "incomplete" || details["reason"] != "max_output_tokens" ||
		!strings.Contains(string(response.Payload), "integration hello") || response.Headers.Get("X-GitHub-Request-Id") != "real-host" {
		t.Fatalf("real host execute response = payload=%s headers=%v", response.Payload, response.Headers)
	}

	options.Stream = true
	streamResult, errExecuteStream := executor.ExecuteStream(executionContext, auth, request, options)
	if errExecuteStream != nil {
		t.Fatalf("real host execute stream: %v", errExecuteStream)
	}
	var emitted bytes.Buffer
	for chunk := range streamResult.Chunks {
		if chunk.Err != nil {
			t.Fatalf("real host stream chunk: %v", chunk.Err)
		}
		emitted.Write(chunk.Payload)
	}
	if streamResult.Headers.Get("X-GitHub-Request-Id") != "real-host" || !strings.Contains(emitted.String(), "response.incomplete") ||
		!strings.Contains(emitted.String(), `"reason":"max_output_tokens"`) || strings.Contains(emitted.String(), "response.completed") {
		t.Fatalf("real host stream response = payload=%q headers=%v", emitted.String(), streamResult.Headers)
	}
	rawHTTPRequest, errNewRequest := http.NewRequestWithContext(executionContext, http.MethodPost, individualCopilotAPIURL+"/responses", strings.NewReader(
		`{"model":"gpt-5.4","input":[{"role":"user","content":"hello"}],"stream":false}`,
	))
	if errNewRequest != nil {
		t.Fatal(errNewRequest)
	}
	rawResponse, errRawRequest := executor.HttpRequest(executionContext, auth, rawHTTPRequest)
	if rawResponse != nil && rawResponse.Body != nil {
		rawResponse.Body.Close()
	}
	if errRawRequest == nil || strings.Contains(errRawRequest.Error(), "PRIVATE_RAW_TERMINAL_SENTINEL") {
		t.Fatalf("real host raw Responses terminal validation = response=%#v error=%v", rawResponse, errRawRequest)
	}
	requestMu.Lock()
	defer requestMu.Unlock()
	if len(requestModes) != 2 || requestModes[0] || !requestModes[1] || rawResponsesRequests != 1 {
		t.Fatalf("real host upstream requests = modes=%v raw_responses=%d", requestModes, rawResponsesRequests)
	}
}
