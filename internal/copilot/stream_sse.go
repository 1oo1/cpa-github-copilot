package copilot

import (
	"bytes"
	"encoding/json"
)

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
