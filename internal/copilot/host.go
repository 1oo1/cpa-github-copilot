package copilot

import (
	"encoding/json"
	"fmt"
	"strings"

	contract "github.com/1oo1/cpa-github-copilot/internal/contract"
	operation "github.com/1oo1/cpa-github-copilot/internal/operation"
)

type hostBridge interface {
	Call(method string, payload any) (json.RawMessage, error)
}

type hostClient struct {
	bridge     hostBridge
	callbackID string
}

type localHostHTTPRequest struct {
	HostCallbackID string      `json:"host_callback_id,omitempty"`
	Method         string      `json:"method,omitempty"`
	URL            string      `json:"url,omitempty"`
	Headers        httpHeaders `json:"headers,omitempty"`
	Body           []byte      `json:"body,omitempty"`
}

type httpHeaders map[string][]string

type localHostHTTPStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    httpHeaders `json:"headers,omitempty"`
	StreamID   string      `json:"stream_id,omitempty"`
}

type localHostHTTPStreamReadRequest struct {
	StreamID string `json:"stream_id"`
}

type localHostHTTPStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

type localHostHTTPStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
}

type localHostStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
}

type localHostStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

func (c hostClient) do(req contract.HTTPRequest) (contract.HTTPResponse, error) {
	if c.bridge == nil {
		return contract.HTTPResponse{}, fmt.Errorf("host HTTP bridge is unavailable")
	}
	raw, errCall := c.bridge.Call(operation.MethodHostHTTPDo, localHostHTTPRequest{
		HostCallbackID: c.callbackID,
		Method:         req.Method,
		URL:            req.URL,
		Headers:        httpHeaders(req.Headers),
		Body:           append([]byte(nil), req.Body...),
	})
	if errCall != nil {
		return contract.HTTPResponse{}, fmt.Errorf("host HTTP request failed")
	}
	var resp contract.HTTPResponse
	if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
		return contract.HTTPResponse{}, fmt.Errorf("decode host HTTP response: %w", errDecode)
	}
	return resp, nil
}

func (c hostClient) openStream(req contract.HTTPRequest) (localHostHTTPStreamResponse, error) {
	if c.bridge == nil {
		return localHostHTTPStreamResponse{}, fmt.Errorf("host HTTP bridge is unavailable")
	}
	raw, errCall := c.bridge.Call(operation.MethodHostHTTPDoStream, localHostHTTPRequest{
		HostCallbackID: c.callbackID,
		Method:         req.Method,
		URL:            req.URL,
		Headers:        httpHeaders(req.Headers),
		Body:           append([]byte(nil), req.Body...),
	})
	if errCall != nil {
		return localHostHTTPStreamResponse{}, fmt.Errorf("host HTTP stream request failed")
	}
	var resp localHostHTTPStreamResponse
	if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
		return localHostHTTPStreamResponse{}, fmt.Errorf("decode host HTTP stream response: %w", errDecode)
	}
	if strings.TrimSpace(resp.StreamID) == "" {
		return localHostHTTPStreamResponse{}, fmt.Errorf("host HTTP stream returned no stream id")
	}
	return resp, nil
}

func (c hostClient) readStream(streamID string) (localHostHTTPStreamReadResponse, error) {
	raw, errCall := c.bridge.Call(operation.MethodHostHTTPStreamRead, localHostHTTPStreamReadRequest{StreamID: streamID})
	if errCall != nil {
		return localHostHTTPStreamReadResponse{}, fmt.Errorf("host HTTP stream read failed")
	}
	var resp localHostHTTPStreamReadResponse
	if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
		return localHostHTTPStreamReadResponse{}, fmt.Errorf("decode host HTTP stream chunk: %w", errDecode)
	}
	return resp, nil
}

func (c hostClient) closeStream(streamID string) {
	if c.bridge == nil || strings.TrimSpace(streamID) == "" {
		return
	}
	_, _ = c.bridge.Call(operation.MethodHostHTTPStreamClose, localHostHTTPStreamCloseRequest{StreamID: streamID})
}

func (c hostClient) emit(streamID string, payload []byte) error {
	if c.bridge == nil {
		return fmt.Errorf("host stream bridge is unavailable")
	}
	_, errCall := c.bridge.Call(operation.MethodHostStreamEmit, localHostStreamEmitRequest{
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
	_, _ = c.bridge.Call(operation.MethodHostStreamClose, localHostStreamCloseRequest{
		StreamID: streamID,
		Error:    strings.TrimSpace(message),
	})
}
