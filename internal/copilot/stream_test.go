package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	contract "github.com/1oo1/cpa-github-copilot/internal/contract"
)

// streamFakeHost is a hostRuntime that replays scripted upstream stream reads and
// records everything the service sends to the upstream and downstream sides.
type streamFakeHost struct {
	mu sync.Mutex

	readChunks         []hostHTTPStreamReadResponse
	upstream           []hostHTTPCall
	readStreamIDs      []string
	closedUpstreamIDs  []string
	emitted            [][]byte
	emitStreamIDs      []string
	downstreamClosedID string
	downstreamError    string
	upstreamClosed     bool
	downstreamClosed   bool
	emitError          bool
	logs               []hostLogEntry
	done               chan struct{}
	doneOnce           sync.Once
}

// shutdownStreamFakeHost blocks inside emit and upstream reads so a test can
// observe how shutdown cancels and joins an in-flight forwarding task.
type shutdownStreamFakeHost struct {
	mu sync.Mutex

	readCount        int
	emitStarted      chan struct{}
	emitRelease      chan struct{}
	upstreamClosed   chan struct{}
	downstreamClosed chan struct{}
	emitOnce         sync.Once
	upstreamOnce     sync.Once
	downstreamOnce   sync.Once
}

func newShutdownStreamHost() *shutdownStreamFakeHost {
	return &shutdownStreamFakeHost{
		emitStarted:      make(chan struct{}),
		emitRelease:      make(chan struct{}),
		upstreamClosed:   make(chan struct{}),
		downstreamClosed: make(chan struct{}),
	}
}

func (f *shutdownStreamFakeHost) do(context.Context, string, contract.HTTPRequest) (contract.HTTPResponse, error) {
	return contract.HTTPResponse{}, fmt.Errorf("unexpected host HTTP call")
}

func (f *shutdownStreamFakeHost) openStream(context.Context, string, contract.HTTPRequest) (hostHTTPStreamResponse, error) {
	return hostHTTPStreamResponse{StatusCode: 200, Headers: http.Header{"Content-Type": []string{"text/event-stream"}}, StreamID: "upstream-shutdown"}, nil
}

func (f *shutdownStreamFakeHost) readStream(string) (hostHTTPStreamReadResponse, error) {
	f.mu.Lock()
	f.readCount++
	readCount := f.readCount
	f.mu.Unlock()
	if readCount == 1 {
		return hostHTTPStreamReadResponse{Payload: []byte("data: {\"id\":\"chatcmpl-shutdown\",\"choices\":[]}\n\n")}, nil
	}
	<-f.upstreamClosed
	return hostHTTPStreamReadResponse{Done: true}, nil
}

func (f *shutdownStreamFakeHost) closeStream(string) {
	f.upstreamOnce.Do(func() { close(f.upstreamClosed) })
}

func (f *shutdownStreamFakeHost) emit(string, []byte) error {
	f.emitOnce.Do(func() { close(f.emitStarted) })
	<-f.emitRelease
	return nil
}

func (f *shutdownStreamFakeHost) closeOutput(string, string) {
	f.downstreamOnce.Do(func() { close(f.downstreamClosed) })
}

func (f *shutdownStreamFakeHost) log(string, string, string, map[string]any) {}

func newStreamHost(chunks ...hostHTTPStreamReadResponse) *streamFakeHost {
	return &streamFakeHost{readChunks: chunks, done: make(chan struct{})}
}

func (f *streamFakeHost) do(context.Context, string, contract.HTTPRequest) (contract.HTTPResponse, error) {
	return contract.HTTPResponse{}, fmt.Errorf("unexpected host HTTP call")
}

func (f *streamFakeHost) openStream(_ context.Context, callbackID string, req contract.HTTPRequest) (hostHTTPStreamResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upstream = append(f.upstream, newHostHTTPCall(callbackID, req))
	return hostHTTPStreamResponse{
		StatusCode: 200,
		Headers:    http.Header{"Content-Type": []string{"text/event-stream"}, "x-copilot-service-request-id": []string{"synthetic-stream-service"}, "x-github-copilot-request-te": []string{" TRUE "}},
		StreamID:   "upstream-1",
	}, nil
}

func (f *streamFakeHost) readStream(streamID string) (hostHTTPStreamReadResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readStreamIDs = append(f.readStreamIDs, streamID)
	if len(f.readChunks) == 0 {
		return hostHTTPStreamReadResponse{}, fmt.Errorf("unexpected stream read")
	}
	chunk := f.readChunks[0]
	f.readChunks = f.readChunks[1:]
	return chunk, nil
}

func (f *streamFakeHost) closeStream(streamID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upstreamClosed = true
	f.closedUpstreamIDs = append(f.closedUpstreamIDs, streamID)
}

func (f *streamFakeHost) emit(streamID string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.emitError {
		return fmt.Errorf("downstream closed")
	}
	f.emitted = append(f.emitted, append([]byte(nil), payload...))
	f.emitStreamIDs = append(f.emitStreamIDs, streamID)
	return nil
}

func (f *streamFakeHost) closeOutput(streamID, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downstreamClosed = true
	f.downstreamClosedID = streamID
	f.downstreamError = message
	f.doneOnce.Do(func() { close(f.done) })
}

func (f *streamFakeHost) log(callbackID, level, event string, fields map[string]any) {
	entry := newHostLogEntry(callbackID, level, event, fields)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, entry)
}

func (f *streamFakeHost) wait(t *testing.T) {
	t.Helper()
	select {
	case <-f.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for downstream stream close")
	}
}

func (f *streamFakeHost) snapshotLogs() []hostLogEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]hostLogEntry(nil), f.logs...)
}

func TestExecuteStreamPassThroughNormalizesChatChunksAndClosesAtDone(t *testing.T) {
	chatChunk := `{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`
	fake := newStreamHost(
		hostHTTPStreamReadResponse{Payload: []byte("data: " + chatChunk + "\n\n")},
		hostHTTPStreamReadResponse{Payload: []byte("data: [DONE]\n\n")},
	)
	service := newService(fake)
	now := time.Unix(100_000, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"gpt-6","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	headers, errStream := service.executeStreamRequest(context.Background(), "callback-stream", "downstream-1", contract.ExecutorRequest{
		AuthID: "auth", Model: "gpt-6", Format: formatOpenAI, SourceFormat: formatOpenAI,
		Payload: payload, OriginalRequest: payload,
		StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-6", Format: formatOpenAI})),
	})
	if errStream != nil {
		t.Fatal(errStream)
	}
	if headers.Get("Content-Type") != "text/event-stream" || headers.Get("X-Copilot-Service-Request-Id") != "synthetic-stream-service" || headers.Get("X-GitHub-Copilot-Request-Te") != " TRUE " {
		t.Fatalf("stream headers = %#v", headers)
	}
	fake.wait(t)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !fake.upstreamClosed || !fake.downstreamClosed || fake.downstreamError != "" {
		t.Fatalf("close state: upstream=%v downstream=%v error=%q", fake.upstreamClosed, fake.downstreamClosed, fake.downstreamError)
	}
	if got := string(bytesJoin(fake.emitted)); got != chatChunk || !json.Valid([]byte(got)) {
		t.Fatalf("emitted = %q", got)
	}
	if len(fake.upstream) != 1 || fake.upstream[0].CallbackID != "callback-stream" || fake.upstream[0].Method != http.MethodPost ||
		fake.upstream[0].URL != "https://api.individual.githubcopilot.com/chat/completions" {
		t.Fatalf("upstream stream request = %#v", fake.upstream)
	}
	var upstreamBody map[string]any
	if json.Unmarshal(fake.upstream[0].Body, &upstreamBody) != nil || upstreamBody["model"] != "gpt-6" || upstreamBody["stream"] != true {
		t.Fatalf("upstream stream body = %s", fake.upstream[0].Body)
	}
	if got := fake.upstream[0].Headers.Get("Authorization"); got != "Bearer tid=session;proxy-ep=proxy.individual.githubcopilot.com" {
		t.Fatalf("upstream stream Authorization = %q", got)
	}
	for _, id := range fake.readStreamIDs {
		if id != "upstream-1" {
			t.Fatalf("read stream ids = %v, want only upstream-1", fake.readStreamIDs)
		}
	}
	if len(fake.closedUpstreamIDs) == 0 || fake.closedUpstreamIDs[0] != "upstream-1" {
		t.Fatalf("closed upstream ids = %v", fake.closedUpstreamIDs)
	}
	for _, id := range fake.emitStreamIDs {
		if id != "downstream-1" {
			t.Fatalf("emit stream ids = %v, want only downstream-1", fake.emitStreamIDs)
		}
	}
	if fake.downstreamClosedID != "downstream-1" {
		t.Fatalf("downstream closed id = %q", fake.downstreamClosedID)
	}
}

func TestExecuteStreamPassThroughAcceptsSingleNewlineChatEvents(t *testing.T) {
	chatChunk := `{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`
	fake := newStreamHost(
		hostHTTPStreamReadResponse{Payload: []byte("data: " + chatChunk + "\n")},
		hostHTTPStreamReadResponse{Payload: []byte("data: [DONE]\n")},
	)
	service := newService(fake)
	now := time.Unix(102_500, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"gpt-6","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, errStream := service.executeStreamRequest(context.Background(), "callback-stream", "downstream-single-newline-chat", contract.ExecutorRequest{
		AuthID: "auth", Model: "gpt-6", Format: formatOpenAI, SourceFormat: formatOpenAI,
		Payload: payload, OriginalRequest: payload,
		StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-6", Format: formatOpenAI})),
	})
	if errStream != nil {
		t.Fatal(errStream)
	}
	fake.wait(t)
	fake.mu.Lock()
	emitted := string(bytesJoin(fake.emitted))
	downstreamError := fake.downstreamError
	fake.mu.Unlock()
	if downstreamError != "" || emitted != chatChunk {
		t.Fatalf("single-newline stream: error=%q emitted=%q", downstreamError, emitted)
	}
}

func TestExecuteStreamPassThroughFramesSplitSSEData(t *testing.T) {
	fake := newStreamHost(
		hostHTTPStreamReadResponse{Payload: []byte(`data: {"type":"response.created","response":{"id":"resp`)},
		hostHTTPStreamReadResponse{Payload: []byte("-1\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"output\":[]}}\n\n")},
	)
	service := newService(fake)
	now := time.Unix(105_000, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"gpt-6","input":"hi","stream":true}`)
	_, errStream := service.executeStreamRequest(context.Background(), "callback-stream", "downstream-split-sse", contract.ExecutorRequest{
		AuthID: "auth", Model: "gpt-6", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
		Payload: payload, OriginalRequest: payload,
		StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-6", Format: formatOpenAIResponse})),
	})
	if errStream != nil {
		t.Fatal(errStream)
	}
	fake.wait(t)
	fake.mu.Lock()
	emitted := append([][]byte(nil), fake.emitted...)
	downstreamError := fake.downstreamError
	fake.mu.Unlock()
	if downstreamError != "" {
		t.Fatalf("downstream stream error = %q", downstreamError)
	}
	want := []string{
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n",
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"output\":[]}}\n\n",
	}
	if len(emitted) != len(want) {
		t.Fatalf("emitted chunks = %q, want %q", emitted, want)
	}
	for index := range want {
		if string(emitted[index]) != want[index] {
			t.Fatalf("emitted[%d] = %q, want %q", index, emitted[index], want[index])
		}
		data := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(emitted[index])), "data:"))
		if !json.Valid([]byte(data)) {
			t.Fatalf("emitted[%d] has invalid SSE JSON: %q", index, data)
		}
	}
}

func TestExecuteStreamGrokNormalizesCodexToolsAndRestoresCustomCalls(t *testing.T) {
	stream := "" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_shell","type":"function_call","call_id":"call_shell","name":"shell","arguments":""}}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_shell","delta":"pwd"}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_shell","arguments":"{\"input\":\"pwd\"}"}` + "\n\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_shell","type":"function_call","call_id":"call_shell","name":"shell","arguments":"{\"input\":\"pwd\"}","status":"completed"}}` + "\n\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","output":[{"id":"fc_shell","type":"function_call","call_id":"call_shell","name":"shell","arguments":"{\"input\":\"pwd\"}"}]}}` + "\n\n"
	fake := newStreamHost(hostHTTPStreamReadResponse{Payload: []byte(stream)})
	service := newService(fake)
	now := time.Unix(106_000, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{
		"model":"grok-4.6","stream":true,"service_tier":"priority","input":"run pwd",
		"tools":[
			{"type":"custom","name":"apply_patch","format":{"type":"grammar"}},
			{"type":"custom","name":"shell","format":{"type":"grammar"}}
		]
	}`)
	_, errStream := service.executeStreamRequest(context.Background(), "", "downstream-grok-custom-tools", contract.ExecutorRequest{
		AuthID: "auth", Model: "grok-4.6", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
		Payload: payload, OriginalRequest: payload,
		StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "grok-4.6", Family: "grok-4.6", Format: formatOpenAIResponse})),
	})
	if errStream != nil {
		t.Fatal(errStream)
	}
	fake.wait(t)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.downstreamError != "" || len(fake.upstream) != 1 {
		t.Fatalf("stream state: error=%q upstream=%d", fake.downstreamError, len(fake.upstream))
	}
	var upstream map[string]any
	if errDecode := json.Unmarshal(fake.upstream[0].Body, &upstream); errDecode != nil {
		t.Fatal(errDecode)
	}
	if _, exists := upstream["service_tier"]; exists {
		t.Fatal("service_tier was sent upstream")
	}
	tools := upstream["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["type"] != "function" || tools[0].(map[string]any)["name"] != "shell" {
		t.Fatalf("upstream tools = %#v", tools)
	}
	emitted := string(bytesJoin(fake.emitted))
	for _, want := range []string{"response.custom_tool_call_input.delta", "response.custom_tool_call_input.done", `"type":"custom_tool_call"`, `"input":"pwd"`} {
		if !strings.Contains(emitted, want) {
			t.Fatalf("restored stream missing %q: %s", want, emitted)
		}
	}
	if strings.Contains(emitted, "response.function_call_arguments") {
		t.Fatalf("function call events leaked to Codex: %s", emitted)
	}
}

func TestExecuteStreamNativeClaudeClosesAtMessageStop(t *testing.T) {
	fake := newStreamHost(
		hostHTTPStreamReadResponse{Payload: []byte(`data: {"type":"message_start","message":{"id":"msg-1"}}` + "\n\n")},
		hostHTTPStreamReadResponse{Payload: []byte(`data: {"type":"message_stop"}` + "\n\n")},
	)
	service := newService(fake)
	now := time.Unix(107_500, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"claude-sonnet-5-1","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, errStream := service.executeStreamRequest(context.Background(), "callback-stream", "downstream-native-claude", contract.ExecutorRequest{
		AuthID: "auth", Model: "claude-sonnet-5-1", Format: formatClaude, SourceFormat: formatClaude,
		Payload: payload, OriginalRequest: payload,
		StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "claude-sonnet-5-1", Format: formatClaude})),
	})
	if errStream != nil {
		t.Fatal(errStream)
	}
	fake.wait(t)
	fake.mu.Lock()
	emitted := string(bytesJoin(fake.emitted))
	downstreamError := fake.downstreamError
	fake.mu.Unlock()
	if downstreamError != "" || !strings.Contains(emitted, `"type":"message_stop"`) {
		t.Fatalf("native Claude terminal state: error=%q emitted=%q", downstreamError, emitted)
	}
}

func TestExecuteStreamTranslatesSplitSSEFrames(t *testing.T) {
	first := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-6","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}` + "\n\n"
	finish := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-6","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	done := "data: [DONE]\n\n"
	fake := newStreamHost(
		hostHTTPStreamReadResponse{Payload: []byte(first[:31])},
		hostHTTPStreamReadResponse{Payload: []byte(first[31:] + finish + done)},
	)
	service := newService(fake)
	now := time.Unix(110_000, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"gpt-6","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, errStream := service.executeStreamRequest(context.Background(), "callback-stream", "downstream-2", contract.ExecutorRequest{
		AuthID: "auth", Model: "gpt-6", Format: formatClaude, SourceFormat: formatClaude,
		Payload: payload, OriginalRequest: payload,
		StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-6", Format: formatOpenAI})),
	})
	if errStream != nil {
		t.Fatal(errStream)
	}
	fake.wait(t)
	fake.mu.Lock()
	emitted := string(bytesJoin(fake.emitted))
	upstreamClosed, downstreamError := fake.upstreamClosed, fake.downstreamError
	fake.mu.Unlock()
	if !upstreamClosed || downstreamError != "" {
		t.Fatalf("close state: upstream=%v error=%q", upstreamClosed, downstreamError)
	}
	if !strings.Contains(emitted, "event: message_start") || !strings.Contains(emitted, "hello") || !strings.Contains(emitted, "event: message_stop") {
		t.Fatalf("translated stream = %q", emitted)
	}
}

func TestExecuteStreamPassesAnthropicWebSearchToClaudeEndpoint(t *testing.T) {
	frames := []string{
		`data: {"type":"message_start","message":{"id":"msg-search","model":"claude-sonnet-5-1"}}` + "\n\n",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srv_123","name":"web_search","input":{"query":"HarmonyOS command-line tools"}}}` + "\n\n",
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"web_search_tool_result","tool_use_id":"srv_123","content":[{"type":"web_search_result","title":"HarmonyOS","url":"https://example.com/harmonyos"}]}}` + "\n\n",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":3,"output_tokens":2}}` + "\n\n",
		`data: {"type":"message_stop"}` + "\n\n",
	}
	chunks := make([]hostHTTPStreamReadResponse, 0, len(frames))
	for _, frame := range frames {
		chunks = append(chunks, hostHTTPStreamReadResponse{Payload: []byte(frame)})
	}
	fake := newStreamHost(chunks...)
	service := newService(fake)
	now := time.Unix(115_000, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{
		"model":"claude-sonnet-5-1",
		"max_tokens":64000,
		"messages":[{"role":"user","content":"Perform a web search"}],
		"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":1}],
		"tool_choice":{"type":"tool","name":"web_search"}
	}`)
	_, errStream := service.executeStreamRequest(context.Background(), "callback-web-search", "downstream-web-search", contract.ExecutorRequest{
		AuthID: "auth", Model: "claude-sonnet-5-1", Format: formatClaude, SourceFormat: formatClaude,
		Payload: payload, OriginalRequest: payload,
		StorageJSON: mustJSON(t, executorStorage(now, storedModel{
			ID: "claude-sonnet-5-1", Format: formatClaude,
		})),
	})
	if errStream != nil {
		t.Fatal(errStream)
	}
	fake.wait(t)
	fake.mu.Lock()
	emitted := string(bytesJoin(fake.emitted))
	upstream := append([]hostHTTPCall(nil), fake.upstream...)
	downstreamError := fake.downstreamError
	fake.mu.Unlock()
	if downstreamError != "" {
		t.Fatalf("downstream stream error = %q", downstreamError)
	}
	if len(upstream) != 1 || !strings.HasSuffix(upstream[0].URL, "/v1/messages") {
		t.Fatalf("upstream requests = %#v", upstream)
	}
	for _, needle := range []string{`"type":"server_tool_use"`, `"type":"web_search_tool_result"`, "HarmonyOS command-line tools", `"type":"message_stop"`} {
		if !strings.Contains(emitted, needle) {
			t.Fatalf("native Claude web search stream missing %q: %s", needle, emitted)
		}
	}
}

func TestExecuteStreamDownstreamFailureStillClosesUpstream(t *testing.T) {
	fake := newStreamHost(hostHTTPStreamReadResponse{Payload: []byte("data: chunk\n\n"), Done: true})
	fake.emitError = true
	service := newService(fake)
	now := time.Unix(120_000, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"gpt-6","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, errStream := service.executeStreamRequest(context.Background(), "", "downstream-3", contract.ExecutorRequest{
		AuthID: "auth", Model: "gpt-6", Format: formatOpenAI, SourceFormat: formatOpenAI,
		Payload: payload, StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-6", Format: formatOpenAI})),
	})
	if errStream != nil {
		t.Fatal(errStream)
	}
	fake.wait(t)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !fake.upstreamClosed || fake.downstreamError == "" || strings.Contains(fake.downstreamError, "chunk") {
		t.Fatalf("close state: upstream=%v error=%q", fake.upstreamClosed, fake.downstreamError)
	}
}

func TestShutdownCancelsAndWaitsForForwardingTasks(t *testing.T) {
	fake := newShutdownStreamHost()
	service := newService(fake)
	now := time.Unix(122_500, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"gpt-6","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, errStream := service.executeStreamRequest(context.Background(), "callback-stream", "downstream-shutdown-wait", contract.ExecutorRequest{
		AuthID: "auth", Model: "gpt-6", Format: formatOpenAI, SourceFormat: formatOpenAI,
		Payload: payload, OriginalRequest: payload,
		StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-6", Format: formatOpenAI})),
	})
	if errStream != nil {
		t.Fatal(errStream)
	}
	select {
	case <-fake.emitStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("forwarder did not enter host emit")
	}

	shutdownDone := make(chan struct{})
	go func() {
		service.shutdown()
		close(shutdownDone)
	}()
	select {
	case <-fake.upstreamClosed:
	case <-time.After(5 * time.Second):
		close(fake.emitRelease)
		t.Fatal("shutdown did not cancel the upstream stream")
	}
	select {
	case <-shutdownDone:
		close(fake.emitRelease)
		t.Fatal("shutdown returned while a forwarding task was still using host callbacks")
	case <-time.After(50 * time.Millisecond):
	}
	close(fake.emitRelease)
	select {
	case <-shutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not join the forwarding task")
	}
	select {
	case <-fake.downstreamClosed:
	default:
		t.Fatal("forwarder did not close the downstream stream before shutdown returned")
	}
	service.streamMu.Lock()
	remaining := len(service.streamTasks)
	service.streamMu.Unlock()
	if remaining != 0 {
		t.Fatalf("forwarding tasks remaining after shutdown = %d", remaining)
	}
}

func TestExecuteStreamUpstreamErrorStillClosesBothStreams(t *testing.T) {
	fake := newStreamHost(hostHTTPStreamReadResponse{Error: "private upstream detail", Done: true})
	service := newService(fake)
	now := time.Unix(125_000, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"gpt-6","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, errStream := service.executeStreamRequest(context.Background(), "", "downstream-upstream-error", contract.ExecutorRequest{
		AuthID: "auth", Model: "gpt-6", Format: formatOpenAI, SourceFormat: formatOpenAI,
		Payload: payload, StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-6", Format: formatOpenAI})),
	})
	if errStream != nil {
		t.Fatal(errStream)
	}
	fake.wait(t)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !fake.upstreamClosed || !fake.downstreamClosed || fake.downstreamError == "" || strings.Contains(fake.downstreamError, "private upstream detail") {
		t.Fatalf("close state: upstream=%v downstream=%v error=%q", fake.upstreamClosed, fake.downstreamClosed, fake.downstreamError)
	}
}

func TestSSEFramerHandlesSplitAndMultipleFrames(t *testing.T) {
	framer := newSSEFramer(1024)
	frames, errPush := framer.Push([]byte("event: one\ndata: {\"a\":"))
	if errPush != nil || len(frames) != 0 {
		t.Fatalf("first push = %#v, %v", frames, errPush)
	}
	frames, errPush = framer.Push([]byte("1}\n\nevent: two\r\ndata: {\"b\":2}\r\n\r\n"))
	if errPush != nil || len(frames) != 2 {
		t.Fatalf("second push = %#v, %v", frames, errPush)
	}
	if got := string(normalizeSSEFrame(frames[0])); got != `data: {"a":1}` {
		t.Fatalf("first normalized frame = %q", got)
	}
	if got := string(normalizeSSEFrame(frames[1])); got != `data: {"b":2}` {
		t.Fatalf("second normalized frame = %q", got)
	}
	if tail := framer.Flush(); len(tail) != 0 {
		t.Fatalf("tail = %q", tail)
	}
}

func TestSSEFramerEmitsCompleteJSONDataLineWithoutBlankSeparator(t *testing.T) {
	framer := newSSEFramer(1024)
	frames, errPush := framer.Push([]byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n"))
	if errPush != nil || len(frames) != 1 {
		t.Fatalf("frames = %#v, error = %v", frames, errPush)
	}
	if got := string(normalizeSSEFrame(frames[0])); got != `data: {"type":"response.completed"}` {
		t.Fatalf("normalized frame = %q", got)
	}
}

func TestSSEFramerWaitsForSplitMultiLineJSONEvent(t *testing.T) {
	framer := newSSEFramer(1024)
	frames, errPush := framer.Push([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\n"))
	if errPush != nil || len(frames) != 0 {
		t.Fatalf("partial frames = %#v, error = %v", frames, errPush)
	}
	frames, errPush = framer.Push([]byte("data: \"response\":{\"id\":\"resp-1\"}}\n\n"))
	if errPush != nil || len(frames) != 1 {
		t.Fatalf("completed frames = %#v, error = %v", frames, errPush)
	}
	if got := string(normalizeSSEFrame(frames[0])); got != "data: {\"type\":\"response.completed\",\n\"response\":{\"id\":\"resp-1\"}}" {
		t.Fatalf("normalized frame = %q", got)
	}
}

func TestSSEFramerBoundsPartialEvent(t *testing.T) {
	framer := newSSEFramer(16)
	if _, errPush := framer.Push([]byte("data: 12345678901234567890")); errPush == nil {
		t.Fatal("oversized partial event was accepted")
	}
}

func TestSSEFramerAllowsLargeChunkOfSmallCompleteEvents(t *testing.T) {
	framer := newSSEFramer(16)
	frames, errPush := framer.Push([]byte("data: 1\n\ndata: 2\n\ndata: 3\n\n"))
	if errPush != nil || len(frames) != 3 {
		t.Fatalf("frames = %#v, error = %v", frames, errPush)
	}
}

func bytesJoin(chunks [][]byte) []byte {
	var out []byte
	for _, chunk := range chunks {
		out = append(out, chunk...)
	}
	return out
}

func hasLogEvent(logs []hostLogEntry, event string) bool {
	for _, entry := range logs {
		if entry.Fields["event"] == event {
			return true
		}
	}
	return false
}

func executeOpenAIStream(t *testing.T, fake *streamFakeHost, streamID string, at time.Time) {
	t.Helper()
	service := newService(fake)
	service.now = func() time.Time { return at }
	payload := []byte(`{"model":"gpt-6","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, errStream := service.executeStreamRequest(context.Background(), "callback-stream", streamID, contract.ExecutorRequest{
		AuthID: "auth", Model: "gpt-6", Format: formatOpenAI, SourceFormat: formatOpenAI,
		Payload: payload, StorageJSON: mustJSON(t, executorStorage(at, storedModel{ID: "gpt-6", Format: formatOpenAI})),
	})
	if errStream != nil {
		t.Fatal(errStream)
	}
	fake.wait(t)
}

func TestForwardStreamSuccessLogsCompletedDebug(t *testing.T) {
	fake := newStreamHost(
		hostHTTPStreamReadResponse{Payload: []byte("data: first\n\n")},
		hostHTTPStreamReadResponse{Payload: []byte("data: [DONE]\n\n"), Done: true},
	)
	executeOpenAIStream(t, fake, "downstream-success-log", time.Unix(135_000, 0).UTC())
	logs := fake.snapshotLogs()
	if hasLogEvent(logs, "inference.stream.forward_failed") {
		t.Fatal("successful stream must not log inference.stream.forward_failed")
	}
	entry := findLogEvent(t, logs, "inference.stream.completed")
	if entry.Level != "debug" {
		t.Fatalf("completed level = %q, want debug", entry.Level)
	}
	if entry.Fields["success"] != true {
		t.Fatalf("success = %v, want true", entry.Fields["success"])
	}
	if _, hasReason := entry.Fields["reason"]; hasReason {
		t.Fatalf("completed log should not include a reason field")
	}
}

func TestForwardStreamBenignDownstreamCloseLogsDebug(t *testing.T) {
	fake := newStreamHost(hostHTTPStreamReadResponse{Payload: []byte("data: chunk\n\n"), Done: true})
	fake.emitError = true
	executeOpenAIStream(t, fake, "downstream-benign-emit", time.Unix(130_000, 0).UTC())
	logs := fake.snapshotLogs()
	if hasLogEvent(logs, "inference.stream.forward_failed") {
		t.Fatal("benign downstream close must not log inference.stream.forward_failed")
	}
	entry := findLogEvent(t, logs, "inference.stream.client_disconnected")
	if entry.Level != "debug" {
		t.Fatalf("client_disconnected level = %q, want debug", entry.Level)
	}
	if entry.Fields["reason"] != streamReasonDownstreamClosed {
		t.Fatalf("reason = %v, want %q", entry.Fields["reason"], streamReasonDownstreamClosed)
	}
	if entry.Fields["success"] != false {
		t.Fatalf("success = %v, want false", entry.Fields["success"])
	}
}

func TestForwardStreamReadFailureLogsWarn(t *testing.T) {
	fake := newStreamHost() // no chunks: the host stream read call fails
	executeOpenAIStream(t, fake, "downstream-read-failure", time.Unix(131_000, 0).UTC())
	logs := fake.snapshotLogs()
	if hasLogEvent(logs, "inference.stream.client_disconnected") {
		t.Fatal("a host read failure must not be logged as a benign client disconnect")
	}
	entry := findLogEvent(t, logs, "inference.stream.forward_failed")
	if entry.Level != "warn" {
		t.Fatalf("forward_failed level = %q, want warn", entry.Level)
	}
	if entry.Fields["reason"] != streamReasonReadFailed {
		t.Fatalf("reason = %v, want %q", entry.Fields["reason"], streamReasonReadFailed)
	}
}

func TestForwardStreamUpstreamErrorLogsWarn(t *testing.T) {
	fake := newStreamHost(hostHTTPStreamReadResponse{Error: "private upstream detail", Done: true})
	executeOpenAIStream(t, fake, "downstream-upstream-warn", time.Unix(132_000, 0).UTC())
	logs := fake.snapshotLogs()
	if hasLogEvent(logs, "inference.stream.client_disconnected") {
		t.Fatal("upstream error must not be logged as client_disconnected")
	}
	entry := findLogEvent(t, logs, "inference.stream.forward_failed")
	if entry.Level != "warn" {
		t.Fatalf("forward_failed level = %q, want warn", entry.Level)
	}
	if entry.Fields["reason"] != streamReasonUpstreamError {
		t.Fatalf("reason = %v, want %q", entry.Fields["reason"], streamReasonUpstreamError)
	}
	if entry.Fields["upstream_cause"] != upstreamCauseOther {
		t.Fatalf("upstream_cause = %v, want %q", entry.Fields["upstream_cause"], upstreamCauseOther)
	}
	errMessage, _ := entry.Fields["error"].(string)
	if strings.Contains(errMessage, "private upstream detail") {
		t.Fatalf("log leaked upstream detail: %q", errMessage)
	}
	if errMessage != "GitHub Copilot upstream stream failed" {
		t.Fatalf("error message = %q", errMessage)
	}
}

func TestForwardStreamUpstreamCancelLogsDebug(t *testing.T) {
	fake := newStreamHost(hostHTTPStreamReadResponse{Error: "context canceled", Done: true})
	executeOpenAIStream(t, fake, "downstream-upstream-cancel", time.Unix(133_000, 0).UTC())
	logs := fake.snapshotLogs()
	if hasLogEvent(logs, "inference.stream.forward_failed") {
		t.Fatal("a cancelled upstream read must not be logged as a forwarding failure")
	}
	entry := findLogEvent(t, logs, "inference.stream.client_disconnected")
	if entry.Level != "debug" {
		t.Fatalf("client_disconnected level = %q, want debug", entry.Level)
	}
	if entry.Fields["reason"] != streamReasonUpstreamCanceled {
		t.Fatalf("reason = %v, want %q", entry.Fields["reason"], streamReasonUpstreamCanceled)
	}
	if entry.Fields["upstream_cause"] != upstreamCauseCanceled {
		t.Fatalf("upstream_cause = %v, want %q", entry.Fields["upstream_cause"], upstreamCauseCanceled)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !fake.upstreamClosed || !fake.downstreamClosed {
		t.Fatalf("close state: upstream=%v downstream=%v", fake.upstreamClosed, fake.downstreamClosed)
	}
	if fake.downstreamError != "GitHub Copilot upstream stream canceled" {
		t.Fatalf("downstream stream error = %q", fake.downstreamError)
	}
}

func TestForwardStreamRemoteHTTP2CancelLogsWarn(t *testing.T) {
	fake := newStreamHost(hostHTTPStreamReadResponse{Error: "stream error: stream ID 3; CANCEL", Done: true})
	executeOpenAIStream(t, fake, "downstream-upstream-remote-cancel", time.Unix(133_500, 0).UTC())
	logs := fake.snapshotLogs()
	if hasLogEvent(logs, "inference.stream.client_disconnected") {
		t.Fatal("a remote HTTP/2 CANCEL must not be logged as a client disconnect")
	}
	entry := findLogEvent(t, logs, "inference.stream.forward_failed")
	if entry.Level != "warn" || entry.Fields["reason"] != streamReasonUpstreamError {
		t.Fatalf("remote cancel log = %#v", entry)
	}
	if entry.Fields["upstream_cause"] != upstreamCauseRemoteCanceled {
		t.Fatalf("upstream_cause = %v, want %q", entry.Fields["upstream_cause"], upstreamCauseRemoteCanceled)
	}
}

func TestForwardStreamNonUpstreamFailureOmitsUpstreamCause(t *testing.T) {
	fake := newStreamHost() // no chunks: the host stream read call fails
	executeOpenAIStream(t, fake, "downstream-no-cause", time.Unix(134_000, 0).UTC())
	entry := findLogEvent(t, fake.snapshotLogs(), "inference.stream.forward_failed")
	if _, hasCause := entry.Fields["upstream_cause"]; hasCause {
		t.Fatalf("read failure should not carry an upstream_cause: %v", entry.Fields["upstream_cause"])
	}
}

func TestClassifyUpstreamStreamError(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"", upstreamCauseOther},
		{"context canceled", upstreamCauseCanceled},
		{"stream error: stream ID 3; CANCEL", upstreamCauseRemoteCanceled},
		{"net/http: request canceled", upstreamCauseCanceled},
		{"request canceled by upstream peer", upstreamCauseRemoteCanceled},
		{"context deadline exceeded", upstreamCauseTimeout},
		{"read tcp 10.0.0.1:443: i/o timeout", upstreamCauseTimeout},
		// net/http reports a client timeout as a cancellation; it must classify
		// as a timeout rather than as a benign client abort.
		{"net/http: request canceled (Client.Timeout exceeded while reading body)", upstreamCauseTimeout},
		{"read tcp 10.0.0.1:443: connection reset by peer", upstreamCauseConnectionReset},
		{"write tcp 10.0.0.1:443: broken pipe", upstreamCauseConnectionReset},
		{"use of closed network connection", upstreamCauseConnectionReset},
		{"unexpected EOF", upstreamCauseEOF},
		{"stream error: stream ID 5; INTERNAL_ERROR", upstreamCauseOther},
		{"private upstream detail", upstreamCauseOther},
	}
	allowed := map[string]bool{
		upstreamCauseCanceled:        true,
		upstreamCauseRemoteCanceled:  true,
		upstreamCauseTimeout:         true,
		upstreamCauseEOF:             true,
		upstreamCauseConnectionReset: true,
		upstreamCauseOther:           true,
	}
	for _, testCase := range cases {
		got := classifyUpstreamStreamError(testCase.raw)
		if got != testCase.want {
			t.Fatalf("classifyUpstreamStreamError(%q) = %q, want %q", testCase.raw, got, testCase.want)
		}
		if !allowed[got] {
			t.Fatalf("classifyUpstreamStreamError(%q) returned uncategorized %q", testCase.raw, got)
		}
	}
}

func TestNewUpstreamStreamErrorRedactsRawDetail(t *testing.T) {
	const raw = "read tcp 10.0.0.1:443->140.82.113.22:443: private upstream detail"
	forwardErr := newUpstreamStreamError(raw)
	if strings.Contains(forwardErr.Error(), "private upstream detail") || strings.Contains(forwardErr.Error(), "10.0.0.1") {
		t.Fatalf("forward error leaked upstream detail: %q", forwardErr.Error())
	}
	if forwardErr.cause != upstreamCauseOther || forwardErr.benign {
		t.Fatalf("forward error = (cause=%q, benign=%v)", forwardErr.cause, forwardErr.benign)
	}
	if cause := upstreamStreamCause(forwardErr); cause != upstreamCauseOther {
		t.Fatalf("upstreamStreamCause = %q, want %q", cause, upstreamCauseOther)
	}
	if cause := upstreamStreamCause(newStreamForwardError(streamReasonReadFailed, "boom", false)); cause != "" {
		t.Fatalf("non-upstream error carried cause %q", cause)
	}
	if cause := upstreamStreamCause(nil); cause != "" {
		t.Fatalf("nil error carried cause %q", cause)
	}
}

func TestClassifyStreamForwardError(t *testing.T) {
	if reason, message, benign := classifyStreamForwardError(nil); reason != "" || message != "" || benign {
		t.Fatalf("nil error classified as (%q, %q, %v)", reason, message, benign)
	}
	benignErr := newStreamForwardError(streamReasonDownstreamClosed, "closed", true)
	if reason, message, benign := classifyStreamForwardError(benignErr); reason != streamReasonDownstreamClosed || message != "closed" || !benign {
		t.Fatalf("benign error classified as (%q, %q, %v)", reason, message, benign)
	}
	fatalErr := newStreamForwardError(streamReasonUpstreamError, "boom", false)
	if reason, _, benign := classifyStreamForwardError(fatalErr); reason != streamReasonUpstreamError || benign {
		t.Fatalf("fatal error classified as (%q, benign=%v)", reason, benign)
	}
	if reason, message, benign := classifyStreamForwardError(errors.New("raw")); reason != "unknown" || message != "raw" || benign {
		t.Fatalf("raw error classified as (%q, %q, %v)", reason, message, benign)
	}
}

func TestSSEFramerBufferExceededReturnsTypedError(t *testing.T) {
	framer := newSSEFramer(16)
	_, errPush := framer.Push([]byte("data: 12345678901234567890"))
	if errPush == nil {
		t.Fatal("oversized partial event was accepted")
	}
	var forwardErr *streamForwardError
	if !errors.As(errPush, &forwardErr) {
		t.Fatalf("framer error type = %T, want *streamForwardError", errPush)
	}
	if forwardErr.reason != streamReasonBufferExceeded || forwardErr.benign {
		t.Fatalf("framer error = (reason=%q, benign=%v)", forwardErr.reason, forwardErr.benign)
	}
}

func TestResponsesPassthroughRequiresTerminalEvent(t *testing.T) {
	fake := newStreamHost(
		hostHTTPStreamReadResponse{Payload: []byte(`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n"), Done: true},
	)
	service := newService(fake)
	now := time.Unix(190_000, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"gpt-6","input":[{"role":"user","content":"hi"}]}`)
	_, errStream := service.executeStreamRequest(context.Background(), "", "downstream-missing-terminal", contract.ExecutorRequest{
		AuthID: "auth", Model: "gpt-6", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
		Payload: payload, OriginalRequest: payload,
		StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-6", Format: formatOpenAIResponse})),
	})
	if errStream != nil {
		t.Fatal(errStream)
	}
	fake.wait(t)
	fake.mu.Lock()
	emitted := string(bytesJoin(fake.emitted))
	downstreamError := fake.downstreamError
	logs := append([]hostLogEntry(nil), fake.logs...)
	fake.mu.Unlock()
	if downstreamError == "" {
		t.Fatalf("stream without a terminal Responses event unexpectedly closed cleanly; emitted=%q", emitted)
	}
	if !strings.Contains(strings.ToLower(downstreamError), "unexpected eof") {
		t.Fatalf("missing-terminal stream error must be classified as a connection-lifecycle failure: %q", downstreamError)
	}
	if !strings.Contains(emitted, "response.output_text.delta") {
		t.Fatalf("non-terminal event was not preserved byte-for-byte: %q", emitted)
	}
	if strings.Contains(emitted, "response.completed") {
		t.Fatalf("must never synthesize response.completed: emitted=%q", emitted)
	}
	entry := findLogEvent(t, logs, "inference.stream.forward_failed")
	if entry.Fields["reason"] != streamReasonMissingTerminal {
		t.Fatalf("forward_failed reason = %#v", entry.Fields["reason"])
	}
}

func TestResponsesPassthroughAcceptsEachTerminalType(t *testing.T) {
	for _, terminal := range []string{
		`{"type":"response.completed","response":{"id":"resp_1"}}`,
		`{"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete"}}`,
		`{"type":"response.failed","response":{"id":"resp_1","status":"failed"}}`,
		`{"type":"error","error":{"message":"boom"}}`,
	} {
		t.Run(terminal, func(t *testing.T) {
			fake := newStreamHost(
				hostHTTPStreamReadResponse{Payload: []byte("data: " + terminal + "\n\n")},
			)
			service := newService(fake)
			now := time.Unix(191_000, 0).UTC()
			service.now = func() time.Time { return now }
			payload := []byte(`{"model":"gpt-6","input":[{"role":"user","content":"hi"}]}`)
			_, errStream := service.executeStreamRequest(context.Background(), "", "downstream-terminal-ok", contract.ExecutorRequest{
				AuthID: "auth", Model: "gpt-6", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
				Payload: payload, OriginalRequest: payload,
				StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-6", Format: formatOpenAIResponse})),
			})
			if errStream != nil {
				t.Fatal(errStream)
			}
			fake.wait(t)
			fake.mu.Lock()
			downstreamError := fake.downstreamError
			fake.mu.Unlock()
			if downstreamError != "" {
				t.Fatalf("unexpected error = %q", downstreamError)
			}
		})
	}
}

func TestResponsesStreamLogsTerminalStatus(t *testing.T) {
	fake := newStreamHost(
		hostHTTPStreamReadResponse{Payload: []byte(`data: {"type":"response.completed","response":{"id":"resp_1"}}` + "\n\n")},
	)
	service := newService(fake)
	now := time.Unix(196_000, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"gpt-6","input":[{"role":"user","content":"hi"}]}`)
	_, errStream := service.executeStreamRequest(context.Background(), "", "downstream-terminal-status-log", contract.ExecutorRequest{
		AuthID: "auth", Model: "gpt-6", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
		Payload: payload, OriginalRequest: payload,
		StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-6", Format: formatOpenAIResponse})),
	})
	if errStream != nil {
		t.Fatal(errStream)
	}
	fake.wait(t)
	fake.mu.Lock()
	logs := append([]hostLogEntry(nil), fake.logs...)
	fake.mu.Unlock()
	entry := findLogEvent(t, logs, "inference.stream.completed")
	if entry.Fields["responses_terminal_status"] != "response.completed" {
		t.Fatalf("responses_terminal_status = %#v", entry.Fields["responses_terminal_status"])
	}
}

func TestResponsesStreamLogsNeverContainCompactionOrPromptContent(t *testing.T) {
	const encryptedSentinel = "SENTINEL_ENCRYPTED_REASONING_PAYLOAD"
	const promptSentinel = "SENTINEL_USER_PROMPT_TEXT"
	compaction := `{"type":"response.output_item.done","output_index":0,"item":{"type":"compaction","id":"cmp_1","encrypted_content":"` + encryptedSentinel + `"}}`
	completed := `{"type":"response.completed","response":{"id":"resp_1"}}`
	fake := newStreamHost(
		hostHTTPStreamReadResponse{Payload: []byte("data: " + compaction + "\n\ndata: " + completed + "\n\n"), Done: true},
	)
	service := newService(fake)
	now := time.Unix(197_000, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"gpt-6","input":[{"role":"user","content":"` + promptSentinel + `"}]}`)
	_, errStream := service.executeStreamRequest(context.Background(), "", "downstream-log-content-sentinel", contract.ExecutorRequest{
		AuthID: "auth", Model: "gpt-6", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
		Payload: payload, OriginalRequest: payload,
		StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-6", Format: formatOpenAIResponse})),
	})
	if errStream != nil {
		t.Fatal(errStream)
	}
	fake.wait(t)
	fake.mu.Lock()
	emitted := string(bytesJoin(fake.emitted))
	logs := append([]hostLogEntry(nil), fake.logs...)
	fake.mu.Unlock()
	// The client-facing stream must carry the real content...
	if !strings.Contains(emitted, encryptedSentinel) {
		t.Fatalf("compaction content was not forwarded to the client: %q", emitted)
	}
	// ...but diagnostic logs must never contain prompt text or encrypted reasoning content.
	assertLogsExclude(t, logs, encryptedSentinel, promptSentinel)
}

func TestResponsesPassthroughPreservesCompactionEventsBeforeTerminal(t *testing.T) {
	compaction := `{"type":"response.output_item.done","output_index":0,"item":{"type":"compaction","id":"cmp_1","encrypted_content":"enc"}}`
	completed := `{"type":"response.completed","response":{"id":"resp_1"}}`
	fake := newStreamHost(
		hostHTTPStreamReadResponse{Payload: []byte("data: " + compaction + "\n\ndata: " + completed + "\n\n"), Done: true},
	)
	service := newService(fake)
	now := time.Unix(192_000, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"gpt-6","input":[{"role":"user","content":"hi"}]}`)
	_, errStream := service.executeStreamRequest(context.Background(), "", "downstream-compaction-before-terminal", contract.ExecutorRequest{
		AuthID: "auth", Model: "gpt-6", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
		Payload: payload, OriginalRequest: payload,
		StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-6", Format: formatOpenAIResponse})),
	})
	if errStream != nil {
		t.Fatal(errStream)
	}
	fake.wait(t)
	fake.mu.Lock()
	emitted := string(bytesJoin(fake.emitted))
	downstreamError := fake.downstreamError
	fake.mu.Unlock()
	if downstreamError != "" {
		t.Fatalf("unexpected error = %q; emitted=%q", downstreamError, emitted)
	}
	if !strings.Contains(emitted, "cmp_1") || !strings.Contains(emitted, "response.completed") {
		t.Fatalf("compaction or terminal event missing: %q", emitted)
	}
}

func TestExecuteStreamTranslatedResponsesRejectsDoneWithoutAuthoritativeFinish(t *testing.T) {
	// Chat 的 [DONE] 只是传输哨兵；没有非空 finish_reason 时不能把 translator 合成的
	// response.completed 当成真实 Responses 终态。
	const sentinel = "PRIVATE_TRANSLATED_STREAM_FAILURE"
	chunk := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-6","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}` + "\n\n"
	finish := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-6","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	done := "data: [DONE]\n\n"
	errorFrame := `data: {"error":{"message":"` + sentinel + `","type":"server_error"}}` + "\n\n"
	for _, test := range []struct {
		name   string
		frames string
	}{
		{name: "done", frames: chunk + done},
		{name: "error then done", frames: chunk + errorFrame + done},
		{name: "finish then error then done", frames: chunk + finish + errorFrame + done},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newStreamHost(hostHTTPStreamReadResponse{Payload: []byte(test.frames)})
			service := newService(fake)
			now := time.Unix(193_000, 0).UTC()
			service.now = func() time.Time { return now }
			payload := []byte(`{"model":"gpt-6","input":[{"role":"user","content":"hi"}],"stream":true}`)
			_, errStream := service.executeStreamRequest(context.Background(), "", "downstream-translated-missing-terminal", contract.ExecutorRequest{
				AuthID: "auth", Model: "gpt-6", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
				Payload: payload, OriginalRequest: payload,
				StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-6", Format: formatOpenAI})),
			})
			if errStream != nil {
				t.Fatal(errStream)
			}
			fake.wait(t)
			fake.mu.Lock()
			emitted := string(bytesJoin(fake.emitted))
			downstreamError := fake.downstreamError
			fake.mu.Unlock()
			if downstreamError == "" {
				t.Fatalf("translated stream without a terminal Responses event unexpectedly closed cleanly; emitted=%q", emitted)
			}
			if strings.Contains(emitted, "response.completed") {
				t.Fatalf("must never synthesize response.completed: emitted=%q", emitted)
			}
			if strings.Contains(downstreamError, sentinel) {
				t.Fatalf("stream error leaked upstream body: %q", downstreamError)
			}
			assertLogsExclude(t, fake.snapshotLogs(), sentinel)
		})
	}
}

func TestExecuteStreamTranslatedResponsesAcceptsTerminalEvent(t *testing.T) {
	chunk := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-6","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}` + "\n\n"
	finish := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-6","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	done := "data: [DONE]\n\n"
	fake := newStreamHost(hostHTTPStreamReadResponse{Payload: []byte(chunk + finish + done)})
	service := newService(fake)
	now := time.Unix(194_000, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"gpt-6","input":[{"role":"user","content":"hi"}],"stream":true}`)
	_, errStream := service.executeStreamRequest(context.Background(), "", "downstream-translated-terminal", contract.ExecutorRequest{
		AuthID: "auth", Model: "gpt-6", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
		Payload: payload, OriginalRequest: payload,
		StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-6", Format: formatOpenAI})),
	})
	if errStream != nil {
		t.Fatal(errStream)
	}
	fake.wait(t)
	fake.mu.Lock()
	emitted := string(bytesJoin(fake.emitted))
	downstreamError := fake.downstreamError
	fake.mu.Unlock()
	if downstreamError != "" {
		t.Fatalf("unexpected error = %q; emitted=%q", downstreamError, emitted)
	}
	if !strings.Contains(emitted, "response.completed") {
		t.Fatalf("terminal response.completed event missing: emitted=%q", emitted)
	}
}

func TestExecuteStreamTranslatedResponsesPreservesIncompleteReasons(t *testing.T) {
	chatChunk := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-6","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}` + "\n\n"
	chatFinish := func(reason string) string {
		return `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-6","choices":[{"index":0,"delta":{},"finish_reason":"` + reason + `"}]}` + "\n\ndata: [DONE]\n\n"
	}
	claudeFrames := func(reason string) string {
		return strings.Join([]string{
			`data: {"type":"message_start","message":{"id":"msg-1","model":"claude-sonnet-5-1","usage":{"input_tokens":1,"output_tokens":0}}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
			`data: {"type":"content_block_stop","index":0}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"` + reason + `","stop_sequence":null},"usage":{"output_tokens":1}}`,
			`data: {"type":"message_stop"}`,
		}, "\n\n") + "\n\n"
	}
	for _, test := range []struct {
		name           string
		model          string
		upstreamFormat string
		frames         string
		wantStatus     string
		wantReason     string
	}{
		{name: "chat length", model: "gpt-6", upstreamFormat: formatOpenAI, frames: chatChunk + chatFinish("length"), wantStatus: "response.incomplete", wantReason: "max_output_tokens"},
		{name: "chat content filter", model: "gpt-6", upstreamFormat: formatOpenAI, frames: chatChunk + chatFinish("content_filter"), wantStatus: "response.incomplete", wantReason: "content_filter"},
		{name: "claude max tokens", model: "claude-sonnet-5-1", upstreamFormat: formatClaude, frames: claudeFrames("max_tokens"), wantStatus: "response.incomplete", wantReason: "max_output_tokens"},
		{name: "claude context window", model: "claude-sonnet-5-1", upstreamFormat: formatClaude, frames: claudeFrames("model_context_window_exceeded"), wantStatus: "response.incomplete", wantReason: "max_output_tokens"},
		{name: "claude end turn", model: "claude-sonnet-5-1", upstreamFormat: formatClaude, frames: claudeFrames("end_turn"), wantStatus: "response.completed"},
		{name: "claude tool use", model: "claude-sonnet-5-1", upstreamFormat: formatClaude, frames: claudeFrames("tool_use"), wantStatus: "response.completed"},
		{name: "claude refusal", model: "claude-sonnet-5-1", upstreamFormat: formatClaude, frames: claudeFrames("refusal"), wantStatus: "response.completed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newStreamHost(hostHTTPStreamReadResponse{Payload: []byte(test.frames)})
			service := newService(fake)
			now := time.Unix(194_250, 0).UTC()
			service.now = func() time.Time { return now }
			payload := []byte(`{"model":"` + test.model + `","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`)
			_, errStream := service.executeStreamRequest(context.Background(), "", "downstream-translated-terminal-reason", contract.ExecutorRequest{
				AuthID: "auth", Model: test.model, Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
				Payload: payload, OriginalRequest: payload,
				StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: test.model, Format: test.upstreamFormat})),
			})
			if errStream != nil {
				t.Fatal(errStream)
			}
			fake.wait(t)
			fake.mu.Lock()
			emitted := string(bytesJoin(fake.emitted))
			downstreamError := fake.downstreamError
			fake.mu.Unlock()
			if downstreamError != "" {
				t.Fatalf("unexpected error = %q; emitted=%q", downstreamError, emitted)
			}
			if !strings.Contains(emitted, test.wantStatus) {
				t.Fatalf("terminal %s missing: emitted=%q", test.wantStatus, emitted)
			}
			if test.wantStatus == "response.incomplete" && strings.Contains(emitted, "response.completed") {
				t.Fatalf("incomplete source became completed: emitted=%q", emitted)
			}
			if test.wantReason != "" && !strings.Contains(emitted, `"reason":"`+test.wantReason+`"`) {
				t.Fatalf("incomplete reason %q missing: emitted=%q", test.wantReason, emitted)
			}
		})
	}
}

func TestExecuteStreamResponsesToChatRejectsFailureAndMissingTerminal(t *testing.T) {
	const sentinel = "PRIVATE_RESPONSES_STREAM_FAILURE"
	for _, test := range []struct {
		name  string
		frame string
	}{
		{
			name:  "failed",
			frame: `data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"message":"` + sentinel + `"}}}` + "\n\n",
		},
		{
			name:  "missing terminal",
			frame: `data: {"type":"response.output_text.delta","response_id":"resp_1","delta":"partial"}` + "\n\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newStreamHost(hostHTTPStreamReadResponse{Payload: []byte(test.frame), Done: true})
			service := newService(fake)
			now := time.Unix(194_500, 0).UTC()
			service.now = func() time.Time { return now }
			payload := []byte(`{"model":"gpt-6","messages":[{"role":"user","content":"hi"}],"stream":true}`)
			_, errStream := service.executeStreamRequest(context.Background(), "", "downstream-responses-to-chat-failure", contract.ExecutorRequest{
				AuthID: "auth", Model: "gpt-6", Format: formatOpenAI, SourceFormat: formatOpenAI,
				Payload: payload, OriginalRequest: payload,
				StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-6", Format: formatOpenAIResponse})),
			})
			if errStream != nil {
				t.Fatal(errStream)
			}
			fake.wait(t)
			fake.mu.Lock()
			downstreamError := fake.downstreamError
			emitted := string(bytesJoin(fake.emitted))
			fake.mu.Unlock()
			if downstreamError == "" {
				t.Fatalf("Responses-to-Chat stream closed cleanly; emitted=%q", emitted)
			}
			if strings.Contains(downstreamError, sentinel) {
				t.Fatalf("stream error leaked upstream body: %q", downstreamError)
			}
			assertLogsExclude(t, fake.snapshotLogs(), sentinel)
		})
	}
}

func TestExecuteStreamResponsesToChatAcceptsEventsWithoutSSESeparators(t *testing.T) {
	delta := `{"type":"response.output_text.delta","response_id":"resp_1","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hello"}`
	completed := `{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"gpt-6","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
	fake := newStreamHost(
		hostHTTPStreamReadResponse{Payload: []byte("data: " + delta)},
		hostHTTPStreamReadResponse{Payload: []byte("data: " + completed)},
	)
	service := newService(fake)
	now := time.Unix(194_750, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"gpt-6","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, errStream := service.executeStreamRequest(context.Background(), "", "downstream-responses-to-chat-no-separator", contract.ExecutorRequest{
		AuthID: "auth", Model: "gpt-6", Format: formatOpenAI, SourceFormat: formatOpenAI,
		Payload: payload, OriginalRequest: payload,
		StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-6", Format: formatOpenAIResponse})),
	})
	if errStream != nil {
		t.Fatal(errStream)
	}
	fake.wait(t)
	fake.mu.Lock()
	downstreamError := fake.downstreamError
	emitted := string(bytesJoin(fake.emitted))
	fake.mu.Unlock()
	if downstreamError != "" {
		t.Fatalf("unexpected error = %q; emitted=%q", downstreamError, emitted)
	}
	if !strings.Contains(emitted, "hello") || !strings.Contains(emitted, `"finish_reason":"stop"`) {
		t.Fatalf("translated Chat terminal chunks missing: %q", emitted)
	}
}

// TestResponsesCompactionRoundTripPreservesClientManagedState proves the service performs no
// selection or history management of its own: request 1 streams multiple compaction items and
// a terminal response.completed; this TEST (playing the role of the VS Code client) selects the
// item at the highest output_index and builds request 2. The service must send that second
// request with the selected compaction item, previous_response_id, and input ordering preserved,
// only adding the documented Responses defaults.
func TestResponsesCompactionRoundTripPreservesClientManagedState(t *testing.T) {
	item1 := `{"type":"response.output_item.done","output_index":0,"item":{"type":"compaction","id":"cmp_1","encrypted_content":"enc-old"}}`
	item2 := `{"type":"response.output_item.done","output_index":1,"item":{"type":"compaction","id":"cmp_2","encrypted_content":"enc-new"}}`
	completed := `{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","output":[]}}`
	fake := newStreamHost(
		hostHTTPStreamReadResponse{Payload: []byte("data: " + item1 + "\n\ndata: " + item2 + "\n\n")},
		hostHTTPStreamReadResponse{Payload: []byte("data: " + completed + "\n\n"), Done: true},
	)
	service := newService(fake)
	now := time.Unix(195_000, 0).UTC()
	service.now = func() time.Time { return now }
	firstPayload := []byte(`{"model":"gpt-6","input":[{"role":"user","content":[{"type":"input_text","text":"start"}]}]}`)
	_, errStream := service.executeStreamRequest(context.Background(), "", "downstream-compaction-1", contract.ExecutorRequest{
		AuthID: "auth", Model: "gpt-6", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
		Payload: firstPayload, OriginalRequest: firstPayload,
		StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-6", Format: formatOpenAIResponse})),
	})
	if errStream != nil {
		t.Fatal(errStream)
	}
	fake.wait(t)
	fake.mu.Lock()
	emitted := string(bytesJoin(fake.emitted))
	downstreamError := fake.downstreamError
	fake.mu.Unlock()
	if downstreamError != "" {
		t.Fatalf("unexpected stream error = %q", downstreamError)
	}

	latestID, latestEncrypted, responseID := selectLatestCompactionForTest(t, emitted)
	if latestID != "cmp_2" || latestEncrypted != "enc-new" || responseID != "resp_1" {
		t.Fatalf("test client selection = id=%q encrypted=%q responseID=%q", latestID, latestEncrypted, responseID)
	}

	secondPayload := mustJSON(t, map[string]any{
		"model":                "gpt-6",
		"previous_response_id": responseID,
		"input": []any{
			map[string]any{"type": "compaction", "id": latestID, "encrypted_content": latestEncrypted},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "continue"}}},
		},
	})
	var capturedBody []byte
	fake2 := newFakeHost(t, func(req hostHTTPCall) (contract.HTTPResponse, error) {
		capturedBody = req.Body
		return contract.HTTPResponse{StatusCode: 200, Body: []byte(`{"id":"resp_2","object":"response","status":"completed","output":[]}`)}, nil
	})
	service2 := newService(fake2)
	service2.now = func() time.Time { return now }
	_, errExecute := service2.executeRequest(context.Background(), "", contract.ExecutorRequest{
		AuthID: "auth", Model: "gpt-6", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
		Payload: secondPayload, OriginalRequest: secondPayload,
		StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-6", Format: formatOpenAIResponse})),
	})
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	var upstreamBody map[string]any
	if json.Unmarshal(capturedBody, &upstreamBody) != nil {
		t.Fatalf("decode upstream body: %s", capturedBody)
	}
	if upstreamBody["previous_response_id"] != "resp_1" {
		t.Fatalf("previous_response_id = %#v", upstreamBody["previous_response_id"])
	}
	input, ok := upstreamBody["input"].([]any)
	if !ok || len(input) != 2 {
		t.Fatalf("input = %#v", upstreamBody["input"])
	}
	compactionItem, ok := input[0].(map[string]any)
	if !ok || compactionItem["id"] != "cmp_2" || compactionItem["encrypted_content"] != "enc-new" || compactionItem["type"] != "compaction" {
		t.Fatalf("compaction item was not preserved byte-for-byte: %#v", compactionItem)
	}
	userItem, ok := input[1].(map[string]any)
	if !ok || userItem["role"] != "user" {
		t.Fatalf("incremental input item = %#v", input[1])
	}
}

func selectLatestCompactionForTest(t *testing.T, emitted string) (id, encryptedContent, responseID string) {
	t.Helper()
	bestIndex := -1
	for _, frame := range strings.Split(emitted, "\n\n") {
		frame = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(frame), "data:"))
		if frame == "" {
			continue
		}
		var event struct {
			Type        string `json:"type"`
			OutputIndex int    `json:"output_index"`
			Item        struct {
				Type             string `json:"type"`
				ID               string `json:"id"`
				EncryptedContent string `json:"encrypted_content"`
			} `json:"item"`
			Response struct {
				ID string `json:"id"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(frame), &event) != nil {
			continue
		}
		if event.Type == "response.output_item.done" && event.Item.Type == "compaction" && event.OutputIndex >= bestIndex {
			bestIndex = event.OutputIndex
			id = event.Item.ID
			encryptedContent = event.Item.EncryptedContent
		}
		if event.Type == "response.completed" {
			responseID = event.Response.ID
		}
	}
	return id, encryptedContent, responseID
}
