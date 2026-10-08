package copilot

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	contract "github.com/1oo1/cpa-github-copilot/internal/contract"
)

type hostRuntime interface {
	do(context.Context, string, contract.HTTPRequest) (contract.HTTPResponse, error)
	openStream(context.Context, string, contract.HTTPRequest) (hostHTTPStreamResponse, error)
	readStream(string) (hostHTTPStreamReadResponse, error)
	closeStream(string)
	emit(string, []byte) error
	closeOutput(string, string)
	log(string, string, string, map[string]any)
}

type hostClient struct {
	ctx        context.Context
	host       hostRuntime
	callbackID string
}

type hostHTTPStreamResponse struct {
	StatusCode int
	Headers    http.Header
	StreamID   string
}

type hostHTTPStreamReadResponse struct {
	Payload []byte
	Error   string
	Done    bool
}

func (s *service) client(ctx context.Context, callbackID string) hostClient {
	return hostClient{ctx: ctx, host: s.host, callbackID: callbackID}
}

func (c hostClient) do(req contract.HTTPRequest) (contract.HTTPResponse, error) {
	if c.host == nil {
		return contract.HTTPResponse{}, fmt.Errorf("upstream transport is unavailable")
	}
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return c.host.do(ctx, c.callbackID, req)
}

func (c hostClient) openStream(req contract.HTTPRequest) (hostHTTPStreamResponse, error) {
	if c.host == nil {
		return hostHTTPStreamResponse{}, fmt.Errorf("upstream transport is unavailable")
	}
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	resp, err := c.host.openStream(ctx, c.callbackID, req)
	if err != nil {
		return hostHTTPStreamResponse{}, fmt.Errorf("upstream stream request failed")
	}
	if strings.TrimSpace(resp.StreamID) == "" {
		return hostHTTPStreamResponse{}, fmt.Errorf("upstream stream returned no stream id")
	}
	return resp, nil
}

func (c hostClient) readStream(streamID string) (hostHTTPStreamReadResponse, error) {
	if c.host == nil {
		return hostHTTPStreamReadResponse{}, fmt.Errorf("upstream transport is unavailable")
	}
	return c.host.readStream(streamID)
}

func (c hostClient) closeStream(streamID string) {
	if c.host == nil || strings.TrimSpace(streamID) == "" {
		return
	}
	c.host.closeStream(streamID)
}

func (c hostClient) emit(streamID string, payload []byte) error {
	if c.host == nil {
		return fmt.Errorf("downstream stream is unavailable")
	}
	return c.host.emit(streamID, append([]byte(nil), payload...))
}

func (c hostClient) closeDownstreamStream(streamID, message string) {
	if c.host == nil || strings.TrimSpace(streamID) == "" {
		return
	}
	c.host.closeOutput(streamID, strings.TrimSpace(message))
}
