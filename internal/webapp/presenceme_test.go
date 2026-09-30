package webapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// presence/me hands a viewer's own open doc to their own agent. These pin the
// four ways it must answer nothing, and that the roster never learns of it.

func TestPresenceMe(t *testing.T) {
	srv, _, p := authHub(t, true)
	h := srv.Handler()
	alice := signupAndSession(t, h, "alice@example.com", "Alice", "correct-horse-1")
	bob := signupAndSession(t, h, "bob@example.com", "Bob", "correct-horse-2")
	base := "/api/p/" + p.ID + "/presence"
	me := func(c *http.Cookie) map[string]string {
		t.Helper()
		rec := doAs(t, h, "GET", base+"/me", nil, c)
		if rec.Code != 200 {
			t.Fatalf("presence/me: %d %s", rec.Code, rec.Body)
		}
		var out map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	post := func(body map[string]any) string {
		t.Helper()
		rec := doAs(t, h, "POST", base, body, alice)
		if rec.Code != 200 {
			t.Fatalf("presence: %d %s", rec.Code, rec.Body)
		}
		return rec.Body.String()
	}

	// Off by default: an open doc without the opt-in is nobody's business.
	post(map[string]any{"path": "docs/guide.md"})
	if got := me(alice); len(got) != 0 {
		t.Fatalf("reported without opt-in: %v", got)
	}

	// Opted in: the same account gets it; the roster carries only name+path.
	roster := post(map[string]any{"path": "docs/guide.md", "agent": true})
	if got := me(alice); got["path"] != "docs/guide.md" || got["seen"] == "" {
		t.Fatalf("opted-in view not reported: %v", got)
	}
	if strings.Contains(roster, "agent") || strings.Contains(roster, "seen") {
		t.Fatalf("roster leaked the opt-in: %s", roster)
	}

	// Another account in the same project gets nothing — not even its own.
	if got := me(bob); len(got) != 0 {
		t.Fatalf("another account saw alice's view: %v", got)
	}
	// Nor does an unauthenticated caller claiming a device.
	anon := httptest.NewRequest("GET", base+"/me", nil)
	anon.Header.Set("X-Bdrive-Device", "alice@example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, anon)
	if strings.Contains(rec.Body.String(), "guide.md") {
		t.Fatalf("anonymous caller got a view: %d %s", rec.Code, rec.Body)
	}

	// Toggling off clears it.
	post(map[string]any{"path": "docs/guide.md", "agent": false})
	if got := me(alice); len(got) != 0 {
		t.Fatalf("reported after opting out: %v", got)
	}

	// Leaving clears it.
	post(map[string]any{"path": "docs/guide.md", "agent": true})
	post(map[string]any{"leave": true})
	if got := me(alice); len(got) != 0 {
		t.Fatalf("reported after leave: %v", got)
	}
}

// Idle expiry and liveness are clock questions, so they are driven directly.
func TestPresenceMeExpires(t *testing.T) {
	s := &Server{}
	const project, actor = "p1", "alice@example.com"
	sub, _ := s.events().subscribe(project, actor)
	t0 := time.Now()
	s.markPresence(project, actor, "Alice", "a.md", t0)
	s.presence().setAgent(project, actor, true, t0)

	if got := s.presenceMe(project, actor, t0.Add(29*time.Minute)); got["path"] != "a.md" {
		t.Fatalf("a streaming, opted-in view expired early: %v", got)
	}
	if got := s.presenceMe(project, actor, t0.Add(presenceAgentIdle+time.Minute)); len(got) != 0 {
		t.Fatalf("idle view still reported: %v", got)
	}
	// Refocusing re-arms it.
	t1 := t0.Add(presenceAgentIdle + time.Minute)
	s.markPresence(project, actor, "Alice", "a.md", t1)
	s.presence().setAgent(project, actor, true, t1)
	if got := s.presenceMe(project, actor, t1); got["path"] != "a.md" {
		t.Fatalf("re-announce did not re-arm: %v", got)
	}
	// A dead tab (no stream, past the TTL) answers nothing even before a
	// teammate's announce gets round to expiring it from the map.
	s.events().unsubscribe(project, sub)
	if got := s.presenceMe(project, actor, t1.Add(2*presenceTTL)); len(got) != 0 {
		t.Fatalf("dead tab still reported: %v", got)
	}
}

// Flipping the opt-in changes nothing a teammate sees, so it publishes no frame.
func TestPresenceAgentFlipPublishesNothing(t *testing.T) {
	s := &Server{}
	const project = "p1"
	sub, _ := s.events().subscribe(project, "watcher@example.com")
	defer s.events().unsubscribe(project, sub)
	t0 := time.Now()
	s.markPresence(project, "alice@example.com", "Alice", "a.md", t0)
	<-sub.ch // the arrival frame
	s.markPresence(project, "alice@example.com", "Alice", "a.md", t0)
	s.presence().setAgent(project, "alice@example.com", true, t0)
	select {
	case f := <-sub.ch:
		t.Fatalf("opt-in flip published a frame: %s", f)
	default:
	}
}
