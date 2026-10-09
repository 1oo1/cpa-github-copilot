package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	contract "github.com/1oo1/cpa-github-copilot/internal/contract"
)

// hostHTTPCall is an upstream request exactly as the service handed it to hostRuntime.
type hostHTTPCall struct {
	CallbackID string
	contract.HTTPRequest
}

// hostLogEntry is a diagnostic log call exactly as the service handed it to hostRuntime.
type hostLogEntry struct {
	CallbackID string
	Level      string
	Event      string
	Fields     map[string]any
}

func newHostHTTPCall(callbackID string, req contract.HTTPRequest) hostHTTPCall {
	req.Headers = req.Headers.Clone()
	req.Body = append([]byte(nil), req.Body...)
	if req.WireProfile != nil {
		profile := *req.WireProfile
		profile.HeaderProfile = append([]string(nil), profile.HeaderProfile...)
		req.WireProfile = &profile
	}
	return hostHTTPCall{CallbackID: callbackID, HTTPRequest: req}
}

func newHostLogEntry(callbackID, level, event string, fields map[string]any) hostLogEntry {
	copied := make(map[string]any, len(fields))
	for key, value := range fields {
		copied[key] = value
	}
	return hostLogEntry{CallbackID: callbackID, Level: level, Event: event, Fields: copied}
}

// fakeHost is a hostRuntime for non-streaming tests. It records every upstream
// HTTP request and diagnostic log, and fails the test on any stream operation.
type fakeHost struct {
	t       testing.TB
	handler func(hostHTTPCall) (contract.HTTPResponse, error)

	mu    sync.Mutex
	calls []hostHTTPCall
	logs  []hostLogEntry
}

func newFakeHost(t testing.TB, handler func(hostHTTPCall) (contract.HTTPResponse, error)) *fakeHost {
	return &fakeHost{t: t, handler: handler}
}

func (f *fakeHost) do(_ context.Context, callbackID string, req contract.HTTPRequest) (contract.HTTPResponse, error) {
	call := newHostHTTPCall(callbackID, req)
	f.mu.Lock()
	f.calls = append(f.calls, call)
	handler := f.handler
	f.mu.Unlock()
	if handler == nil {
		return contract.HTTPResponse{}, fmt.Errorf("unexpected host HTTP call %s %s", req.Method, req.URL)
	}
	return handler(call)
}

func (f *fakeHost) openStream(context.Context, string, contract.HTTPRequest) (hostHTTPStreamResponse, error) {
	return hostHTTPStreamResponse{}, f.unexpected("openStream")
}

func (f *fakeHost) readStream(string) (hostHTTPStreamReadResponse, error) {
	return hostHTTPStreamReadResponse{}, f.unexpected("readStream")
}

func (f *fakeHost) closeStream(string) { _ = f.unexpected("closeStream") }

func (f *fakeHost) emit(string, []byte) error { return f.unexpected("emit") }

func (f *fakeHost) closeOutput(string, string) { _ = f.unexpected("closeOutput") }

func (f *fakeHost) unexpected(operation string) error {
	f.t.Errorf("unexpected host call %s", operation)
	return fmt.Errorf("unexpected host call %s", operation)
}

func (f *fakeHost) log(callbackID, level, event string, fields map[string]any) {
	entry := newHostLogEntry(callbackID, level, event, fields)
	f.mu.Lock()
	f.logs = append(f.logs, entry)
	f.mu.Unlock()
}

// snapshot returns the upstream HTTP requests recorded so far.
func (f *fakeHost) snapshot() []hostHTTPCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]hostHTTPCall(nil), f.calls...)
}

func (f *fakeHost) snapshotLogs() []hostLogEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]hostLogEntry(nil), f.logs...)
}

func testHostClient(host hostRuntime) hostClient {
	return hostClient{ctx: context.Background(), host: host}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatalf("marshal JSON: %v", errMarshal)
	}
	return raw
}
