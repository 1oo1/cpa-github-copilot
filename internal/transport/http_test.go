package transport

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/1oo1/cpa-github-copilot/internal/contract"
)

func TestTransportRejectsUnsafeURLAndProxy(t *testing.T) {
	client := New("")
	defer client.Close()
	for _, target := range []string{"http://github.com", "https://user:secret@github.com", "file:///tmp/test", "https:///missing"} {
		if _, err := client.Do(context.Background(), contract.HTTPRequest{Method: "GET", URL: target}); err == nil {
			t.Fatalf("accepted %s", target)
		}
	}
	if _, err := client.Do(WithProxy(context.Background(), "ftp://127.0.0.1"), contract.HTTPRequest{Method: "GET", URL: "https://api.github.com"}); err == nil {
		t.Fatal("unsafe proxy accepted")
	}
}
func TestTransportPreservesHeadersAndRejectsRedirects(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Error("missing auth header")
		}
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client := New("")
	defer client.Close()
	httpClient := server.Client()
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.clients[""] = httpClient
	headers := http.Header{"Authorization": {"Bearer synthetic-token"}}
	response, err := client.Do(context.Background(), contract.HTTPRequest{Method: "GET", URL: server.URL, Headers: headers})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 307 || redirected.Load() != 0 {
		t.Fatal("redirect followed")
	}
	if headers.Get("Authorization") != "Bearer synthetic-token" {
		t.Fatal("caller header mutated")
	}
}
func TestTransportExplicitProxyOverrideAndCancellation(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer server.Close()
	client := New("http://invalid-proxy.example")
	defer client.Close()
	client.clients[""] = server.Client()
	response, err := client.Do(WithProxy(context.Background(), ""), contract.HTTPRequest{Method: "GET", URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if strings.TrimSpace(string(body)) != "ok" {
		t.Fatal("explicit proxy override ignored")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Do(WithProxy(ctx, ""), contract.HTTPRequest{Method: "GET", URL: server.URL}); err == nil {
		t.Fatal("canceled request dispatched")
	}
}
