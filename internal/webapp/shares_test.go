package webapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func httptestNewRequestBody(method, url string, data []byte) *http.Request {
	return httptest.NewRequest(method, url, bytes.NewReader(data))
}

func doHTTP(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// shareHub returns an auth-enabled hub with sharing on and one synced file.
func shareHub(t *testing.T) (*Server, Project, string, *fakeRemote, http.Handler) {
	t.Helper()
	srv, p, root := newHub(t, true, nil)
	auth, err := OpenBuiltinAuth(filepath.Join(t.TempDir(), "auth.json"), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.Auth = auth
	sharesPath := filepath.Join(t.TempDir(), "shares.json")
	srv.Shares, err = OpenShareDB(sharesPath)
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.put("dev1", "wiki/report.html", "<h1>Q3</h1><script>alert(1)</script>")
	f.put("dev1", "wiki/notes.md", "# Notes\n\nhello **team**")
	return srv, p, sharesPath, f, srv.Handler()
}

// authedShare creates a share as a signed-in user and returns its token+url.
func authedShare(t *testing.T, srv *Server, h http.Handler, project, path string) (string, string) {
	t.Helper()
	auth := srv.Auth.(*BuiltinAuth)
	u, err := auth.signup("s@x.io", "Sharer", "password1")
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatal(err)
	}
	var uid string
	if u != nil {
		uid = u.ID
	} else {
		auth.mu.Lock()
		uid = auth.findByEmail("s@x.io").ID
		auth.mu.Unlock()
	}
	tok, err := auth.issueToken(uid, "test")
	if err != nil {
		t.Fatal(err)
	}
	req := jsonReq(t, "POST", "/api/p/"+project+"/shares", map[string]string{"path": path})
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := doHTTP(h, req)
	if rec.Code != 200 {
		t.Fatalf("create share: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Token string `json:"token"`
		URL   string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Token, out.URL
}

func TestShareLinks(t *testing.T) {
	srv, p, _, f, h := shareHub(t)

	// creating a share requires sign-in
	if rec := do(t, h, "POST", "/api/p/"+p.ID+"/shares", map[string]string{"path": "wiki/report.html"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated share create: %d, want 401", rec.Code)
	}

	token, url := authedShare(t, srv, h, p.ID, "wiki/report.html")
	if !strings.Contains(url, "/s/"+token) {
		t.Fatalf("url = %q", url)
	}

	// the public link needs NO auth and renders the HTML, sandboxed
	rec := do(t, h, "GET", "/s/"+token, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<h1>Q3</h1>") {
		t.Fatalf("public fetch: %d %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q, want html (rendered)", ct)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "sandbox") {
		t.Fatalf("shared HTML must be sandboxed, CSP = %q", csp)
	}

	// sharing the same file again returns the SAME link
	token2, _ := authedShare(t, srv, h, p.ID, "wiki/report.html")
	if token2 != token {
		t.Fatalf("re-share minted a new token: %s vs %s", token2, token)
	}

	// the link serves the LATEST content after the file changes
	f.put("dev1", "wiki/report.html", "<h1>Q4 update</h1>")
	rec = do(t, h, "GET", "/s/"+token, nil)
	if !strings.Contains(rec.Body.String(), "Q4 update") {
		t.Fatalf("share must serve latest content, got %s", rec.Body)
	}

	// unknown tokens and unsynced paths
	if rec := do(t, h, "GET", "/s/ffffffffffffffffffffffffffffffff", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown token: %d, want 404", rec.Code)
	}
	req := jsonReq(t, "POST", "/api/p/"+p.ID+"/shares", map[string]string{"path": "never-synced.md"})
	authAs(t, srv, req)
	if rec := doHTTP(h, req); rec.Code != http.StatusNotFound {
		t.Fatalf("share of unsynced file: %d, want 404", rec.Code)
	}

	// revoke kills the link
	req = jsonReq(t, "DELETE", "/api/shares/"+token, nil)
	authAs(t, srv, req)
	if rec := doHTTP(h, req); rec.Code != 200 {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", "/s/"+token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("revoked link: %d, want 404", rec.Code)
	}
}

func TestShareMarkdownRendersAndExpires(t *testing.T) {
	srv, p, sharesPath, _, h := shareHub(t)

	// markdown renders as a full page
	token, _ := authedShare(t, srv, h, p.ID, "wiki/notes.md")
	rec := do(t, h, "GET", "/s/"+token, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<strong>team</strong>") ||
		!strings.Contains(rec.Body.String(), "<!doctype html>") {
		t.Fatalf("markdown share: %d %s", rec.Code, rec.Body)
	}
	// download variant attaches raw
	rec = do(t, h, "GET", "/s/"+token+"?download=1", nil)
	if !strings.Contains(rec.Header().Get("Content-Disposition"), "notes.md") ||
		!strings.Contains(rec.Body.String(), "**team**") {
		t.Fatalf("download variant: %v %s", rec.Header(), rec.Body)
	}

	// expiring shares die on time
	req := jsonReq(t, "POST", "/api/p/"+p.ID+"/shares", map[string]string{"path": "wiki/notes.md", "expires_in": "1ms"})
	authAs(t, srv, req)
	recC := doHTTP(h, req)
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(recC.Body.Bytes(), &out); err != nil || out.Token == "" {
		t.Fatalf("expiring create: %d %s", recC.Code, recC.Body)
	}
	time.Sleep(5 * time.Millisecond)
	if rec := do(t, h, "GET", "/s/"+out.Token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("expired link: %d, want 404", rec.Code)
	}

	// list shows live links only, and persists across a registry reload
	req = jsonReq(t, "GET", "/api/p/"+p.ID+"/shares", nil)
	authAs(t, srv, req)
	rec = doHTTP(h, req)
	if !strings.Contains(rec.Body.String(), token) || strings.Contains(rec.Body.String(), out.Token) {
		t.Fatalf("list = %s", rec.Body)
	}
	db2, err := OpenShareDB(sharesPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := db2.Get(token); !ok {
		t.Fatal("share lost across reload")
	}
}

// helpers

// TestShareLastUpdatedStamp: a share page promises the latest version, so a
// markdown page says when latest was — and nothing else gets injected into.
func TestShareLastUpdatedStamp(t *testing.T) {
	srv, p, _, f, h := shareHub(t)
	const rawHTML = "<h1>Q3</h1><script>alert(1)</script>"
	const pdf = "%PDF-1.4 fake\n"
	f.put("dev1", "wiki/deck.pdf", pdf)

	// markdown: the stamp is the FILE's time, human date + precise title=
	when := time.Date(2026, 3, 14, 9, 26, 53, 0, time.UTC)
	f.putAt("dev1", "wiki/notes.md", "# Notes\n\nhello **team**", when)
	token, _ := authedShare(t, srv, h, p.ID, "wiki/notes.md")
	rec := do(t, h, "GET", "/s/"+token, nil)
	body := rec.Body.String()
	if !strings.Contains(body, "Last updated 14 Mar 2026") {
		t.Fatalf("no last-updated stamp: %s", body)
	}
	if !strings.Contains(body, `title="2026-03-14T09:26:53Z"`) {
		t.Fatalf("no precise timestamp: %s", body)
	}
	// ...and it sits above the content, not after it
	if strings.Index(body, "Last updated") > strings.Index(body, "<strong>team</strong>") {
		t.Fatal("stamp must render before the content")
	}
	// the sandbox + footer survive
	if csp := rec.Header().Get("Content-Security-Policy"); csp != "sandbox allow-scripts allow-popups" {
		t.Fatalf("CSP = %q", csp)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("share headers = %v", rec.Header())
	}
	if !strings.Contains(body, "Shared with <a") {
		t.Fatal("footer went missing")
	}

	// re-syncing the file moves the stamp; the token does not change
	f.putAt("dev1", "wiki/notes.md", "# Notes\n\nhello **team**", when.AddDate(0, 0, 40))
	rec = do(t, h, "GET", "/s/"+token, nil)
	if !strings.Contains(rec.Body.String(), "Last updated 23 Apr 2026") {
		t.Fatalf("stamp must track the file, got %s", rec.Body)
	}

	// zero time: no stamp at all, not a 1970 date
	f.putAt("dev1", "wiki/notes.md", "# Notes\n\nhello **team**", time.Time{})
	rec = do(t, h, "GET", "/s/"+token, nil)
	if strings.Contains(rec.Body.String(), "Last updated") || strings.Contains(rec.Body.String(), `class="updated"`) {
		t.Fatalf("zero time must print no stamp, got %s", rec.Body)
	}

	// HTML shares are served byte-for-byte — never injected into
	htmlTok, _ := authedShare(t, srv, h, p.ID, "wiki/report.html")
	rec = do(t, h, "GET", "/s/"+htmlTok, nil)
	if !bytes.Equal(rec.Body.Bytes(), []byte(rawHTML)) {
		t.Fatalf("html share must be byte-identical, got %q", rec.Body)
	}

	// so are binaries, Content-Length included
	pdfTok, _ := authedShare(t, srv, h, p.ID, "wiki/deck.pdf")
	rec = do(t, h, "GET", "/s/"+pdfTok, nil)
	if !bytes.Equal(rec.Body.Bytes(), []byte(pdf)) {
		t.Fatalf("binary share must be unchanged, got %q", rec.Body)
	}
	if cl := rec.Header().Get("Content-Length"); cl != fmt.Sprint(len(pdf)) {
		t.Fatalf("Content-Length = %q, want %d", cl, len(pdf))
	}
}

func jsonReq(t *testing.T, method, url string, body any) *http.Request {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		if data, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptestNewRequestBody(method, url, data)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func authAs(t *testing.T, srv *Server, req *http.Request) {
	t.Helper()
	auth := srv.Auth.(*BuiltinAuth)
	auth.mu.Lock()
	u := auth.findByEmail("s@x.io")
	auth.mu.Unlock()
	if u == nil {
		var err error
		u, err = auth.signup("s@x.io", "Sharer", "password1")
		if err != nil {
			t.Fatal(err)
		}
	}
	tok, err := auth.issueToken(u.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
}
