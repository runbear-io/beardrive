package webapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Security round 1 — scoreboard rows 7 (share links), 8 (invites & signup)
// and 9 (password & token handling). Every test asserts the SECURE behavior,
// so it goes green the moment the hole is closed.

// secshareGet issues a GET with an explicit connection address and optional
// X-Forwarded-For, which is what the /s/* rate limiter keys on.
func secshareGet(h http.Handler, target, remoteAddr, xff string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", target, nil)
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// secshareForm POSTs a urlencoded form (the /auth/* pages speak forms).
func secshareForm(h http.Handler, target string, form url.Values, c *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// secshareBearer does a JSON request carrying a device token.
func secshareBearer(h http.Handler, method, target string, body any, token string) *httptest.ResponseRecorder {
	req := jsonReqNoT(method, target, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func jsonReqNoT(method, target string, body any) *http.Request {
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	req := httptestNewRequestBody(method, target, data)
	req.Header.Set("Content-Type", "application/json")
	return req
}

// ---------------------------------------------------------------------------
// Row 7 — share links
// ---------------------------------------------------------------------------

// A share token is a public bearer URL for a file. The org-wide share audit
// (/api/orgs/{org}/shares) checks only org membership, so a member who has
// been explicitly denied a project (PermNone) is still handed the tokens of
// that project's shares — and with them the file contents the denial exists
// to withhold. The per-project list (/api/p/{id}/shares) refuses them
// correctly; that delta is the finding.
func TestSec_Share_OrgAuditLeaksDeniedProjectTokens(t *testing.T) {
	h, srv, c, p := permHub(t)

	// alice cuts bob off from the project entirely.
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions/bob@x.io",
		map[string]string{"level": PermNone}, c["alice"]); rec.Code != 200 {
		t.Fatalf("deny bob: %d %s", rec.Code, rec.Body)
	}
	sh, err := srv.Shares.Create(p.ID, "secret/salaries.md", "alice@x.io", 0, FileInfo{})
	if err != nil {
		t.Fatal(err)
	}

	// Control: the project's own share list correctly refuses bob.
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/shares", nil, c["bob"]); rec.Code != http.StatusForbidden {
		t.Fatalf("per-project share list for a denied member: %d %s, want 403", rec.Code, rec.Body)
	}
	// Control: alice, who may read the project, does see it.
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/shares", nil, c["alice"]); !strings.Contains(rec.Body.String(), sh.Token) {
		t.Fatalf("owner cannot see her own share: %d %s", rec.Code, rec.Body)
	}

	// Attack: the org-wide audit hands the same token to the denied member.
	rec := doAs(t, h, "GET", "/api/orgs/"+p.Org+"/shares", nil, c["bob"])
	if strings.Contains(rec.Body.String(), sh.Token) || strings.Contains(rec.Body.String(), "salaries.md") {
		t.Fatalf("org share audit leaked a denied project's share token to bob: %d %s", rec.Code, rec.Body)
	}

	// dave is in no org at all and must see nothing.
	if rec := doAs(t, h, "GET", "/api/orgs/"+p.Org+"/shares", nil, c["dave"]); rec.Code != http.StatusForbidden {
		t.Fatalf("non-member org share audit: %d %s, want 403", rec.Code, rec.Body)
	}
}

// The /s/* token bucket is the only thing between a stranger and unlimited
// reads of every shared file on the hub. clientIP() trusts X-Forwarded-For
// from any peer with no trusted-proxy configuration, so one connection that
// varies the header gets a fresh bucket on every request.
func TestSec_Share_RateLimitIgnoresSpoofedForwardedFor(t *testing.T) {
	srv, p, _, _, h := shareHub(t)
	srv.ShareRPM = 1 // burst floor is 10, so request 11 must be refused
	token, _ := authedShare(t, srv, h, p.ID, "wiki/notes.md")

	const attacker = "203.0.113.9:44321"

	// Attack: one connection, a different forwarded hop each time.
	spoofed := 0
	for i := 0; i < 40; i++ {
		rec := secshareGet(h, "/s/"+token, attacker, "10.0.0."+string(rune('0'+i%10))+".1")
		if rec.Code != http.StatusTooManyRequests {
			spoofed++
		}
	}

	// Control: the same connection without the header IS throttled, which
	// proves the limiter works and the delta above is the header's doing.
	plain := 0
	for i := 0; i < 40; i++ {
		if rec := secshareGet(h, "/s/"+token, attacker, ""); rec.Code != http.StatusTooManyRequests {
			plain++
		}
	}
	if plain >= 40 {
		t.Fatalf("harness problem: unspoofed requests were never throttled (%d/40 served)", plain)
	}
	if spoofed >= 40 {
		t.Fatalf("X-Forwarded-For defeats the /s/* rate limit: %d/40 spoofed requests served, %d/40 unspoofed", spoofed, plain)
	}
}

// Every /s/* response must carry the sandbox CSP, not just the successful
// ones: the header is what keeps shared content in an opaque origin, and a
// browser that gets one response without it has no way to know the next was
// meant to be sandboxed. handleShared sets the header only after the share,
// the volume, and the file have all resolved.
func TestSec_Share_ErrorResponsesKeepSandboxCSP(t *testing.T) {
	srv, p, _, f, h := shareHub(t)
	token, _ := authedShare(t, srv, h, p.ID, "wiki/notes.md")

	sandboxed := func(t *testing.T, what string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
			t.Errorf("%s (%d) carries no sandbox CSP: %q", what, rec.Code, csp)
		}
	}

	// Control: the happy path is sandboxed.
	sandboxed(t, "served share", secshareGet(h, "/s/"+token, "", ""))

	// Unknown / revoked token.
	sandboxed(t, "unknown token", secshareGet(h, "/s/"+strings.Repeat("a", 32), "", ""))

	// The share exists but the file is gone from the project.
	f.del("dev1", "wiki/notes.md")
	sandboxed(t, "vanished file", secshareGet(h, "/s/"+token, "", ""))

	// Rate-limited response.
	srv.ShareRPM = 1
	var limited *httptest.ResponseRecorder
	for i := 0; i < 40; i++ {
		rec := secshareGet(h, "/s/"+token, "198.51.100.7:1", "")
		if rec.Code == http.StatusTooManyRequests {
			limited = rec
			break
		}
	}
	if limited == nil {
		t.Fatal("harness problem: never reached the rate limit")
	}
	sandboxed(t, "rate-limited", limited)
}

// DELETE /api/shares/{token} lives outside the proj() wrapper and checks
// itself — but only when Shares.Get resolves the token, and Get filters out
// expired shares. An expired share is still in the registry, so the delete
// goes through for anyone signed in, including an account in another org,
// and the 200/404 split confirms whether the token ever existed.
func TestSec_Share_OutsiderCannotRevokeExpiredShare(t *testing.T) {
	h, srv, c, p := permHub(t)
	sh, err := srv.Shares.Create(p.ID, "wiki/notes.md", "alice@x.io", time.Millisecond, FileInfo{})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)

	// Control: an unknown token is a plain 404 for the same caller.
	if rec := doAs(t, h, "DELETE", "/api/shares/"+strings.Repeat("b", 32), nil, c["dave"]); rec.Code == 200 {
		t.Fatalf("harness problem: unknown token deleted: %s", rec.Body)
	}

	// Attack: dave belongs to no org on this hub and has no permission on
	// alice's project.
	rec := doAs(t, h, "DELETE", "/api/shares/"+sh.Token, nil, c["dave"])
	if rec.Code == http.StatusOK {
		t.Fatalf("outsider revoked an expired share in another org's project: %d %s (want 403/404)", rec.Code, rec.Body)
	}
}

// Minting and killing a public link are the same authority: a read-only
// member and a non-member must not be able to revoke or re-date one.
func TestSec_Share_LiveShareMutationNeedsWrite(t *testing.T) {
	h, srv, c, p := permHub(t)
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions/bob@x.io",
		map[string]string{"level": PermRead}, c["alice"]); rec.Code != 200 {
		t.Fatalf("demote bob: %d %s", rec.Code, rec.Body)
	}
	sh, err := srv.Shares.Create(p.ID, "wiki/notes.md", "alice@x.io", 0, FileInfo{})
	if err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"bob", "dave"} {
		for _, m := range []string{"DELETE", "PATCH"} {
			rec := doAs(t, h, m, "/api/shares/"+sh.Token, map[string]string{"expires_in": "1h"}, c[who])
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s /api/shares/{token} as %s: %d %s, want 403", m, who, rec.Code, rec.Body)
			}
		}
	}
	// ...and nothing above half-succeeded: the share is still live and still
	// permanent. (Checked on the registry, not /s/ — this fixture's project
	// has no synced content, so /s/ 404s for an unrelated reason.)
	got, ok := srv.Shares.Get(sh.Token)
	if !ok {
		t.Fatal("a refused revoke still killed the link")
	}
	if !got.Expires.IsZero() {
		t.Fatalf("a refused PATCH still re-dated the link: expires %v", got.Expires)
	}
}

// A revoked or expired token must be dead on the public surface.
func TestSec_Share_RevokedAndExpiredTokensAreDead(t *testing.T) {
	srv, p, _, _, h := shareHub(t)

	expiring, err := srv.Shares.Create(p.ID, "wiki/notes.md", "s@x.io", time.Millisecond, FileInfo{})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if rec := secshareGet(h, "/s/"+expiring.Token, "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("expired token served: %d", rec.Code)
	}

	live, _ := authedShare(t, srv, h, p.ID, "wiki/report.html")
	srv.Shares.Revoke(live)
	if rec := secshareGet(h, "/s/"+live, "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("revoked token served: %d", rec.Code)
	}

	// Tokens are 128 bits of crypto/rand, hex-encoded, and never repeat.
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		s, err := srv.Shares.Create(p.ID, "wiki/notes.md", "s@x.io", time.Hour, FileInfo{})
		if err != nil {
			t.Fatal(err)
		}
		if len(s.Token) != 32 {
			t.Fatalf("share token %q is %d hex chars, want 32 (128 bits)", s.Token, len(s.Token))
		}
		if seen[s.Token] {
			t.Fatalf("share token repeated: %s", s.Token)
		}
		seen[s.Token] = true
	}
}

// A public share must never hand a browser an authenticated hub session.
func TestSec_Share_NoAuthCookieOnPublicResponse(t *testing.T) {
	srv, p, _, _, h := shareHub(t)
	token, _ := authedShare(t, srv, h, p.ID, "wiki/notes.md")
	for _, target := range []string{"/s/" + token, "/s/" + token + "?download=1", "/s/" + strings.Repeat("c", 32)} {
		rec := secshareGet(h, target, "", "")
		for _, ck := range rec.Result().Cookies() {
			t.Errorf("GET %s set cookie %q", target, ck.Name)
		}
	}
}

// ---------------------------------------------------------------------------
// Row 8 — invites & signup
// ---------------------------------------------------------------------------

// secshareInviteHub is an invite-only hub (allow_signup:false) with a real
// org directory wired to the signup page, matching cmd/bdrive/web.go.
func secshareInviteHub(t *testing.T) (*Server, *BuiltinAuth, *OrgDB, http.Handler) {
	t.Helper()
	srv, auth, _ := authHub(t, false)
	orgs, err := OpenOrgDB(filepath.Join(t.TempDir(), "orgs.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Dir = LocalDirectory{OrgDB: orgs}
	auth.InviteValid = orgs.ValidInvite
	return srv, auth, orgs, srv.Handler()
}

// signupInvited skips the domain allowlist and the approval/verification
// gates, so the invite token is the whole vetting: a forged, expired, or
// revoked one must not create an account on an invite-only hub.
func TestSec_Invite_ForgedExpiredRevokedCannotCreateAccount(t *testing.T) {
	srv, auth, orgs, h := secshareInviteHub(t)
	auth.AllowedDomains = []string{"runbear.io"}
	auth.RequireApproval = true
	org, err := orgs.Create("acme", "owner@runbear.io")
	if err != nil {
		t.Fatal(err)
	}
	_ = srv

	trySignup := func(next, email string) *httptest.ResponseRecorder {
		return secshareForm(h, "/auth/signup?next="+url.QueryEscape(next),
			url.Values{"email": {email}, "name": {"N"}, "password": {"password1"}}, nil)
	}

	// Forged token.
	trySignup("/join/"+strings.Repeat("d", 32), "forged@evil.example")
	if auth.findByEmail("forged@evil.example") != nil {
		t.Error("a forged invite token created an account")
	}

	// Revoked token.
	revoked, err := orgs.CreateInvite(org.ID, "owner@runbear.io", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	orgs.RevokeInvite(revoked.Token)
	trySignup("/join/"+revoked.Token, "revoked@evil.example")
	if auth.findByEmail("revoked@evil.example") != nil {
		t.Error("a revoked invite token created an account")
	}

	// Expired token.
	stale, err := orgs.CreateInvite(org.ID, "owner@runbear.io", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	trySignup("/join/"+stale.Token, "stale@evil.example")
	if auth.findByEmail("stale@evil.example") != nil {
		t.Error("an expired invite token created an account")
	}

	// No token at all.
	trySignup("/", "nobody@runbear.io")
	if auth.findByEmail("nobody@runbear.io") != nil {
		t.Error("signup with no invite created an account on an invite-only hub")
	}

	// Control: a live invite does work, so the refusals above are the
	// server's decision and not a broken fixture.
	good, err := orgs.CreateInvite(org.ID, "owner@runbear.io", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if rec := trySignup("/join/"+good.Token, "guest@elsewhere.example"); rec.Code != http.StatusSeeOther {
		t.Fatalf("live invite signup: %d %s", rec.Code, rec.Body)
	}
	if u := auth.findByEmail("guest@elsewhere.example"); u == nil || !u.active() {
		t.Fatalf("live invite did not produce an active account: %+v", u)
	}
}

// An invite is scoped to one org: redeeming it must join that org and no
// other, and a revoked token must stop working for redemption too.
func TestSec_Invite_RedemptionIsOrgScopedAndRevocable(t *testing.T) {
	srv, _, orgs, h := secshareInviteHub(t)
	_ = srv
	orgA, _ := orgs.Create("alpha", "alice@x.io")
	orgB, _ := orgs.Create("beta", "boss@x.io")

	// mallory needs an account: mint an invite to A, sign up through it,
	// then try to reach B with it.
	inv, err := orgs.CreateInvite(orgA.ID, "alice@x.io", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rec := secshareForm(h, "/auth/signup?next="+url.QueryEscape("/join/"+inv.Token),
		url.Values{"email": {"mallory@x.io"}, "name": {"M"}, "password": {"password1"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("invited signup: %d %s", rec.Code, rec.Body)
	}
	var session *http.Cookie
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == sessionCookie {
			session = ck
		}
	}
	if session == nil {
		t.Fatal("invited signup set no session")
	}

	if rec := doAs(t, h, "POST", "/api/invites/"+inv.Token, nil, session); rec.Code != 200 {
		t.Fatalf("redeem: %d %s", rec.Code, rec.Body)
	}
	if orgs.Role(orgA.ID, "mallory@x.io") != RoleMember {
		t.Fatal("redeeming did not join the invite's org")
	}
	if r := orgs.Role(orgB.ID, "mallory@x.io"); r != "" {
		t.Fatalf("an org A invite put mallory in org B as %q", r)
	}
	// The invite never grants ownership.
	if orgs.Role(orgA.ID, "mallory@x.io") == RoleOwner {
		t.Fatal("redeeming an invite made the joiner an owner")
	}

	// Revoked invites stop redeeming.
	dead, _ := orgs.CreateInvite(orgB.ID, "boss@x.io", time.Hour)
	orgs.RevokeInvite(dead.Token)
	if rec := doAs(t, h, "POST", "/api/invites/"+dead.Token, nil, session); rec.Code != http.StatusNotFound {
		t.Fatalf("revoked invite redeemed: %d %s, want 404", rec.Code, rec.Body)
	}
	if r := orgs.Role(orgB.ID, "mallory@x.io"); r != "" {
		t.Fatalf("a revoked invite still joined org B as %q", r)
	}

	// An unauthenticated caller cannot redeem at all.
	if rec := do(t, h, "POST", "/api/invites/"+inv.Token, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous redeem: %d, want 401", rec.Code)
	}
}

// Only an org owner mints or reads invite links: an invite token in the
// wrong hands is an account on the hub.
func TestSec_Invite_OnlyOwnersMintAndListLinks(t *testing.T) {
	h, _, c, p := permHub(t)
	for _, who := range []string{"bob", "dave"} {
		if rec := doAs(t, h, "POST", "/api/orgs/"+p.Org+"/invites", nil, c[who]); rec.Code != http.StatusForbidden {
			t.Errorf("%s minted an invite: %d %s", who, rec.Code, rec.Body)
		}
		if rec := doAs(t, h, "GET", "/api/orgs/"+p.Org+"/invites", nil, c[who]); rec.Code != http.StatusForbidden {
			t.Errorf("%s listed invites: %d %s", who, rec.Code, rec.Body)
		}
	}
	rec := doAs(t, h, "POST", "/api/orgs/"+p.Org+"/invites", nil, c["alice"])
	if rec.Code != 200 {
		t.Fatalf("owner mint: %d %s", rec.Code, rec.Body)
	}
	var out struct{ Token string }
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Token) != 32 {
		t.Fatalf("invite token %q is %d hex chars, want 32 (128 bits)", out.Token, len(out.Token))
	}
	// A non-owner cannot revoke someone else's link either.
	if rec := doAs(t, h, "DELETE", "/api/orgs/"+p.Org+"/invites/"+out.Token, nil, c["bob"]); rec.Code != http.StatusForbidden {
		t.Errorf("member revoked an invite: %d %s", rec.Code, rec.Body)
	}
}

// The CLI one-time codes must be single-use: an exchange or a device poll
// that already handed out a token must never hand out a second one, and a
// device code must yield nothing before a human approves it.
func TestSec_Invite_CLIOneTimeCodesAreNotReplayable(t *testing.T) {
	srv, _, _ := authHub(t, true)
	h := srv.Handler()
	session := signupAndSession(t, h, "alice@x.io", "Alice", "password1")

	// --- browser flow: /auth/cli -> code -> /api/auth/exchange ---
	pkceQ, pkceV := pkceParams()
	rec := doAs(t, h, "POST", "/auth/cli?redirect="+url.QueryEscape("http://127.0.0.1:9999/callback")+"&state=s1"+pkceQ, nil, session)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approve cli login: %d %s", rec.Code, rec.Body)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in %q", rec.Header().Get("Location"))
	}
	if got := do(t, h, "POST", "/api/auth/exchange", map[string]string{"code": code, "device": "d1", "code_verifier": pkceV}); got.Code != 200 {
		t.Fatalf("first exchange: %d %s", got.Code, got.Body)
	}
	if got := do(t, h, "POST", "/api/auth/exchange", map[string]string{"code": code, "device": "d1", "code_verifier": pkceV}); got.Code != http.StatusUnauthorized {
		t.Fatalf("replayed exchange: %d %s, want 401", got.Code, got.Body)
	}
	// A code that was never issued buys nothing.
	if got := do(t, h, "POST", "/api/auth/exchange", map[string]string{"code": strings.Repeat("e", 32)}); got.Code != http.StatusUnauthorized {
		t.Fatalf("forged exchange code: %d, want 401", got.Code)
	}
	// The browser flow only redirects to loopback.
	if got := doAs(t, h, "POST", "/auth/cli?redirect="+url.QueryEscape("http://evil.example/callback"), nil, session); got.Code != http.StatusBadRequest {
		t.Fatalf("off-host cli redirect: %d %s, want 400", got.Code, got.Body)
	}

	// --- device flow: start -> poll (pending) -> approve -> poll -> replay ---
	rec = do(t, h, "POST", "/api/auth/device/start", map[string]string{"device": "laptop", "os": "linux"})
	if rec.Code != 200 {
		t.Fatalf("device start: %d %s", rec.Code, rec.Body)
	}
	var start struct{ Code string }
	json.Unmarshal(rec.Body.Bytes(), &start)
	if len(start.Code) != 32 {
		t.Fatalf("device code %q is %d hex chars, want 32 (128 bits)", start.Code, len(start.Code))
	}

	poll := func() *httptest.ResponseRecorder {
		return do(t, h, "POST", "/api/auth/device/poll", map[string]string{"code": start.Code})
	}
	if got := poll(); got.Code != 200 || !strings.Contains(got.Body.String(), "pending") {
		t.Fatalf("poll before approval: %d %s, want pending", got.Code, got.Body)
	}
	// An unapproved code must not mint a token even after many polls.
	for i := 0; i < 5; i++ {
		if got := poll(); strings.Contains(got.Body.String(), "bdt_") {
			t.Fatalf("unapproved device poll minted a token: %s", got.Body)
		}
	}
	if got := doAs(t, h, "POST", "/auth/device/"+start.Code, nil, session); got.Code != 200 {
		t.Fatalf("approve device: %d %s", got.Code, got.Body)
	}
	first := poll()
	if first.Code != 200 || !strings.Contains(first.Body.String(), "bdt_") {
		t.Fatalf("approved poll: %d %s", first.Code, first.Body)
	}
	if got := poll(); got.Code != http.StatusUnauthorized {
		t.Fatalf("replayed device poll: %d %s, want 401", got.Code, got.Body)
	}
}

// ---------------------------------------------------------------------------
// Row 9 — password & token handling
// ---------------------------------------------------------------------------

// A password reset is the documented way to recover a stolen account, so it
// has to end the thief's access: every session cookie and device token
// issued under the old password must stop authenticating.
func TestSec_Password_ResetRevokesExistingTokens(t *testing.T) {
	srv, auth, _ := authHub(t, true)
	h := srv.Handler()
	session := signupAndSession(t, h, "victim@x.io", "Victim", "password1")
	u := auth.findByEmail("victim@x.io")
	if u == nil {
		t.Fatal("no account")
	}
	// The thief's device token, minted while they had the password.
	stolen, err := auth.issueToken(u.ID, "thief-laptop")
	if err != nil {
		t.Fatal(err)
	}
	if rec := secshareBearer(h, "GET", "/api/auth/me", nil, stolen); rec.Code != 200 {
		t.Fatalf("harness problem: stolen token does not work to begin with: %d %s", rec.Code, rec.Body)
	}

	// The victim resets their password. newGrant is exactly what POST
	// /auth/reset does; going through it is the only way to learn the token,
	// which otherwise only reaches the mailbox.
	grant := auth.newGrant("reset", u.ID, time.Hour)
	rec := secshareForm(h, "/auth/reset/confirm",
		url.Values{"token": {grant}, "password": {"brand-new-password"}}, nil)
	if !strings.Contains(rec.Body.String(), "Password updated") {
		t.Fatalf("reset confirm: %d %s", rec.Code, rec.Body)
	}
	if auth.verifyPassword("victim@x.io", "brand-new-password") == nil {
		t.Fatal("harness problem: the password did not actually change")
	}

	if rec := secshareBearer(h, "GET", "/api/auth/me", nil, stolen); rec.Code != http.StatusUnauthorized {
		t.Errorf("device token issued before the reset still authenticates: %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "GET", "/api/auth/me", nil, session); rec.Code != http.StatusUnauthorized {
		t.Errorf("browser session from before the reset still authenticates: %d %s", rec.Code, rec.Body)
	}
}

// A reset link is one-use and short-lived, and it can only ever re-key the
// account it was minted for.
func TestSec_Password_ResetGrantIsSingleUseAndExpires(t *testing.T) {
	srv, auth, _ := authHub(t, true)
	h := srv.Handler()
	signupAndSession(t, h, "a@x.io", "A", "password1")
	signupAndSession(t, h, "b@x.io", "B", "password1")
	a, b := auth.findByEmail("a@x.io"), auth.findByEmail("b@x.io")

	grant := auth.newGrant("reset", a.ID, time.Hour)
	if rec := secshareForm(h, "/auth/reset/confirm",
		url.Values{"token": {grant}, "password": {"first-new-pass"}}, nil); !strings.Contains(rec.Body.String(), "Password updated") {
		t.Fatalf("first use: %s", rec.Body)
	}
	// Replay must not re-key the account a third time.
	rec := secshareForm(h, "/auth/reset/confirm",
		url.Values{"token": {grant}, "password": {"replayed-pass"}}, nil)
	if strings.Contains(rec.Body.String(), "Password updated") {
		t.Fatal("a reset link worked twice")
	}
	if auth.verifyPassword("a@x.io", "replayed-pass") != nil {
		t.Fatal("the replayed reset changed the password")
	}
	if auth.verifyPassword("a@x.io", "first-new-pass") == nil {
		t.Fatal("the first reset did not stick")
	}

	// Expired grants are refused.
	stale := auth.newGrant("reset", a.ID, time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	if rec := secshareForm(h, "/auth/reset/confirm",
		url.Values{"token": {stale}, "password": {"expired-pass"}}, nil); strings.Contains(rec.Body.String(), "Password updated") {
		t.Fatal("an expired reset link worked")
	}
	// A grant minted for a is never usable against b, whatever is posted.
	if auth.verifyPassword("b@x.io", "first-new-pass") != nil {
		t.Fatal("a's reset re-keyed b")
	}
	// Forged grant ids buy nothing.
	if rec := secshareForm(h, "/auth/reset/confirm",
		url.Values{"token": {strings.Repeat("f", 32)}, "password": {"forged-pass"}}, nil); strings.Contains(rec.Body.String(), "Password updated") {
		t.Fatal("a forged reset token worked")
	}
	if auth.verifyPassword("b@x.io", "forged-pass") != nil || auth.verifyPassword("a@x.io", "forged-pass") != nil {
		t.Fatal("a forged reset token changed a password")
	}
	_ = b
}

// Login and password-reset must answer identically whether or not the
// account exists — otherwise the hub is an account-enumeration oracle for
// anyone who can reach the sign-in page.
func TestSec_Password_LoginAndResetDoNotEnumerateAccounts(t *testing.T) {
	srv, _, _ := authHub(t, true)
	h := srv.Handler()
	signupAndSession(t, h, "known@x.io", "Known", "password1")

	// The page echoes the submitted address back into the form, so blank it
	// out before comparing: the finding would be any OTHER difference.
	normalize := func(body, email string) string {
		return strings.ReplaceAll(body, email, "SUBMITTED")
	}

	for _, path := range []string{"/auth/login", "/auth/reset"} {
		form := func(email string) url.Values {
			v := url.Values{"email": {email}}
			if path == "/auth/login" {
				v.Set("password", "definitely-wrong-password")
			}
			return v
		}
		known := secshareForm(h, path, form("known@x.io"), nil)
		absent := secshareForm(h, path, form("nobody@x.io"), nil)
		if known.Code != absent.Code {
			t.Errorf("POST %s: status %d for a known account vs %d for an unknown one", path, known.Code, absent.Code)
		}
		k := normalize(known.Body.String(), "known@x.io")
		a := normalize(absent.Body.String(), "nobody@x.io")
		if k != a {
			t.Errorf("POST %s: response body differs for a known vs unknown account\nknown:  %s\nabsent: %s", path, k, a)
		}
	}
}

// Nothing that authenticates or hashes a credential may reach a client.
func TestSec_Password_NoCredentialMaterialInResponses(t *testing.T) {
	srv, auth, _ := authHub(t, true)
	h := srv.Handler()
	session := signupAndSession(t, h, "leak@x.io", "Leak", "password1")
	u := auth.findByEmail("leak@x.io")
	tok, err := auth.issueToken(u.ID, "cli")
	if err != nil {
		t.Fatal(err)
	}

	bad := []string{"$2a$", "$2b$", u.Pass, hashToken(tok), "password1"}
	check := func(what, body string) {
		for _, needle := range bad {
			if needle != "" && strings.Contains(body, needle) {
				t.Errorf("%s leaked credential material (%q): %s", what, needle, body)
			}
		}
	}
	check("/api/auth/me (cookie)", doAs(t, h, "GET", "/api/auth/me", nil, session).Body.String())
	check("/api/auth/me (bearer)", secshareBearer(h, "GET", "/api/auth/me", nil, tok).Body.String())
	check("login page", secshareForm(h, "/auth/login",
		url.Values{"email": {"leak@x.io"}, "password": {"password1"}}, nil).Body.String())
	check("/api/config", do(t, h, "GET", "/api/config", nil).Body.String())
}
