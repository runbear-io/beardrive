package webapp

import (
	"bytes"
	"encoding/json"
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
func shareHub(t *testing.T) (*Server, Project, *fakeRemote, http.Handler) {
	t.Helper()
	srv, p, root := newHub(t, true, nil)
	auth, err := OpenBuiltinAuth(filepath.Join(t.TempDir(), "auth.json"), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.Auth = auth
	srv.Shares, err = OpenShareDB(filepath.Join(t.TempDir(), "shares.json"))
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.put("dev1", "wiki/report.html", "<h1>Q3</h1><script>alert(1)</script>")
	f.put("dev1", "wiki/notes.md", "# Notes\n\nhello **team**")
	return srv, p, f, srv.Handler()
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
	srv, p, f, h := shareHub(t)

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
	srv, p, _, h := shareHub(t)

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
	db2, err := OpenShareDB(srv.Shares.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := db2.Get(token); !ok {
		t.Fatal("share lost across reload")
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
