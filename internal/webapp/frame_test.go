package webapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// frameHub is permHub with a report page, files beside and below it, and one
// file outside its folder.
func frameHub(t *testing.T) (http.Handler, *Server, map[string]*http.Cookie, Project) {
	t.Helper()
	h, srv, c, p, root := permHubAt(t)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.put("seed", "site/index.html", `<img src="pic.png"><img src="img/deep.png">`)
	f.put("seed", "site/pic.png", "png-bytes")
	f.put("seed", "site/img/deep.png", "deep-bytes")
	f.put("seed", "site/secret/key.png", "secret-bytes")
	f.put("seed", "outside.png", "outside-bytes")
	return h, srv, c, p
}

func frameURL(t *testing.T, h http.Handler, p Project, page string, c *http.Cookie) string {
	t.Helper()
	rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/frame?path="+url.QueryEscape(page), nil, c)
	if rec.Code != 200 {
		t.Fatalf("frame %s: %d %s", page, rec.Code, rec.Body)
	}
	var out struct{ URL string }
	json.Unmarshal(rec.Body.Bytes(), &out)
	return out.URL
}

// What a sandboxed page sends: no cookie at all.
func cookieless(h http.Handler, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
	return rec
}

func resolveRef(t *testing.T, base, ref string) string {
	t.Helper()
	b, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := url.Parse(ref)
	return b.ResolveReference(r).String()
}

func TestFramedPageLoadsItsRelativeFilesWithoutACookie(t *testing.T) {
	h, _, c, p := frameHub(t)
	page := frameURL(t, h, p, "site/index.html", c["bob"])

	rec := cookieless(h, page)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `src="pic.png"`) {
		t.Fatalf("page: %d %s", rec.Code, rec.Body)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp != inlineSandbox {
		t.Errorf("page CSP = %q, want the inline sandbox", csp)
	}
	for ref, want := range map[string]string{"pic.png": "png-bytes", "img/deep.png": "deep-bytes"} {
		rec := cookieless(h, resolveRef(t, page, ref))
		if rec.Code != 200 || rec.Body.String() != want {
			t.Errorf("%s: %d %q", ref, rec.Code, rec.Body)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
			t.Errorf("%s: Content-Type %q", ref, ct)
		}
	}
}

// The token reads the page's folder and nothing above it.
func TestFrameTokenStaysInThePagesFolder(t *testing.T) {
	h, _, c, p := frameHub(t)
	page := frameURL(t, h, p, "site/index.html", c["bob"])
	tok := strings.Split(strings.TrimPrefix(page, "/api/p/"+p.ID+"/raw/"), "/")[0]

	for _, target := range []string{
		resolveRef(t, page, "../outside.png"),
		"/api/p/" + p.ID + "/raw/" + tok + "/outside.png",
		"/api/p/" + p.ID + "/raw/" + tok + "/site/%2E%2E/outside.png",
	} {
		if rec := cookieless(h, target); rec.Code == 200 {
			t.Errorf("%s: served %q outside the page's folder", target, rec.Body)
		}
	}
}

func TestFrameTokenIsCheckedOnEveryRequest(t *testing.T) {
	h, srv, c, p := frameHub(t)

	if rec := cookieless(h, "/api/p/"+p.ID+"/raw/forged.token/site/pic.png"); rec.Code != http.StatusUnauthorized {
		t.Errorf("forged token: %d", rec.Code)
	}

	page := frameURL(t, h, p, "site/index.html", c["bob"])
	tampered := strings.Replace(page, "/raw/", "/raw/A", 1)
	if rec := cookieless(h, tampered); rec.Code != http.StatusUnauthorized {
		t.Errorf("tampered token: %d", rec.Code)
	}

	expired, err := srv.signFrame(context.Background(), frameClaims{
		Project: p.ID, Dir: "site", Email: "bob@x.io", Expires: time.Now().Add(-time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec := cookieless(h, "/api/p/"+p.ID+"/raw/"+expired+"/site/pic.png"); rec.Code != http.StatusUnauthorized {
		t.Errorf("expired token: %d", rec.Code)
	}
}

// The token carries an identity, not a permission: what the reader may read is
// decided at request time, so revoking access or narrowing a folder applies to
// a page that is already open.
func TestFrameTokenReadsAsItsReaderNow(t *testing.T) {
	h, srv, c, p := frameHub(t)
	page := frameURL(t, h, p, "site/index.html", c["carol"])

	if rec := cookieless(h, resolveRef(t, page, "secret/key.png")); rec.Code != 200 {
		t.Fatalf("before the rule: %d", rec.Code)
	}
	rule := map[string]any{"prefix": "site/secret", "default": PermNone}
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/folders", rule, c["alice"]); rec.Code != 200 {
		t.Fatalf("set folder rule: %d %s", rec.Code, rec.Body)
	}
	if rec := cookieless(h, resolveRef(t, page, "secret/key.png")); rec.Code != http.StatusNotFound {
		t.Errorf("hidden folder: %d %q", rec.Code, rec.Body)
	}

	if err := srv.Projects.SetPerm(p.ID, "carol@x.io", PermNone); err != nil {
		t.Fatal(err)
	}
	if rec := cookieless(h, resolveRef(t, page, "pic.png")); rec.Code == 200 {
		t.Errorf("access revoked, still served %q", rec.Body)
	}
}

func TestFrameTokenIsBoundToItsProject(t *testing.T) {
	h, _, c, p := frameHub(t)
	page := frameURL(t, h, p, "site/index.html", c["alice"])

	rec := doAs(t, h, "POST", "/api/projects", map[string]string{"name": "other"}, c["alice"])
	var out struct{ Project Project }
	json.Unmarshal(rec.Body.Bytes(), &out)
	moved := strings.Replace(page, "/api/p/"+p.ID+"/", "/api/p/"+out.Project.ID+"/", 1)
	if rec := cookieless(h, moved); rec.Code == 200 {
		t.Errorf("token for %s served from %s", p.ID, out.Project.ID)
	}
}

// Minting a frame is reading the page, so it answers like /file does.
func TestFrameMintingNeedsTheReader(t *testing.T) {
	h, _, c, p := frameHub(t)
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/frame?path=site/index.html", nil, c["dave"]); rec.Code == 200 {
		t.Errorf("outsider minted a frame: %s", rec.Body)
	}
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/frame?path=site/nope.html", nil, c["bob"]); rec.Code != http.StatusNotFound {
		t.Errorf("missing page: %d", rec.Code)
	}
	if rec := cookieless(h, "/api/p/"+p.ID+"/frame?path=site/index.html"); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out: %d", rec.Code)
	}
}

// The file a raw URL serves is the one its path names: a ?path= in the query
// cannot point the token somewhere else.
func TestFrameQueryCannotRedirectThePath(t *testing.T) {
	h, _, c, p := frameHub(t)
	page := frameURL(t, h, p, "site/index.html", c["bob"])
	rec := cookieless(h, resolveRef(t, page, "pic.png?path=outside.png"))
	if rec.Code != 200 || rec.Body.String() != "png-bytes" {
		t.Errorf("%d %q", rec.Code, rec.Body)
	}
}

// A hub is several processes: a token one of them minted verifies on another
// that shares its storage.
func TestFrameKeyIsSharedThroughStorage(t *testing.T) {
	_, srv, _, p := frameHub(t)
	tok, err := srv.signFrame(context.Background(), frameClaims{
		Project: p.ID, Dir: "site", Email: "bob@x.io", Expires: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	other := &Server{Root: srv.Root}
	if _, ok := other.verifyFrame(context.Background(), tok); !ok {
		t.Fatal("a second process on the same storage rejected the token")
	}
}
