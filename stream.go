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
	go s.forwardCopilotStream(client, streamID, upstream.StreamID, prepared)
	headers := cloneResponseHeaders(http.Header(upstream.Headers), "text/event-stream")
	headers.Set("Content-Type", "text/event-stream")
	return okEnvelope(rpcExecutorStreamResponse{Headers: headers})
}

func (s *pluginService) forwardCopilotStream(client hostClient, pluginStreamID, upstreamStreamID string, prepared preparedInference) {
	var forwardErr error
	defer func() {
		panicked := recover() != nil
		if panicked {
			forwardErr = newStreamForwardError(streamReasonPanic, "GitHub Copilot stream forwarding failed", false)
		}
		reason, errorMessage, benign := classifyStreamForwardError(forwardErr)
		fields := preparedInferenceLogFields(prepared, true)
		fields["success"] = forwardErr == nil
		fields["panicked"] = panicked
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
		forwardErr = forwardStreamPassThrough(client, pluginStreamID, upstreamStreamID, maxBuffer)
		return
	}
	forwardErr = forwardTranslatedStream(client, pluginStreamID, upstreamStreamID, prepared, maxBuffer)
}

// streamForwardError classifies why stream forwarding stopped so the deferred
// logger in forwardCopilotStream can choose an appropriate level. Only a benign
// downstream close is expected during normal operation and must not be logged as
// a warning: when the client goes away the host rejects the next emit. Host read
// failures are deliberately NOT treated as benign. The host dispatches plugin
// stream reads on a background context, so a client disconnect surfaces either as
// a normal end-of-stream (the upstream channel closes) or as a rejected emit,
// never as a read error. A read error therefore signals a real host/ABI failure
// (teardown, a closed-stream race, or an undecodable response) and must warn.
type streamForwardError struct {
	reason  string
	message string
	benign  bool
}

const (
	streamReasonDownstreamClosed = "downstream_closed"
	streamReasonReadFailed       = "read_failed"
	streamReasonUpstreamError    = "upstream_error"
	streamReasonBufferExceeded   = "buffer_exceeded"
	streamReasonPanic            = "panic"
)

func newStreamForwardError(reason, message string, benign bool) *streamForwardError {
	return &streamForwardError{reason: reason, message: message, benign: benign}
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

func forwardStreamPassThrough(client hostClient, pluginStreamID, upstreamStreamID string, maxBuffer int) error {
	framer := newSSEFramer(maxBuffer)
	emitFrame := func(frame []byte) error {
		if len(frame) == 0 {
			return nil
		}
		payload := make([]byte, 0, len(frame)+2)
		payload = append(payload, frame...)
		payload = append(payload, '\n', '\n')
		if errEmit := client.emit(pluginStreamID, payload); errEmit != nil {
			return newStreamForwardError(streamReasonDownstreamClosed, "GitHub Copilot downstream stream closed", true)
		}
		return nil
	}
	for {
		chunk, errRead := client.readStream(upstreamStreamID)
		if errRead != nil {
			return newStreamForwardError(streamReasonReadFailed, "GitHub Copilot upstream stream read failed", false)
		}
		if chunk.Error != "" {
			return newStreamForwardError(streamReasonUpstreamError, "GitHub Copilot upstream stream failed", false)
		}
		frames, errFrame := framer.Push(chunk.Payload)
		if errFrame != nil {
			return errFrame
		}
		for _, frame := range frames {
			if errEmit := emitFrame(frame); errEmit != nil {
				return errEmit
			}
		}
		if chunk.Done {
			if tail := framer.Flush(); len(tail) > 0 {
				if errEmit := emitFrame(tail); errEmit != nil {
					return errEmit
				}
			}
			return nil
		}
	}
}

func forwardTranslatedStream(client hostClient, pluginStreamID, upstreamStreamID string, prepared preparedInference, maxBuffer int) error {
	framer := newSSEFramer(maxBuffer)
	var state any
	original := prepared.request.OriginalRequest
	if len(original) == 0 {
		original = prepared.request.Payload
	}
	emitFrame := func(frame []byte) error {
		normalized := normalizeSSEFrame(frame)
		if len(normalized) == 0 {
			return nil
		}
		outputs := sdktranslator.TranslateStream(
			context.Background(),
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
			if errEmit := client.emit(pluginStreamID, output); errEmit != nil {
				return newStreamForwardError(streamReasonDownstreamClosed, "GitHub Copilot downstream stream closed", true)
			}
		}
		return nil
	}
	for {
		chunk, errRead := client.readStream(upstreamStreamID)
		if errRead != nil {
			return newStreamForwardError(streamReasonReadFailed, "GitHub Copilot upstream stream read failed", false)
		}
		if chunk.Error != "" {
			return newStreamForwardError(streamReasonUpstreamError, "GitHub Copilot upstream stream failed", false)
		}
		frames, errFrame := framer.Push(chunk.Payload)
		if errFrame != nil {
			return errFrame
		}
		for _, frame := range frames {
			if errEmit := emitFrame(frame); errEmit != nil {
				return errEmit
			}
		}
		if chunk.Done {
			if tail := framer.Flush(); len(tail) > 0 {
				if errEmit := emitFrame(tail); errEmit != nil {
					return errEmit
				}
			}
			return nil
		}
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
