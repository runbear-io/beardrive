package webapp

import (
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

// clientIP is the rate-limit key: the first X-Forwarded-For hop when a
// proxy fronts the server, else the connection's address.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, ok := strings.Cut(xff, ","); ok || first != "" {
			return strings.TrimSpace(first)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
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

// rateLimitAuth wraps the auth mux so POSTs to /auth/login and /auth/signup
// are throttled per IP; GETs (rendering the forms) pass freely.
func (s *Server) rateLimitAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if r.Method == http.MethodPost && (p == "/auth/login" || p == "/auth/signup") {
			if !s.authLimiter().allow(clientIP(r)) {
				http.Error(w, "too many attempts — wait a minute and try again", http.StatusTooManyRequests)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
