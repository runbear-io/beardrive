package webapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// agentGet is a GET with an Accept header — the whole input to the branch.
func agentGet(t *testing.T, h http.Handler, url, accept string, c *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	hdr := map[string]string{}
	if accept != "" {
		hdr["Accept"] = accept
	}
	return sec8Do(t, h, "GET", url, nil, c, hdr)
}

const browserAccept = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"

// The gated links the hook teaches every agent to emit are HUMAN urls, and
// this is the branch that makes them answer an agent. The four cases that used
// to share one byte-identical 200-with-an-empty-shell — file exists, no such
// file, no such project, not permitted — must now be four different answers,
// and the unauthenticated one must never be a 200.
//
// The security gate is the first subtest: Server.frontend runs OUTSIDE
// authGate (auth.go treats every non-/api/ path as open so a browser can reach
// the login page), so a branch here that does not authenticate itself is an
// anonymous read of private project content.
func TestAgentFetch_NegotiatedFileOnTheHumanURL(t *testing.T) {
	h, srv, c, p := permHub(t)
	srv.Reads = nil // heat has its own test below
	sec8Seed(t, h, p, c["alice"], "notes/guide.md", "# Guide\nhello\n")
	const body = "# Guide\nhello\n"

	t.Run("anonymous is 401 and never the bytes", func(t *testing.T) {
		rec := agentGet(t, h, "/"+p.ID+"/notes/guide.md", "text/markdown", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous agent fetch: %d, want 401\n%s", rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "hello") {
			t.Fatalf("anonymous fetch leaked file content: %s", rec.Body)
		}
		// 401 before the project is resolved: otherwise the branch tells an
		// anonymous caller which project ids are real.
		if strings.Contains(rec.Body.String(), p.ID) {
			t.Fatalf("401 body is a project-existence oracle: %s", rec.Body)
		}
	})

	t.Run("member gets the bytes with a content type and provenance", func(t *testing.T) {
		rec := agentGet(t, h, "/"+p.ID+"/notes/guide.md", "text/markdown", c["alice"])
		if rec.Code != 200 {
			t.Fatalf("alice: %d %s", rec.Code, rec.Body)
		}
		if rec.Body.String() != body {
			t.Fatalf("body = %q, want %q", rec.Body, body)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "markdown") && !strings.Contains(ct, "text/plain") {
			t.Fatalf("Content-Type = %q, want a text type, never HTML", ct)
		}
		if strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
			t.Fatal("agent fetch answered text/html")
		}
		prov := rec.Header().Get("X-Bdrive-Provenance")
		for _, want := range []string{`path="notes/guide.md"`, `sha="`, `modified="`, `by="alice@x.io"`} {
			if !strings.Contains(prov, want) {
				t.Errorf("provenance %q is missing %s", prov, want)
			}
		}
	})

	t.Run("a bare curl Accept counts as an agent", func(t *testing.T) {
		for _, accept := range []string{"*/*", ""} {
			rec := agentGet(t, h, "/"+p.ID+"/notes/guide.md", accept, c["alice"])
			if rec.Code != 200 || rec.Body.String() != body {
				t.Fatalf("Accept %q: %d %q", accept, rec.Code, rec.Body)
			}
		}
	})

	// The credential this branch was built for: a device token, which is what
	// `bdrive login` stores and what an agent has to hand.
	t.Run("a device token is a credential here too", func(t *testing.T) {
		tok := secauthzToken(t, srv, "alice@x.io", "alice-laptop-01")
		rec := secauthzDo(h, "GET", "/"+p.ID+"/notes/guide.md", nil, tok, nil)
		if rec.Code != 200 || rec.Body.String() != body {
			t.Fatalf("device token: %d %q", rec.Code, rec.Body)
		}
		if strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
			t.Fatal("device token got the app shell")
		}
		if rec.Header().Get("X-Bdrive-Provenance") == "" {
			t.Fatal("no provenance")
		}
	})

	t.Run("an org outsider is 403", func(t *testing.T) {
		rec := agentGet(t, h, "/"+p.ID+"/notes/guide.md", "text/markdown", c["dave"])
		if rec.Code != http.StatusForbidden {
			t.Fatalf("outsider: %d, want 403\n%s", rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "hello") {
			t.Fatal("403 leaked file content")
		}
	})

	t.Run("a revoked member is 403", func(t *testing.T) {
		if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions/carol@x.io",
			map[string]string{"level": "none"}, c["alice"]); rec.Code != 200 {
			t.Fatalf("revoke carol: %d %s", rec.Code, rec.Body)
		}
		if rec := agentGet(t, h, "/"+p.ID+"/notes/guide.md", "text/markdown", c["carol"]); rec.Code != http.StatusForbidden {
			t.Fatalf("revoked member: %d, want 403", rec.Code)
		}
	})

	t.Run("no such path and no such project are 404", func(t *testing.T) {
		if rec := agentGet(t, h, "/"+p.ID+"/notes/nope.md", "text/markdown", c["alice"]); rec.Code != http.StatusNotFound {
			t.Fatalf("missing path: %d, want 404\n%s", rec.Code, rec.Body)
		}
		bogus := "00000000-0000-0000-0000-000000000000"
		if rec := agentGet(t, h, "/"+bogus+"/notes/guide.md", "text/markdown", c["alice"]); rec.Code != http.StatusNotFound {
			t.Fatalf("missing project: %d, want 404\n%s", rec.Code, rec.Body)
		}
	})
}

// The human URL is unchanged: a browser navigation gets the same shell it got
// before, with the app document's own headers. If this goes red the change has
// stopped being content negotiation and started being a new URL shape.
func TestAgentFetch_BrowserAnswerIsUntouched(t *testing.T) {
	h, srv, c, p := permHub(t)
	srv.Reads = nil
	sec8Seed(t, h, p, c["alice"], "notes/guide.md", "hello")

	shell := agentGet(t, h, "/", browserAccept, c["alice"])
	if shell.Code != 200 || !strings.Contains(shell.Body.String(), "<") {
		t.Fatalf("app shell: %d", shell.Code)
	}

	for _, u := range []string{
		"/" + p.ID + "/notes/guide.md", // a real file
		"/" + p.ID + "/history",        // a view route
		"/" + p.ID + "/dashboard/notes",
		"/" + p.ID + "/settings",
		"/" + p.ID + "/insights", // the legacy view name
	} {
		rec := agentGet(t, h, u, browserAccept, c["alice"])
		if rec.Code != 200 {
			t.Fatalf("%s: %d, want the shell", u, rec.Code)
		}
		// Not byte-compared with "/": the shell's <title> is named per URL (og.go).
		if !strings.Contains(rec.Body.String(), `<div id="root">`) {
			t.Fatalf("%s: body is not the shell", u)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" ||
			rec.Header().Get("Content-Security-Policy") != "frame-ancestors 'none'" ||
			rec.Header().Get("Cache-Control") != "no-cache" ||
			!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s: shell headers changed: %v", u, rec.Header())
		}
	}

	// The view routes are pages, not files: they answer the shell to ANY
	// Accept, or "every user-facing page owns a URL" breaks for a curl.
	for _, u := range []string{"/" + p.ID + "/history", "/" + p.ID + "/dashboard", "/" + p.ID + "/install",
		"/" + p.ID + "/settings", "/" + p.ID + "/insights", "/" + p.ID + "/history/notes/guide.md"} {
		rec := agentGet(t, h, u, "text/markdown", c["alice"])
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `<div id="root">`) {
			t.Fatalf("%s with a non-HTML Accept: %d, want the shell", u, rec.Code)
		}
	}

	// A link unfurler sends no text/html either, but wants the titled shell,
	// not the file — and it carries no session, so the branch would 401 it.
	unfurl := sec8Do(t, h, "GET", "/"+p.ID+"/notes/guide.md", nil, nil,
		map[string]string{"Accept": "*/*", "User-Agent": "Slackbot-LinkExpanding 1.0"})
	if unfurl.Code != 200 || !strings.Contains(unfurl.Body.String(), "<title>guide.md") {
		t.Fatalf("unfurler: %d, want the titled shell", unfurl.Code)
	}

	// A project root is still a page, and so is everything the branch's
	// predicate deliberately excludes.
	for _, u := range []string{"/" + p.ID, "/join/sometoken", "/"} {
		if rec := agentGet(t, h, u, "text/markdown", c["alice"]); rec.Code != 200 {
			t.Fatalf("%s: %d, want the shell", u, rec.Code)
		}
	}

	// The deliberate root-dotted-path 404 (server.go: /llms.txt, /robots.txt)
	// is untouched — the branch sits above it and must not weaken it.
	for _, u := range []string{"/llms.txt", "/robots.txt"} {
		for _, a := range []string{"text/markdown", browserAccept, "*/*"} {
			if rec := agentGet(t, h, u, a, c["alice"]); rec.Code != http.StatusNotFound {
				t.Fatalf("%s (Accept %q): %d, want 404", u, a, rec.Code)
			}
		}
	}

	// Reserved prefixes keep 404ing rather than being read as project files.
	for _, u := range []string{"/api/nope", "/auth/nope", "/s/nope/deeper"} {
		if rec := agentGet(t, h, u, "text/markdown", c["alice"]); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: %d, want 404", u, rec.Code)
		}
	}
}

// Read heat is the number the Dashboard's reads-x-staleness quadrant and the
// folder heat dots are built on. An agent fetching a human URL must land in
// the agent bucket, and the human count for that path must not move.
func TestAgentFetch_RecordsAnAgentReadNotAHumanOne(t *testing.T) {
	h, srv, c, p := permHub(t)
	ledger, err := OpenReadLedger(filepath.Join(t.TempDir(), "reads.json"), 0)
	if err != nil {
		t.Fatal(err)
	}
	srv.Reads = ledger
	sec8Seed(t, h, p, c["alice"], "notes/guide.md", "hello")

	heat := func() HeatEntry {
		t.Helper()
		rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/heat", nil, c["alice"])
		if rec.Code != 200 {
			t.Fatalf("heat: %d %s", rec.Code, rec.Body)
		}
		var out struct {
			Entries map[string]HeatEntry `json:"entries"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Entries["notes/guide.md"]
	}

	before := heat()
	if rec := agentGet(t, h, "/"+p.ID+"/notes/guide.md", "text/markdown", c["alice"]); rec.Code != 200 {
		t.Fatalf("agent fetch: %d %s", rec.Code, rec.Body)
	}
	after := heat()
	if after.Agent != before.Agent+1 {
		t.Errorf("agent count %d -> %d, want +1", before.Agent, after.Agent)
	}
	if after.Human != before.Human {
		t.Errorf("an agent fetch moved the HUMAN count: %d -> %d", before.Human, after.Human)
	}

	// The actor on an agent bucket is a device id or the fixed string "agent",
	// NEVER an email: /heat?by=device serves agent actors to every project
	// member as device ids, so an email there is an identity leak.
	rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/heat?by=device", nil, c["alice"])
	if rec.Code != 200 {
		t.Fatalf("heat by device: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "@") {
		t.Fatalf("an email reached the agent actor map: %s", rec.Body)
	}

	// A browser navigation to the same URL is still a person.
	if rec := agentGet(t, h, "/"+p.ID+"/notes/guide.md", browserAccept, c["bob"]); rec.Code != 200 {
		t.Fatalf("browser: %d", rec.Code)
	}
	if got := heat(); got.Human != after.Human {
		// the shell records nothing at all, which is what it did before
		t.Errorf("the app shell recorded a read: human %d -> %d", after.Human, got.Human)
	}
}

// A syncing device that names its own id gets that id as the agent actor, so
// its fetches show up on /heat?by=device next to the reads it reports itself.
func TestAgentFetch_UsesTheValidatedDeviceIDAsTheActor(t *testing.T) {
	h, srv, c, p := permHub(t)
	ledger, err := OpenReadLedger(filepath.Join(t.TempDir(), "reads.json"), 0)
	if err != nil {
		t.Fatal(err)
	}
	srv.Reads = ledger
	sec8Seed(t, h, p, c["alice"], "notes/guide.md", "hello")

	rec := sec8Do(t, h, "GET", "/"+p.ID+"/notes/guide.md", nil, c["alice"],
		map[string]string{"Accept": "text/markdown", "X-Bdrive-Device": "alice-laptop-01"})
	if rec.Code != 200 {
		t.Fatalf("agent fetch: %d %s", rec.Code, rec.Body)
	}
	// Fetching does not REGISTER a device (only /store/* does), so the id has
	// no owner yet and is therefore this account's to act as.
	byDev := doAs(t, h, "GET", "/api/p/"+p.ID+"/heat?by=device", nil, c["alice"])
	if !strings.Contains(byDev.Body.String(), "alice-laptop-01") {
		t.Fatalf("device actor missing: %s", byDev.Body)
	}

	// bob naming alice's id claims nothing — but he must not be able to plant
	// an arbitrary actor either. With no registry claim on this id both are
	// unowned; what matters is that no email ever appears.
	if strings.Contains(byDev.Body.String(), "@") {
		t.Fatalf("an email reached the agent actor map: %s", byDev.Body)
	}
}

// /store/* is replication, never a read — the branch must not have widened
// what counts as one.
func TestAgentFetch_StoreTrafficStillRecordsNoRead(t *testing.T) {
	h, srv, c, p := permHub(t)
	ledger, err := OpenReadLedger(filepath.Join(t.TempDir(), "reads.json"), 0)
	if err != nil {
		t.Fatal(err)
	}
	srv.Reads = ledger
	sec8Seed(t, h, p, c["alice"], "notes/guide.md", "hello")

	rec := sec8Do(t, h, "GET", "/api/p/"+p.ID+"/store/list?prefix=blobs/", nil, c["alice"],
		map[string]string{"Accept": "*/*", "X-Bdrive-Device": "alice-laptop-01"})
	if rec.Code != 200 {
		t.Fatalf("store list: %d %s", rec.Code, rec.Body)
	}
	if got := ledger.Heat(p.ID, "", time.Time{}); len(got) != 0 {
		t.Fatalf("/store/* recorded reads: %+v", got)
	}
}

// A folder this account may not read is 404 through the agent door too — the
// same answer as a path that does not exist, as on every viewer route.
func TestAgentFetch_HiddenFolderIsNotFound(t *testing.T) {
	h, _, c, p, _ := hiddenHub(t)
	u := "/" + p.ID + "/vault/secret.md"

	rec := agentGet(t, h, u, "text/markdown", c["bob"])
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "the secret") {
		t.Fatalf("bob, hidden from vault/: %d %s — want 404, no content", rec.Code, rec.Body)
	}
	if rec := agentGet(t, h, u, "text/markdown", c["carol"]); rec.Code != 200 || !strings.Contains(rec.Body.String(), "the secret") {
		t.Fatalf("carol, granted vault/: %d %s", rec.Code, rec.Body)
	}
}
