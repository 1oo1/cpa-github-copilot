package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/1oo1/cpa-github-copilot/internal/contract"
	"github.com/1oo1/cpa-github-copilot/internal/copilot"
)

// prTimeout bounds every wait so a lock regression fails instead of hanging the suite.
const prTimeout = 15 * time.Second

// prUpstream serves token exchange and model discovery; quota calls go to fakeUpstream.
type prUpstream struct {
	quota                 *fakeUpstream
	mu                    sync.Mutex
	failToken, failModels bool
}

func (u *prUpstream) Do(ctx context.Context, req contract.HTTPRequest) (*http.Response, error) {
	u.mu.Lock()
	failToken, failModels := u.failToken, u.failModels
	u.mu.Unlock()
	switch {
	case strings.HasSuffix(req.URL, "/copilot_internal/v2/token"):
		if failToken {
			return nil, fmt.Errorf("synthetic token exchange failure")
		}
		return fakeResponse(fmt.Sprintf(`{"token":"tid=renewed;proxy-ep=proxy.individual.githubcopilot.com","expires_at":%d}`, time.Now().Add(time.Hour).Unix())), nil
	case strings.HasSuffix(req.URL, "/models"):
		if failModels {
			return nil, fmt.Errorf("synthetic model discovery failure")
		}
		return fakeResponse(`{"data":[{"id":"updated-model","name":"Updated","model_picker_enabled":true,"supported_endpoints":["/chat/completions"],"capabilities":{"supports":{"streaming":true,"tool_calls":true}}}]}`), nil
	}
	return u.quota.Do(ctx, req)
}

func prNewServer(t *testing.T) (*Server, *prUpstream, Config, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := Config{ManagementKey: "management-test-key", APIKeys: []string{"api-test-key-012345"}, Routing: "fill-first", Copilot: copilot.DefaultSettings()}
	up := &prUpstream{quota: &fakeUpstream{counts: map[string]int{}, used: map[string]string{}, reset: time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)}}
	s, err := New(cfg, path, up, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, up, cfg, path
}

// prCred derives a synthetic credential and overlays extra top-level fields.
func prCred(t *testing.T, token string, priority int, extra map[string]any) []byte {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(credential(token, priority), &raw); err != nil {
		t.Fatal(err)
	}
	for key, value := range extra {
		raw[key] = value
	}
	out, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// prStaleCred has an expired model cache so catalog discovery rewrites the credential.
func prStaleCred(t *testing.T, token string) []byte {
	return prCred(t, token, 2, map[string]any{"models_fetched_at": 1})
}

func prImport(t *testing.T, s *Server, name string, body []byte) *account {
	t.Helper()
	if _, err := s.importAccount(name, body); err != nil {
		t.Fatal(err)
	}
	a := s.account(name)
	if a == nil {
		t.Fatal("account was not installed")
	}
	return a
}

func prAuthFile(s *Server, id string) string { return filepath.Join(s.authDir, id) }

func prRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func prReadJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(prRead(t, path), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func prConfig(s *Server) Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// prBlockedPath lies beneath a regular file, so atomicWrite fails even when running as root.
func prBlockedPath(t *testing.T, leaf string) string {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(blocker, leaf)
}

// prBreakAuthDir makes every credential write fail until the returned restore runs.
func prBreakAuthDir(t *testing.T, s *Server) (restore func()) {
	t.Helper()
	original := s.authDir
	s.authDir = prBlockedPath(t, "auths")
	return func() { s.authDir = original }
}

func prBreakConfig(t *testing.T, s *Server) (restore func()) {
	t.Helper()
	original := s.configPath
	s.configPath = prBlockedPath(t, "config.yaml")
	return func() { s.configPath = original }
}

// prBreakState replaces the state directory with a regular file so state writes fail.
func prBreakState(t *testing.T, s *Server) (restore func()) {
	t.Helper()
	dir := filepath.Join(s.dataDir, "state")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.Remove(dir); err != nil {
			t.Fatal(err)
		}
	}
}

func prNoTempFiles(t *testing.T, roots ...string) {
	t.Helper()
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err == nil && strings.HasPrefix(d.Name(), ".write-") {
				t.Errorf("leftover temporary file %s", path)
			}
			return nil
		})
	}
}

func prAsync(fn func() error) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- fn() }()
	return ch
}

func prStacks() string {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		buf = make([]byte, 2*len(buf))
	}
}

func prWaitErr(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(prTimeout):
		t.Fatalf("%s did not finish (deadlock?)\n%s", what, prStacks())
		return nil
	}
}

func prFree(t *testing.T, name string, mu *sync.Mutex) {
	t.Helper()
	if !mu.TryLock() {
		t.Fatalf("%s is held while the operation waits for persistence", name)
	}
	mu.Unlock()
}

// prHoldPersist takes the persistence gate so the operation under test parks right before its disk write.
func prHoldPersist(t *testing.T, s *Server) (release func()) {
	t.Helper()
	s.persistMu.Lock()
	var once sync.Once
	release = func() { once.Do(s.persistMu.Unlock) }
	t.Cleanup(release)
	return release
}

func prParked(substrs ...string) bool {
	for _, g := range strings.Split(prStacks(), "\n\n") {
		ok := strings.Contains(g, "(*Mutex).lockSlow")
		for _, sub := range substrs {
			ok = ok && strings.Contains(g, sub)
		}
		if ok {
			return true
		}
	}
	return false
}

// prWaitParked polls goroutine stacks until one is blocked acquiring a mutex inside every named frame.
func prWaitParked(t *testing.T, substrs ...string) {
	t.Helper()
	deadline := time.Now().Add(prTimeout)
	for !prParked(substrs...) {
		if time.Now().After(deadline) {
			t.Fatalf("no goroutine parked on a mutex in %v\n%s", substrs, prStacks())
		}
		time.Sleep(time.Millisecond)
	}
}

func prWaitFile(t *testing.T, path string, done func(raw []byte, err error) bool) {
	t.Helper()
	deadline := time.Now().Add(prTimeout)
	for {
		raw, err := os.ReadFile(path)
		if done(raw, err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not reach the expected state while the test held the server and account mutexes", path)
		}
		time.Sleep(time.Millisecond)
	}
}
