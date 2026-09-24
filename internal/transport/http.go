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
	mu           sync.Mutex
	defaultProxy string
	clients      map[string]*http.Client
}

func New(proxy string) *Client {
	return &Client{defaultProxy: proxy, clients: map[string]*http.Client{}}
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
	c.mu.Lock()
	client := c.clients[proxy]
	if client == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		if proxy != "" {
			p, e := url.Parse(proxy)
			if e != nil || p.Host == "" || (p.Scheme != "http" && p.Scheme != "https" && p.Scheme != "socks5" && p.Scheme != "socks5h") {
				c.mu.Unlock()
				return nil, fmt.Errorf("invalid upstream proxy")
			}
			tr.Proxy = http.ProxyURL(p)
		}
		client = &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		c.clients[proxy] = client
	}
	c.mu.Unlock()
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
	for _, client := range c.clients {
		client.CloseIdleConnections()
	}
}
