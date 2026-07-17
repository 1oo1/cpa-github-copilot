package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type hostBridge interface {
	Call(method string, payload any) (json.RawMessage, error)
}

type hostClient struct {
	bridge     hostBridge
	callbackID string
}

type rpcHostHTTPRequest struct {
	HostCallbackID string      `json:"host_callback_id,omitempty"`
	Method         string      `json:"method,omitempty"`
	URL            string      `json:"url,omitempty"`
	Headers        httpHeaders `json:"headers,omitempty"`
	Body           []byte      `json:"body,omitempty"`
}

type httpHeaders map[string][]string

type rpcHostHTTPStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    httpHeaders `json:"headers,omitempty"`
	StreamID   string      `json:"stream_id,omitempty"`
}

type rpcHostHTTPStreamReadRequest struct {
	StreamID string `json:"stream_id"`
}

type rpcHostHTTPStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

type rpcHostHTTPStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
}

type rpcHostStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
}

type rpcHostStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

func (c hostClient) do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	if c.bridge == nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("host HTTP bridge is unavailable")
	}
	raw, errCall := c.bridge.Call(pluginabi.MethodHostHTTPDo, rpcHostHTTPRequest{
		HostCallbackID: c.callbackID,
		Method:         req.Method,
		URL:            req.URL,
		Headers:        httpHeaders(req.Headers),
		Body:           append([]byte(nil), req.Body...),
	})
	if errCall != nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("host HTTP request failed")
	}
	var resp pluginapi.HTTPResponse
	if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("decode host HTTP response: %w", errDecode)
	}
	return resp, nil
}

func (c hostClient) openStream(req pluginapi.HTTPRequest) (rpcHostHTTPStreamResponse, error) {
	if c.bridge == nil {
		return rpcHostHTTPStreamResponse{}, fmt.Errorf("host HTTP bridge is unavailable")
	}
	raw, errCall := c.bridge.Call(pluginabi.MethodHostHTTPDoStream, rpcHostHTTPRequest{
		HostCallbackID: c.callbackID,
		Method:         req.Method,
		URL:            req.URL,
		Headers:        httpHeaders(req.Headers),
		Body:           append([]byte(nil), req.Body...),
	})
	if errCall != nil {
		return rpcHostHTTPStreamResponse{}, fmt.Errorf("host HTTP stream request failed")
	}
	var resp rpcHostHTTPStreamResponse
	if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
		return rpcHostHTTPStreamResponse{}, fmt.Errorf("decode host HTTP stream response: %w", errDecode)
	}
	if strings.TrimSpace(resp.StreamID) == "" {
		return rpcHostHTTPStreamResponse{}, fmt.Errorf("host HTTP stream returned no stream id")
	}
	return resp, nil
}

func (c hostClient) readStream(streamID string) (rpcHostHTTPStreamReadResponse, error) {
	raw, errCall := c.bridge.Call(pluginabi.MethodHostHTTPStreamRead, rpcHostHTTPStreamReadRequest{StreamID: streamID})
	if errCall != nil {
		return rpcHostHTTPStreamReadResponse{}, fmt.Errorf("host HTTP stream read failed")
	}
	var resp rpcHostHTTPStreamReadResponse
	if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
		return rpcHostHTTPStreamReadResponse{}, fmt.Errorf("decode host HTTP stream chunk: %w", errDecode)
	}
	return resp, nil
}

func (c hostClient) closeStream(streamID string) {
	if c.bridge == nil || strings.TrimSpace(streamID) == "" {
		return
	}
	_, _ = c.bridge.Call(pluginabi.MethodHostHTTPStreamClose, rpcHostHTTPStreamCloseRequest{StreamID: streamID})
}

func (c hostClient) emit(streamID string, payload []byte) error {
	if c.bridge == nil {
		return fmt.Errorf("host stream bridge is unavailable")
	}
	_, errCall := c.bridge.Call(pluginabi.MethodHostStreamEmit, rpcHostStreamEmitRequest{
		StreamID: streamID,
		Payload:  append([]byte(nil), payload...),
	})
	if errCall != nil {
		return fmt.Errorf("host stream emit failed")
	}
	return nil
}

func (c hostClient) closePluginStream(streamID, message string) {
	if c.bridge == nil || strings.TrimSpace(streamID) == "" {
		return
	}
	_, _ = c.bridge.Call(pluginabi.MethodHostStreamClose, rpcHostStreamCloseRequest{
		StreamID: streamID,
		Error:    strings.TrimSpace(message),
	})
}
