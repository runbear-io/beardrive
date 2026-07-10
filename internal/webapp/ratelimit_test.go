package webapp

import (
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
