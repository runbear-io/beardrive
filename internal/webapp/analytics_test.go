package webapp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// phSpy stands in for PostHog's ingestion host.
type phSpy struct {
	*httptest.Server
	mu   sync.Mutex
	got  []map[string]any
	seen chan struct{}
}

func newPHSpy(t *testing.T) *phSpy {
	t.Helper()
	spy := &phSpy{seen: make(chan struct{}, 64)}
	spy.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var ev map[string]any
		if err := json.Unmarshal(body, &ev); err != nil {
			t.Errorf("posthog got unparseable body %q: %v", body, err)
		}
		if r.URL.Path != "/i/v0/e/" {
			t.Errorf("capture posted to %q, want /i/v0/e/", r.URL.Path)
		}
		spy.mu.Lock()
		spy.got = append(spy.got, ev)
		spy.mu.Unlock()
		w.WriteHeader(200)
		spy.seen <- struct{}{}
	}))
	t.Cleanup(spy.Close)
	return spy
}

// events waits for n deliveries. capture is fire-and-forget, so the send
// outlives the request that triggered it.
func (spy *phSpy) events(t *testing.T, n int) []map[string]any {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-spy.seen:
		case <-time.After(3 * time.Second):
			spy.mu.Lock()
			defer spy.mu.Unlock()
			t.Fatalf("waited for %d events, got %d: %v", n, len(spy.got), spy.got)
		}
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	return append([]map[string]any(nil), spy.got...)
}

func (spy *phSpy) count(t *testing.T) int {
	t.Helper()
	// Nothing more should arrive; give a stray goroutine a moment to land.
	time.Sleep(200 * time.Millisecond)
	spy.mu.Lock()
	defer spy.mu.Unlock()
	return len(spy.got)
}

func evProp(t *testing.T, ev map[string]any, key string) any {
	t.Helper()
	props, ok := ev["properties"].(map[string]any)
	if !ok {
		t.Fatalf("event has no properties: %v", ev)
	}
	return props[key]
}

// A device PUTs its WHOLE journal every cycle, so the count has to come from
// the ops the hub has not already stored. Counting the body would report the
// device's entire history again every ten seconds, and "number of file
// changes" would climb on its own while nobody edited anything.
func TestAnalytics_SyncCountsOnlyNewOps(t *testing.T) {
	spy := newPHSpy(t)
	h, srv, c, p := permHub(t)
	srv.Analytics = AnalyticsConfig{Key: "phc_test", Host: spy.URL}

	const dev = "alice-laptop-6f2a"
	// Alice syncs once so the device id is hers.
	if rec := secfx4Store(t, h, "GET", "/api/p/"+p.ID+"/store/list", "", c["alice"], dev); rec.Code != 200 {
		t.Fatalf("control: alice's own sync: %d %s", rec.Code, rec.Body)
	}

	first := secaudOpLine(1, dev, "put", "plan.md", strings.Repeat("a", 64)) +
		secaudOpLine(2, dev, "put", "notes.md", strings.Repeat("b", 64))
	if rec := secfx4PushJournal(t, h, p.ID, dev, first, c["alice"]); rec.Code != 200 {
		t.Fatalf("first push: %d %s", rec.Code, rec.Body)
	}
	ev := spy.events(t, 1)[0]
	if got := ev["event"]; got != "files_changed" {
		t.Errorf("event = %v, want files_changed", got)
	}
	if got := ev["distinct_id"]; got != "alice@x.io" {
		t.Errorf("distinct_id = %v, want alice@x.io — it must match the id the frontend "+
			"identifies with (analytics.ts), or one person counts as two users", got)
	}
	if got := evProp(t, ev, "puts"); got != float64(2) {
		t.Errorf("puts = %v, want 2", got)
	}
	if got := evProp(t, ev, "source"); got != "sync" {
		t.Errorf("source = %v, want sync", got)
	}

	// The second cycle repeats both ops and appends one delete, exactly as a
	// real client does. Only the delete is new.
	second := first + secaudOpLine(3, dev, "delete", "plan.md", "")
	if rec := secfx4PushJournal(t, h, p.ID, dev, second, c["alice"]); rec.Code != 200 {
		t.Fatalf("second push: %d %s", rec.Code, rec.Body)
	}
	ev = spy.events(t, 1)[1]
	if got, want := evProp(t, ev, "deletes"), float64(1); got != want {
		t.Errorf("deletes = %v, want %v", got, want)
	}
	if got := evProp(t, ev, "puts"); got != float64(0) {
		t.Errorf("puts = %v on a re-push of the same journal, want 0 — the whole history "+
			"is being counted again every cycle", got)
	}

	// A cycle that adds nothing (the daemon re-pushing an unchanged journal)
	// is not a file change and must not land as one.
	if rec := secfx4PushJournal(t, h, p.ID, dev, second, c["alice"]); rec.Code != 200 {
		t.Fatalf("idempotent push: %d %s", rec.Code, rec.Body)
	}
	if n := spy.count(t); n != 2 {
		t.Errorf("%d events after a no-op re-push, want 2", n)
	}
}

// The OSS default: no key, no third-party request. A self-hosted hub must not
// phone home, which is the same rule the frontend follows.
func TestAnalytics_UnconfiguredHubSendsNothing(t *testing.T) {
	spy := newPHSpy(t)
	h, srv, c, p := permHub(t)
	srv.Analytics = AnalyticsConfig{Host: spy.URL} // host set, key empty

	const dev = "alice-laptop-6f2a"
	if rec := secfx4Store(t, h, "GET", "/api/p/"+p.ID+"/store/list", "", c["alice"], dev); rec.Code != 200 {
		t.Fatalf("control: alice's own sync: %d %s", rec.Code, rec.Body)
	}
	body := secaudOpLine(1, dev, "put", "plan.md", strings.Repeat("a", 64))
	if rec := secfx4PushJournal(t, h, p.ID, dev, body, c["alice"]); rec.Code != 200 {
		t.Fatalf("push: %d %s", rec.Code, rec.Body)
	}
	if n := spy.count(t); n != 0 {
		t.Errorf("a hub with no analytics key sent %d events", n)
	}
}
