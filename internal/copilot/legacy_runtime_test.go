package copilot

// Test-only compatibility fixtures preserve the old serialized host-call
// shapes. Production code uses typed in-process calls and never crosses this
// envelope.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	contract "github.com/1oo1/cpa-github-copilot/internal/contract"
	"gopkg.in/yaml.v3"
)

type legacyBridge interface {
	Call(string, any) (json.RawMessage, error)
}

const (
	legacyMethodHostHTTPDo          = "host.http.do"
	legacyMethodHostHTTPDoStream    = "host.http.do_stream"
	legacyMethodHostHTTPStreamClose = "host.http.stream_close"
	legacyMethodHostHTTPStreamRead  = "host.http.stream_read"
	legacyMethodHostLog             = "host.log"
	legacyMethodHostStreamClose     = "host.stream.close"
	legacyMethodHostStreamEmit      = "host.stream.emit"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type httpHeaders map[string][]string

type localAuthLoginStartRequest struct {
	contract.AuthLoginStartRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type localAuthLoginPollRequest struct {
	contract.AuthLoginPollRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type localAuthRefreshRequest struct {
	contract.AuthRefreshRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type localAuthModelRequest struct {
	contract.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type localExecutorRequest struct {
	contract.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type localExecutorHTTPRequest struct {
	contract.ExecutorHTTPRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type localExecutorStreamResponse struct {
	Headers http.Header `json:"headers,omitempty"`
}

type localQuotaFetchRequest struct {
	contract.QuotaFetchRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type localHostLogRequest struct {
	HostCallbackID string         `json:"host_callback_id,omitempty"`
	Level          string         `json:"level,omitempty"`
	Message        string         `json:"message,omitempty"`
	Fields         map[string]any `json:"fields,omitempty"`
}

type localHostHTTPRequest struct {
	HostCallbackID string      `json:"host_callback_id,omitempty"`
	Method         string      `json:"method,omitempty"`
	URL            string      `json:"url,omitempty"`
	Headers        httpHeaders `json:"headers,omitempty"`
	Body           []byte      `json:"body,omitempty"`
}

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

type legacyRuntime struct {
	bridge legacyBridge
}

func okEnvelope(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func defaultPluginConfig() settings { return defaultSettings() }

func pendingLoginResponse() ([]byte, error) {
	return okEnvelope(pendingLoginResponseValue())
}

func newLegacyRuntime(bridge legacyBridge) hostRuntime {
	if bridge == nil {
		return nil
	}
	return legacyRuntime{bridge: bridge}
}

func testHostClient(bridge legacyBridge) hostClient {
	return hostClient{ctx: context.Background(), host: newLegacyRuntime(bridge)}
}

func decodeLegacy[T any](raw json.RawMessage) (T, error) {
	var value T
	if len(raw) == 0 {
		return value, fmt.Errorf("empty legacy response")
	}
	return value, json.Unmarshal(raw, &value)
}

func (r legacyRuntime) do(_ context.Context, callbackID string, req contract.HTTPRequest) (contract.HTTPResponse, error) {
	raw, err := r.bridge.Call("host.http.do", localHostHTTPRequest{
		HostCallbackID: callbackID,
		Method:         req.Method,
		URL:            req.URL,
		Headers:        httpHeaders(req.Headers),
		Body:           req.Body,
	})
	if err != nil {
		return contract.HTTPResponse{}, err
	}
	return decodeLegacy[contract.HTTPResponse](raw)
}

func (r legacyRuntime) openStream(_ context.Context, callbackID string, req contract.HTTPRequest) (hostHTTPStreamResponse, error) {
	raw, err := r.bridge.Call("host.http.do_stream", localHostHTTPRequest{
		HostCallbackID: callbackID,
		Method:         req.Method,
		URL:            req.URL,
		Headers:        httpHeaders(req.Headers),
		Body:           req.Body,
	})
	if err != nil {
		return hostHTTPStreamResponse{}, err
	}
	response, err := decodeLegacy[localHostHTTPStreamResponse](raw)
	if err != nil {
		return hostHTTPStreamResponse{}, err
	}
	return hostHTTPStreamResponse{StatusCode: response.StatusCode, Headers: http.Header(response.Headers), StreamID: response.StreamID}, nil
}

func (r legacyRuntime) readStream(streamID string) (hostHTTPStreamReadResponse, error) {
	raw, err := r.bridge.Call("host.http.stream_read", localHostHTTPStreamReadRequest{StreamID: streamID})
	if err != nil {
		return hostHTTPStreamReadResponse{}, err
	}
	response, err := decodeLegacy[localHostHTTPStreamReadResponse](raw)
	if err != nil {
		return hostHTTPStreamReadResponse{}, err
	}
	return hostHTTPStreamReadResponse{Payload: response.Payload, Error: response.Error, Done: response.Done}, nil
}

func (r legacyRuntime) closeStream(streamID string) {
	_, _ = r.bridge.Call("host.http.stream_close", localHostHTTPStreamCloseRequest{StreamID: streamID})
}

func (r legacyRuntime) emit(streamID string, payload []byte) error {
	_, err := r.bridge.Call("host.stream.emit", localHostStreamEmitRequest{StreamID: streamID, Payload: payload})
	return err
}

func (r legacyRuntime) closeOutput(streamID, message string) {
	_, _ = r.bridge.Call("host.stream.close", localHostStreamCloseRequest{StreamID: streamID, Error: message})
}

func (r legacyRuntime) log(callbackID, level, event string, fields map[string]any) {
	_, _ = r.bridge.Call("host.log", localHostLogRequest{HostCallbackID: callbackID, Level: level, Message: "github-copilot: " + event, Fields: fields})
}

func (s *service) parseAuth(raw []byte) ([]byte, error) {
	var req contract.AuthParseRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return okEnvelope(contract.AuthParseResponse{Handled: false})
	}
	out, err := s.parseAuthRequest(req)
	if err != nil {
		return nil, err
	}
	return okEnvelope(out)
}

func (s *service) configure(raw []byte) error {
	var request lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &request); err != nil {
			return err
		}
	}
	cfg := defaultSettings()
	if len(request.ConfigYAML) > 0 {
		if err := yaml.Unmarshal(request.ConfigYAML, &cfg); err != nil {
			return err
		}
	}
	return s.configureSettings(cfg)
}

func (s *service) startLogin(raw []byte) ([]byte, error) {
	var req localAuthLoginStartRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	out, err := s.startLoginRequest(context.Background(), req.HostCallbackID, req.AuthLoginStartRequest)
	if err != nil {
		return nil, err
	}
	return okEnvelope(out)
}

func (s *service) pollLogin(raw []byte) ([]byte, error) {
	var req localAuthLoginPollRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	out, err := s.pollLoginRequest(context.Background(), req.HostCallbackID, req.AuthLoginPollRequest)
	if err != nil {
		return nil, err
	}
	return okEnvelope(out)
}

func (s *service) refreshAuth(raw []byte) ([]byte, error) {
	var req localAuthRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	out, err := s.refreshAuthRequest(context.Background(), req.HostCallbackID, req.AuthRefreshRequest)
	if err != nil {
		return nil, err
	}
	return okEnvelope(out)
}

func (s *service) modelsForAuth(raw []byte) ([]byte, error) {
	var req localAuthModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	out, err := s.modelsForAuthRequest(context.Background(), req.HostCallbackID, req.AuthModelRequest)
	if err != nil {
		return nil, err
	}
	return okEnvelope(out)
}

func (s *service) fetchQuota(raw []byte) ([]byte, error) {
	var req localQuotaFetchRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	out, err := s.fetchQuotaRequest(context.Background(), req.HostCallbackID, req.QuotaFetchRequest)
	if err != nil {
		return nil, err
	}
	return okEnvelope(out)
}

func (s *service) execute(raw []byte) ([]byte, error) {
	var req localExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	out, err := s.executeRequest(context.Background(), req.HostCallbackID, req.ExecutorRequest)
	if err != nil {
		return nil, err
	}
	return okEnvelope(out)
}

func (s *service) executeStream(raw []byte) ([]byte, error) {
	var req localExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	headers, err := s.executeStreamRequest(context.Background(), req.HostCallbackID, req.StreamID, req.ExecutorRequest)
	if err != nil {
		return nil, err
	}
	return okEnvelope(localExecutorStreamResponse{Headers: headers})
}

func (s *service) executeHTTPRequest(raw []byte) ([]byte, error) {
	var req localExecutorHTTPRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	out, err := s.executeProviderHTTPRequest(context.Background(), req.HostCallbackID, req.ExecutorHTTPRequest)
	if err != nil {
		return nil, err
	}
	return okEnvelope(out)
}
