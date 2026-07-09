package webapp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func mustJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatal(err)
	}
}

// recQuota records every hook call and can be told to say no.
type recQuota struct {
	mu     sync.Mutex
	denyW  bool
	denyS  bool
	writes []struct {
		org   string
		bytes int64
	}
	usage []struct {
		org   string
		bytes int64
	}
	seats []struct {
		org     string
		members int
	}
}

func (q *recQuota) CheckWrite(org string, b int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.writes = append(q.writes, struct {
		org   string
		bytes int64
	}{org, b})
	if q.denyW {
		return fmt.Errorf("storage quota exceeded")
	}
	return nil
}

func (q *recQuota) CheckSeat(org string, members int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.seats = append(q.seats, struct {
		org     string
		members int
	}{org, members})
	if q.denyS {
		return fmt.Errorf("seat limit reached")
	}
	return nil
}

func (q *recQuota) RecordUsage(org string, b int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.usage = append(q.usage, struct {
		org   string
		bytes int64
	}{org, b})
}

// Every write path must consult the quota with the project's org and the
// byte count, record usage on success, and turn a denial into a 403 —
// while the OSS default (nil provider) stays unlimited.
func TestQuotaHooks(t *testing.T) {
	h, srv, alice, bob, pa := orgHubSrv(t)
	q := &recQuota{}
	srv.Quota = q

	// browser upload through the server: CheckWrite then RecordUsage
	content := []byte("hello quota")
	rec := doAs(t, h, "PUT", "/api/p/"+pa.ID+"/upload/content?path=q.md", content, alice)
	if rec.Code != 200 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	if len(q.writes) != 1 || q.writes[0].org != pa.Org || q.writes[0].bytes != int64(len(content)) {
		t.Fatalf("CheckWrite calls = %+v, want one for org %s with %d bytes", q.writes, pa.Org, len(content))
	}
	if len(q.usage) != 1 || q.usage[0].org != pa.Org || q.usage[0].bytes != int64(len(content)) {
		t.Fatalf("RecordUsage calls = %+v", q.usage)
	}

	// device sync store put: same discipline
	jl := []byte(`{"seq":1}`)
	rec = doAs(t, h, "PUT", "/api/p/"+pa.ID+"/store/object?key=journal/dev1.jsonl", jl, alice)
	if rec.Code != 200 {
		t.Fatalf("store put: %d %s", rec.Code, rec.Body)
	}
	if len(q.writes) != 2 || q.writes[1].org != pa.Org || q.writes[1].bytes != int64(len(jl)) {
		t.Fatalf("store CheckWrite = %+v", q.writes)
	}
	if len(q.usage) != 2 {
		t.Fatalf("store RecordUsage missing: %+v", q.usage)
	}

	// store sign checks before any bytes move
	rec = doAs(t, h, "POST", "/api/p/"+pa.ID+"/store/sign",
		map[string]any{"key": "blobs/" + strings.Repeat("b", 64), "size": 512}, alice)
	if rec.Code != 200 {
		t.Fatalf("store sign: %d %s", rec.Code, rec.Body)
	}
	if last := q.writes[len(q.writes)-1]; last.org != pa.Org || last.bytes != 512 {
		t.Fatalf("sign CheckWrite = %+v", last)
	}

	// denial → 403, and no usage recorded
	q.denyW = true
	usageBefore := len(q.usage)
	rec = doAs(t, h, "PUT", "/api/p/"+pa.ID+"/upload/content?path=q2.md", content, alice)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "quota") {
		t.Fatalf("denied upload: %d %s", rec.Code, rec.Body)
	}
	if len(q.usage) != usageBefore {
		t.Fatal("denied write still recorded usage")
	}
	q.denyW = false

	// invite redemption consults CheckSeat with the current member count
	rec = doAs(t, h, "POST", "/api/orgs/"+pa.Org+"/invites", nil, alice)
	var inv struct {
		Token string `json:"token"`
	}
	mustJSON(t, rec, &inv)
	q.denyS = true
	if rec := doAs(t, h, "POST", "/api/invites/"+inv.Token, nil, bob); rec.Code != http.StatusForbidden {
		t.Fatalf("seat-denied join: %d %s", rec.Code, rec.Body)
	}
	if len(q.seats) != 1 || q.seats[0].org != pa.Org || q.seats[0].members != 1 {
		t.Fatalf("CheckSeat calls = %+v, want one for org %s with 1 member", q.seats, pa.Org)
	}
	q.denyS = false
	if rec := doAs(t, h, "POST", "/api/invites/"+inv.Token, nil, bob); rec.Code != 200 {
		t.Fatalf("join after seat freed: %d %s", rec.Code, rec.Body)
	}
	// a member re-accepting the same link doesn't re-check seats
	seatCalls := len(q.seats)
	doAs(t, h, "POST", "/api/invites/"+inv.Token, nil, bob)
	if len(q.seats) != seatCalls {
		t.Fatal("re-join of an existing member consulted CheckSeat")
	}
}
