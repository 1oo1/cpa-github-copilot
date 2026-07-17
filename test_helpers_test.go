package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

type recordedHostCall struct {
	Method  string
	Payload any
}

type fakeBridge struct {
	mu      sync.Mutex
	calls   []recordedHostCall
	logs    []rpcHostLogRequest
	handler func(string, any) (any, error)
}

func (f *fakeBridge) Call(method string, payload any) (json.RawMessage, error) {
	f.mu.Lock()
	if method == pluginabi.MethodHostLog {
		request, ok := payload.(rpcHostLogRequest)
		if ok {
			f.logs = append(f.logs, request)
		}
		f.mu.Unlock()
		return json.RawMessage(`{}`), nil
	}
	f.calls = append(f.calls, recordedHostCall{Method: method, Payload: payload})
	handler := f.handler
	f.mu.Unlock()
	if handler == nil {
		return nil, fmt.Errorf("unexpected host call %s", method)
	}
	result, errCall := handler(method, payload)
	if errCall != nil {
		return nil, errCall
	}
	raw, errMarshal := json.Marshal(result)
	return raw, errMarshal
}

func (f *fakeBridge) snapshot() []recordedHostCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedHostCall(nil), f.calls...)
}

func (f *fakeBridge) snapshotLogs() []rpcHostLogRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]rpcHostLogRequest(nil), f.logs...)
}

func decodePluginResult[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("unexpected plugin error: %#v", env.Error)
	}
	var result T
	if errUnmarshal := json.Unmarshal(env.Result, &result); errUnmarshal != nil {
		t.Fatalf("decode plugin result: %v", errUnmarshal)
	}
	return result
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatalf("marshal JSON: %v", errMarshal)
	}
	return raw
}
