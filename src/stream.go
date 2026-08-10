package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func (s *pluginService) executeStream(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, &pluginFailure{code: "invalid_request", message: "decode executor stream request"}
	}
	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		return nil, &pluginFailure{code: "invalid_stream", message: "stream_id is required for executor.execute_stream"}
	}
	prepared, failure := s.prepareInference(req.ExecutorRequest, true)
	if failure != nil {
		s.logFailure(req.HostCallbackID, "inference.rejected", failure, inferenceRequestLogFields(req.ExecutorRequest, true, s.now().UTC()))
		return nil, failure
	}
	s.logEvent(req.HostCallbackID, "debug", "inference.stream.started", preparedInferenceLogFields(prepared, true))
	client := hostClient{bridge: s.bridge, callbackID: req.HostCallbackID}
	upstream, errOpen := client.openStream(pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     prepared.upstreamURL,
		Headers: prepared.headers,
		Body:    prepared.upstreamPayload,
	})
	if errOpen != nil {
		failure = &pluginFailure{code: "upstream_network_error", message: "GitHub Copilot stream request failed", retryable: true, httpStatus: http.StatusBadGateway}
		s.logFailure(req.HostCallbackID, "inference.stream.failed", failure, preparedInferenceLogFields(prepared, true))
		return nil, failure
	}
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		client.closeStream(upstream.StreamID)
		failure = upstreamFailure("upstream_http_error", "GitHub Copilot stream request failed", upstream.StatusCode)
		s.logFailure(req.HostCallbackID, "inference.stream.failed", failure, preparedInferenceLogFields(prepared, true))
		return nil, failure
	}
	openedFields := preparedInferenceLogFields(prepared, true)
	openedFields["upstream_status"] = upstream.StatusCode
	s.logEvent(req.HostCallbackID, "debug", "inference.stream.opened", openedFields)
	if !s.startStreamForwarding(client, streamID, upstream.StreamID, prepared) {
		client.closeStream(upstream.StreamID)
		failure = &pluginFailure{code: "plugin_shutdown", message: "plugin is shutting down", retryable: true, httpStatus: http.StatusServiceUnavailable}
		s.logFailure(req.HostCallbackID, "inference.stream.failed", failure, preparedInferenceLogFields(prepared, true))
		return nil, failure
	}
	headers := cloneResponseHeaders(http.Header(upstream.Headers), "text/event-stream")
	headers.Set("Content-Type", "text/event-stream")
	return okEnvelope(rpcExecutorStreamResponse{Headers: headers})
}

type streamForwardTask struct {
	client           hostClient
	upstreamStreamID string
	cancel           context.CancelFunc
}

func (s *pluginService) startStreamForwarding(client hostClient, pluginStreamID, upstreamStreamID string, prepared preparedInference) bool {
	ctx, cancel := context.WithCancel(context.Background())
	task := &streamForwardTask{client: client, upstreamStreamID: upstreamStreamID, cancel: cancel}
	s.streamMu.Lock()
	if s.streamsShuttingDown {
		s.streamMu.Unlock()
		cancel()
		return false
	}
	s.streamTasks[task] = struct{}{}
	s.streamWG.Add(1)
	s.streamMu.Unlock()
	go func() {
		defer func() {
			cancel()
			s.streamMu.Lock()
			delete(s.streamTasks, task)
			s.streamMu.Unlock()
			s.streamWG.Done()
		}()
		s.forwardCopilotStream(ctx, client, pluginStreamID, upstreamStreamID, prepared)
	}()
	return true
}

func (s *pluginService) stopStreamForwarding() {
	s.streamMu.Lock()
	s.streamsShuttingDown = true
	tasks := make([]*streamForwardTask, 0, len(s.streamTasks))
	for task := range s.streamTasks {
		tasks = append(tasks, task)
	}
	s.streamMu.Unlock()
	for _, task := range tasks {
		task.cancel()
		task.client.closeStream(task.upstreamStreamID)
	}
	s.streamWG.Wait()
}

func (s *pluginService) forwardCopilotStream(ctx context.Context, client hostClient, pluginStreamID, upstreamStreamID string, prepared preparedInference) {
	var forwardErr error
	tracker := newResponsesTerminalTracker(prepared.outputFormat)
	defer func() {
		panicked := recover() != nil
		if panicked {
			forwardErr = newStreamForwardError(streamReasonPanic, "GitHub Copilot stream forwarding failed", false)
		}
		reason, errorMessage, benign := classifyStreamForwardError(forwardErr)
		fields := preparedInferenceLogFields(prepared, true)
		fields["success"] = forwardErr == nil
		fields["panicked"] = panicked
		if cause := upstreamStreamCause(forwardErr); cause != "" {
			fields["upstream_cause"] = cause
		}
		if status, observed := tracker.terminalStatus(); observed {
			fields["responses_terminal_status"] = status
		}
		switch {
		case forwardErr == nil:
			s.logEvent(client.callbackID, "debug", "inference.stream.completed", fields)
		case benign:
			fields["reason"] = reason
			fields["error"] = errorMessage
			s.logEvent(client.callbackID, "debug", "inference.stream.client_disconnected", fields)
		default:
			fields["reason"] = reason
			fields["error"] = errorMessage
			s.logEvent(client.callbackID, "warn", "inference.stream.forward_failed", fields)
		}
		client.closeStream(upstreamStreamID)
		client.closePluginStream(pluginStreamID, errorMessage)
	}()
	maxBuffer := s.loadedConfig().MaxStreamBytes
	if prepared.translatorFormat == prepared.outputFormat {
		forwardErr = forwardStreamPassThrough(ctx, client, pluginStreamID, upstreamStreamID, prepared.outputFormat, maxBuffer, tracker)
		return
	}
	forwardErr = forwardTranslatedStream(ctx, client, pluginStreamID, upstreamStreamID, prepared, maxBuffer, tracker)
}

// streamForwardError classifies why stream forwarding stopped so the deferred
// logger in forwardCopilotStream can choose an appropriate level. Two benign
// endings are expected during normal operation and must not be logged as
// warnings: a downstream close, where the client goes away and the host rejects
// the next emit, and a cancelled upstream read, where the client goes away while
// the plugin is parked on a host read. Host read failures are deliberately NOT
// treated as benign. The host dispatches plugin stream reads on a background
// context, so a client disconnect surfaces as a normal end-of-stream (the
// upstream channel closes), a rejected emit, or a cancelled upstream chunk,
// never as a read error. A read error therefore signals a real host/ABI failure
// (teardown, a closed-stream race, or an undecodable response) and must warn.
//
// cause carries the redacted upstream category for chunk errors and is empty for
// every other reason.
type streamForwardError struct {
	reason  string
	message string
	cause   string
	benign  bool
}

const (
	streamReasonDownstreamClosed = "downstream_closed"
	streamReasonReadFailed       = "read_failed"
	streamReasonUpstreamError    = "upstream_error"
	streamReasonUpstreamCanceled = "upstream_canceled"
	streamReasonBufferExceeded   = "buffer_exceeded"
	streamReasonPanic            = "panic"
	streamReasonMissingTerminal  = "missing_terminal_event"
	streamReasonPluginShutdown   = "plugin_shutdown"
)

// The host classifies an unexpected EOF as a connection-lifecycle failure, so
// a truncated Responses stream remains an error without cooling the credential.
const responsesMissingTerminalError = "GitHub Copilot Responses stream ended without a terminal event: unexpected EOF"

const (
	upstreamCauseCanceled        = "canceled"
	upstreamCauseRemoteCanceled  = "remote_canceled"
	upstreamCauseTimeout         = "timeout"
	upstreamCauseEOF             = "eof"
	upstreamCauseConnectionReset = "connection_reset"
	upstreamCauseOther           = "other"
)

// responsesTerminalTracker 记录 openai-response 输出协议的流是否出现过终态事件
// （response.completed/incomplete/failed 或顶层 error），以及具体是哪一种，供关闭时的
// 诊断日志使用。outputFormat 不是 openai-response 时 tracker 处于非激活状态，不影响
// 其他协议的流语义。
type responsesTerminalTracker struct {
	active       bool
	seen         bool
	sourceStatus string
	sourceReason string
	status       string
}

func newResponsesTerminalTracker(outputFormat string) *responsesTerminalTracker {
	return &responsesTerminalTracker{active: outputFormat == formatOpenAIResponse}
}

// observe 检查即将发给下游的帧（passthrough 为原始帧，translated 为转换后的帧），
// 只要出现过一次终态事件就记住其类型，不会被后续非终态帧清除。
func (t *responsesTerminalTracker) observe(frame []byte) {
	if t == nil || !t.active || t.seen {
		return
	}
	if status, terminal := responsesFrameTerminalStatus(frame); terminal {
		t.seen = true
		t.status = status
	}
}

// observeSource 只记录源协议自己的终态信号；error 可推翻先前的暂定成功，incomplete
// 也可推翻 completed，避免多 choice 或迟到错误被第一个 finish_reason 掩盖。
func (t *responsesTerminalTracker) observeSource(status, reason string) {
	if t == nil || !t.active || sourceTerminalPriority(status) <= sourceTerminalPriority(t.sourceStatus) {
		return
	}
	t.sourceStatus = status
	t.sourceReason = reason
}

func (t *responsesTerminalTracker) translatedFrame(frame []byte) ([]byte, bool) {
	if t == nil || !t.active {
		return frame, true
	}
	status, terminal := responsesFrameTerminalStatus(frame)
	if !terminal {
		return frame, true
	}
	if status == "response.completed" && t.sourceStatus == "response.incomplete" {
		return rewriteResponsesIncompleteFrame(frame, t.sourceReason)
	}
	return frame, sourceAuthorizesTranslatedTerminal(t.sourceStatus, status)
}

func (t *responsesTerminalTracker) allowsSourceTranslation(frame []byte) bool {
	return t == nil || !t.active || !bytes.Equal(streamFrameData(frame), []byte("[DONE]")) ||
		t.sourceStatus == "response.completed" || t.sourceStatus == "response.incomplete"
}

func (t *responsesTerminalTracker) missingTerminal() bool {
	return t != nil && t.active && !t.seen
}

// terminalStatus 返回用于诊断日志的终态取值：观察到的事件类型，或者 tracker 已激活但
// 还没有出现终态事件时返回 "missing"。tracker 未激活（非 Responses 输出协议）时
// observed 为 false。
func (t *responsesTerminalTracker) terminalStatus() (status string, observed bool) {
	if t == nil || !t.active {
		return "", false
	}
	if t.seen {
		return t.status, true
	}
	return "missing", true
}

var responsesTerminalEventTypes = map[string]bool{
	"response.completed":  true,
	"response.incomplete": true,
	"response.failed":     true,
	"error":               true,
}

func responsesFrameTerminalStatus(frame []byte) (status string, terminal bool) {
	data := streamFrameData(frame)
	if len(data) == 0 {
		return "", false
	}
	var event struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &event) != nil || !responsesTerminalEventTypes[event.Type] {
		return "", false
	}
	return event.Type, true
}

func sourceFrameTerminalStatus(frame []byte, format string) (status, reason string, terminal bool) {
	data := streamFrameData(frame)
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return "", "", false
	}
	if format == formatOpenAIResponse || format == string(sdktranslator.FormatCodex) {
		if status, terminal := responsesFrameTerminalStatus(frame); terminal {
			var event map[string]any
			_ = json.Unmarshal(data, &event)
			return status, responsesIncompleteReason(event), true
		}
		var event map[string]any
		if json.Unmarshal(data, &event) == nil {
			if errorValue, exists := event["error"]; exists && errorValue != nil {
				return "error", "", true
			}
		}
		return "", "", false
	}
	var event map[string]any
	if json.Unmarshal(data, &event) != nil {
		return "", "", false
	}
	switch format {
	case formatOpenAI:
		if errorValue, exists := event["error"]; exists && errorValue != nil {
			return "error", "", true
		}
		bestStatus := ""
		bestReason := ""
		choices, _ := event["choices"].([]any)
		for _, rawChoice := range choices {
			choice, _ := rawChoice.(map[string]any)
			choiceStatus, choiceReason, choiceTerminal := sourceStopReasonStatus(stringValue(choice["finish_reason"]), format)
			if choiceTerminal && sourceTerminalPriority(choiceStatus) > sourceTerminalPriority(bestStatus) {
				bestStatus = choiceStatus
				bestReason = choiceReason
			}
		}
		if bestStatus != "" {
			return bestStatus, bestReason, true
		}
	case formatClaude:
		switch stringValue(event["type"]) {
		case "message_delta":
			delta, _ := event["delta"].(map[string]any)
			return sourceStopReasonStatus(stringValue(delta["stop_reason"]), format)
		case "message_stop":
			return "response.completed", "", true
		case "message":
			return sourceStopReasonStatus(stringValue(event["stop_reason"]), format)
		case "error":
			return "error", "", true
		}
	}
	return "", "", false
}

func sourceNonStreamTerminalStatus(raw []byte, format string) (status, reason string, terminal bool) {
	if status, reason, terminal := sourceFrameTerminalStatus(raw, format); terminal {
		return status, reason, true
	}
	bestStatus := ""
	bestReason := ""
	for _, line := range bytes.Split(bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n")), []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		lineStatus, lineReason, lineTerminal := sourceFrameTerminalStatus(line, format)
		if lineTerminal && sourceTerminalPriority(lineStatus) > sourceTerminalPriority(bestStatus) {
			bestStatus = lineStatus
			bestReason = lineReason
		}
	}
	return bestStatus, bestReason, bestStatus != ""
}

func sourceStopReasonStatus(rawReason, format string) (status, reason string, terminal bool) {
	stopReason := strings.ToLower(strings.TrimSpace(rawReason))
	if stopReason == "" {
		return "", "", false
	}
	switch format {
	case formatOpenAI:
		switch stopReason {
		case "length":
			return "response.incomplete", "max_output_tokens", true
		case "content_filter":
			return "response.incomplete", "content_filter", true
		}
	case formatClaude:
		if stopReason == "max_tokens" || stopReason == "model_context_window_exceeded" {
			return "response.incomplete", "max_output_tokens", true
		}
	}
	return "response.completed", "", true
}

func sourceTerminalPriority(status string) int {
	switch status {
	case "response.failed", "error":
		return 3
	case "response.incomplete":
		return 2
	case "response.completed":
		return 1
	default:
		return 0
	}
}

func responsesIncompleteReason(event map[string]any) string {
	response := event
	if nested, ok := event["response"].(map[string]any); ok {
		response = nested
	}
	details, _ := response["incomplete_details"].(map[string]any)
	return strings.TrimSpace(stringValue(details["reason"]))
}

func rewriteResponsesIncompleteFrame(frame []byte, reason string) ([]byte, bool) {
	var event map[string]any
	if json.Unmarshal(streamFrameData(frame), &event) != nil || event == nil {
		return nil, false
	}
	response, ok := event["response"].(map[string]any)
	if !ok {
		return nil, false
	}
	event["type"] = "response.incomplete"
	response["status"] = "incomplete"
	response["incomplete_details"] = map[string]any{"reason": reason}
	encoded, errMarshal := json.Marshal(event)
	if errMarshal != nil {
		return nil, false
	}
	return append([]byte("event: response.incomplete\ndata: "), encoded...), true
}

func sourceAuthorizesTranslatedTerminal(sourceStatus, translatedStatus string) bool {
	switch translatedStatus {
	case "response.completed":
		return sourceStatus == "response.completed"
	case "response.incomplete":
		return sourceStatus == "response.incomplete"
	case "response.failed", "error":
		return sourceStatus == "response.failed" || sourceStatus == "error"
	default:
		return false
	}
}

func streamFrameData(frame []byte) []byte {
	if data := sseFrameData(frame); len(data) > 0 {
		return data
	}
	return bytes.TrimSpace(frame)
}

// sseFrameData 提取一个 SSE frame 中所有 data: 行并按行拼接，兼容 passthrough 的裸
// "data: {...}" 帧和 translated 路径的 "event: X\ndata: {...}" 帧。
func sseFrameData(frame []byte) []byte {
	lines := bytes.Split(bytes.ReplaceAll(frame, []byte("\r\n"), []byte("\n")), []byte("\n"))
	data := make([][]byte, 0, len(lines))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			data = append(data, bytes.TrimSpace(line[len("data:"):]))
		}
	}
	return bytes.Join(data, []byte("\n"))
}

func newStreamForwardError(reason, message string, benign bool) *streamForwardError {
	return &streamForwardError{reason: reason, message: message, benign: benign}
}

// newUpstreamStreamError converts a host stream chunk error into a forward
// error. Only the host's recognized local cancellation strings map to
// upstreamCauseCanceled; remote or ambiguous cancellation text remains an
// upstream failure. A confirmed local cancellation is benign, matching how a
// rejected downstream emit is treated.
func newUpstreamStreamError(rawError string) *streamForwardError {
	cause := classifyUpstreamStreamError(rawError)
	if cause == upstreamCauseCanceled {
		return &streamForwardError{
			reason:  streamReasonUpstreamCanceled,
			message: "GitHub Copilot upstream stream canceled",
			cause:   cause,
			benign:  true,
		}
	}
	return &streamForwardError{
		reason:  streamReasonUpstreamError,
		message: "GitHub Copilot upstream stream failed",
		cause:   cause,
	}
}

// classifyUpstreamStreamError maps a host stream error onto a fixed set of
// categories so operators can tell a client abort apart from a real upstream
// drop. The raw error may embed upstream response text, so it is inspected only
// here: callers receive one of the upstreamCause constants and never the
// original string.
func classifyUpstreamStreamError(rawError string) string {
	lower := strings.ToLower(strings.TrimSpace(rawError))
	switch {
	case lower == "":
		return upstreamCauseOther
	// Checked before cancellation because net/http reports a client timeout as
	// "request canceled (Client.Timeout exceeded while reading body)".
	case containsAny(lower, "deadline exceeded", "timed out", "timeout"):
		return upstreamCauseTimeout
	// HTTP/2 RST_STREAM CANCEL is sent by the upstream peer and is not evidence
	// that our caller disconnected. Keep it separate from local context
	// cancellation so it remains an operator-visible upstream failure.
	case strings.Contains(lower, "stream error:") && strings.Contains(lower, "cancel"):
		return upstreamCauseRemoteCanceled
	case lower == context.Canceled.Error() || lower == "net/http: request canceled":
		return upstreamCauseCanceled
	case strings.Contains(lower, "cancel"):
		return upstreamCauseRemoteCanceled
	case containsAny(lower, "connection reset", "broken pipe", "use of closed network connection"):
		return upstreamCauseConnectionReset
	case containsAny(lower, "eof"):
		return upstreamCauseEOF
	default:
		return upstreamCauseOther
	}
}

func containsAny(value string, fragments ...string) bool {
	for _, fragment := range fragments {
		if strings.Contains(value, fragment) {
			return true
		}
	}
	return false
}

// upstreamStreamCause returns the redacted upstream category behind err, or an
// empty string when err did not come from an upstream chunk error.
func upstreamStreamCause(err error) string {
	var forwardErr *streamForwardError
	if err == nil || !errors.As(err, &forwardErr) {
		return ""
	}
	return forwardErr.cause
}

func (e *streamForwardError) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

// classifyStreamForwardError extracts the log reason, downstream error message,
// and benign flag from a forwarding error. Errors that are not *streamForwardError
// are treated as non-benign failures with an "unknown" reason.
func classifyStreamForwardError(err error) (reason, message string, benign bool) {
	if err == nil {
		return "", "", false
	}
	var forwardErr *streamForwardError
	if errors.As(err, &forwardErr) {
		reason = forwardErr.reason
		if reason == "" {
			reason = "unknown"
		}
		return reason, forwardErr.Error(), forwardErr.benign
	}
	return "unknown", err.Error(), false
}

func forwardStreamPassThrough(ctx context.Context, client hostClient, pluginStreamID, upstreamStreamID, outputFormat string, maxBuffer int, tracker *responsesTerminalTracker) error {
	framer := newSSEFramer(maxBuffer)
	emitFrame := func(frame []byte) (bool, error) {
		if len(frame) == 0 {
			return false, nil
		}
		terminal := sourceProtocolTerminal(frame, outputFormat)
		if outputFormat == formatOpenAI {
			payload := openAIChatFrameData(frame)
			if terminal {
				return true, nil
			}
			if len(payload) == 0 {
				return false, nil
			}
			if errEmit := client.emit(pluginStreamID, payload); errEmit != nil {
				return false, newStreamForwardError(streamReasonDownstreamClosed, "GitHub Copilot downstream stream closed", true)
			}
			return false, nil
		}
		tracker.observe(frame)
		payload := make([]byte, 0, len(frame)+2)
		payload = append(payload, frame...)
		payload = append(payload, '\n', '\n')
		if errEmit := client.emit(pluginStreamID, payload); errEmit != nil {
			return false, newStreamForwardError(streamReasonDownstreamClosed, "GitHub Copilot downstream stream closed", true)
		}
		return terminal, nil
	}
	for {
		chunk, errRead := client.readStream(upstreamStreamID)
		if ctx.Err() != nil {
			return newStreamForwardError(streamReasonPluginShutdown, "GitHub Copilot stream forwarding stopped during plugin shutdown", true)
		}
		if errRead != nil {
			return newStreamForwardError(streamReasonReadFailed, "GitHub Copilot upstream stream read failed", false)
		}
		if chunk.Error != "" {
			return newUpstreamStreamError(chunk.Error)
		}
		frames, errFrame := framer.Push(chunk.Payload)
		if errFrame != nil {
			return errFrame
		}
		for _, frame := range frames {
			terminal, errEmit := emitFrame(frame)
			if errEmit != nil {
				return errEmit
			}
			if terminal {
				return nil
			}
		}
		if chunk.Done {
			if tail := framer.Flush(); len(tail) > 0 {
				terminal, errEmit := emitFrame(tail)
				if errEmit != nil {
					return errEmit
				}
				if terminal {
					return nil
				}
			}
			// Responses 输出协议下，没有真实终态事件就结束是协议错误，绝不能伪造成功。
			if tracker.missingTerminal() {
				return newStreamForwardError(streamReasonMissingTerminal, responsesMissingTerminalError, false)
			}
			return nil
		}
	}
}

func forwardTranslatedStream(ctx context.Context, client hostClient, pluginStreamID, upstreamStreamID string, prepared preparedInference, maxBuffer int, tracker *responsesTerminalTracker) error {
	framer := newSSEFramer(maxBuffer)
	var state any
	requireResponsesSourceTerminal := prepared.upstreamFormat == formatOpenAIResponse
	responsesSourceStatus := ""
	original := prepared.request.OriginalRequest
	if len(original) == 0 {
		original = prepared.request.Payload
	}
	emitFrame := func(frame []byte) (bool, error) {
		normalized := normalizeSSEFrame(frame)
		if len(normalized) == 0 {
			return false, nil
		}
		protocolTerminal := sourceProtocolTerminal(normalized, prepared.translatorFormat)
		sourceStatus, sourceReason, sourceTerminal := sourceFrameTerminalStatus(normalized, prepared.translatorFormat)
		if sourceTerminal {
			tracker.observeSource(sourceStatus, sourceReason)
			if sourceStatus == "response.failed" || sourceStatus == "error" {
				return false, newStreamForwardError(streamReasonUpstreamError, "GitHub Copilot upstream stream failed", false)
			}
			if requireResponsesSourceTerminal {
				responsesSourceStatus = sourceStatus
			}
		}
		if !tracker.allowsSourceTranslation(normalized) {
			return protocolTerminal, nil
		}
		outputs := sdktranslator.TranslateStream(
			ctx,
			sdktranslator.Format(prepared.translatorFormat),
			sdktranslator.Format(prepared.outputFormat),
			prepared.model,
			original,
			prepared.upstreamPayload,
			normalized,
			&state,
		)
		for _, output := range outputs {
			if len(output) == 0 {
				continue
			}
			output, allowed := tracker.translatedFrame(output)
			if !allowed {
				continue
			}
			tracker.observe(output)
			if errEmit := client.emit(pluginStreamID, output); errEmit != nil {
				return false, newStreamForwardError(streamReasonDownstreamClosed, "GitHub Copilot downstream stream closed", true)
			}
		}
		return protocolTerminal, nil
	}
	validateTermination := func() error {
		if requireResponsesSourceTerminal && responsesSourceStatus == "" {
			return newStreamForwardError(streamReasonMissingTerminal, responsesMissingTerminalError, false)
		}
		if tracker.missingTerminal() {
			return newStreamForwardError(streamReasonMissingTerminal, responsesMissingTerminalError, false)
		}
		return nil
	}
	for {
		chunk, errRead := client.readStream(upstreamStreamID)
		if ctx.Err() != nil {
			return newStreamForwardError(streamReasonPluginShutdown, "GitHub Copilot stream forwarding stopped during plugin shutdown", true)
		}
		if errRead != nil {
			return newStreamForwardError(streamReasonReadFailed, "GitHub Copilot upstream stream read failed", false)
		}
		if chunk.Error != "" {
			return newUpstreamStreamError(chunk.Error)
		}
		frames, errFrame := framer.Push(chunk.Payload)
		if errFrame != nil {
			return errFrame
		}
		for _, frame := range frames {
			terminal, errEmit := emitFrame(frame)
			if errEmit != nil {
				return errEmit
			}
			if terminal {
				return validateTermination()
			}
		}
		if chunk.Done {
			if tail := framer.Flush(); len(tail) > 0 {
				terminal, errEmit := emitFrame(tail)
				if errEmit != nil {
					return errEmit
				}
				if terminal {
					return validateTermination()
				}
			}
			return validateTermination()
		}
	}
}

func openAIChatFrameData(frame []byte) []byte {
	if data := sseFrameData(frame); len(data) > 0 {
		return data
	}
	frame = bytes.TrimSpace(frame)
	if json.Valid(frame) || bytes.Equal(frame, []byte("[DONE]")) {
		return frame
	}
	return nil
}

func sourceProtocolTerminal(frame []byte, format string) bool {
	data := streamFrameData(frame)
	if len(data) == 0 {
		return false
	}
	switch format {
	case formatOpenAI:
		return bytes.Equal(data, []byte("[DONE]"))
	case formatOpenAIResponse, string(sdktranslator.FormatCodex):
		_, terminal := responsesFrameTerminalStatus(frame)
		return terminal
	case formatClaude:
		var event struct {
			Type string `json:"type"`
		}
		return json.Unmarshal(data, &event) == nil && event.Type == "message_stop"
	default:
		return false
	}
}

type sseFramer struct {
	buffer []byte
	max    int
}

func newSSEFramer(max int) *sseFramer {
	if max <= 0 {
		max = 4 << 20
	}
	return &sseFramer{max: max}
}

func (f *sseFramer) Push(chunk []byte) ([][]byte, error) {
	if len(chunk) > 0 {
		f.buffer = append(f.buffer, chunk...)
	}
	var frames [][]byte
	for {
		index, separatorLength := nextSSESeparator(f.buffer)
		lineEnd, lineLength := nextCompleteSSEDataLine(f.buffer)
		// Host reads can end after a complete JSON data value, before the blank
		// line required by strict SSE; some compatible endpoints also use only a
		// single newline. Treat the complete value as an event boundary so the
		// bridge never blocks waiting for delimiter bytes after a terminal event.
		// Prefer a real SSE separator when it ends the same candidate frame so
		// standards-compliant multi-line data events retain their original shape.
		if lineEnd >= 0 && (index < 0 || lineEnd < index) {
			frame := bytes.TrimSpace(f.buffer[:lineEnd])
			if len(frame) > 0 {
				frames = append(frames, append([]byte(nil), frame...))
			}
			f.buffer = append(f.buffer[:0], f.buffer[lineEnd+lineLength:]...)
			continue
		}
		if index < 0 {
			break
		}
		frame := bytes.TrimSpace(f.buffer[:index])
		if len(frame) > 0 {
			frames = append(frames, append([]byte(nil), frame...))
		}
		f.buffer = append(f.buffer[:0], f.buffer[index+separatorLength:]...)
	}
	if len(f.buffer) > f.max {
		return nil, newStreamForwardError(streamReasonBufferExceeded, "GitHub Copilot stream event exceeded the configured buffer", false)
	}
	return frames, nil
}

// nextCompleteSSEDataLine returns the first data line whose payload is a
// complete JSON value (or [DONE]), even when the trailing newline has not yet
// arrived. It is deliberately conservative: once an event contains more than
// one data line, strict SSE framing owns it so split multi-line JSON is never
// emitted prematurely.
func nextCompleteSSEDataLine(raw []byte) (int, int) {
	dataLines := 0
	lineStart := 0
	for lineStart < len(raw) {
		relativeEnd := bytes.IndexByte(raw[lineStart:], '\n')
		lineEnd := len(raw)
		lineLength := 0
		if relativeEnd >= 0 {
			lineEnd = lineStart + relativeEnd
			lineLength = 1
		}
		line := bytes.TrimSpace(raw[lineStart:lineEnd])
		if bytes.HasPrefix(line, []byte("data:")) {
			dataLines++
			if dataLines > 1 {
				return -1, 0
			}
			payload := bytes.TrimSpace(line[len("data:"):])
			if isCompleteStreamData(payload) {
				return lineEnd, lineLength
			}
		}
		if lineLength == 0 {
			return -1, 0
		}
		lineStart = lineEnd + 1
	}
	return -1, 0
}

func isCompleteStreamData(payload []byte) bool {
	payload = bytes.TrimSpace(payload)
	return bytes.Equal(payload, []byte("[DONE]")) ||
		(len(payload) > 0 && (payload[0] == '{' || payload[0] == '[') && json.Valid(payload))
}

func (f *sseFramer) Flush() []byte {
	frame := bytes.TrimSpace(f.buffer)
	f.buffer = nil
	return append([]byte(nil), frame...)
}

func nextSSESeparator(raw []byte) (int, int) {
	lf := bytes.Index(raw, []byte("\n\n"))
	crlf := bytes.Index(raw, []byte("\r\n\r\n"))
	switch {
	case lf < 0:
		if crlf < 0 {
			return -1, 0
		}
		return crlf, 4
	case crlf < 0 || lf < crlf:
		return lf, 2
	default:
		return crlf, 4
	}
}

func normalizeSSEFrame(frame []byte) []byte {
	frame = bytes.TrimSpace(frame)
	if len(frame) == 0 {
		return nil
	}
	lines := bytes.Split(bytes.ReplaceAll(frame, []byte("\r\n"), []byte("\n")), []byte("\n"))
	data := make([][]byte, 0, len(lines))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			data = append(data, bytes.TrimSpace(line[len("data:"):]))
		}
	}
	if len(data) > 0 {
		return append([]byte("data: "), bytes.Join(data, []byte("\n"))...)
	}
	if json.Valid(frame) || bytes.Equal(frame, []byte("[DONE]")) {
		return append([]byte(nil), frame...)
	}
	return nil
}
