package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
		return nil, failure
	}
	client := hostClient{bridge: s.bridge, callbackID: req.HostCallbackID}
	upstream, errOpen := client.openStream(pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     prepared.upstreamURL,
		Headers: prepared.headers,
		Body:    prepared.upstreamPayload,
	})
	if errOpen != nil {
		return nil, &pluginFailure{code: "upstream_network_error", message: "GitHub Copilot stream request failed", retryable: true, httpStatus: http.StatusBadGateway}
	}
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		client.closeStream(upstream.StreamID)
		return nil, upstreamFailure("upstream_http_error", "GitHub Copilot stream request failed", upstream.StatusCode)
	}
	go s.forwardCopilotStream(client, streamID, upstream.StreamID, prepared)
	headers := cloneResponseHeaders(http.Header(upstream.Headers), "text/event-stream")
	headers.Set("Content-Type", "text/event-stream")
	return okEnvelope(rpcExecutorStreamResponse{Headers: headers})
}

func (s *pluginService) forwardCopilotStream(client hostClient, pluginStreamID, upstreamStreamID string, prepared preparedInference) {
	errorMessage := ""
	defer func() {
		if recover() != nil {
			errorMessage = "GitHub Copilot stream forwarding failed"
		}
		client.closeStream(upstreamStreamID)
		client.closePluginStream(pluginStreamID, errorMessage)
	}()
	if prepared.translatorFormat == prepared.outputFormat {
		if errForward := forwardStreamPassThrough(client, pluginStreamID, upstreamStreamID); errForward != nil {
			errorMessage = errForward.Error()
		}
		return
	}
	maxBuffer := s.loadedConfig().MaxStreamBytes
	if errForward := forwardTranslatedStream(client, pluginStreamID, upstreamStreamID, prepared, maxBuffer); errForward != nil {
		errorMessage = errForward.Error()
	}
}

func forwardStreamPassThrough(client hostClient, pluginStreamID, upstreamStreamID string) error {
	for {
		chunk, errRead := client.readStream(upstreamStreamID)
		if errRead != nil {
			return fmt.Errorf("GitHub Copilot upstream stream read failed")
		}
		if chunk.Error != "" {
			return fmt.Errorf("GitHub Copilot upstream stream failed")
		}
		if len(chunk.Payload) > 0 {
			if errEmit := client.emit(pluginStreamID, chunk.Payload); errEmit != nil {
				return fmt.Errorf("GitHub Copilot downstream stream closed")
			}
		}
		if chunk.Done {
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
				return fmt.Errorf("GitHub Copilot downstream stream closed")
			}
		}
		return nil
	}
	for {
		chunk, errRead := client.readStream(upstreamStreamID)
		if errRead != nil {
			return fmt.Errorf("GitHub Copilot upstream stream read failed")
		}
		if chunk.Error != "" {
			return fmt.Errorf("GitHub Copilot upstream stream failed")
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
		return nil, fmt.Errorf("GitHub Copilot stream event exceeded the configured buffer")
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
