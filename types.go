package main

import (
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	pluginIdentifier = "github-copilot"
	pluginVersion    = "0.1.2"

	defaultClientID   = "Iv1.b507a08c87ecfe98"
	defaultGitHubHost = "github.com"

	formatOpenAI         = "openai"
	formatOpenAIResponse = "openai-response"
	formatClaude         = "claude"
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

type pluginFailure struct {
	code       string
	message    string
	retryable  bool
	httpStatus int
}

func (e *pluginFailure) Error() string {
	if e == nil {
		return "plugin request failed"
	}
	return e.message
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type pluginConfig struct {
	ClientID       string `yaml:"client_id"`
	GitHubHost     string `yaml:"github_host"`
	EnableModels   bool   `yaml:"enable_models"`
	ModelCacheTTL  int    `yaml:"model_cache_ttl_seconds"`
	MaxStreamBytes int    `yaml:"max_stream_buffer_bytes"`
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	ModelProvider         bool     `json:"model_provider"`
	AuthProvider          bool     `json:"auth_provider"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type rpcAuthLoginStartRequest struct {
	pluginapi.AuthLoginStartRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcAuthLoginPollRequest struct {
	pluginapi.AuthLoginPollRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcAuthRefreshRequest struct {
	pluginapi.AuthRefreshRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcAuthModelRequest struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcExecutorHTTPRequest struct {
	pluginapi.ExecutorHTTPRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcExecutorStreamResponse struct {
	Headers http.Header `json:"headers,omitempty"`
}

type routeKey struct {
	AuthID  string
	ModelID string
}

type pluginService struct {
	bridge hostBridge
	now    func() time.Time
	random io.Reader

	mu       sync.RWMutex
	config   pluginConfig
	sessions map[string]*loginSession
	routes   map[routeKey]modelRoute
}

func newPluginService(bridge hostBridge) *pluginService {
	return &pluginService{
		bridge:   bridge,
		now:      time.Now,
		random:   rand.Reader,
		config:   defaultPluginConfig(),
		sessions: make(map[string]*loginSession),
		routes:   make(map[routeKey]modelRoute),
	}
}
