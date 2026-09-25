package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math"
	"net/http"
	"net/netip"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxInferenceBody   = 16 << 20
	minInferenceWeight = 64 << 10
	maxManagementBody  = 1 << 20
	// admissionWait bounds how long a request may queue for memory before 503.
	admissionWait = 30 * time.Second
	// downstreamWriteTimeout drops clients that stop reading a response.
	downstreamWriteTimeout = 2 * time.Minute

	authFailureLimit   = 10
	authFailureWindow  = 10 * time.Minute
	authBlockDuration  = 15 * time.Minute
	authTrackedClients = 4096
)

func parseTrustedProxies(values []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if prefix, err := netip.ParsePrefix(value); err == nil {
			out = append(out, prefix.Masked())
			continue
		}
		addr, err := netip.ParseAddr(value)
		if err != nil {
			return nil, fmt.Errorf("trusted-proxies must contain IP addresses or CIDR ranges")
		}
		addr = addr.Unmap()
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}

func trustedAddr(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// clientKey identifies the caller for failed-authentication limits. X-Forwarded-For
// is honored only when the direct peer is a configured reverse proxy; IPv6 clients
// are grouped by /64 because one host usually controls the whole prefix.
func (s *Server) clientKey(r *http.Request) netip.Addr {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	addr := peer.Addr().Unmap()
	if trustedAddr(s.trustedProxies, addr) {
		hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
		for i := len(hops) - 1; i >= 0; i-- {
			hop, errHop := netip.ParseAddr(strings.TrimSpace(hops[i]))
			if errHop != nil {
				break
			}
			addr = hop.Unmap()
			if !trustedAddr(s.trustedProxies, addr) {
				break
			}
		}
	}
	if addr.Is6() {
		addr = netip.PrefixFrom(addr, 64).Masked().Addr()
	}
	return addr
}

type authFailure struct {
	count               int
	window, blockedTill time.Time
}

// authClient separates management and API failures so guessing one key type
// does not lock out callers of the other.
type authClient struct {
	scope string
	addr  netip.Addr
}

// authLimiter blocks a client after repeated wrong keys. Requests without any
// credential are not counted, so plain scanners cannot lock out a shared
// reverse-proxy address. Keys that already authenticated keep working during a
// block; a correct guess made while blocked still gets 429, so it is no oracle.
type authLimiter struct {
	mu      sync.Mutex
	clients map[authClient]*authFailure
	known   map[[32]byte]struct{}
}

func knownKey(scope, key string) [32]byte { return sha256.Sum256([]byte(scope + "\x00" + key)) }

func (l *authLimiter) remember(scope, key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.known) < authTrackedClients {
		l.known[knownKey(scope, key)] = struct{}{}
	}
}

func (l *authLimiter) isKnown(scope, key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.known[knownKey(scope, key)]
	return ok
}

func (l *authLimiter) blocked(client authClient, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if entry := l.clients[client]; entry != nil && now.Before(entry.blockedTill) {
		return entry.blockedTill.Sub(now)
	}
	return 0
}

// fail records a wrong key and reports whether the client just became blocked.
func (l *authLimiter) fail(client authClient, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.clients[client]
	if entry == nil {
		if len(l.clients) >= authTrackedClients {
			for key, old := range l.clients {
				if now.Sub(old.window) > authFailureWindow && !now.Before(old.blockedTill) {
					delete(l.clients, key)
				}
			}
			if len(l.clients) >= authTrackedClients {
				return false
			}
		}
		entry = &authFailure{window: now}
		l.clients[client] = entry
	}
	if now.Sub(entry.window) > authFailureWindow {
		entry.count, entry.window = 0, now
	}
	entry.count++
	if entry.count < authFailureLimit {
		return false
	}
	entry.count, entry.window, entry.blockedTill = 0, now, now.Add(authBlockDuration)
	return true
}

// admitCredential applies the failed-authentication limit. It writes the 429 or
// the caller's rejection itself and returns true only for a valid, unblocked key.
func (s *Server) admitCredential(w http.ResponseWriter, r *http.Request, scope, given string, valid bool, reject func()) bool {
	client := authClient{scope, s.clientKey(r)}
	now := time.Now()
	if wait := s.limiter.blocked(client, now); wait > 0 && !(valid && s.limiter.isKnown(scope, given)) {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		apiError(w, http.StatusTooManyRequests, "too_many_auth_failures", "too many failed authentication attempts; retry later")
		return false
	}
	if valid {
		s.limiter.remember(scope, given)
		return true
	}
	if given != "" && s.limiter.fail(client, now) {
		s.recordLog(Log{Time: now.UTC(), Level: "warn", Event: "auth." + scope + ".client_blocked", Client: client.addr.String()})
	}
	reject()
	return false
}

// memoryGate bounds the request bodies being processed at once. Translation holds
// several copies of each body, so without it a few concurrent large requests can
// exceed a container memory limit.
type memoryGate struct {
	mu       sync.Mutex
	capacity int64
	used     int64
	changed  chan struct{}
}

func newMemoryGate(capacity int64) *memoryGate {
	return &memoryGate{capacity: capacity, changed: make(chan struct{})}
}

type memoryLease struct {
	gate   *memoryGate
	weight int64
}

// acquire always admits a request when the gate is idle so one maximum-size
// body is never rejected outright.
func (g *memoryGate) acquire(ctx context.Context, weight int64) (*memoryLease, bool) {
	for {
		g.mu.Lock()
		if g.used == 0 || g.used+weight <= g.capacity {
			g.used += weight
			g.mu.Unlock()
			return &memoryLease{g, weight}, true
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, false
		case <-changed:
		}
	}
}

// resize lowers the reservation once the real body size is known.
func (l *memoryLease) resize(weight int64) {
	g := l.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if weight >= l.weight {
		return
	}
	g.used -= l.weight - weight
	l.weight = weight
	close(g.changed)
	g.changed = make(chan struct{})
}

func (l *memoryLease) release() { l.resize(0) }

// requestMemoryBudget sizes the gate from the Go memory limit, which main derives
// from the container cgroup when GOMEMLIMIT is unset. A body peaks at roughly 12x
// its size during translation, so the budget is a sixteenth of the limit.
func requestMemoryBudget() int64 {
	limit := debug.SetMemoryLimit(-1)
	if limit <= 0 || limit == math.MaxInt64 {
		return 256 << 20
	}
	return min(max(limit/16, maxInferenceBody), 1<<30)
}

var inlineScript = regexp.MustCompile(`(?is)<script\b[^>]*>(.*?)</script`)

// uiContentSecurityPolicy allows only the embedded UI's own inline scripts.
func uiContentSecurityPolicy(ui []byte) string {
	sources := []string{"'self'"}
	for _, match := range inlineScript.FindAllSubmatch(ui, -1) {
		if len(match[1]) > 0 {
			sum := sha256.Sum256(match[1])
			sources = append(sources, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
		}
	}
	return "default-src 'none'; script-src " + strings.Join(sources, " ") +
		"; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
}
