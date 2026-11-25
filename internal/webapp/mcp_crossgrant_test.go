package webapp

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

/* The 2026-11-25 Muse failure: consent succeeded, then the token exchange
   answered invalid_grant "grant has been revoked", every time.

   authorize put the new grant only in the consenting process's map, while the
   code went to shared storage. The exchange landed on another instance (prod
   runs several), or on the same one after a store re-read rebuilt the maps,
   and issue() found no grant. These tests drive the real handlers on two
   MCPAuths sharing one store, which is the shape prod has. */

type mcpPair struct{ a, b *http.ServeMux }

func newMCPPair(t *testing.T) (*MCPAuth, *MCPAuth, mcpPair) {
	dir := t.TempDir()
	grants := newFileMCPRepo(filepath.Join(dir, "mcp.json"))
	pending := newFilePendingRepo(filepath.Join(dir, "pending.json"))
	sess := func(*http.Request) (User, bool) { return User{Email: "snow@x.io"}, true }
	projs := func(string) []MCPProject {
		return []MCPProject{{Project: Project{ID: "p1", Name: "P"}, Level: PermWrite}}
	}
	mk := func() (*MCPAuth, *http.ServeMux) {
		m, err := NewMCPAuth(grants, pending, sess, projs)
		if err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		m.Register(mux)
		return m, mux
	}
	a, ma := mk()
	b, mb := mk()
	return a, b, mcpPair{ma, mb}
}

const crossRedirect = "https://muse.example/cb"

func mcpDo(mux *http.ServeMux, method, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if method == "POST" && !strings.HasPrefix(target, "/oauth/register") {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func registerClient(t *testing.T, mux *http.ServeMux) string {
	rec := mcpDo(mux, "POST", "/oauth/register", `{"client_name":"Muse","redirect_uris":["`+crossRedirect+`"]}`)
	var reg struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reg); err != nil || reg.ClientID == "" {
		t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
	return reg.ClientID
}

// consent POSTs a selection on mux and returns the code from the redirect.
func consent(t *testing.T, mux *http.ServeMux, clientID, verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {clientID}, "redirect_uri": {crossRedirect}, "response_type": {"code"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}
	rec := mcpDo(mux, "POST", "/oauth/authorize?"+q.Encode(), "project=p1")
	loc, err := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusFound || err != nil || loc.Query().Get("code") == "" {
		t.Fatalf("consent: %d %s", rec.Code, rec.Body)
	}
	return loc.Query().Get("code")
}

type tokenResp struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
	Error   string `json:"error"`
	Desc    string `json:"error_description"`
}

func exchange(mux *http.ServeMux, form url.Values) (int, tokenResp) {
	rec := mcpDo(mux, "POST", "/oauth/token", form.Encode())
	var tr tokenResp
	_ = json.Unmarshal(rec.Body.Bytes(), &tr)
	return rec.Code, tr
}

func codeForm(clientID, code, verifier string) url.Values {
	return url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID},
		"code": {code}, "redirect_uri": {crossRedirect}, "code_verifier": {verifier}}
}

func refreshForm(rt string) url.Values {
	return url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}}
}

const crossVerifier = "verifier-0123456789-0123456789-0123456789"

func TestMCPConsentOnOneInstanceExchangesOnAnother(t *testing.T) {
	a, b, h := newMCPPair(t)
	cid := registerClient(t, h.a)
	code := consent(t, h.a, cid, crossVerifier)

	st, tr := exchange(h.b, codeForm(cid, code, crossVerifier))
	if st != http.StatusOK || tr.Access == "" {
		t.Fatalf("exchange on the other instance: %d %s %s — this is the Muse failure", st, tr.Error, tr.Desc)
	}
	if g, ok := b.Grant(tr.Access); !ok || g.Account != "snow@x.io" || !g.covers("p1") || g.ClientName != "Muse" {
		t.Fatalf("issued grant wrong on b: %+v %v", g, ok)
	}
	if _, ok := a.Grant(tr.Access); !ok {
		t.Fatal("token minted on b is unknown to a")
	}
	// Refresh back on the first instance works too.
	if st, tr2 := exchange(h.a, refreshForm(tr.Refresh)); st != http.StatusOK || tr2.Access == "" {
		t.Fatalf("refresh on a: %d %s", st, tr2.Desc)
	}
}

// Revoked old grant for the same client and account, then a fresh consent:
// the new code must work, and the old refresh token must stay dead.
func TestMCPFreshConsentWorksAfterRevokedGrant(t *testing.T) {
	a, b, h := newMCPPair(t)
	cid := registerClient(t, h.a)

	_, old := exchange(h.b, codeForm(cid, consent(t, h.a, cid, crossVerifier), crossVerifier))
	if old.Refresh == "" {
		t.Fatal("first connect failed")
	}
	oldGrant, _ := a.Grant(old.Access)
	if !a.Revoke("snow@x.io", oldGrant.ID) {
		t.Fatal("revoke")
	}

	st, fresh := exchange(h.a, codeForm(cid, consent(t, h.b, cid, crossVerifier), crossVerifier))
	if st != http.StatusOK || fresh.Access == "" {
		t.Fatalf("fresh consent after a revoked grant: %d %s %s", st, fresh.Error, fresh.Desc)
	}

	// The revoked grant's refresh token, tried on the instance that still had
	// it in memory and on the one that revoked it, fails cleanly.
	for name, mux := range map[string]*http.ServeMux{"b": h.b, "a": h.a} {
		if st, tr := exchange(mux, refreshForm(old.Refresh)); st != http.StatusBadRequest || tr.Error != "invalid_grant" {
			t.Fatalf("revoked refresh on %s: %d %+v", name, st, tr)
		}
	}
	if _, ok := b.Grant(old.Access); ok {
		t.Fatal("revoked access token still works")
	}
	gs, _ := a.repo.LoadGrants()
	for _, g := range gs {
		if g.ID == oldGrant.ID {
			t.Fatal("a refresh attempt wrote the revoked grant back to the store")
		}
	}
}

// The fix moves the grant into the code; PKCE and redirect checks still gate it,
// and a failed attempt still burns the code.
func TestMCPCodeExchangeChecksUnchanged(t *testing.T) {
	_, _, h := newMCPPair(t)
	cid := registerClient(t, h.a)

	code := consent(t, h.a, cid, crossVerifier)
	if st, tr := exchange(h.b, codeForm(cid, code, "wrong-verifier")); st != http.StatusBadRequest || tr.Error != "invalid_grant" {
		t.Fatalf("wrong verifier: %d %+v", st, tr)
	}
	if st, _ := exchange(h.b, codeForm(cid, code, crossVerifier)); st == http.StatusOK {
		t.Fatal("a code survived a failed exchange")
	}

	code = consent(t, h.a, cid, crossVerifier)
	f := codeForm(cid, code, crossVerifier)
	f.Set("redirect_uri", "https://evil.example/cb")
	if st, tr := exchange(h.b, f); st != http.StatusBadRequest || tr.Error != "invalid_grant" {
		t.Fatalf("redirect mismatch: %d %+v", st, tr)
	}

	code = consent(t, h.a, cid, crossVerifier)
	f = codeForm(cid, code, crossVerifier)
	f.Set("client_id", "mcpc_other")
	if st, tr := exchange(h.b, f); st != http.StatusBadRequest || tr.Error != "invalid_grant" {
		t.Fatalf("client mismatch: %d %+v", st, tr)
	}
}
