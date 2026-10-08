package copilot

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/1oo1/cpa-github-copilot/internal/contract"
)

// Settings controls only Copilot behavior; no dynamic plugin configuration exists.
type Settings = settings

func DefaultSettings() Settings { return defaultSettings() }

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
	host    *runtimeHost
}

func New(settings Settings, transport Transport, log LogSink) (*Engine, error) {
	if transport == nil {
		return nil, fmt.Errorf("upstream transport is required")
	}
	runtime := &runtimeHost{transport: transport, logSink: log, readers: map[string]io.ReadCloser{}, outputs: map[string]*streamOutput{}}
	service := newService(runtime)
	host, err := normalizeGitHubHost(settings.GitHubHost)
	if err != nil {
		return nil, err
	}
	settings.GitHubHost = host
	if settings.ClientID == "" || settings.ModelCacheTTL < 0 || settings.MaxStreamBytes < 65536 || settings.MaxStreamBytes > 64<<20 {
		return nil, fmt.Errorf("invalid Copilot configuration")
	}
	service.config = settings
	return &Engine{service: service, host: runtime}, nil
}
func (e *Engine) Close() { e.service.shutdown() }
func (e *serviceError) StatusCode() int {
	if e.httpStatus != 0 {
		return e.httpStatus
	}
	return 502
}

func (e *Engine) Parse(name string, raw []byte) (contract.AuthData, error) {
	out, err := e.service.parseAuthRequest(contract.AuthParseRequest{FileName: name, RawJSON: raw})
	if err == nil && (!out.Handled || out.Auth.Disabled) {
		err = fmt.Errorf("a valid GitHub Copilot credential for the configured host is required")
	}
	return out.Auth, err
}
func (e *Engine) LoginStart(ctx context.Context) (contract.AuthLoginStartResponse, error) {
	return e.service.startLoginRequest(ctx, e.host.newID("login-"), contract.AuthLoginStartRequest{Provider: providerIdentifier})
}
func (e *Engine) LoginPoll(ctx context.Context, state string) (contract.AuthLoginPollResponse, error) {
	return e.service.pollLoginRequest(ctx, e.host.newID("login-poll-"), contract.AuthLoginPollRequest{Provider: providerIdentifier, State: state})
}
func (e *Engine) Refresh(ctx context.Context, auth contract.AuthData) (contract.AuthData, error) {
	out, err := e.service.refreshAuthRequest(ctx, e.host.newID("auth-refresh-"), contract.AuthRefreshRequest{
		AuthID:      auth.ID,
		StorageJSON: auth.StorageJSON,
		Metadata:    auth.Metadata,
		Attributes:  auth.Attributes,
	})
	return out.Auth, err
}
func (e *Engine) Catalog(ctx context.Context, auth contract.AuthData) (contract.ModelResponse, error) {
	return e.service.modelsForAuthRequest(ctx, e.host.newID("models-"), contract.AuthModelRequest{
		AuthID:      auth.ID,
		StorageJSON: auth.StorageJSON,
		Metadata:    auth.Metadata,
		Attributes:  auth.Attributes,
	})
}
func (e *Engine) Quota(ctx context.Context, auth contract.AuthData) (contract.QuotaFetchResponse, error) {
	return e.service.fetchQuotaRequest(ctx, e.host.newID("quota-"), contract.QuotaFetchRequest{
		AuthID:      auth.ID,
		Provider:    providerIdentifier,
		StorageJSON: auth.StorageJSON,
	})
}
func (e *Engine) Execute(ctx context.Context, auth contract.AuthData, model, format string, body []byte, headers http.Header) (contract.ExecutorResponse, error) {
	return e.service.executeRequest(ctx, e.host.newID("inference-"), contract.ExecutorRequest{
		AuthID:       auth.ID,
		AuthProvider: providerIdentifier,
		Model:        model,
		Format:       format,
		SourceFormat: format,
		Payload:      body,
		StorageJSON:  auth.StorageJSON,
		Headers:      headers,
	})
}
func (e *Engine) Stream(ctx context.Context, auth contract.AuthData, model, format string, body []byte, headers http.Header) (http.Header, <-chan Chunk, func(), error) {
	ctx, cancel := context.WithCancel(ctx)
	id := e.host.newID("stream-")
	output := &streamOutput{ctx: ctx, ch: make(chan Chunk, 16)}
	e.host.registerOutput(id, output)
	var once sync.Once
	cleanup := func() {
		once.Do(func() { cancel(); e.host.unregisterOutput(id) })
	}
	resultHeaders, err := e.service.executeStreamRequest(ctx, id, id, contract.ExecutorRequest{
		AuthID:       auth.ID,
		AuthProvider: providerIdentifier,
		Model:        model,
		Format:       format,
		SourceFormat: format,
		Stream:       true,
		Payload:      body,
		StorageJSON:  auth.StorageJSON,
		Headers:      headers,
	})
	if err != nil {
		cleanup()
		return nil, nil, func() {}, err
	}
	return resultHeaders, output.ch, cleanup, nil
}

type streamOutput struct {
	ctx  context.Context
	ch   chan Chunk
	once sync.Once
}

type runtimeHost struct {
	transport Transport
	logSink   LogSink
	mu        sync.Mutex
	seq       atomic.Uint64
	readers   map[string]io.ReadCloser
	outputs   map[string]*streamOutput
}

func (b *runtimeHost) newID(prefix string) string {
	return prefix + strconv.FormatUint(b.seq.Add(1), 10)
}

func (b *runtimeHost) registerOutput(id string, output *streamOutput) {
	b.mu.Lock()
	b.outputs[id] = output
	b.mu.Unlock()
}

func (b *runtimeHost) unregisterOutput(id string) {
	b.mu.Lock()
	delete(b.outputs, id)
	b.mu.Unlock()
}

func (b *runtimeHost) do(ctx context.Context, _ string, request contract.HTTPRequest) (contract.HTTPResponse, error) {
	if err := ctx.Err(); err != nil {
		return contract.HTTPResponse{}, err
	}
	resp, err := b.transport.Do(ctx, request)
	if err != nil {
		return contract.HTTPResponse{}, fmt.Errorf("upstream transport failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20+1))
	if err != nil || len(body) > 32<<20 {
		return contract.HTTPResponse{}, fmt.Errorf("upstream response unavailable")
	}
	return contract.HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Header, Body: body}, nil
}

func (b *runtimeHost) openStream(ctx context.Context, _ string, request contract.HTTPRequest) (hostHTTPStreamResponse, error) {
	if err := ctx.Err(); err != nil {
		return hostHTTPStreamResponse{}, err
	}
	resp, err := b.transport.Do(ctx, request)
	if err != nil {
		return hostHTTPStreamResponse{}, fmt.Errorf("upstream transport failed")
	}
	streamID := b.newID("upstream-")
	b.mu.Lock()
	b.readers[streamID] = resp.Body
	b.mu.Unlock()
	return hostHTTPStreamResponse{StatusCode: resp.StatusCode, Headers: resp.Header, StreamID: streamID}, nil
}

func (b *runtimeHost) readStream(streamID string) (hostHTTPStreamReadResponse, error) {
	b.mu.Lock()
	reader := b.readers[streamID]
	b.mu.Unlock()
	if reader == nil {
		return hostHTTPStreamReadResponse{Done: true}, nil
	}
	buffer := make([]byte, 16<<10)
	n, err := reader.Read(buffer)
	out := hostHTTPStreamReadResponse{Payload: buffer[:n], Done: err != nil}
	if err != nil && err != io.EOF {
		out.Error = "upstream stream interrupted"
	}
	return out, nil
}

func (b *runtimeHost) closeStream(streamID string) {
	b.mu.Lock()
	reader := b.readers[streamID]
	delete(b.readers, streamID)
	b.mu.Unlock()
	if reader != nil {
		_ = reader.Close()
	}
}

func (b *runtimeHost) emit(streamID string, payload []byte) error {
	b.mu.Lock()
	out := b.outputs[streamID]
	b.mu.Unlock()
	if out == nil {
		return fmt.Errorf("downstream closed")
	}
	select {
	case out.ch <- Chunk{Payload: payload}:
		return nil
	case <-out.ctx.Done():
		return out.ctx.Err()
	}
}

func (b *runtimeHost) closeOutput(streamID, message string) {
	b.mu.Lock()
	out := b.outputs[streamID]
	b.mu.Unlock()
	if out == nil {
		return
	}
	out.once.Do(func() {
		if message != "" {
			select {
			case out.ch <- Chunk{Err: fmt.Errorf("upstream stream ended without a valid terminal event")}:
			case <-out.ctx.Done():
			}
		}
		close(out.ch)
	})
}

func (b *runtimeHost) log(_, level, event string, fields map[string]any) {
	if b.logSink != nil {
		b.logSink(level, event, fields)
	}
}
