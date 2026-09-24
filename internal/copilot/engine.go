package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/1oo1/cpa-github-copilot/internal/contract"
	"github.com/1oo1/cpa-github-copilot/internal/operation"
)

// Settings controls only Copilot behavior; no dynamic plugin configuration exists.
type Settings = settings

func DefaultSettings() Settings { return defaultPluginConfig() }

type Transport interface {
	Do(context.Context, contract.HTTPRequest) (*http.Response, error)
}
type LogSink func(level, event string, fields map[string]any)
type Chunk struct {
	Payload []byte
	Err     error
}
type Engine struct {
	service *service
	bridge  *runtimeBridge
}

func New(settings Settings, transport Transport, log LogSink) (*Engine, error) {
	if transport == nil {
		return nil, fmt.Errorf("upstream transport is required")
	}
	bridge := &runtimeBridge{transport: transport, log: log, contexts: map[string]context.Context{}, readers: map[string]io.ReadCloser{}, outputs: map[string]*streamOutput{}}
	service := newService(bridge)
	host, err := normalizeGitHubHost(settings.GitHubHost)
	if err != nil {
		return nil, err
	}
	settings.GitHubHost = host
	if settings.ClientID == "" || settings.ModelCacheTTL < 0 || settings.MaxStreamBytes < 65536 || settings.MaxStreamBytes > 64<<20 {
		return nil, fmt.Errorf("invalid Copilot configuration")
	}
	service.config = settings
	return &Engine{service, bridge}, nil
}
func (e *Engine) Close() { e.service.shutdown() }
func (e *serviceError) StatusCode() int {
	if e.httpStatus != 0 {
		return e.httpStatus
	}
	return 502
}
func decodeResult[T any](fn func([]byte) ([]byte, error), req any) (T, error) {
	var result T
	raw, err := json.Marshal(req)
	if err != nil {
		return result, fmt.Errorf("invalid service request")
	}
	out, err := fn(raw)
	if err != nil {
		return result, err
	}
	var env envelope
	if json.Unmarshal(out, &env) != nil || !env.OK {
		return result, fmt.Errorf("invalid service result")
	}
	if json.Unmarshal(env.Result, &result) != nil {
		return result, fmt.Errorf("invalid service result")
	}
	return result, nil
}
func (e *Engine) Parse(name string, raw []byte) (contract.AuthData, error) {
	out, err := decodeResult[contract.AuthParseResponse](e.service.parseAuth, contract.AuthParseRequest{FileName: name, RawJSON: raw})
	if err == nil && (!out.Handled || out.Auth.Disabled) {
		err = fmt.Errorf("a valid GitHub Copilot credential for the configured host is required")
	}
	return out.Auth, err
}
func (e *Engine) LoginStart(ctx context.Context) (contract.AuthLoginStartResponse, error) {
	id, done := e.bridge.bind(ctx)
	defer done()
	return decodeResult[contract.AuthLoginStartResponse](e.service.startLogin, localAuthLoginStartRequest{HostCallbackID: id})
}
func (e *Engine) LoginPoll(ctx context.Context, state string) (contract.AuthLoginPollResponse, error) {
	id, done := e.bridge.bind(ctx)
	defer done()
	return decodeResult[contract.AuthLoginPollResponse](e.service.pollLogin, localAuthLoginPollRequest{AuthLoginPollRequest: contract.AuthLoginPollRequest{State: state}, HostCallbackID: id})
}
func (e *Engine) Refresh(ctx context.Context, auth contract.AuthData) (contract.AuthData, error) {
	id, done := e.bridge.bind(ctx)
	defer done()
	out, err := decodeResult[contract.AuthRefreshResponse](e.service.refreshAuth, localAuthRefreshRequest{AuthRefreshRequest: contract.AuthRefreshRequest{AuthID: auth.ID, StorageJSON: auth.StorageJSON, Metadata: auth.Metadata, Attributes: auth.Attributes}, HostCallbackID: id})
	return out.Auth, err
}
func (e *Engine) Catalog(ctx context.Context, auth contract.AuthData) (contract.ModelResponse, error) {
	id, done := e.bridge.bind(ctx)
	defer done()
	out, err := decodeResult[contract.ModelResponse](e.service.modelsForAuth, localAuthModelRequest{AuthModelRequest: contract.AuthModelRequest{AuthID: auth.ID, StorageJSON: auth.StorageJSON, Metadata: auth.Metadata, Attributes: auth.Attributes}, HostCallbackID: id})
	return out, err
}
func (e *Engine) Quota(ctx context.Context, auth contract.AuthData) (contract.QuotaFetchResponse, error) {
	id, done := e.bridge.bind(ctx)
	defer done()
	return decodeResult[contract.QuotaFetchResponse](e.service.fetchQuota, localQuotaFetchRequest{QuotaFetchRequest: contract.QuotaFetchRequest{AuthID: auth.ID, Provider: providerIdentifier, StorageJSON: auth.StorageJSON}, HostCallbackID: id})
}
func (e *Engine) Execute(ctx context.Context, auth contract.AuthData, model, format string, body []byte, headers http.Header) (contract.ExecutorResponse, error) {
	id, done := e.bridge.bind(ctx)
	defer done()
	return decodeResult[contract.ExecutorResponse](e.service.execute, localExecutorRequest{ExecutorRequest: contract.ExecutorRequest{AuthID: auth.ID, AuthProvider: providerIdentifier, Model: model, Format: format, SourceFormat: format, Payload: body, StorageJSON: auth.StorageJSON, Headers: headers}, HostCallbackID: id})
}
func (e *Engine) Stream(ctx context.Context, auth contract.AuthData, model, format string, body []byte, headers http.Header) (http.Header, <-chan Chunk, func(), error) {
	ctx, cancel := context.WithCancel(ctx)
	id, unbind := e.bridge.bind(ctx)
	output := &streamOutput{ctx: ctx, ch: make(chan Chunk, 16)}
	e.bridge.mu.Lock()
	e.bridge.outputs[id] = output
	e.bridge.mu.Unlock()
	var once sync.Once
	cleanup := func() {
		once.Do(func() { cancel(); unbind(); e.bridge.mu.Lock(); delete(e.bridge.outputs, id); e.bridge.mu.Unlock() })
	}
	out, err := decodeResult[localExecutorStreamResponse](e.service.executeStream, localExecutorRequest{ExecutorRequest: contract.ExecutorRequest{AuthID: auth.ID, AuthProvider: providerIdentifier, Model: model, Format: format, SourceFormat: format, Stream: true, Payload: body, StorageJSON: auth.StorageJSON, Headers: headers}, HostCallbackID: id, StreamID: id})
	if err != nil {
		cleanup()
		return nil, nil, func() {}, err
	}
	return out.Headers, output.ch, cleanup, nil
}

type streamOutput struct {
	ctx  context.Context
	ch   chan Chunk
	once sync.Once
}
type runtimeBridge struct {
	transport Transport
	log       LogSink
	mu        sync.Mutex
	seq       atomic.Uint64
	contexts  map[string]context.Context
	readers   map[string]io.ReadCloser
	outputs   map[string]*streamOutput
}

func (b *runtimeBridge) bind(ctx context.Context) (string, func()) {
	id := strconv.FormatUint(b.seq.Add(1), 10)
	b.mu.Lock()
	b.contexts[id] = ctx
	b.mu.Unlock()
	return id, func() { b.mu.Lock(); delete(b.contexts, id); b.mu.Unlock() }
}
func (b *runtimeBridge) Call(method string, payload any) (json.RawMessage, error) {
	marshal := func(v any) (json.RawMessage, error) { return json.Marshal(v) }
	switch method {
	case operation.MethodHostHTTPDo, operation.MethodHostHTTPDoStream:
		req := payload.(localHostHTTPRequest)
		b.mu.Lock()
		ctx := b.contexts[req.HostCallbackID]
		b.mu.Unlock()
		if ctx == nil {
			return nil, fmt.Errorf("request is no longer active")
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		resp, err := b.transport.Do(ctx, contract.HTTPRequest{Method: req.Method, URL: req.URL, Headers: http.Header(req.Headers), Body: req.Body})
		if err != nil {
			return nil, fmt.Errorf("upstream transport failed")
		}
		if method == operation.MethodHostHTTPDo {
			defer resp.Body.Close()
			body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20+1))
			if err != nil || len(body) > 32<<20 {
				return nil, fmt.Errorf("upstream response unavailable")
			}
			return marshal(contract.HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Header, Body: body})
		}
		streamID := "http-" + strconv.FormatUint(b.seq.Add(1), 10)
		b.mu.Lock()
		b.readers[streamID] = resp.Body
		b.mu.Unlock()
		return marshal(localHostHTTPStreamResponse{StatusCode: resp.StatusCode, Headers: httpHeaders(resp.Header), StreamID: streamID})
	case operation.MethodHostHTTPStreamRead:
		req := payload.(localHostHTTPStreamReadRequest)
		b.mu.Lock()
		reader := b.readers[req.StreamID]
		b.mu.Unlock()
		if reader == nil {
			return marshal(localHostHTTPStreamReadResponse{Done: true})
		}
		buffer := make([]byte, 16<<10)
		n, err := reader.Read(buffer)
		out := localHostHTTPStreamReadResponse{Payload: buffer[:n], Done: err != nil}
		if err != nil && err != io.EOF {
			out.Error = "upstream stream interrupted"
		}
		return marshal(out)
	case operation.MethodHostHTTPStreamClose:
		req := payload.(localHostHTTPStreamCloseRequest)
		b.mu.Lock()
		reader := b.readers[req.StreamID]
		delete(b.readers, req.StreamID)
		b.mu.Unlock()
		if reader != nil {
			_ = reader.Close()
		}
		return marshal(struct{}{})
	case operation.MethodHostStreamEmit:
		req := payload.(localHostStreamEmitRequest)
		b.mu.Lock()
		out := b.outputs[req.StreamID]
		b.mu.Unlock()
		if out == nil {
			return nil, fmt.Errorf("downstream closed")
		}
		select {
		case out.ch <- Chunk{Payload: req.Payload}:
			return marshal(struct{}{})
		case <-out.ctx.Done():
			return nil, out.ctx.Err()
		}
	case operation.MethodHostStreamClose:
		req := payload.(localHostStreamCloseRequest)
		b.mu.Lock()
		out := b.outputs[req.StreamID]
		b.mu.Unlock()
		if out != nil {
			out.once.Do(func() {
				if req.Error != "" {
					select {
					case out.ch <- Chunk{Err: fmt.Errorf("upstream stream ended without a valid terminal event")}:
					case <-out.ctx.Done():
					}
				}
				close(out.ch)
			})
		}
		return marshal(struct{}{})
	case operation.MethodHostLog:
		req := payload.(localHostLogRequest)
		if b.log != nil {
			event, _ := req.Fields["event"].(string)
			b.log(req.Level, event, req.Fields)
		}
		return marshal(struct{}{})
	default:
		return nil, fmt.Errorf("unsupported internal transport operation")
	}
}
