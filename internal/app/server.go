package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/1oo1/cpa-github-copilot/internal/copilot"
	"github.com/1oo1/cpa-github-copilot/internal/transport"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"gopkg.in/yaml.v3"
)

type binding struct {
	id      string
	expires time.Time
}
type Usage struct {
	Requests int64 `json:"requests"`
	Errors   int64 `json:"errors"`
	Input    int64 `json:"input_tokens"`
	Output   int64 `json:"output_tokens"`
}
type Log struct {
	Time     time.Time `json:"time"`
	Level    string    `json:"level"`
	Event    string    `json:"event"`
	Account  string    `json:"account,omitempty"`
	Model    string    `json:"model,omitempty"`
	Status   int       `json:"status,omitempty"`
	Duration int64     `json:"duration_ms,omitempty"`
	Input    int64     `json:"input_tokens,omitempty"`
	Output   int64     `json:"output_tokens,omitempty"`
	Client   string    `json:"client,omitempty"`
}
type Server struct {
	modelTTL                     time.Duration
	mu                           sync.Mutex
	cfg                          Config
	configPath, dataDir, authDir string
	accounts                     map[string]*account
	affinity                     map[string]binding
	sequence                     uint64
	engine                       *copilot.Engine
	httpClient                   *transport.Client
	handler                      http.Handler
	stop                         context.CancelFunc
	monitorDone                  chan struct{}
	usageMu                      sync.Mutex
	usage                        Usage
	usageDirty                   bool
	logs                         []Log
	limiter                      authLimiter
	trustedProxies               []netip.Prefix
	gate                         *memoryGate
}

func New(cfg Config, path string, network copilot.Transport, ui []byte) (*Server, error) {
	if len(cfg.ManagementKey) < 16 || len(cfg.APIKeys) == 0 {
		return nil, fmt.Errorf("management and API keys must be configured")
	}
	for _, key := range cfg.APIKeys {
		if keyMatches(key, cfg.ManagementKey) {
			return nil, fmt.Errorf("management-key must be distinct from API keys")
		}
		if len(key) < 16 {
			return nil, fmt.Errorf("API keys must have at least 16 characters")
		}
	}
	if cfg.SessionAffinityTTL < 0 {
		return nil, fmt.Errorf("session-affinity-ttl must not be negative")
	}
	if cfg.SessionAffinityTTL == 0 {
		cfg.SessionAffinityTTL = sessionAffinityTTL
	}
	trusted, err := parseTrustedProxies(cfg.TrustedProxies)
	if err != nil {
		return nil, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	s := &Server{modelTTL: time.Duration(cfg.Copilot.ModelCacheTTL) * time.Second, cfg: cfg, configPath: path, dataDir: filepath.Dir(path), authDir: cfg.AuthDir, accounts: map[string]*account{}, affinity: map[string]binding{}, monitorDone: make(chan struct{}), limiter: authLimiter{clients: map[authClient]*authFailure{}, known: map[[32]byte]struct{}{}}, trustedProxies: trusted, gate: newMemoryGate(requestMemoryBudget())}
	if s.authDir == "" {
		s.authDir = filepath.Join(s.dataDir, "auths")
	} else if !filepath.IsAbs(s.authDir) {
		s.authDir = filepath.Join(s.dataDir, s.authDir)
	}
	if network == nil {
		s.httpClient = transport.New(cfg.ProxyURL)
		network = s.httpClient
	}
	s.engine, err = copilot.New(cfg.Copilot, network, func(level, event string, _ map[string]any) {
		s.recordLog(Log{Time: time.Now().UTC(), Level: level, Event: event})
	})
	if err != nil {
		return nil, err
	}
	if err = s.loadAccounts(); err != nil {
		s.engine.Close()
		return nil, err
	}
	if raw, readErr := os.ReadFile(filepath.Join(s.dataDir, "usage.json")); readErr == nil {
		if json.Unmarshal(raw, &s.usage) != nil {
			return nil, fmt.Errorf("invalid usage state")
		}
	}
	csp := uiContentSecurityPolicy(ui)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, map[string]string{"status": "ok"}) })
	mux.Handle("/v1/", s.apiAuth(http.HandlerFunc(s.inference)))
	mux.Handle("/v0/management/", s.managementAuth(s.managementRoutes()))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if (r.Method != "GET" && r.Method != "HEAD") || (r.URL.Path != "/" && r.URL.Path != "/management.html") {
			http.NotFound(w, r)
			return
		}
		if len(ui) == 0 {
			http.Error(w, "Management UI not built; run make build", 503)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		_, _ = w.Write(ui)
	})
	s.handler = mux
	return s, nil
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(downstreamWriteTimeout))
	s.handler.ServeHTTP(w, r)
}
func (s *Server) Start(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		return
	}
	ctx, s.stop = context.WithCancel(ctx)
	go s.monitor(ctx)
}
func (s *Server) Close() {
	s.mu.Lock()
	stop := s.stop
	s.mu.Unlock()
	if stop != nil {
		stop()
		<-s.monitorDone
	}
	s.flushUsage()
	s.engine.Close()
	if s.httpClient != nil {
		s.httpClient.Close()
	}
}
func bearer(r *http.Request) string {
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(value, "Bearer ")
}
func (s *Server) apiAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		given := apiRequestKey(r)
		s.mu.Lock()
		valid := false
		for _, key := range s.cfg.APIKeys {
			if keyMatches(given, key) {
				valid = true
			}
		}
		s.mu.Unlock()
		if !s.admitCredential(w, r, "api", given, valid, func() { apiError(w, 401, "invalid_api_key", "invalid API key") }) {
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (s *Server) managementAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		key := s.cfg.ManagementKey
		s.mu.Unlock()
		given := bearer(r)
		if !s.admitCredential(w, r, "management", given, keyMatches(given, key), func() { apiError(w, 401, "unauthorized", "management authentication required") }) {
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func apiError(w http.ResponseWriter, status int, code, message string) {
	respond(w, status, map[string]any{"type": "error", "error": map[string]string{"type": code, "code": code, "message": message}})
}
func readJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxManagementBody)
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	if dec.Decode(out) != nil {
		apiError(w, 400, "invalid_request", "invalid JSON request")
		return false
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		apiError(w, 400, "invalid_request", "only one JSON value is allowed")
		return false
	}
	return true
}
func statusOf(err error) int {
	var status interface{ StatusCode() int }
	if errors.As(err, &status) {
		return status.StatusCode()
	}
	return 502
}
func (s *Server) candidates(session string) []*account {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*account
	now := time.Now()
	for _, a := range s.accounts {
		a.mu.Lock()
		available := !a.deleted && !a.disabled && !a.guard.Blocked && !now.Before(a.cooldown)
		a.mu.Unlock()
		if available {
			out = append(out, a)
		}
	}
	s.sequence++
	offset := s.sequence
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	if s.cfg.Routing == "round-robin" && len(out) > 0 {
		n := int(offset % uint64(len(out)))
		out = append(out[n:], out[:n]...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		out[i].mu.Lock()
		pi := out[i].priority
		out[i].mu.Unlock()
		out[j].mu.Lock()
		pj := out[j].priority
		out[j].mu.Unlock()
		return pi > pj
	})
	if s.cfg.SessionAffinity && session != "" {
		matched := false
		if bound, ok := s.affinity[session]; ok && bound.expires.After(now) {
			for i, a := range out {
				if a.id == bound.id {
					copy(out[1:i+1], out[:i])
					out[0] = a
					matched = true
					break
				}
			}
		}
		if !matched && len(out) > 0 {
			// Reserve under the selection lock so concurrent first requests in one
			// conversation do not fan out across accounts. Admission/model checks
			// still run below; a successful fallback rebinds the conversation.
			s.bindSessionLocked(session, out[0].id, now)
		}
	}
	return out
}
func (s *Server) inference(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		s.publicModels(w)
		return
	}
	format := ""
	switch r.URL.Path {
	case "/v1/chat/completions":
		format = "openai"
	case "/v1/responses":
		format = "openai-response"
	case "/v1/messages":
		format = "claude"
	case "/v1/responses/compact", "/v1/messages/count_tokens":
		apiError(w, 501, "not_supported", "endpoint not supported by Copilot")
		return
	}
	if r.Method != http.MethodPost || format == "" {
		apiError(w, 404, "not_found", "endpoint not found")
		return
	}
	if r.ContentLength > maxInferenceBody {
		apiError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 16 MiB")
		return
	}
	weight := r.ContentLength
	if weight < 0 {
		weight = maxInferenceBody
	}
	admission, cancel := context.WithTimeout(r.Context(), admissionWait)
	lease, admitted := s.gate.acquire(admission, max(weight, minInferenceWeight))
	cancel()
	if !admitted {
		if r.Context().Err() == nil {
			w.Header().Set("Retry-After", "5")
			apiError(w, 503, "server_busy", "too many large requests are in progress; retry later")
		}
		return
	}
	defer lease.release()
	// Inference may run for minutes before its first byte; writes set their own deadline.
	downstream := http.NewResponseController(w)
	_ = downstream.SetWriteDeadline(time.Time{})
	write := func(payload []byte) error {
		_ = downstream.SetWriteDeadline(time.Now().Add(downstreamWriteTimeout))
		_, err := w.Write(payload)
		return err
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxInferenceBody))
	_ = r.Body.Close()
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		apiError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 16 MiB")
		return
	}
	lease.resize(max(int64(len(body)), minInferenceWeight))
	if err != nil || !json.Valid(body) {
		apiError(w, 400, "invalid_request", "invalid request body")
		return
	}
	model := gjson.GetBytes(body, "model").String()
	if model == "" || len(model) > 200 {
		apiError(w, 400, "invalid_request", "model is required")
		return
	}
	s.mu.Lock()
	native := s.cfg.ModelAliases[model]
	excluded := append([]string(nil), s.cfg.ExcludedModels...)
	s.mu.Unlock()
	if native != "" {
		model = native
		body, _ = sjson.SetBytes(body, "model", model)
	}
	for _, pattern := range excluded {
		if matched, _ := filepath.Match(pattern, model); matched {
			apiError(w, 404, "model_not_found", "model is excluded")
			return
		}
	}
	session := requestSessionKey(r, body, format)
	started := time.Now()
	stream := gjson.GetBytes(body, "stream").Bool()
	var lastErr error
	for _, a := range s.candidates(session) {
		if r.Context().Err() != nil {
			return
		}
		_ = s.checkQuota(r.Context(), a)
		if r.Context().Err() != nil {
			return
		}
		a.mu.Lock()
		blocked := a.guard.Blocked || a.deleted || a.disabled
		a.mu.Unlock()
		if blocked {
			continue
		}
		if err = s.refresh(r.Context(), a, false); err != nil {
			lastErr = err
			continue
		}
		_ = s.catalog(r.Context(), a, s.modelTTL == 0)
		if r.Context().Err() != nil {
			return
		}
		a.mu.Lock()
		auth := a.auth
		ids := modelIDs(a.raw)
		blocked = a.guard.Blocked || a.deleted || a.disabled
		a.mu.Unlock()
		if blocked {
			continue
		}
		supported := false
		for _, id := range ids {
			if id == model {
				supported = true
				break
			}
		}
		if !supported {
			continue
		}
		ctx := s.upstreamContext(r.Context(), a)
		if !stream {
			response, executeErr := s.engine.Execute(ctx, auth, model, format, body, r.Header)
			if executeErr != nil {
				lastErr = executeErr
				if s.retryAccount(a, executeErr) {
					continue
				}
				s.recordUsage(auth.ID, model, statusOf(executeErr), started, 0, 0)
				apiError(w, statusOf(executeErr), "copilot_error", executeErr.Error())
				return
			}
			input, output := usageTokens(response.Payload)
			s.recordUsage(auth.ID, model, 200, started, input, output)
			s.bindSession(session, auth.ID)
			// The Copilot engine has already restricted these to its response-header
			// allowlist. Keep correlation IDs on JSON responses as well as SSE.
			for key, values := range response.Headers {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = write(response.Payload)
			return
		}
		headers, ch, cleanup, executeErr := s.engine.Stream(ctx, auth, model, format, body, r.Header)
		if executeErr != nil {
			lastErr = executeErr
			if s.retryAccount(a, executeErr) {
				continue
			}
			apiError(w, statusOf(executeErr), "copilot_error", executeErr.Error())
			return
		}
		for key, values := range headers {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(200)
		s.bindSession(session, auth.ID)
		var input, output int64
		sentDone := false
		status := 200
		for chunk := range ch {
			if chunk.Err != nil {
				status = 502
				_ = write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"upstream_error\",\"message\":\"Copilot stream interrupted\"}}\n\n"))
				break
			}
			i, o := usageTokens(chunk.Payload)
			input = max(input, i)
			output = max(output, o)
			payload := chunk.Payload
			trimmed := strings.TrimSpace(string(payload))
			if format == "openai" {
				if trimmed == "[DONE]" || trimmed == "data: [DONE]" {
					payload = []byte("data: [DONE]\n\n")
					sentDone = true
				} else if json.Valid(payload) {
					payload = append(append([]byte("data: "), payload...), []byte("\n\n")...)
				}
			}
			// A client that stops reading must not pin the upstream stream and admission slot.
			if err = write(payload); err != nil {
				status = 499
				break
			}
			if err = downstream.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
				status = 499
				break
			}
		}
		if format == "openai" && status == 200 && !sentDone {
			_ = write([]byte("data: [DONE]\n\n"))
		}
		cleanup()
		s.recordUsage(auth.ID, model, status, started, input, output)
		return
	}
	code := 503
	message := "no usable Copilot account for this model"
	if lastErr != nil {
		code = statusOf(lastErr)
		message = "Copilot upstream is unavailable"
	}
	s.recordUsage("", model, code, started, 0, 0)
	apiError(w, code, "copilot_unavailable", message)
}
func (s *Server) retryAccount(a *account, err error) bool {
	status := statusOf(err)
	if status == 401 || status == 403 || status == 429 || status >= 500 {
		a.mu.Lock()
		if status == 401 || status == 403 {
			a.timeRefresh = time.Time{}
			a.cooldown = time.Now().Add(time.Minute)
			a.cooldownKind = "auth"
		}
		if status == 429 {
			a.cooldown = time.Now().Add(time.Minute)
			a.cooldownKind = "rate"
		}
		a.mu.Unlock()
		return true
	}
	return false
}
func (s *Server) publicModels(w http.ResponseWriter) {
	seen := map[string]bool{}
	var models []map[string]any
	s.mu.Lock()
	aliases := map[string]string{}
	for a, n := range s.cfg.ModelAliases {
		aliases[a] = n
	}
	excluded := append([]string(nil), s.cfg.ExcludedModels...)
	s.mu.Unlock()
	for _, a := range s.listAccounts() {
		if a.Disabled {
			continue
		}
		for _, id := range a.Models {
			skip := false
			for _, p := range excluded {
				if match, _ := filepath.Match(p, id); match {
					skip = true
				}
			}
			if !skip && !seen[id] {
				seen[id] = true
				models = append(models, map[string]any{"id": id, "object": "model", "owned_by": "github-copilot"})
			}
		}
	}
	for alias, native := range aliases {
		if seen[native] && !seen[alias] {
			models = append(models, map[string]any{"id": alias, "object": "model", "owned_by": "github-copilot"})
		}
	}
	sort.Slice(models, func(i, j int) bool { return models[i]["id"].(string) < models[j]["id"].(string) })
	if models == nil {
		models = []map[string]any{}
	}
	respond(w, 200, map[string]any{"object": "list", "data": models})
}
func usageTokens(body []byte) (int64, int64) {
	var input, output int64
	records := []string{string(body)}
	if !json.Valid(body) {
		records = strings.Split(string(body), "\n")
	}
	for _, line := range records {
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if !json.Valid([]byte(line)) {
			continue
		}
		for _, prefix := range []string{"usage.", "response.usage.", "message.usage."} {
			for _, key := range []string{"input_tokens", "prompt_tokens"} {
				input = max(input, gjson.Get(line, prefix+key).Int())
			}
			for _, key := range []string{"output_tokens", "completion_tokens"} {
				output = max(output, gjson.Get(line, prefix+key).Int())
			}
		}
	}
	return input, output
}
func (s *Server) recordLog(log Log) {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	s.logs = append(s.logs, log)
	if len(s.logs) > 500 {
		s.logs = s.logs[len(s.logs)-500:]
	}
}
func (s *Server) recordUsage(id, model string, status int, start time.Time, input, output int64) {
	s.usageMu.Lock()
	s.usage.Requests++
	if status >= 400 {
		s.usage.Errors++
	}
	s.usage.Input += input
	s.usage.Output += output
	s.usageDirty = true
	s.usageMu.Unlock()
	s.recordLog(Log{Time: time.Now().UTC(), Level: "info", Event: "request.completed", Account: id, Model: model, Status: status, Duration: time.Since(start).Milliseconds(), Input: input, Output: output})
}

// flushUsage persists counters from the monitor tick and on Close, so request
// completion never waits on an fsync while holding the log lock.
func (s *Server) flushUsage() {
	s.usageMu.Lock()
	if !s.usageDirty {
		s.usageMu.Unlock()
		return
	}
	raw, _ := json.Marshal(s.usage)
	s.usageDirty = false
	s.usageMu.Unlock()
	if atomicWrite(filepath.Join(s.dataDir, "usage.json"), raw) != nil {
		s.usageMu.Lock()
		s.usageDirty = true
		s.usageMu.Unlock()
	}
}
func (s *Server) saveConfigLocked() error {
	raw, err := yaml.Marshal(s.cfg)
	if err != nil {
		return err
	}
	return atomicWrite(s.configPath, raw)
}
