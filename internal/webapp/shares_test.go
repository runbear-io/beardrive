package webapp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
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

// A Revoke button sits on every row of the public-links table, so the row
// order must not move between loads. byToken is a map, so List has to sort.
func TestShareListIsDeterministic(t *testing.T) {
	db, err := OpenShareDB(filepath.Join(t.TempDir(), "shares.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Hand-set Created — Create stamps time.Now(), which never collides, so
	// going through it would leave the tie-break path untested.
	tick := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	for _, s := range []Share{
		{Token: "t3", Project: "p", Path: "z.md", Created: tick},
		{Token: "t2", Project: "p", Path: "a.md", Created: tick}, // same instant as t3
		{Token: "t1", Project: "p", Path: "newest.md", Created: tick.Add(time.Hour)},
		{Token: "t0", Project: "p", Path: "oldest.md", Created: tick.Add(-time.Hour)},
		{Token: "x", Project: "other", Path: "a.md", Created: tick},
		{Token: "dead", Project: "p", Path: "gone.md", Created: tick, Expires: tick},
	} {
		db.byToken[s.Token] = s
		if err := db.repo.Put(s); err != nil {
			t.Fatal(err)
		}
	}

	first := db.List("p")
	want := []string{"t1", "t2", "t3", "t0"} // created desc, then path asc (a.md < z.md)
	got := make([]string, len(first))
	for i, s := range first {
		got[i] = s.Token
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v (newest first, path tie-break)", got, want)
	}
	for i := 0; i < 10; i++ {
		if !reflect.DeepEqual(db.List("p"), first) {
			t.Fatalf("call %d reordered: %v vs %v", i, db.List("p"), first)
		}
	}
	// A project with no shares still returns nil, not an empty slice.
	if db.List("nobody") != nil {
		t.Fatal("empty project should list as nil")
	}
}

// Both share-listing APIs — the project settings table and the org-wide audit
// — must hand back the same bytes on consecutive reads.
func TestShareListAPIsAreStable(t *testing.T) {
	h, srv, c, p := permHub(t)
	// Mint directly: the HTTP route requires a synced file, and this test is
	// about ordering, not about the mint path.
	for _, path := range []string{"b.md", "a.md", "c.md"} {
		if _, err := srv.Shares.Create(p.ID, path, "alice@x.io", 0); err != nil {
			t.Fatal(err)
		}
	}
	for _, url := range []string{"/api/p/" + p.ID + "/shares", "/api/orgs/" + p.Org + "/shares"} {
		first := doAs(t, h, "GET", url, nil, c["alice"])
		if first.Code != 200 {
			t.Fatalf("GET %s: %d %s", url, first.Code, first.Body)
		}
		if !strings.Contains(first.Body.String(), "a.md") {
			t.Fatalf("GET %s listed no shares: %s", url, first.Body)
		}
		for i := 0; i < 5; i++ {
			again := doAs(t, h, "GET", url, nil, c["alice"])
			if again.Body.String() != first.Body.String() {
				t.Fatalf("GET %s moved between loads:\n%s\n%s", url, first.Body, again.Body)
			}
		}
	}
	// The newest link is the first row — what you just minted is what you are
	// most likely to want to undo.
	rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/shares", nil, c["alice"])
	var out struct {
		Shares []struct {
			Path string `json:"path"`
		} `json:"shares"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Shares) != 3 || out.Shares[0].Path != "c.md" {
		t.Fatalf("newest share is not first: %s", rec.Body)
	}
}

// helpers

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
