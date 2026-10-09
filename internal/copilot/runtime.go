package copilot

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"

	contract "github.com/1oo1/cpa-github-copilot/internal/contract"
)

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
