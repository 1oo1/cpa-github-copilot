package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type streamBridgeFake struct {
	mu sync.Mutex

	readChunks     []rpcHostHTTPStreamReadResponse
	emitted        [][]byte
	upstreamClosed bool
	pluginClosed   bool
	pluginError    string
	emitError      bool
	logs           []rpcHostLogRequest
	done           chan struct{}
	doneOnce       sync.Once
}

type shutdownStreamBridgeFake struct {
	mu sync.Mutex

	readCount      int
	emitStarted    chan struct{}
	emitRelease    chan struct{}
	upstreamClosed chan struct{}
	pluginClosed   chan struct{}
	emitOnce       sync.Once
	upstreamOnce   sync.Once
	pluginOnce     sync.Once
}

func newShutdownStreamBridgeFake() *shutdownStreamBridgeFake {
	return &shutdownStreamBridgeFake{
		emitStarted:    make(chan struct{}),
		emitRelease:    make(chan struct{}),
		upstreamClosed: make(chan struct{}),
		pluginClosed:   make(chan struct{}),
	}
}

func (f *shutdownStreamBridgeFake) Call(method string, _ any) (json.RawMessage, error) {
	var result any = map[string]any{}
	switch method {
	case pluginabi.MethodHostLog:
	case pluginabi.MethodHostHTTPDoStream:
		result = rpcHostHTTPStreamResponse{StatusCode: 200, Headers: httpHeaders{"Content-Type": []string{"text/event-stream"}}, StreamID: "upstream-shutdown"}
	case pluginabi.MethodHostHTTPStreamRead:
		f.mu.Lock()
		f.readCount++
		readCount := f.readCount
		f.mu.Unlock()
		if readCount == 1 {
			result = rpcHostHTTPStreamReadResponse{Payload: []byte("data: {\"id\":\"chatcmpl-shutdown\",\"choices\":[]}\n\n")}
		} else {
			<-f.upstreamClosed
			result = rpcHostHTTPStreamReadResponse{Done: true}
		}
	case pluginabi.MethodHostHTTPStreamClose:
		f.upstreamOnce.Do(func() { close(f.upstreamClosed) })
	case pluginabi.MethodHostStreamEmit:
		f.emitOnce.Do(func() { close(f.emitStarted) })
		<-f.emitRelease
	case pluginabi.MethodHostStreamClose:
		f.pluginOnce.Do(func() { close(f.pluginClosed) })
	default:
		return nil, fmt.Errorf("unexpected method %s", method)
	}
	raw, errMarshal := json.Marshal(result)
	return raw, errMarshal
}

func newStreamBridgeFake(chunks ...rpcHostHTTPStreamReadResponse) *streamBridgeFake {
	return &streamBridgeFake{readChunks: chunks, done: make(chan struct{})}
}

func (f *streamBridgeFake) Call(method string, payload any) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result any = map[string]any{}
	switch method {
	case pluginabi.MethodHostLog:
		if request, ok := payload.(rpcHostLogRequest); ok {
			f.logs = append(f.logs, request)
		}
	case pluginabi.MethodHostHTTPDoStream:
		result = rpcHostHTTPStreamResponse{StatusCode: 200, Headers: httpHeaders{"Content-Type": []string{"text/event-stream"}}, StreamID: "upstream-1"}
	case pluginabi.MethodHostHTTPStreamRead:
		if len(f.readChunks) == 0 {
			return nil, fmt.Errorf("unexpected stream read")
		}
		result = f.readChunks[0]
		f.readChunks = f.readChunks[1:]
	case pluginabi.MethodHostHTTPStreamClose:
		f.upstreamClosed = true
	case pluginabi.MethodHostStreamEmit:
		if f.emitError {
			return nil, fmt.Errorf("downstream closed")
		}
		req := payload.(rpcHostStreamEmitRequest)
		f.emitted = append(f.emitted, append([]byte(nil), req.Payload...))
	case pluginabi.MethodHostStreamClose:
		req := payload.(rpcHostStreamCloseRequest)
		f.pluginClosed = true
		f.pluginError = req.Error
		f.doneOnce.Do(func() { close(f.done) })
	default:
		return nil, fmt.Errorf("unexpected method %s", method)
	}
	raw, errMarshal := json.Marshal(result)
	return raw, errMarshal
}

func (f *streamBridgeFake) wait(t *testing.T) {
	t.Helper()
	select {
	case <-f.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for plugin stream close")
	}
}

func (f *streamBridgeFake) snapshotLogs() []rpcHostLogRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]rpcHostLogRequest(nil), f.logs...)
}

func TestExecuteStreamPassThroughNormalizesChatChunksAndClosesAtDone(t *testing.T) {
	chatChunk := `{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`
	bridge := newStreamBridgeFake(
		rpcHostHTTPStreamReadResponse{Payload: []byte("data: " + chatChunk + "\n\n")},
		rpcHostHTTPStreamReadResponse{Payload: []byte("data: [DONE]\n\n")},
	)
	service := newPluginService(bridge)
	now := time.Unix(100_000, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"gpt-4.1","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	raw, errStream := service.executeStream(mustJSON(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID: "auth", Model: "gpt-4.1", Format: formatOpenAI, SourceFormat: formatOpenAI,
			Payload: payload, OriginalRequest: payload,
			StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-4.1", Format: formatOpenAI})),
		},
		StreamID: "plugin-1", HostCallbackID: "callback-stream",
	}))
	if errStream != nil {
		t.Fatal(errStream)
	}
	result := decodePluginResult[rpcExecutorStreamResponse](t, raw)
	if result.Headers.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream headers = %#v", result.Headers)
	}
	bridge.wait(t)
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if !bridge.upstreamClosed || !bridge.pluginClosed || bridge.pluginError != "" {
		t.Fatalf("close state: upstream=%v plugin=%v error=%q", bridge.upstreamClosed, bridge.pluginClosed, bridge.pluginError)
	}
	if got := string(bytesJoin(bridge.emitted)); got != chatChunk || !json.Valid([]byte(got)) {
		t.Fatalf("emitted = %q", got)
	}
}

func TestExecuteStreamPassThroughFramesSplitSSEData(t *testing.T) {
	bridge := newStreamBridgeFake(
		rpcHostHTTPStreamReadResponse{Payload: []byte(`data: {"type":"response.created","response":{"id":"resp`)},
		rpcHostHTTPStreamReadResponse{Payload: []byte("-1\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"output\":[]}}\n\n")},
	)
	service := newPluginService(bridge)
	now := time.Unix(105_000, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"gpt-5.6-sol","input":"hi","stream":true}`)
	_, errStream := service.executeStream(mustJSON(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID: "auth", Model: "gpt-5.6-sol", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
			Payload: payload, OriginalRequest: payload,
			StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-5.6-sol", Format: formatOpenAIResponse})),
		},
		StreamID: "plugin-split-sse", HostCallbackID: "callback-stream",
	}))
	if errStream != nil {
		t.Fatal(errStream)
	}
	bridge.wait(t)
	bridge.mu.Lock()
	emitted := append([][]byte(nil), bridge.emitted...)
	pluginError := bridge.pluginError
	bridge.mu.Unlock()
	if pluginError != "" {
		t.Fatalf("plugin stream error = %q", pluginError)
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

func TestExecuteStreamNativeClaudeClosesAtMessageStop(t *testing.T) {
	bridge := newStreamBridgeFake(
		rpcHostHTTPStreamReadResponse{Payload: []byte(`data: {"type":"message_start","message":{"id":"msg-1"}}` + "\n\n")},
		rpcHostHTTPStreamReadResponse{Payload: []byte(`data: {"type":"message_stop"}` + "\n\n")},
	)
	service := newPluginService(bridge)
	now := time.Unix(107_500, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"claude-sonnet-4.6","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, errStream := service.executeStream(mustJSON(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID: "auth", Model: "claude-sonnet-4.6", Format: formatClaude, SourceFormat: formatClaude,
			Payload: payload, OriginalRequest: payload,
			StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "claude-sonnet-4.6", Format: formatClaude})),
		},
		StreamID: "plugin-native-claude", HostCallbackID: "callback-stream",
	}))
	if errStream != nil {
		t.Fatal(errStream)
	}
	bridge.wait(t)
	bridge.mu.Lock()
	emitted := string(bytesJoin(bridge.emitted))
	pluginError := bridge.pluginError
	bridge.mu.Unlock()
	if pluginError != "" || !strings.Contains(emitted, `"type":"message_stop"`) {
		t.Fatalf("native Claude terminal state: error=%q emitted=%q", pluginError, emitted)
	}
}

func TestExecuteStreamTranslatesSplitSSEFrames(t *testing.T) {
	first := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4.1","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}` + "\n\n"
	finish := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4.1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	done := "data: [DONE]\n\n"
	bridge := newStreamBridgeFake(
		rpcHostHTTPStreamReadResponse{Payload: []byte(first[:31])},
		rpcHostHTTPStreamReadResponse{Payload: []byte(first[31:] + finish + done)},
	)
	service := newPluginService(bridge)
	now := time.Unix(110_000, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"gpt-4.1","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, errStream := service.executeStream(mustJSON(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID: "auth", Model: "gpt-4.1", Format: formatClaude, SourceFormat: formatClaude,
			Payload: payload, OriginalRequest: payload,
			StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-4.1", Format: formatOpenAI})),
		},
		StreamID: "plugin-2", HostCallbackID: "callback-stream",
	}))
	if errStream != nil {
		t.Fatal(errStream)
	}
	bridge.wait(t)
	bridge.mu.Lock()
	emitted := string(bytesJoin(bridge.emitted))
	upstreamClosed, pluginError := bridge.upstreamClosed, bridge.pluginError
	bridge.mu.Unlock()
	if !upstreamClosed || pluginError != "" {
		t.Fatalf("close state: upstream=%v error=%q", upstreamClosed, pluginError)
	}
	if !strings.Contains(emitted, "event: message_start") || !strings.Contains(emitted, "hello") || !strings.Contains(emitted, "event: message_stop") {
		t.Fatalf("translated stream = %q", emitted)
	}
}

func TestExecuteStreamRoutesAnthropicWebSearchToResponses(t *testing.T) {
	frames := []string{
		`data: {"type":"response.created","response":{"id":"resp-search","model":"gpt-5.6-terra"}}` + "\n\n",
		`data: {"type":"response.output_item.added","item":{"id":"ws_123","type":"web_search_call","status":"in_progress"}}` + "\n\n",
		`data: {"type":"response.web_search_call.searching","item_id":"ws_123"}` + "\n\n",
		`data: {"type":"response.web_search_call.completed","item_id":"ws_123"}` + "\n\n",
		`data: {"type":"response.output_item.done","item":{"id":"ws_123","type":"web_search_call","status":"completed","action":{"type":"search","query":"HarmonyOS command-line tools"}}}` + "\n\n",
		`data: {"type":"response.completed","response":{"id":"resp-search","status":"completed","stop_reason":"stop","error":null,"usage":{"input_tokens":3,"output_tokens":2}}}` + "\n\n",
	}
	chunks := make([]rpcHostHTTPStreamReadResponse, 0, len(frames))
	for _, frame := range frames {
		chunks = append(chunks, rpcHostHTTPStreamReadResponse{Payload: []byte(frame)})
	}
	bridge := newStreamBridgeFake(chunks...)
	service := newPluginService(bridge)
	now := time.Unix(115_000, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{
		"model":"claude-sonnet-5",
		"max_tokens":64000,
		"messages":[{"role":"user","content":"Perform a web search"}],
		"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":1}],
		"tool_choice":{"type":"tool","name":"web_search"}
	}`)
	_, errStream := service.executeStream(mustJSON(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID: "auth", Model: "claude-sonnet-5", Format: formatClaude, SourceFormat: formatClaude,
			Payload: payload, OriginalRequest: payload,
			StorageJSON: mustJSON(t, executorStorage(now,
				storedModel{ID: "claude-sonnet-5", Format: formatClaude},
				storedModel{ID: "gpt-5.6-terra", Format: formatOpenAIResponse},
			)),
		},
		StreamID: "plugin-web-search", HostCallbackID: "callback-web-search",
	}))
	if errStream != nil {
		t.Fatal(errStream)
	}
	bridge.wait(t)
	bridge.mu.Lock()
	emitted := string(bytesJoin(bridge.emitted))
	pluginError := bridge.pluginError
	bridge.mu.Unlock()
	if pluginError != "" {
		t.Fatalf("plugin stream error = %q", pluginError)
	}
	for _, needle := range []string{`"type":"server_tool_use"`, `"type":"web_search_tool_result"`, "HarmonyOS command-line tools", `"type":"message_stop"`} {
		if !strings.Contains(emitted, needle) {
			t.Fatalf("translated web search stream missing %q: %s", needle, emitted)
		}
	}
	if strings.Index(emitted, `"type":"web_search_tool_result"`) < strings.Index(emitted, `"type":"server_tool_use"`) {
		t.Fatalf("web search result preceded server tool use: %s", emitted)
	}
}

func TestExecuteStreamDownstreamFailureStillClosesUpstream(t *testing.T) {
	bridge := newStreamBridgeFake(rpcHostHTTPStreamReadResponse{Payload: []byte("data: chunk\n\n"), Done: true})
	bridge.emitError = true
	service := newPluginService(bridge)
	now := time.Unix(120_000, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"gpt-4.1","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, errStream := service.executeStream(mustJSON(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID: "auth", Model: "gpt-4.1", Format: formatOpenAI, SourceFormat: formatOpenAI,
			Payload: payload, StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-4.1", Format: formatOpenAI})),
		},
		StreamID: "plugin-3",
	}))
	if errStream != nil {
		t.Fatal(errStream)
	}
	bridge.wait(t)
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if !bridge.upstreamClosed || bridge.pluginError == "" || strings.Contains(bridge.pluginError, "chunk") {
		t.Fatalf("close state: upstream=%v error=%q", bridge.upstreamClosed, bridge.pluginError)
	}
}

func TestShutdownCancelsAndWaitsForForwardingTasks(t *testing.T) {
	bridge := newShutdownStreamBridgeFake()
	service := newPluginService(bridge)
	now := time.Unix(122_500, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"gpt-4.1","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, errStream := service.executeStream(mustJSON(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID: "auth", Model: "gpt-4.1", Format: formatOpenAI, SourceFormat: formatOpenAI,
			Payload: payload, OriginalRequest: payload,
			StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-4.1", Format: formatOpenAI})),
		},
		StreamID: "plugin-shutdown-wait", HostCallbackID: "callback-stream",
	}))
	if errStream != nil {
		t.Fatal(errStream)
	}
	select {
	case <-bridge.emitStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("forwarder did not enter host emit")
	}

	shutdownDone := make(chan struct{})
	go func() {
		service.shutdown()
		close(shutdownDone)
	}()
	select {
	case <-bridge.upstreamClosed:
	case <-time.After(5 * time.Second):
		close(bridge.emitRelease)
		t.Fatal("shutdown did not cancel the upstream stream")
	}
	select {
	case <-shutdownDone:
		close(bridge.emitRelease)
		t.Fatal("shutdown returned while a forwarding task was still using host callbacks")
	case <-time.After(50 * time.Millisecond):
	}
	close(bridge.emitRelease)
	select {
	case <-shutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not join the forwarding task")
	}
	select {
	case <-bridge.pluginClosed:
	default:
		t.Fatal("forwarder did not close the plugin stream before shutdown returned")
	}
	service.streamMu.Lock()
	remaining := len(service.streamTasks)
	service.streamMu.Unlock()
	if remaining != 0 {
		t.Fatalf("forwarding tasks remaining after shutdown = %d", remaining)
	}
}

func TestExecuteStreamUpstreamErrorStillClosesBothStreams(t *testing.T) {
	bridge := newStreamBridgeFake(rpcHostHTTPStreamReadResponse{Error: "private upstream detail", Done: true})
	service := newPluginService(bridge)
	now := time.Unix(125_000, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"gpt-4.1","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, errStream := service.executeStream(mustJSON(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID: "auth", Model: "gpt-4.1", Format: formatOpenAI, SourceFormat: formatOpenAI,
			Payload: payload, StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-4.1", Format: formatOpenAI})),
		},
		StreamID: "plugin-upstream-error",
	}))
	if errStream != nil {
		t.Fatal(errStream)
	}
	bridge.wait(t)
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if !bridge.upstreamClosed || !bridge.pluginClosed || bridge.pluginError == "" || strings.Contains(bridge.pluginError, "private upstream detail") {
		t.Fatalf("close state: upstream=%v plugin=%v error=%q", bridge.upstreamClosed, bridge.pluginClosed, bridge.pluginError)
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

func hasLogEvent(logs []rpcHostLogRequest, event string) bool {
	for _, entry := range logs {
		if entry.Fields["event"] == event {
			return true
		}
	}
	return false
}

func executeOpenAIStream(t *testing.T, bridge *streamBridgeFake, streamID string, at time.Time) {
	t.Helper()
	service := newPluginService(bridge)
	service.now = func() time.Time { return at }
	payload := []byte(`{"model":"gpt-4.1","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, errStream := service.executeStream(mustJSON(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID: "auth", Model: "gpt-4.1", Format: formatOpenAI, SourceFormat: formatOpenAI,
			Payload: payload, StorageJSON: mustJSON(t, executorStorage(at, storedModel{ID: "gpt-4.1", Format: formatOpenAI})),
		},
		StreamID: streamID, HostCallbackID: "callback-stream",
	}))
	if errStream != nil {
		t.Fatal(errStream)
	}
	bridge.wait(t)
}

func TestForwardStreamSuccessLogsCompletedDebug(t *testing.T) {
	bridge := newStreamBridgeFake(
		rpcHostHTTPStreamReadResponse{Payload: []byte("data: first\n\n")},
		rpcHostHTTPStreamReadResponse{Payload: []byte("data: [DONE]\n\n"), Done: true},
	)
	executeOpenAIStream(t, bridge, "plugin-success-log", time.Unix(135_000, 0).UTC())
	logs := bridge.snapshotLogs()
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
	bridge := newStreamBridgeFake(rpcHostHTTPStreamReadResponse{Payload: []byte("data: chunk\n\n"), Done: true})
	bridge.emitError = true
	executeOpenAIStream(t, bridge, "plugin-benign-emit", time.Unix(130_000, 0).UTC())
	logs := bridge.snapshotLogs()
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
	bridge := newStreamBridgeFake() // no chunks: the host stream read call fails
	executeOpenAIStream(t, bridge, "plugin-read-failure", time.Unix(131_000, 0).UTC())
	logs := bridge.snapshotLogs()
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
	bridge := newStreamBridgeFake(rpcHostHTTPStreamReadResponse{Error: "private upstream detail", Done: true})
	executeOpenAIStream(t, bridge, "plugin-upstream-warn", time.Unix(132_000, 0).UTC())
	logs := bridge.snapshotLogs()
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
	bridge := newStreamBridgeFake(rpcHostHTTPStreamReadResponse{Error: "context canceled", Done: true})
	executeOpenAIStream(t, bridge, "plugin-upstream-cancel", time.Unix(133_000, 0).UTC())
	logs := bridge.snapshotLogs()
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
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if !bridge.upstreamClosed || !bridge.pluginClosed {
		t.Fatalf("close state: upstream=%v plugin=%v", bridge.upstreamClosed, bridge.pluginClosed)
	}
	if bridge.pluginError != "GitHub Copilot upstream stream canceled" {
		t.Fatalf("plugin stream error = %q", bridge.pluginError)
	}
}

func TestForwardStreamRemoteHTTP2CancelLogsWarn(t *testing.T) {
	bridge := newStreamBridgeFake(rpcHostHTTPStreamReadResponse{Error: "stream error: stream ID 3; CANCEL", Done: true})
	executeOpenAIStream(t, bridge, "plugin-upstream-remote-cancel", time.Unix(133_500, 0).UTC())
	logs := bridge.snapshotLogs()
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
	bridge := newStreamBridgeFake() // no chunks: the host stream read call fails
	executeOpenAIStream(t, bridge, "plugin-no-cause", time.Unix(134_000, 0).UTC())
	entry := findLogEvent(t, bridge.snapshotLogs(), "inference.stream.forward_failed")
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
	bridge := newStreamBridgeFake(
		rpcHostHTTPStreamReadResponse{Payload: []byte(`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n"), Done: true},
	)
	service := newPluginService(bridge)
	now := time.Unix(190_000, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hi"}]}`)
	_, errStream := service.executeStream(mustJSON(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID: "auth", Model: "gpt-5.4", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
			Payload: payload, OriginalRequest: payload,
			StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-5.4", Format: formatOpenAIResponse})),
		},
		StreamID: "plugin-missing-terminal",
	}))
	if errStream != nil {
		t.Fatal(errStream)
	}
	bridge.wait(t)
	bridge.mu.Lock()
	emitted := string(bytesJoin(bridge.emitted))
	pluginError := bridge.pluginError
	logs := append([]rpcHostLogRequest(nil), bridge.logs...)
	bridge.mu.Unlock()
	if pluginError == "" {
		t.Fatalf("stream without a terminal Responses event unexpectedly closed cleanly; emitted=%q", emitted)
	}
	if !strings.Contains(strings.ToLower(pluginError), "unexpected eof") {
		t.Fatalf("missing-terminal stream error must be classified as a connection-lifecycle failure: %q", pluginError)
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

func TestResponsesMissingTerminalErrorDoesNotCoolHostCredential(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "auth", Provider: pluginIdentifier}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}

	const model = "gpt-5.4"
	manager.MarkResult(context.Background(), coreauth.Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    model,
		Success:  false,
		Error:    &coreauth.Error{Message: responsesMissingTerminalError},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("registered auth disappeared")
	}
	if state := updated.ModelStates[model]; state != nil && (state.Unavailable || !state.NextRetryAfter.IsZero()) {
		t.Fatalf("missing-terminal stream error cooled credential: %#v", state)
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
			bridge := newStreamBridgeFake(
				rpcHostHTTPStreamReadResponse{Payload: []byte("data: " + terminal + "\n\n")},
			)
			service := newPluginService(bridge)
			now := time.Unix(191_000, 0).UTC()
			service.now = func() time.Time { return now }
			payload := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hi"}]}`)
			_, errStream := service.executeStream(mustJSON(t, rpcExecutorRequest{
				ExecutorRequest: pluginapi.ExecutorRequest{
					AuthID: "auth", Model: "gpt-5.4", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
					Payload: payload, OriginalRequest: payload,
					StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-5.4", Format: formatOpenAIResponse})),
				},
				StreamID: "plugin-terminal-ok",
			}))
			if errStream != nil {
				t.Fatal(errStream)
			}
			bridge.wait(t)
			bridge.mu.Lock()
			pluginError := bridge.pluginError
			bridge.mu.Unlock()
			if pluginError != "" {
				t.Fatalf("unexpected error = %q", pluginError)
			}
		})
	}
}

func TestResponsesStreamLogsTerminalStatus(t *testing.T) {
	bridge := newStreamBridgeFake(
		rpcHostHTTPStreamReadResponse{Payload: []byte(`data: {"type":"response.completed","response":{"id":"resp_1"}}` + "\n\n")},
	)
	service := newPluginService(bridge)
	now := time.Unix(196_000, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hi"}]}`)
	_, errStream := service.executeStream(mustJSON(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID: "auth", Model: "gpt-5.4", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
			Payload: payload, OriginalRequest: payload,
			StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-5.4", Format: formatOpenAIResponse})),
		},
		StreamID: "plugin-terminal-status-log",
	}))
	if errStream != nil {
		t.Fatal(errStream)
	}
	bridge.wait(t)
	bridge.mu.Lock()
	logs := append([]rpcHostLogRequest(nil), bridge.logs...)
	bridge.mu.Unlock()
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
	bridge := newStreamBridgeFake(
		rpcHostHTTPStreamReadResponse{Payload: []byte("data: " + compaction + "\n\ndata: " + completed + "\n\n"), Done: true},
	)
	service := newPluginService(bridge)
	now := time.Unix(197_000, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"` + promptSentinel + `"}]}`)
	_, errStream := service.executeStream(mustJSON(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID: "auth", Model: "gpt-5.4", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
			Payload: payload, OriginalRequest: payload,
			StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-5.4", Format: formatOpenAIResponse})),
		},
		StreamID: "plugin-log-content-sentinel",
	}))
	if errStream != nil {
		t.Fatal(errStream)
	}
	bridge.wait(t)
	bridge.mu.Lock()
	emitted := string(bytesJoin(bridge.emitted))
	logs := append([]rpcHostLogRequest(nil), bridge.logs...)
	bridge.mu.Unlock()
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
	bridge := newStreamBridgeFake(
		rpcHostHTTPStreamReadResponse{Payload: []byte("data: " + compaction + "\n\ndata: " + completed + "\n\n"), Done: true},
	)
	service := newPluginService(bridge)
	now := time.Unix(192_000, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hi"}]}`)
	_, errStream := service.executeStream(mustJSON(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID: "auth", Model: "gpt-5.4", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
			Payload: payload, OriginalRequest: payload,
			StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-5.4", Format: formatOpenAIResponse})),
		},
		StreamID: "plugin-compaction-before-terminal",
	}))
	if errStream != nil {
		t.Fatal(errStream)
	}
	bridge.wait(t)
	bridge.mu.Lock()
	emitted := string(bytesJoin(bridge.emitted))
	pluginError := bridge.pluginError
	bridge.mu.Unlock()
	if pluginError != "" {
		t.Fatalf("unexpected error = %q; emitted=%q", pluginError, emitted)
	}
	if !strings.Contains(emitted, "cmp_1") || !strings.Contains(emitted, "response.completed") {
		t.Fatalf("compaction or terminal event missing: %q", emitted)
	}
}

func TestExecuteStreamTranslatedResponsesRejectsDoneWithoutAuthoritativeFinish(t *testing.T) {
	// Chat 的 [DONE] 只是传输哨兵；没有非空 finish_reason 时不能把 translator 合成的
	// response.completed 当成真实 Responses 终态。
	const sentinel = "PRIVATE_TRANSLATED_STREAM_FAILURE"
	chunk := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4.1","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}` + "\n\n"
	finish := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4.1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
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
			bridge := newStreamBridgeFake(rpcHostHTTPStreamReadResponse{Payload: []byte(test.frames)})
			service := newPluginService(bridge)
			now := time.Unix(193_000, 0).UTC()
			service.now = func() time.Time { return now }
			payload := []byte(`{"model":"gpt-4.1","input":[{"role":"user","content":"hi"}],"stream":true}`)
			_, errStream := service.executeStream(mustJSON(t, rpcExecutorRequest{
				ExecutorRequest: pluginapi.ExecutorRequest{
					AuthID: "auth", Model: "gpt-4.1", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
					Payload: payload, OriginalRequest: payload,
					StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-4.1", Format: formatOpenAI})),
				},
				StreamID: "plugin-translated-missing-terminal",
			}))
			if errStream != nil {
				t.Fatal(errStream)
			}
			bridge.wait(t)
			bridge.mu.Lock()
			emitted := string(bytesJoin(bridge.emitted))
			pluginError := bridge.pluginError
			bridge.mu.Unlock()
			if pluginError == "" {
				t.Fatalf("translated stream without a terminal Responses event unexpectedly closed cleanly; emitted=%q", emitted)
			}
			if strings.Contains(emitted, "response.completed") {
				t.Fatalf("must never synthesize response.completed: emitted=%q", emitted)
			}
			if strings.Contains(pluginError, sentinel) {
				t.Fatalf("stream error leaked upstream body: %q", pluginError)
			}
			assertLogsExclude(t, bridge.snapshotLogs(), sentinel)
		})
	}
}

func TestExecuteStreamTranslatedResponsesAcceptsTerminalEvent(t *testing.T) {
	chunk := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4.1","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}` + "\n\n"
	finish := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4.1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	done := "data: [DONE]\n\n"
	bridge := newStreamBridgeFake(rpcHostHTTPStreamReadResponse{Payload: []byte(chunk + finish + done)})
	service := newPluginService(bridge)
	now := time.Unix(194_000, 0).UTC()
	service.now = func() time.Time { return now }
	payload := []byte(`{"model":"gpt-4.1","input":[{"role":"user","content":"hi"}],"stream":true}`)
	_, errStream := service.executeStream(mustJSON(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID: "auth", Model: "gpt-4.1", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
			Payload: payload, OriginalRequest: payload,
			StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-4.1", Format: formatOpenAI})),
		},
		StreamID: "plugin-translated-terminal",
	}))
	if errStream != nil {
		t.Fatal(errStream)
	}
	bridge.wait(t)
	bridge.mu.Lock()
	emitted := string(bytesJoin(bridge.emitted))
	pluginError := bridge.pluginError
	bridge.mu.Unlock()
	if pluginError != "" {
		t.Fatalf("unexpected error = %q; emitted=%q", pluginError, emitted)
	}
	if !strings.Contains(emitted, "response.completed") {
		t.Fatalf("terminal response.completed event missing: emitted=%q", emitted)
	}
}

func TestExecuteStreamTranslatedResponsesPreservesIncompleteReasons(t *testing.T) {
	chatChunk := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4.1","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}` + "\n\n"
	chatFinish := func(reason string) string {
		return `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4.1","choices":[{"index":0,"delta":{},"finish_reason":"` + reason + `"}]}` + "\n\ndata: [DONE]\n\n"
	}
	claudeFrames := func(reason string) string {
		return strings.Join([]string{
			`data: {"type":"message_start","message":{"id":"msg-1","model":"claude-sonnet-4.6","usage":{"input_tokens":1,"output_tokens":0}}}`,
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
		{name: "chat length", model: "gpt-4.1", upstreamFormat: formatOpenAI, frames: chatChunk + chatFinish("length"), wantStatus: "response.incomplete", wantReason: "max_output_tokens"},
		{name: "chat content filter", model: "gpt-4.1", upstreamFormat: formatOpenAI, frames: chatChunk + chatFinish("content_filter"), wantStatus: "response.incomplete", wantReason: "content_filter"},
		{name: "claude max tokens", model: "claude-sonnet-4.6", upstreamFormat: formatClaude, frames: claudeFrames("max_tokens"), wantStatus: "response.incomplete", wantReason: "max_output_tokens"},
		{name: "claude context window", model: "claude-sonnet-4.6", upstreamFormat: formatClaude, frames: claudeFrames("model_context_window_exceeded"), wantStatus: "response.incomplete", wantReason: "max_output_tokens"},
		{name: "claude end turn", model: "claude-sonnet-4.6", upstreamFormat: formatClaude, frames: claudeFrames("end_turn"), wantStatus: "response.completed"},
		{name: "claude tool use", model: "claude-sonnet-4.6", upstreamFormat: formatClaude, frames: claudeFrames("tool_use"), wantStatus: "response.completed"},
		{name: "claude refusal", model: "claude-sonnet-4.6", upstreamFormat: formatClaude, frames: claudeFrames("refusal"), wantStatus: "response.completed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			bridge := newStreamBridgeFake(rpcHostHTTPStreamReadResponse{Payload: []byte(test.frames)})
			service := newPluginService(bridge)
			now := time.Unix(194_250, 0).UTC()
			service.now = func() time.Time { return now }
			payload := []byte(`{"model":"` + test.model + `","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`)
			_, errStream := service.executeStream(mustJSON(t, rpcExecutorRequest{
				ExecutorRequest: pluginapi.ExecutorRequest{
					AuthID: "auth", Model: test.model, Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
					Payload: payload, OriginalRequest: payload,
					StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: test.model, Format: test.upstreamFormat})),
				},
				StreamID: "plugin-translated-terminal-reason",
			}))
			if errStream != nil {
				t.Fatal(errStream)
			}
			bridge.wait(t)
			bridge.mu.Lock()
			emitted := string(bytesJoin(bridge.emitted))
			pluginError := bridge.pluginError
			bridge.mu.Unlock()
			if pluginError != "" {
				t.Fatalf("unexpected error = %q; emitted=%q", pluginError, emitted)
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
			bridge := newStreamBridgeFake(rpcHostHTTPStreamReadResponse{Payload: []byte(test.frame), Done: true})
			service := newPluginService(bridge)
			now := time.Unix(194_500, 0).UTC()
			service.now = func() time.Time { return now }
			payload := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}],"stream":true}`)
			_, errStream := service.executeStream(mustJSON(t, rpcExecutorRequest{
				ExecutorRequest: pluginapi.ExecutorRequest{
					AuthID: "auth", Model: "gpt-5.4", Format: formatOpenAI, SourceFormat: formatOpenAI,
					Payload: payload, OriginalRequest: payload,
					StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-5.4", Format: formatOpenAIResponse})),
				},
				StreamID: "plugin-responses-to-chat-failure",
			}))
			if errStream != nil {
				t.Fatal(errStream)
			}
			bridge.wait(t)
			bridge.mu.Lock()
			pluginError := bridge.pluginError
			emitted := string(bytesJoin(bridge.emitted))
			bridge.mu.Unlock()
			if pluginError == "" {
				t.Fatalf("Responses-to-Chat stream closed cleanly; emitted=%q", emitted)
			}
			if strings.Contains(pluginError, sentinel) {
				t.Fatalf("stream error leaked upstream body: %q", pluginError)
			}
			assertLogsExclude(t, bridge.snapshotLogs(), sentinel)
		})
	}
}

// TestResponsesCompactionRoundTripPreservesClientManagedState proves the plugin performs no
// selection or history management of its own: request 1 streams multiple compaction items and
// a terminal response.completed; this TEST (playing the role of the VS Code client) selects the
// item at the highest output_index and builds request 2. The plugin must send that second
// request with the selected compaction item, previous_response_id, and input ordering preserved,
// only adding the documented Responses defaults.
func TestResponsesCompactionRoundTripPreservesClientManagedState(t *testing.T) {
	item1 := `{"type":"response.output_item.done","output_index":0,"item":{"type":"compaction","id":"cmp_1","encrypted_content":"enc-old"}}`
	item2 := `{"type":"response.output_item.done","output_index":1,"item":{"type":"compaction","id":"cmp_2","encrypted_content":"enc-new"}}`
	completed := `{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","output":[]}}`
	bridge := newStreamBridgeFake(
		rpcHostHTTPStreamReadResponse{Payload: []byte("data: " + item1 + "\n\ndata: " + item2 + "\n\n")},
		rpcHostHTTPStreamReadResponse{Payload: []byte("data: " + completed + "\n\n"), Done: true},
	)
	service := newPluginService(bridge)
	now := time.Unix(195_000, 0).UTC()
	service.now = func() time.Time { return now }
	firstPayload := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":[{"type":"input_text","text":"start"}]}]}`)
	_, errStream := service.executeStream(mustJSON(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID: "auth", Model: "gpt-5.4", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
			Payload: firstPayload, OriginalRequest: firstPayload,
			StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-5.4", Format: formatOpenAIResponse})),
		},
		StreamID: "plugin-compaction-1",
	}))
	if errStream != nil {
		t.Fatal(errStream)
	}
	bridge.wait(t)
	bridge.mu.Lock()
	emitted := string(bytesJoin(bridge.emitted))
	pluginError := bridge.pluginError
	bridge.mu.Unlock()
	if pluginError != "" {
		t.Fatalf("unexpected stream error = %q", pluginError)
	}

	latestID, latestEncrypted, responseID := selectLatestCompactionForTest(t, emitted)
	if latestID != "cmp_2" || latestEncrypted != "enc-new" || responseID != "resp_1" {
		t.Fatalf("test client selection = id=%q encrypted=%q responseID=%q", latestID, latestEncrypted, responseID)
	}

	secondPayload := mustJSON(t, map[string]any{
		"model":                "gpt-5.4",
		"previous_response_id": responseID,
		"input": []any{
			map[string]any{"type": "compaction", "id": latestID, "encrypted_content": latestEncrypted},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "continue"}}},
		},
	})
	var capturedBody []byte
	bridge2 := &fakeBridge{handler: func(_ string, payload any) (any, error) {
		req := payload.(rpcHostHTTPRequest)
		capturedBody = req.Body
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"id":"resp_2","object":"response","status":"completed","output":[]}`)}, nil
	}}
	service2 := newPluginService(bridge2)
	service2.now = func() time.Time { return now }
	_, errExecute := service2.execute(mustJSON(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID: "auth", Model: "gpt-5.4", Format: formatOpenAIResponse, SourceFormat: formatOpenAIResponse,
			Payload: secondPayload, OriginalRequest: secondPayload,
			StorageJSON: mustJSON(t, executorStorage(now, storedModel{ID: "gpt-5.4", Format: formatOpenAIResponse})),
		},
	}))
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
