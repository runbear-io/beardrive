package webapp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// TestNotifyText pins the copy, which is the feature: every line names an
// actor, and "1 file changed" appears nowhere.
func TestNotifyText(t *testing.T) {
	for _, tc := range []struct {
		name string
		ops  []journal.Op
		want string
	}{{
		name: "agent note names the platform",
		ops: []journal.Op{{
			Seq: 1, Lamport: 1, Kind: journal.KindPut, Path: "shared/findings/eu-checkout.md",
			UserName: "Dana Kim", Note: "claude session 41f2",
		}},
		want: "Dana Kim's Claude updated shared/findings/eu-checkout.md",
	}, {
		name: "plain daemon push has no agent",
		ops: []journal.Op{{
			Seq: 1, Lamport: 1, Kind: journal.KindPut, Path: "runbook.md", UserName: "Dana Kim",
		}},
		want: "Dana Kim updated runbook.md",
	}, {
		name: "falls back to the git identity",
		ops: []journal.Op{{
			Seq: 1, Lamport: 1, Kind: journal.KindPut, Path: "runbook.md", Author: "dana@laptop",
		}},
		want: "dana@laptop updated runbook.md",
	}, {
		name: "a hand-typed note is not an agent",
		ops: []journal.Op{{
			Seq: 1, Lamport: 1, Kind: journal.KindPut, Path: "a.md",
			UserName: "Sam Ito", Note: "tidying up",
		}},
		want: "Sam Ito updated a.md",
	}, {
		name: "deletes read as deletes",
		ops: []journal.Op{{
			Seq: 1, Lamport: 1, Kind: journal.KindDelete, Path: "old.md", UserName: "Sam Ito",
		}},
		want: "Sam Ito deleted old.md",
	}, {
		name: "the last op per path wins",
		ops: []journal.Op{
			{Seq: 1, Lamport: 1, Kind: journal.KindPut, Path: "a.md", UserName: "Dana Kim"},
			{Seq: 2, Lamport: 2, Kind: journal.KindDelete, Path: "a.md", UserName: "Sam Ito"},
		},
		want: "Sam Ito deleted a.md",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := notifyText(tc.ops)
			if got != tc.want {
				t.Fatalf("notifyText = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "file changed") {
				t.Fatalf("notifyText says %q — the copy is the feature", got)
			}
		})
	}
}

// A big first cycle must not post a wall of lines.
func TestNotifyTextCapsTheBatch(t *testing.T) {
	var ops []journal.Op
	for i := range 50 {
		ops = append(ops, journal.Op{
			Seq: int64(i + 1), Lamport: int64(i + 1), Kind: journal.KindPut,
			Path: fmt.Sprintf("f%02d.md", i), UserName: "Dana Kim",
		})
	}
	got := notifyText(ops)
	lines := strings.Split(got, "\n")
	if len(lines) != notifyLineMax+1 {
		t.Fatalf("got %d lines, want %d plus the overflow line", len(lines), notifyLineMax)
	}
	if want := fmt.Sprintf("…and %d more", 50-notifyLineMax); lines[notifyLineMax] != want {
		t.Fatalf("last line = %q, want %q", lines[notifyLineMax], want)
	}
}

// setWebhookDirect bypasses the host allowlist, which (correctly) refuses an
// httptest host. The delivery tests are about the delivery path; the gate has
// its own test.
func setWebhookDirect(t *testing.T, db *ProjectDB, id, url string) {
	t.Helper()
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	p, ok := db.byID[id]
	if !ok {
		t.Fatalf("no such project %q", id)
	}
	next := p
	next.Webhook = url
	if err := db.put(p, next); err != nil {
		t.Fatal(err)
	}
}

// uploadFile drives the real two-step browser upload.
func uploadFile(t *testing.T, h http.Handler, id, path, content string, c *http.Cookie) {
	t.Helper()
	rec := doAs(t, h, "POST", "/api/p/"+id+"/upload/init", initReq(path, content), c)
	if rec.Code != 200 {
		t.Fatalf("upload init: %d %s", rec.Code, rec.Body)
	}
	rec = doAs(t, h, "PUT", "/api/p/"+id+"/upload/content?path="+path, []byte(content), c)
	if rec.Code != 200 {
		t.Fatalf("upload content: %d %s", rec.Code, rec.Body)
	}
}

// setWebhook points a project at url as an admin, through the real endpoint.
func setWebhook(t *testing.T, h http.Handler, id, url string, c *http.Cookie) {
	t.Helper()
	rec := doAs(t, h, "PATCH", "/api/projects/"+id, map[string]string{"webhook": url}, c)
	if rec.Code != 200 {
		t.Fatalf("set webhook: %d %s", rec.Code, rec.Body)
	}
}

// TestWebhookNeverLeavesTheServer is the AC that inverts if projectJSON
// forgets to zero the field: projectView embeds Project, so a new field ships
// to every reader by default. Asserted on the RAW body — a decode into
// Project would pass while the JSON carries the URL.
func TestWebhookNeverLeavesTheServer(t *testing.T) {
	h, _, cookies, p := permHub(t)
	const secret = "https://hooks.slack.com/services/T0/B0/zzSECRETzz"
	setWebhook(t, h, p.ID, secret, cookies["alice"])

	for _, req := range []struct{ method, url string }{
		{"GET", "/api/projects"},
		{"GET", "/api/projects/" + p.ID},
	} {
		rec := doAs(t, h, req.method, req.url, nil, cookies["alice"])
		body := rec.Body.String()
		if strings.Contains(body, "zzSECRETzz") || strings.Contains(body, "hooks.slack.com") {
			t.Fatalf("%s %s leaked the webhook URL: %s", req.method, req.url, body)
		}
		if !strings.Contains(body, `"webhook_set":true`) {
			t.Fatalf("%s %s does not report webhook_set: %s", req.method, req.url, body)
		}
	}

	// And the create response, the third site that renders a project.
	rec := doAs(t, h, "POST", "/api/projects", map[string]string{"name": "wiki"}, cookies["alice"])
	if strings.Contains(rec.Body.String(), "zzSECRETzz") {
		t.Fatalf("create response leaked the webhook URL: %s", rec.Body)
	}
}

// Setting or clearing the webhook is admin-only; a plain member is refused.
func TestWebhookSetIsAdminOnly(t *testing.T) {
	h, srv, cookies, p := permHub(t)
	if err := srv.Projects.SetPerm(p.ID, "bob@x.io", PermWrite); err != nil {
		t.Fatal(err)
	}
	if err := srv.Projects.SetPerm(p.ID, "carol@x.io", PermRead); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"bob", "carol", "dave"} {
		rec := doAs(t, h, "PATCH", "/api/projects/"+p.ID,
			map[string]string{"webhook": "https://hooks.slack.com/services/T0/B0/x"}, cookies[who])
		if rec.Code != 403 && rec.Code != 404 {
			t.Fatalf("%s set webhook: %d %s, want refused", who, rec.Code, rec.Body)
		}
	}
	if got, _ := srv.Projects.Get(p.ID); got.Webhook != "" {
		t.Fatalf("webhook = %q after refused writes, want empty", got.Webhook)
	}
}

// An admin-set URL the hub then fetches is an SSRF primitive. Only https on
// the Slack/Teams incoming-webhook hosts is accepted, and a rejection names
// the reason.
func TestWebhookRejectsNonSlackHosts(t *testing.T) {
	h, srv, cookies, p := permHub(t)

	for _, bad := range []string{
		"http://hooks.slack.com/services/T0/B0/x",   // scheme
		"https://169.254.169.254/latest/meta-data/", // the reason the check exists
		"https://evil.example.com/x",                // host
		"https://hooks.slack.com.evil.example/x",    // suffix trick
		"file:///etc/passwd",
	} {
		rec := doAs(t, h, "PATCH", "/api/projects/"+p.ID, map[string]string{"webhook": bad}, cookies["alice"])
		if rec.Code != 400 {
			t.Fatalf("PATCH webhook=%q: %d, want 400", bad, rec.Code)
		}
		if body := rec.Body.String(); !strings.Contains(body, "https") && !strings.Contains(body, "host") {
			t.Fatalf("PATCH webhook=%q rejected without a reason: %s", bad, body)
		}
	}

	for _, good := range []string{
		"https://hooks.slack.com/services/T0/B0/x",
		"https://acme.webhook.office.com/webhookb2/abc",
		"https://prod-12.westus.logic.azure.com/workflows/abc/triggers/manual/paths/invoke",
	} {
		setWebhook(t, h, p.ID, good, cookies["alice"])
		if got, _ := srv.Projects.Get(p.ID); got.Webhook != good {
			t.Fatalf("webhook = %q, want %q", got.Webhook, good)
		}
	}

	// Empty clears it — the way notifications get turned off.
	setWebhook(t, h, p.ID, "", cookies["alice"])
	if got, _ := srv.Projects.Get(p.ID); got.Webhook != "" {
		t.Fatalf("webhook = %q after clear, want empty", got.Webhook)
	}
}

// hangingHook is an endpoint that never answers until the test releases it —
// the shape a real outage takes.
func hangingHook(t *testing.T) (url string, release func(), got chan string) {
	t.Helper()
	block, delivered := make(chan struct{}), make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		delivered <- body.Text
		<-block
	}))
	var once bool
	t.Cleanup(func() {
		if !once {
			close(block)
		}
		srv.Close()
	})
	return srv.URL, func() { once = true; close(block) }, delivered
}

// TestWebhookNeverDelaysAPush is the invariant this whole feature is gated
// on: the response is written first and the POST happens after, so a hanging
// endpoint cannot stall — or 502 — a sync.
func TestWebhookNeverDelaysAPush(t *testing.T) {
	h, srv, cookies, p, _ := permHubAt(t)
	hookURL, release, delivered := hangingHook(t)
	defer release()
	// Set the URL directly: the allowlist (correctly) refuses an httptest
	// host, and what is under test here is the delivery path, not the gate.
	setWebhookDirect(t, srv.Projects, p.ID, hookURL)

	done := make(chan struct{}, 1)
	go func() {
		uploadFile(t, h, p.ID, "notes.md", "hello from the browser", cookies["alice"])
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the upload blocked on the hanging webhook — it must respond first, notify second")
	}

	select {
	case text := <-delivered:
		if !strings.Contains(text, "notes.md") {
			t.Fatalf("delivered %q, want it to name notes.md", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notification was delivered")
	}
}

// The hub notifies about its own writes, or "the hub tells you" is false for
// anyone who only ever uses the browser.
func TestWebhookFiresFromBrowserWrites(t *testing.T) {
	h, srv, cookies, p, _ := permHubAt(t)
	hookURL, release, delivered := hangingHook(t)
	release() // answer immediately; this test is about coverage, not timing
	setWebhookDirect(t, srv.Projects, p.ID, hookURL)

	uploadFile(t, h, p.ID, "notes.md", "v1", cookies["alice"])
	if text := waitForText(t, delivered); !strings.Contains(text, "updated notes.md") {
		t.Fatalf("upload notified %q", text)
	}

	rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/remove",
		map[string]string{"path": "notes.md"}, cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
	if text := waitForText(t, delivered); !strings.Contains(text, "deleted notes.md") {
		t.Fatalf("remove notified %q", text)
	}
}

// A project with no webhook makes no outbound request at all.
func TestNoWebhookNoRequests(t *testing.T) {
	h, _, cookies, p, _ := permHubAt(t)
	hits := make(chan struct{}, 4)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- struct{}{}
	}))
	defer hook.Close()

	uploadFile(t, h, p.ID, "a.md", "x", cookies["alice"])
	select {
	case <-hits:
		t.Fatal("an unconfigured project made an outbound request")
	case <-time.After(300 * time.Millisecond):
	}
}

func waitForText(t *testing.T, ch chan string) string {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("no notification was delivered")
		return ""
	}
}
