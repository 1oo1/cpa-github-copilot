package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestExecuteStreamPassThroughAndClosesBothStreams(t *testing.T) {
	bridge := newStreamBridgeFake(
		rpcHostHTTPStreamReadResponse{Payload: []byte("data: first\n\n")},
		rpcHostHTTPStreamReadResponse{Payload: []byte("data: [DONE]\n\n"), Done: true},
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
	if got := string(bytesJoin(bridge.emitted)); got != "data: first\n\ndata: [DONE]\n\n" {
		t.Fatalf("emitted = %q", got)
	}
}

func TestExecuteStreamPassThroughFramesSplitSSEData(t *testing.T) {
	bridge := newStreamBridgeFake(
		rpcHostHTTPStreamReadResponse{Payload: []byte(`data: {"type":"response.created","response":{"id":"resp`)},
		rpcHostHTTPStreamReadResponse{Payload: []byte("-1\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"output\":[]}}\n\n"), Done: true},
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

func TestExecuteStreamTranslatesSplitSSEFrames(t *testing.T) {
	first := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4.1","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}` + "\n\n"
	finish := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4.1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	done := "data: [DONE]\n\n"
	bridge := newStreamBridgeFake(
		rpcHostHTTPStreamReadResponse{Payload: []byte(first[:31])},
		rpcHostHTTPStreamReadResponse{Payload: []byte(first[31:] + finish + done), Done: true},
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
	errMessage, _ := entry.Fields["error"].(string)
	if strings.Contains(errMessage, "private upstream detail") {
		t.Fatalf("log leaked upstream detail: %q", errMessage)
	}
	if errMessage != "GitHub Copilot upstream stream failed" {
		t.Fatalf("error message = %q", errMessage)
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
