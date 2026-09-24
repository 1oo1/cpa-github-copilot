package copilot

import (
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	contract "github.com/1oo1/cpa-github-copilot/internal/contract"
)

const (
	providerIdentifier = "github-copilot"

	defaultClientID   = "Iv1.b507a08c87ecfe98"
	defaultGitHubHost = "github.com"

	formatOpenAI         = "openai"
	formatOpenAIResponse = "openai-response"
	formatClaude         = "claude"
)

var providerVersion = "0.0.0-dev"

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

type serviceError struct {
	code       string
	message    string
	retryable  bool
	httpStatus int
}

func (e *serviceError) Error() string {
	if e == nil {
		return "plugin request failed"
	}
	return e.message
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type settings struct {
	ClientID       string `yaml:"client_id"`
	GitHubHost     string `yaml:"github_host"`
	EnableModels   bool   `yaml:"enable_models"`
	ModelCacheTTL  int    `yaml:"model_cache_ttl_seconds"`
	MaxStreamBytes int    `yaml:"max_stream_buffer_bytes"`
	// WebSearchModel 是 Anthropic 原生 web_search 独占请求使用的同账户 Responses 模型；
	// 空值表示禁用该分流并在本地拒绝请求。
	WebSearchModel string `yaml:"web_search_model"`
	// EnableResponsesContextManagement 控制是否在原生 Responses 路由上默认启用
	// VS Code 可选的 server-side context compaction（context_management 阈值）。
	EnableResponsesContextManagement bool `yaml:"enable_responses_context_management"`
}

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

type routeKey struct {
	AuthID  string
	ModelID string
}

type service struct {
	bridge hostBridge
	now    func() time.Time
	random io.Reader

	mu       sync.RWMutex
	config   settings
	sessions map[string]*loginSession
	routes   map[routeKey]modelRoute

	streamMu            sync.Mutex
	streamWG            sync.WaitGroup
	streamTasks         map[*streamForwardTask]struct{}
	streamsShuttingDown bool
}

func newService(bridge hostBridge) *service {
	return &service{
		bridge:      bridge,
		now:         time.Now,
		random:      rand.Reader,
		config:      defaultPluginConfig(),
		sessions:    make(map[string]*loginSession),
		routes:      make(map[routeKey]modelRoute),
		streamTasks: make(map[*streamForwardTask]struct{}),
	}
}
