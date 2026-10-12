package copilot

import (
	"crypto/rand"
	"io"
	"sync"
	"time"
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

type serviceError struct {
	code       string
	message    string
	retryable  bool
	httpStatus int
}

func (e *serviceError) Error() string {
	if e == nil {
		return "service request failed"
	}
	return e.message
}

type settings struct {
	ClientID       string `yaml:"client_id"`
	GitHubHost     string `yaml:"github_host"`
	ModelCacheTTL  int    `yaml:"model_cache_ttl_seconds"`
	MaxStreamBytes int    `yaml:"max_stream_buffer_bytes"`
	// EnableResponsesContextManagement 控制是否在原生 Responses 路由上默认启用
	// VS Code 可选的 server-side context compaction（context_management 阈值）。
	EnableResponsesContextManagement bool `yaml:"enable_responses_context_management"`
}

type routeKey struct {
	AuthID  string
	ModelID string
}

type service struct {
	host   hostRuntime
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

func newService(host hostRuntime) *service {
	return &service{
		host:        host,
		now:         time.Now,
		random:      rand.Reader,
		config:      defaultSettings(),
		sessions:    make(map[string]*loginSession),
		routes:      make(map[routeKey]modelRoute),
		streamTasks: make(map[*streamForwardTask]struct{}),
	}
}
