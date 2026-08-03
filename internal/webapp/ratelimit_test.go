package webapp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRateLimiterBucket(t *testing.T) {
	l := newRateLimiter(60) // 1/s sustained, burst 15
	now := time.Unix(0, 0)
	l.now = func() time.Time { return now }

	for i := 0; i < 15; i++ {
		if !l.allow("a") {
			t.Fatalf("burst request %d denied", i)
		}
	}
	if l.allow("a") {
		t.Fatal("request past the burst allowed")
	}
	if !l.allow("b") {
		t.Fatal("another key must have its own bucket")
	}
	now = now.Add(2 * time.Second) // refills 2 tokens
	if !l.allow("a") || !l.allow("a") {
		t.Fatal("refilled tokens denied")
	}
	if l.allow("a") {
		t.Fatal("third request after a 2-token refill allowed")
	}
}

// /s/* answers 429 once an IP exhausts its bucket; other IPs are unaffected.
func TestSharedRouteRateLimited(t *testing.T) {
	srv, p, _, _, h := shareHub(t)
	srv.ShareRPM = 60 // burst 15
	_, shareURL := authedShare(t, srv, h, p.ID, "wiki/notes.md")
	path := shareURL[strings.Index(shareURL, "/s/"):]

	get := func(ip string) int {
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = ip + ":1234"
		return doHTTP(h, req).Code
	}
	last := 0
	for i := 0; i < 16; i++ {
		last = get("10.0.0.1")
	}
	if last != 429 {
		t.Fatalf("16th request from one IP: %d, want 429", last)
	}
	if code := get("10.0.0.2"); code != 200 {
		t.Fatalf("fresh IP after another's limit: %d, want 200", code)
	}
}

// clientIP decides the rate-limit key by PEER. The security half (a public
// peer's header is ignored) is pinned by TestSec_RateLimit_*; this is the
// usability half: a hub behind nginx/Caddy/Fly/Cloud Run gets per-user
// buckets out of the box, instead of keying every user in the world on the
// proxy's one address and answering correct passwords with "too many
// attempts" the morning after an upgrade.
func TestClientIPTrustsLocalProxyWithoutConfig(t *testing.T) {
	req := func(remoteAddr string, xff ...string) *http.Request {
		r := httptest.NewRequest("GET", "http://hub.test/", nil)
		r.RemoteAddr = remoteAddr
		for _, v := range xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		return r
	}
	open := &Server{}
	trusting := &Server{TrustProxy: true}

	for _, c := range []struct {
		name string
		srv  *Server
		req  *http.Request
		want string
	}{
		{"loopback proxy is the operator's own", open, req("127.0.0.1:5555", "198.51.100.7"), "198.51.100.7"},
		{"ipv6 loopback too", open, req("[::1]:5555", "198.51.100.7"), "198.51.100.7"},
		{"rfc1918 sidecar", open, req("10.1.2.3:5555", "198.51.100.7"), "198.51.100.7"},
		{"ipv6 unique-local (fly.io)", open, req("[fdaa:0:1::3]:5555", "198.51.100.7"), "198.51.100.7"},
		{"public peer is a client, header ignored", open, req("203.0.113.9:5555", "198.51.100.7"), "203.0.113.9"},
		{"public peer with trust_proxy on", trusting, req("203.0.113.9:5555", "198.51.100.7"), "198.51.100.7"},
		{"no header, unchanged", open, req("127.0.0.1:5555"), "127.0.0.1"},
		// The round-13/14 holes: the trusted hop is the last element of the
		// last field line, whichever peer supplied it.
		{"last element wins", open, req("127.0.0.1:5555", "1.2.3.4, 10.9.9.9"), "10.9.9.9"},
		{"last field line wins", open, req("127.0.0.1:5555", "1.2.3.4", "10.9.9.9"), "10.9.9.9"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.srv.clientIP(c.req); got != c.want {
				t.Errorf("clientIP = %q, want %q", got, c.want)
			}
		})
	}
}

// Rendered markdown share pages carry the BearDrive footer; shared raw HTML
// is served byte-for-byte, never injected into.
func TestSharedFooterOnMarkdownOnly(t *testing.T) {
	srv, p, _, _, h := shareHub(t)
	const footer = "Shared with"

	_, mdURL := authedShare(t, srv, h, p.ID, "wiki/notes.md")
	rec := doHTTP(h, httptest.NewRequest("GET", mdURL[strings.Index(mdURL, "/s/"):], nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), footer) ||
		!strings.Contains(rec.Body.String(), "github.com/runbear-io/beardrive") {
		t.Fatalf("markdown share must carry the footer: %d %s", rec.Code, rec.Body)
	}

	_, htmlURL := authedShare(t, srv, h, p.ID, "wiki/report.html")
	rec = doHTTP(h, httptest.NewRequest("GET", htmlURL[strings.Index(htmlURL, "/s/"):], nil))
	if rec.Code != 200 || strings.Contains(rec.Body.String(), footer) {
		t.Fatalf("raw HTML share must be untouched: %d %s", rec.Code, rec.Body)
	}
}
