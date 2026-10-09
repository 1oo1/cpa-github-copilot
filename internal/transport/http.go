// Package transport owns all outbound HTTP and proxy policy.
package transport

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"

	"github.com/1oo1/cpa-github-copilot/internal/contract"
)

type proxyKey struct{}

func WithProxy(ctx context.Context, proxy string) context.Context {
	return context.WithValue(ctx, proxyKey{}, proxy)
}

type Client struct {
	mu            sync.Mutex
	defaultProxy  string
	defaultClient *http.Client
	defaultErr    error
	clients       map[string]*http.Client
}

func New(proxy string) *Client {
	client, err := newHTTPClient(proxy)
	return &Client{defaultProxy: proxy, defaultClient: client, defaultErr: err, clients: map[string]*http.Client{}}
}

func newHTTPClient(proxy string) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if proxy != "" {
		p, err := url.Parse(proxy)
		if err != nil || p.Host == "" || (p.Scheme != "http" && p.Scheme != "https" && p.Scheme != "socks5" && p.Scheme != "socks5h") {
			return nil, fmt.Errorf("invalid upstream proxy")
		}
		tr.Proxy = http.ProxyURL(p)
	}
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

func (c *Client) clientFor(proxy string) (*http.Client, error) {
	if proxy == c.defaultProxy {
		if c.defaultErr != nil {
			return nil, c.defaultErr
		}
		return c.defaultClient, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	client := c.clients[proxy]
	if client != nil {
		return client, nil
	}
	client, err := newHTTPClient(proxy)
	if err != nil {
		return nil, err
	}
	c.clients[proxy] = client
	return client, nil
}

func (c *Client) Do(ctx context.Context, request contract.HTTPRequest) (*http.Response, error) {
	u, err := url.Parse(request.URL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("invalid upstream origin")
	}
	proxy, specified := ctx.Value(proxyKey{}).(string)
	if !specified {
		proxy = c.defaultProxy
	}
	client, err := c.clientFor(proxy)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, request.Method, request.URL, bytes.NewReader(request.Body))
	if err != nil {
		return nil, fmt.Errorf("invalid upstream request")
	}
	req.Header = request.Headers.Clone()
	return client.Do(req)
}
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.defaultClient != nil {
		c.defaultClient.CloseIdleConnections()
	}
	for _, client := range c.clients {
		client.CloseIdleConnections()
	}
}
