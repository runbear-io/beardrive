package webapp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Security round 2 — the rows round 1 reported green without exercising the
// dangerous case:
//
//	rows 5/6  the /store/* and /upload/* routes under a REAL permission model
//	          (round 1 ran them on newHub, which has no Auth and no Dir, so
//	          projectPerm short-circuited to PermAdmin) and under a DEVICE
//	          TOKEN, which is how every sync client actually authenticates
//	row 1     forged, tampered, foreign and dead credentials
//	row 7     a share that outlived its minter's access
//	row 8     CheckSeat on invite redemption
//
// Every test asserts the SECURE behavior, so it goes green the moment the
// hole is closed and stays as a regression guard.
//
// Fixture is permHub (perms_test.go): alice owns the org and the project,
// bob and carol are plain org members, dave is in a different org.

// ---------------------------------------------------------------------------
// helpers (all prefixed secauthz — one file, no collisions)
// ---------------------------------------------------------------------------

// secauthzReq builds a request; a []byte body is sent raw (store/upload
// content), anything else is JSON.
func secauthzReq(method, target string, body any) *http.Request {
	var data []byte
	switch b := body.(type) {
	case nil:
	case []byte:
		data = b
	default:
		data, _ = json.Marshal(b)
	}
	req := httptestNewRequestBody(method, target, data)
	req.Header.Set("Content-Type", "application/json")
	return req
}

// secauthzDo issues one request carrying a device token, a session cookie, or
// no credential at all. It never touches *testing.T so it is safe to call
// from a goroutine.
func secauthzDo(h http.Handler, method, target string, body any, tok string, c *http.Cookie) *httptest.ResponseRecorder {
	req := secauthzReq(method, target, body)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// secauthzToken mints a device token for an existing account — the credential
// `bdrive login` stores and every sync cycle presents.
func secauthzToken(t *testing.T, srv *Server, email, device string) string {
	t.Helper()
	auth, ok := srv.Auth.(*BuiltinAuth)
	if !ok {
		t.Fatal("hub is not using BuiltinAuth")
	}
	auth.mu.Lock()
	u := auth.findByEmail(email)
	auth.mu.Unlock()
	if u == nil {
		t.Fatalf("no account %s", email)
	}
	tok, err := auth.issueToken(u.ID, device)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// secauthzRoute is one request shape against a project, with the permission
// level the route table declares for it.
type secauthzRoute struct {
	name, method, target string
	body                 any
	level                string
	// journal writes must name their device (one journal, one writer), so the
	// only refusal left in the answer is the permission one.
	device string
}

// secauthzStoreUpload returns every /store/* and /upload/* route: the two
// families a device and a browser write a project through.
func secauthzStoreUpload(id string) []secauthzRoute {
	base := "/api/p/" + id + "/"
	sha := strings.Repeat("a", 64)
	return []secauthzRoute{
		{"store/list", "GET", base + "store/list?prefix=journal/", nil, PermRead, ""},
		{"store/object GET", "GET", base + "store/object?key=journal/dev1.jsonl", nil, PermRead, ""},
		{"store/exists", "GET", base + "store/exists?key=journal/dev1.jsonl", nil, PermRead, ""},
		{"store/sign", "POST", base + "store/sign", map[string]any{"key": "blobs/" + sha, "size": 1}, PermWrite, ""},
		{"store/object PUT", "PUT", base + "store/object?key=journal/dev1.jsonl", []byte(`{"seq":1}`), PermWrite, "dev1"},
		{"upload/init", "POST", base + "upload/init", map[string]any{"path": "x.md", "sha256": sha, "size": 1}, PermWrite, ""},
		{"upload/content", "PUT", base + "upload/content?path=x.md", []byte("hi"), PermWrite, ""},
		{"upload/commit", "POST", base + "upload/commit", map[string]any{"path": "x.md", "sha256": sha, "size": 1}, PermWrite, ""},
	}
}

// secauthzCall runs one route with a credential.
func secauthzCall(h http.Handler, rt secauthzRoute, tok string, c *http.Cookie) *httptest.ResponseRecorder {
	req := secauthzReq(rt.method, rt.target, rt.body)
	if rt.device != "" {
		req.Header.Set("X-Bdrive-Device", rt.device)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// secauthzUpload puts a file in the project so shares and reads have
// something real to point at.
func secauthzUpload(t *testing.T, h http.Handler, projectID, path, content string, c *http.Cookie) {
	t.Helper()
	rec := secauthzDo(h, "PUT", "/api/p/"+projectID+"/upload/content?path="+path, []byte(content), "", c)
	if rec.Code != 200 {
		t.Fatalf("seed upload %s: %d %s", path, rec.Code, rec.Body)
	}
}

// ---------------------------------------------------------------------------
// Rows 5 + 6 — /store/* and /upload/* under a real permission model, reached
// with the credential a sync client actually uses.
// ---------------------------------------------------------------------------

// Round 1 exercised these routes on newHub — no Auth, no Dir — where
// projectPerm returns PermAdmin for everyone, so every check it thought it
// was testing was vacuous. Here they run on permHub, once with a session
// cookie and once with a device token (the Bearer path, never before held to
// the permission model at all).
//
// The admin column is the control: the same request, same shape, from someone
// allowed to make it must NOT be 403, which is what proves a 403 elsewhere is
// the server's authorization decision and not a malformed request.
func TestSec_Perms_StoreAndUploadRoutesUnderDeviceToken(t *testing.T) {
	h, srv, c, p := permHub(t)
	if err := srv.Projects.SetPerm(p.ID, "bob@x.io", PermRead); err != nil {
		t.Fatal(err)
	}
	tokens := map[string]string{
		"alice": secauthzToken(t, srv, "alice@x.io", "alice-laptop"),
		"bob":   secauthzToken(t, srv, "bob@x.io", "bob-laptop"),
		"dave":  secauthzToken(t, srv, "dave@x.io", "dave-laptop"),
	}

	for _, cred := range []string{"token", "cookie"} {
		call := func(rt secauthzRoute, who string) *httptest.ResponseRecorder {
			if cred == "token" {
				return secauthzCall(h, rt, tokens[who], nil)
			}
			return secauthzCall(h, rt, "", c[who])
		}
		for _, rt := range secauthzStoreUpload(p.ID) {
			// control: an admin's identical request is not a 403
			if rec := call(rt, "alice"); rec.Code == http.StatusForbidden {
				t.Fatalf("[%s] harness problem: admin refused on %s: %d %s", cred, rt.name, rec.Code, rec.Body)
			}
			// a read-only member may read and must not write
			rec := call(rt, "bob")
			if rt.level == PermWrite && rec.Code != http.StatusForbidden {
				t.Errorf("[%s] read-only member %s %s: %d, want 403", cred, rt.method, rt.name, rec.Code)
			}
			if rt.level == PermRead && rec.Code == http.StatusForbidden {
				t.Errorf("[%s] read-only member refused a read route %s: %s", cred, rt.name, rec.Body)
			}
			// an outsider reaches nothing
			if rec := call(rt, "dave"); rec.Code != http.StatusForbidden {
				t.Errorf("[%s] outsider %s %s: %d, want 403", cred, rt.method, rt.name, rec.Code)
			}
			// and no credential at all is refused before any of that
			if rec := secauthzCall(h, rt, "", nil); rec.Code != http.StatusUnauthorized {
				t.Errorf("[anon] %s %s: %d, want 401", rt.method, rt.name, rec.Code)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Row 1 — forged, tampered, foreign and dead credentials.
// ---------------------------------------------------------------------------

// Round 1 only attacked the anonymous/open-path half of the gate. A credential
// that is present but not genuine must be refused exactly like no credential:
// 401, never a fall-through to some other identity and never an accepted one.
func TestSec_AuthGate_ForgedAndTamperedCredentialsRefused(t *testing.T) {
	h, srv, c, p := permHub(t)
	good := secauthzToken(t, srv, "alice@x.io", "alice-laptop")

	// A token minted by a DIFFERENT hub for the same email. Same shape, same
	// account name, foreign issuer: it must mean nothing here.
	otherHub, err := OpenBuiltinAuth(filepath.Join(t.TempDir(), "auth.json"), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	ou, err := otherHub.signup("alice@x.io", "Alice", "password1")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := otherHub.issueToken(ou.ID, "alice-laptop")
	if err != nil {
		t.Fatal(err)
	}

	flip := func(s string) string { // tamper with one character
		b := []byte(s)
		if b[len(b)-1] == 'a' {
			b[len(b)-1] = 'b'
		} else {
			b[len(b)-1] = 'a'
		}
		return string(b)
	}
	aliceID := ""
	if auth, ok := srv.Auth.(*BuiltinAuth); ok {
		auth.mu.Lock()
		aliceID = auth.findByEmail("alice@x.io").ID
		auth.mu.Unlock()
	}

	bad := []struct {
		name  string
		apply func(*http.Request)
	}{
		{"no credential", func(*http.Request) {}},
		{"forged bearer token", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer bdt_"+randHex(20))
		}},
		{"tampered bearer token", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+flip(good))
		}},
		{"stored digest replayed as a token", func(r *http.Request) {
			// auth.json holds sha256(token); leaking that file must not be
			// the same as leaking the tokens.
			r.Header.Set("Authorization", "Bearer "+hashToken(good))
		}},
		{"token from another hub", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+foreign)
		}},
		{"account id as a bearer token", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+aliceID)
		}},
		{"tampered session cookie", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: flip(c["alice"].Value)})
		}},
		{"account id as a session cookie", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: aliceID})
		}},
		{"foreign hub token as a session cookie", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: foreign})
		}},
		{"empty bearer", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer ")
		}},
		{"bearer with no scheme", func(r *http.Request) {
			r.Header.Set("Authorization", good)
		}},
		{"lowercase scheme", func(r *http.Request) {
			r.Header.Set("Authorization", "bearer "+good)
		}},
		{"basic scheme", func(r *http.Request) {
			r.Header.Set("Authorization", "Basic "+good)
		}},
		{"trailing junk after the token", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+good+" extra")
		}},
		{"double space after the scheme", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer  "+good)
		}},
		{
			// A malformed Authorization header must not be ignored in favor of
			// a cookie that happens to be attached: the request asserted an
			// identity it could not prove, so it fails closed.
			"bad bearer alongside a valid cookie", func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer "+flip(good))
				r.AddCookie(c["alice"])
			},
		},
	}

	targets := []struct{ method, target string }{
		{"GET", "/api/p/" + p.ID + "/tree"},
		{"GET", "/api/p/" + p.ID + "/store/list?prefix=journal/"},
		{"PUT", "/api/p/" + p.ID + "/upload/content?path=x.md"},
		{"GET", "/api/projects"},
		{"GET", "/api/orgs"},
	}
	for _, tg := range targets {
		// control: the genuine credential works on this exact target
		if rec := secauthzDo(h, tg.method, tg.target, []byte("hi"), good, nil); rec.Code == http.StatusUnauthorized {
			t.Fatalf("harness problem: genuine token refused on %s: %s", tg.target, rec.Body)
		}
		for _, b := range bad {
			req := secauthzReq(tg.method, tg.target, []byte("hi"))
			b.apply(req)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with %s: %d, want 401 — body %s",
					tg.method, tg.target, b.name, rec.Code, strings.TrimSpace(rec.Body.String()))
			}
		}
	}
}

// A credential outlives nothing: not the account it names, and not that
// account's membership of the org whose project it reaches.
func TestSec_AuthGate_CredentialDiesWithAccountAndMembership(t *testing.T) {
	h, srv, _, p := permHub(t)
	auth := srv.Auth.(*BuiltinAuth)

	// carol's device token, minted while she is a member in good standing
	carolTok := secauthzToken(t, srv, "carol@x.io", "carol-laptop")
	if rec := secauthzDo(h, "GET", "/api/p/"+p.ID+"/tree", nil, carolTok, nil); rec.Code != 200 {
		t.Fatalf("harness problem: carol's token does not work to begin with: %d %s", rec.Code, rec.Body)
	}

	// Removed from the org, the token still authenticates the account but must
	// reach nothing of that org's.
	if err := srv.Dir.RemoveMember(p.Org, "carol@x.io"); err != nil {
		t.Fatal(err)
	}
	if rec := secauthzDo(h, "GET", "/api/p/"+p.ID+"/tree", nil, carolTok, nil); rec.Code != http.StatusForbidden {
		t.Errorf("removed member's device token still reads the project: %d %s", rec.Code, rec.Body)
	}
	if rec := secauthzDo(h, "GET", "/api/p/"+p.ID+"/store/list?prefix=journal/", nil, carolTok, nil); rec.Code != http.StatusForbidden {
		t.Errorf("removed member's device token still syncs the project: %d %s", rec.Code, rec.Body)
	}

	// bob's account is deleted (what POST /api/admin/pending/{id}/deny does).
	// Every credential it minted must be dead, not merely un-listed.
	bobTok := secauthzToken(t, srv, "bob@x.io", "bob-laptop")
	auth.mu.Lock()
	bobID := auth.findByEmail("bob@x.io").ID
	auth.mu.Unlock()
	if rec := secauthzDo(h, "GET", "/api/auth/me", nil, bobTok, nil); rec.Code != 200 {
		t.Fatalf("harness problem: bob's token does not work to begin with: %d %s", rec.Code, rec.Body)
	}
	if err := auth.Deny(bobID); err != nil {
		t.Fatal(err)
	}
	if rec := secauthzDo(h, "GET", "/api/auth/me", nil, bobTok, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("token of a deleted account still authenticates: %d %s", rec.Code, rec.Body)
	}
	if rec := secauthzDo(h, "GET", "/api/p/"+p.ID+"/tree", nil, bobTok, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("token of a deleted account still reaches a project: %d %s", rec.Code, rec.Body)
	}
}

// The token an account is left with after `bdrive login` is minted by the CLI
// exchange, not by issueToken — so the password reset's revocation has to
// reach that one too. Same claim round 1 proved for a hand-minted token, made
// against the real flow a CLI walks.
func TestSec_Password_ResetKillsCLIIssuedToken(t *testing.T) {
	h, srv, c, p := permHub(t)
	auth := srv.Auth.(*BuiltinAuth)

	// the real browser-callback flow: approve at /auth/cli, then exchange the
	// one-time code for a device token
	pkceQ, pkceV := pkceParams()
	rec := secauthzDo(h, "POST", "/auth/cli?redirect="+url.QueryEscape("http://127.0.0.1:53123/callback")+"&state=xyz"+pkceQ, nil, "", c["bob"])
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("cli approval: %d %s", rec.Code, rec.Body)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no one-time code in %s", rec.Header().Get("Location"))
	}
	rec = secauthzDo(h, "POST", "/api/auth/exchange", map[string]string{"code": code, "device": "bob-cli", "code_verifier": pkceV}, "", nil)
	var out struct {
		Token string `json:"token"`
	}
	mustJSON(t, rec, &out)
	if rec := secauthzDo(h, "GET", "/api/p/"+p.ID+"/tree", nil, out.Token, nil); rec.Code != 200 {
		t.Fatalf("harness problem: CLI token does not work to begin with: %d %s", rec.Code, rec.Body)
	}

	// bob resets his password — the documented recovery from a stolen laptop.
	auth.mu.Lock()
	bobID := auth.findByEmail("bob@x.io").ID
	auth.mu.Unlock()
	grant := auth.newGrant("reset", bobID, time.Hour)
	form := url.Values{"token": {grant}, "password": {"a-brand-new-password"}}
	req := httptest.NewRequest("POST", "/auth/reset/confirm", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), "Password updated") {
		t.Fatalf("reset confirm: %d %s", rr.Code, rr.Body)
	}

	if rec := secauthzDo(h, "GET", "/api/p/"+p.ID+"/tree", nil, out.Token, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("CLI device token survived the password reset: %d %s", rec.Code, rec.Body)
	}
	if rec := secauthzDo(h, "GET", "/api/p/"+p.ID+"/tree", nil, "", c["bob"]); rec.Code != http.StatusUnauthorized {
		t.Errorf("browser session survived the password reset: %d %s", rec.Code, rec.Body)
	}
}

// ---------------------------------------------------------------------------
// Row 7 — a share that outlived its minter's access.
// ---------------------------------------------------------------------------

// Minting and killing a public link are the same authority (PermWrite), so an
// account that has lost it keeps neither: a demoted member can no longer
// re-date or revoke the link they created, and cannot list the project's
// links at all once denied. Nothing here asserts what happens to the LINK —
// only to its minter's control over it.
func TestSec_Share_DemotedMinterCannotManageTheirLink(t *testing.T) {
	h, srv, c, p := permHub(t)
	secauthzUpload(t, h, p.ID, "secret.md", "# internal roadmap", c["bob"])

	rec := secauthzDo(h, "POST", "/api/p/"+p.ID+"/shares", map[string]string{"path": "secret.md"}, "", c["bob"])
	var sh struct {
		Token string `json:"token"`
	}
	mustJSON(t, rec, &sh)

	// alice demotes bob to read-only
	if rec := secauthzDo(h, "PUT", "/api/p/"+p.ID+"/permissions/bob@x.io", map[string]string{"level": PermRead}, "", c["alice"]); rec.Code != 200 {
		t.Fatalf("demote: %d %s", rec.Code, rec.Body)
	}
	for _, tc := range []struct {
		method, target string
		body           any
	}{
		{"PATCH", "/api/shares/" + sh.Token, map[string]string{"expires_in": "1h"}},
		{"DELETE", "/api/shares/" + sh.Token, nil},
	} {
		if rec := secauthzDo(h, tc.method, tc.target, tc.body, "", c["bob"]); rec.Code != http.StatusForbidden {
			t.Errorf("demoted minter %s %s: %d, want 403 — %s", tc.method, tc.target, rec.Code, rec.Body)
		}
	}
	// control: an admin's identical request works
	if rec := secauthzDo(h, "PATCH", "/api/shares/"+sh.Token, map[string]string{"expires_in": "1h"}, "", c["alice"]); rec.Code != 200 {
		t.Fatalf("harness problem: admin cannot re-date the link: %d %s", rec.Code, rec.Body)
	}

	// denied outright: he cannot even see the project's links any more
	if err := srv.Projects.SetPerm(p.ID, "bob@x.io", PermNone); err != nil {
		t.Fatal(err)
	}
	if rec := secauthzDo(h, "GET", "/api/p/"+p.ID+"/shares", nil, "", c["bob"]); rec.Code != http.StatusForbidden {
		t.Errorf("denied minter lists the project's shares: %d %s", rec.Code, rec.Body)
	}
}

// Offboarding must offboard. A share is a public grant made on the org's data
// by one of its members; when that member is removed from the org, the grant
// is the last live channel they still hold into it — /s/<token> serves the
// file's LATEST content forever, so an ex-member who minted links on their way
// out keeps reading the org's data as it changes, with no credential at all.
//
// Round 1 settled the same question one layer down: a project grant that
// outlived org membership was a hole (TestSec_Perms_RemovedOrgMemberLosesProjectAccess),
// because RemoveMember did not walk the grants. It does not walk the shares
// either.
func TestSec_Share_RemovedOrgMemberLinkStopsServing(t *testing.T) {
	h, srv, c, p := permHub(t)
	secauthzUpload(t, h, p.ID, "secret.md", "# internal roadmap", c["bob"])

	rec := secauthzDo(h, "POST", "/api/p/"+p.ID+"/shares", map[string]string{"path": "secret.md"}, "", c["bob"])
	var sh struct {
		Token string `json:"token"`
	}
	mustJSON(t, rec, &sh)
	if rec := secauthzDo(h, "GET", "/s/"+sh.Token, nil, "", nil); rec.Code != 200 {
		t.Fatalf("harness problem: the link does not serve to begin with: %d %s", rec.Code, rec.Body)
	}

	// bob leaves the company
	if rec := secauthzDo(h, "DELETE", "/api/orgs/"+p.Org+"/members/bob@x.io", nil, "", c["alice"]); rec.Code != 200 {
		t.Fatalf("remove member: %d %s", rec.Code, rec.Body)
	}
	if role := srv.Dir.Role(p.Org, "bob@x.io"); role != "" {
		t.Fatalf("harness problem: bob is still %q in the org", role)
	}

	// he keeps no control over the link...
	for _, tc := range []struct {
		method, target string
		body           any
	}{
		{"PATCH", "/api/shares/" + sh.Token, map[string]string{"expires_in": "1h"}},
		{"DELETE", "/api/shares/" + sh.Token, nil},
		{"GET", "/api/p/" + p.ID + "/shares", nil},
	} {
		if rec := secauthzDo(h, tc.method, tc.target, tc.body, "", c["bob"]); rec.Code != http.StatusForbidden {
			t.Errorf("removed member %s %s: %d, want 403 — %s", tc.method, tc.target, rec.Code, rec.Body)
		}
	}

	// ...and the link he minted must not keep serving the org's live content.
	if rec := secauthzDo(h, "GET", "/s/"+sh.Token, nil, "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("a share minted by an account since removed from the org still serves the file: %d, content present=%t",
			rec.Code, strings.Contains(rec.Body.String(), "internal roadmap"))
	}

	// The remediation path has to exist either way: an owner can still see the
	// orphaned link and revoke it.
	rec = secauthzDo(h, "GET", "/api/p/"+p.ID+"/shares", nil, "", c["alice"])
	if !strings.Contains(rec.Body.String(), sh.Token) {
		t.Errorf("owner cannot see the orphaned link to revoke it: %d %s", rec.Code, rec.Body)
	}
	if rec := secauthzDo(h, "DELETE", "/api/shares/"+sh.Token, nil, "", c["alice"]); rec.Code != 200 {
		t.Errorf("owner cannot revoke the orphaned link: %d %s", rec.Code, rec.Body)
	}
	if rec := secauthzDo(h, "GET", "/s/"+sh.Token, nil, "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("revoked link still serves: %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Row 8 — CheckSeat on invite redemption.
// ---------------------------------------------------------------------------

// secauthzQuota is a QuotaProvider that enforces a real seat cap, and can be
// told to hold callers inside CheckSeat so two redemptions overlap.
type secauthzQuota struct {
	// Embedded so the read-side hooks (CheckRead/RecordEgress) come for
	// free: this fake exercises the write path, and a widened interface
	// should not need a no-op added here every time.
	UnlimitedQuota

	limit int

	mu      sync.Mutex
	seats   []int // members count as the server reported it, per call
	overlap bool  // hold the first caller until a second one arrives
	both    chan struct{}
	closed  bool
}

func newSecauthzQuota(limit int) *secauthzQuota {
	return &secauthzQuota{limit: limit, both: make(chan struct{})}
}

func (q *secauthzQuota) CheckWrite(string, int64) error { return nil }
func (q *secauthzQuota) RecordUsage(string, int64)      {}

func (q *secauthzQuota) calls() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.seats)
}

func (q *secauthzQuota) CheckSeat(_ string, members int) error {
	q.mu.Lock()
	q.seats = append(q.seats, members)
	n := len(q.seats)
	hold := q.overlap
	q.mu.Unlock()
	if hold {
		// The window under test is between the count the server read and the
		// member it then adds. Nothing here changes that window; it only makes
		// the interleaving deterministic. A server that held one lock across
		// check-and-add would never get a second caller in here, the first
		// would fall through on the timeout, and the second would then see the
		// updated count.
		if n >= 2 {
			q.mu.Lock()
			if !q.closed {
				q.closed = true
				close(q.both)
			}
			q.mu.Unlock()
		} else {
			select {
			case <-q.both:
			case <-time.After(2 * time.Second):
			}
		}
	}
	if members >= q.limit {
		return fmt.Errorf("seat limit reached")
	}
	return nil
}

// secauthzInvite mints an org invite as the owner and returns its token.
func secauthzInvite(t *testing.T, h http.Handler, org string, owner *http.Cookie) string {
	t.Helper()
	rec := secauthzDo(h, "POST", "/api/orgs/"+org+"/invites", nil, "", owner)
	var out struct {
		Token string `json:"token"`
	}
	mustJSON(t, rec, &out)
	return out.Token
}

// Seat enforcement is one CheckSeat call on one route. It has to fire for
// every account that actually joins, honour a refusal (no membership, no
// invite-use bump), and not be reachable around: an account that is already a
// member consumes no new seat, and a refused join must not leave the account
// in the org anyway.
func TestSec_Invite_SeatCheckCannotBeSkipped(t *testing.T) {
	h, srv, c, p := permHub(t)
	q := newSecauthzQuota(99)
	srv.Quota = q
	before := len(secauthzOrgMembers(t, srv, p.Org))
	tok := secauthzInvite(t, h, p.Org, c["alice"])

	// a refused seat leaves the org exactly as it was
	q.limit = before
	rec := secauthzDo(h, "POST", "/api/invites/"+tok, nil, "", c["dave"])
	if rec.Code != http.StatusForbidden {
		t.Fatalf("join over the seat cap: %d %s, want 403", rec.Code, rec.Body)
	}
	if q.calls() != 1 {
		t.Fatalf("CheckSeat calls = %d, want 1", q.calls())
	}
	if got := len(secauthzOrgMembers(t, srv, p.Org)); got != before {
		t.Errorf("a seat-refused join still added the member: %d members, want %d", got, before)
	}
	if role := srv.Dir.Role(p.Org, "dave@x.io"); role != "" {
		t.Errorf("seat-refused account is in the org as %q", role)
	}
	// and it reaches nothing of the org's
	if rec := secauthzDo(h, "GET", "/api/p/"+p.ID+"/tree", nil, "", c["dave"]); rec.Code != http.StatusForbidden {
		t.Errorf("seat-refused account reads the project: %d %s", rec.Code, rec.Body)
	}

	// with room, the same call joins — the delta proving the 403 was the seat
	q.limit = before + 1
	if rec := secauthzDo(h, "POST", "/api/invites/"+tok, nil, "", c["dave"]); rec.Code != 200 {
		t.Fatalf("join with a free seat: %d %s", rec.Code, rec.Body)
	}
	// an existing member re-opening the link buys no seat
	calls := q.calls()
	if rec := secauthzDo(h, "POST", "/api/invites/"+tok, nil, "", c["dave"]); rec.Code != 200 {
		t.Fatalf("re-join: %d %s", rec.Code, rec.Body)
	}
	if q.calls() != calls {
		t.Errorf("re-join of an existing member consulted CheckSeat again")
	}
	if got := len(secauthzOrgMembers(t, srv, p.Org)); got != before+1 {
		t.Errorf("members after one join and one re-join = %d, want %d", got, before+1)
	}
}

// Two people opening the same invite link at once must not both take the last
// seat. CheckSeat reads the member count and handleInviteAccept adds the
// member afterwards, with nothing holding the two together, so a hub can be
// pushed past a paid seat cap by clicking a link twice at the same moment.
func TestSec_Invite_SeatCheckIsAtomic(t *testing.T) {
	h, srv, c, p := permHub(t)
	erin := signupAndSession(t, h, "erin@x.io", "Erin", "password1")

	members := len(secauthzOrgMembers(t, srv, p.Org))
	q := newSecauthzQuota(members + 1) // exactly one seat left
	q.overlap = true
	srv.Quota = q
	tok := secauthzInvite(t, h, p.Org, c["alice"])

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, who := range []*http.Cookie{c["dave"], erin} {
		wg.Add(1)
		go func(i int, ck *http.Cookie) {
			defer wg.Done()
			codes[i] = secauthzDo(h, "POST", "/api/invites/"+tok, nil, "", ck).Code
		}(i, who)
	}
	wg.Wait()

	if codes[0] != 200 && codes[1] != 200 {
		t.Fatalf("harness problem: neither join succeeded (%v)", codes)
	}
	if got := len(secauthzOrgMembers(t, srv, p.Org)); got > q.limit {
		t.Errorf("two concurrent redemptions took %d seats past the cap: org has %d members, limit %d (responses %v)",
			got-q.limit, got, q.limit, codes)
	}
}

// orgMembers reads the org's current membership straight from the directory.
func secauthzOrgMembers(t *testing.T, srv *Server, org string) map[string]string {
	t.Helper()
	o, ok := srv.Dir.Get(org)
	if !ok {
		t.Fatalf("no org %s", org)
	}
	out := make(map[string]string, len(o.Members))
	for k, v := range o.Members {
		out[k] = v
	}
	return out
}
