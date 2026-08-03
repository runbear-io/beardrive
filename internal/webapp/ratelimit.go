package webapp

import (
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Share links are the one unauthenticated surface of a hub, so /s/* gets a
// per-IP token bucket — generous enough that no human reader ever sees it,
// tight enough that a scraper can't turn the server into a free CDN.

// DefaultShareRPM is the per-IP sustained rate on /s/* when the config
// doesn't say otherwise.
const DefaultShareRPM = 120

type rateLimiter struct {
	rate  float64 // tokens per second
	burst float64
	now   func() time.Time // injectable for tests

	mu      sync.Mutex
	buckets map[string]*tokenBucket
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

// newRateLimiter allows rpm sustained requests per key with a burst of
// rpm/4 (min 10) on top.
func newRateLimiter(rpm int) *rateLimiter {
	if rpm <= 0 {
		rpm = DefaultShareRPM
	}
	return &rateLimiter{
		rate:    float64(rpm) / 60,
		burst:   max(float64(rpm)/4, 10),
		now:     time.Now,
		buckets: make(map[string]*tokenBucket),
	}
}

func (l *rateLimiter) allow(key string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	// Keep the map bounded: when it grows past 10k keys, drop buckets idle
	// long enough to be full again anyway.
	if len(l.buckets) > 10000 {
		idle := time.Duration(l.burst/l.rate) * time.Second
		for k, b := range l.buckets {
			if now.Sub(b.last) > idle {
				delete(l.buckets, k)
			}
		}
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &tokenBucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// clientIP is the rate-limit key: the connection's address, or the hop the
// operator's own proxy added.
//
// The header is attacker-controlled on a directly-reachable hub, and this key
// is what throttles both /s/* and the login endpoint — so trusting it from
// anyone hands every client an unlimited-rate bucket per request and turns off
// the password brute-force limiter.
//
// So it is trusted by PEER, not by configuration. A reverse proxy that fronts
// a hub reaches it over loopback or a private network — a sidecar, a container
// network, a Fly/Cloud Run internal address — so an X-Forwarded-For arriving
// from such a peer is the operator's own infrastructure and is honored with no
// config at all. Requiring opt-in instead was a day-one outage for every
// proxied hub: the peer is the proxy, so ALL its users shared one 10/min login
// bucket and correct passwords started answering "too many attempts".
// A peer on a PUBLIC address is a client, and its header is a lie — that hub
// keeps ignoring it, and logs once so the case is not silent. TrustProxy stays
// as the override for the remaining shape: a proxy on a public address.
//
// The LAST element is the trusted one. X-Forwarded-For grows left to right —
// each proxy APPENDS what it saw — so a client that sends its own header keeps
// its forged value at the head, and the entry our proxy added is at the tail.
// Reading the first entry meant turning TrustProxy on disabled the limiter it
// was added to fix: every request got a fresh bucket of the client's choosing.
//
// And the last entry has to be read from the last FIELD LINE. Header.Get
// returns only the first line, but a request may carry several X-Forwarded-For
// lines — RFC 9110 says they are the comma-joined list, in order — and a proxy
// that ADDS its own line rather than rewriting the client's puts the trusted
// hop in the last one. With Get, a client that sends its own line was the
// entire key again.
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	fwd := lastForwardedFor(r)
	if fwd == "" {
		return host
	}
	if s.TrustProxy || localPeer(host) {
		return fwd
	}
	s.xffWarnOnce.Do(func() {
		log.Printf("beardrive: ignoring X-Forwarded-For from %s — that is a public address, "+
			"so the header is client-supplied; set trust_proxy if a proxy you control fronts this hub", host)
	})
	return host
}

// lastForwardedFor is the hop the nearest proxy appended: the last element of
// the last field line, empty when there is no header.
func lastForwardedFor(r *http.Request) string {
	lines := r.Header.Values("X-Forwarded-For")
	if len(lines) == 0 {
		return ""
	}
	parts := strings.Split(lines[len(lines)-1], ",")
	return strings.TrimSpace(parts[len(parts)-1])
}

// localPeer reports whether the connection came from an address only the
// operator's own network can hold. Both predicates are stdlib: IsPrivate is
// RFC 1918 plus IPv6 unique-local (fc00::/7), which is what a sidecar, a
// container network and a cloud runtime's internal hop all sit in.
func localPeer(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

// shareLimiter lazily builds the /s/* limiter from ShareRPM.
func (s *Server) shareLimiter() *rateLimiter {
	s.shareLimOnce.Do(func() {
		s.shareLim = newRateLimiter(s.ShareRPM)
	})
	return s.shareLim
}

// authLimiter throttles credential endpoints (login, signup) per IP to blunt
// password brute-force and signup floods. Deliberately tight (10/min).
func (s *Server) authLimiter() *rateLimiter {
	s.authLimOnce.Do(func() {
		s.authLim = newRateLimiter(10)
	})
	return s.authLim
}

// rateLimitAuth wraps the auth mux so POSTs to the credential endpoints are
// throttled per IP; GETs (rendering the forms) pass freely. /auth/reset is in
// the set because it both sends mail and answers differently for a known
// address — unmetered, it is an account-enumeration and mail-flood endpoint.
func (s *Server) rateLimitAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if r.Method == http.MethodPost && (p == "/auth/login" || p == "/auth/signup" || p == "/auth/reset") {
			if !s.authLimiter().allow(s.clientIP(r)) {
				http.Error(w, "too many attempts — wait a minute and try again", http.StatusTooManyRequests)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
