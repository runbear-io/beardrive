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

// Round 12, the adversarial member lifecycle.
//
// Four earlier rounds each found a DIFFERENT grant that outlived a DIFFERENT
// revocation — project perms (r1), share links (r2), device rows (r7), org
// roles (r10) — and each was found by accident while looking at something
// else. This file looks for the pattern on purpose: enumerate everything a
// member accumulates and check each one against each revocation path.
//
// The neighbour that gets it right is the model. shares.go resolves the
// minter's org membership at READ time (shareCreatorStillBelongs), on the
// stated grounds that "a share is the strongest grant on the hub ... so
// offboarding has to reach it: the day someone leaves, every link they minted
// stops serving". An org INVITE is strictly stronger than a share — it hands
// out membership itself, and on the default invite-only posture it also
// bootstraps the ACCOUNT that will hold it — and OrgDB.Redeem/ValidInvite
// check only the token's own expiry.

// sec12rOrgs reaches the OrgDB behind a permHub's directory.
func sec12rOrgs(t *testing.T, srv *Server) *OrgDB {
	t.Helper()
	ld, ok := srv.Dir.(LocalDirectory)
	if !ok || ld.OrgDB == nil {
		t.Fatal("fixture has no local org directory")
	}
	return ld.OrgDB
}

// sec12rMintInvite has `who` mint an org invite and returns its token.
func sec12rMintInvite(t *testing.T, h http.Handler, org string, who *http.Cookie) string {
	t.Helper()
	rec := doAs(t, h, "POST", "/api/orgs/"+org+"/invites", nil, who)
	if rec.Code != 200 {
		t.Fatalf("mint invite: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Token == "" {
		t.Fatalf("mint invite body = %s", rec.Body)
	}
	return out.Token
}

// sec12rMintShare has `who` publish a file and mint a share link on it.
func sec12rMintShare(t *testing.T, h http.Handler, project, path, body string, who *http.Cookie) string {
	t.Helper()
	if rec := doAs(t, h, "PUT", "/api/p/"+project+"/upload/content?path="+path, []byte(body), who); rec.Code != 200 {
		t.Fatalf("publish %s: %d %s", path, rec.Code, rec.Body)
	}
	rec := doAs(t, h, "POST", "/api/p/"+project+"/shares", map[string]string{"path": path}, who)
	if rec.Code != 200 {
		t.Fatalf("mint share: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Token == "" {
		t.Fatalf("mint share body = %s", rec.Body)
	}
	return out.Token
}

// An invite link is a capability to hand out org membership. It must not
// outlive the membership of the account that minted it.
//
// The delta this test proves: the SAME POST /api/invites/<token>, by an
// account outside the org, succeeds while the minter is a live owner (that is
// the feature) and must be refused once the minter has been removed. Nothing
// about the token changes in between — only bob's row in the org.
//
// The share link bob minted in the same breath is asserted alongside it, as
// the control: the hub already kills that one, through exactly the read-time
// membership resolution the invite lacks.
func TestSec_Lifecycle_AnInviteDiesWithTheMembershipThatMintedIt(t *testing.T) {
	h, srv, c, p := permHub(t)
	orgs := sec12rOrgs(t, srv)

	// Only an owner can mint an invite, so bob is promoted — the ordinary
	// shape of a departing team lead.
	if err := orgs.SetRole(p.Org, "bob@x.io", RoleOwner); err != nil {
		t.Fatal(err)
	}
	shareTok := sec12rMintShare(t, h, p.ID, "secret.md", "s3cret", c["bob"])
	inviteTok := sec12rMintInvite(t, h, p.Org, c["bob"])

	// Baseline, with bob a live owner: an outsider redeems and joins. This is
	// the authorized behaviour, and it proves the request below is well-formed.
	erin := signupAndSession(t, h, "erin@x.io", "Erin", "password1")
	if rec := doAs(t, h, "POST", "/api/invites/"+inviteTok, nil, erin); rec.Code != 200 {
		t.Fatalf("baseline redeem by an outsider: %d %s (the attack request is malformed, not refused)", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "GET", "/s/"+shareTok, nil, nil); rec.Code != 200 {
		t.Fatalf("baseline share fetch: %d %s", rec.Code, rec.Body)
	}

	// Alice offboards bob.
	if rec := doAs(t, h, "DELETE", "/api/orgs/"+p.Org+"/members/bob@x.io", nil, c["alice"]); rec.Code != 200 {
		t.Fatalf("remove bob: %d %s", rec.Code, rec.Body)
	}
	if orgs.Role(p.Org, "bob@x.io") != "" {
		t.Fatal("bob is still an org member; the offboarding step did not happen")
	}

	// Control: the share link bob minted is dead. Same accumulated capability,
	// same revocation, and this one the hub reaches.
	if rec := doAs(t, h, "GET", "/s/"+shareTok, nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("share minted by a removed member still serves: %d, want 404", rec.Code)
	}

	// The finding: the invite bob minted is still redeemable, by anyone.
	frank := signupAndSession(t, h, "frank@x.io", "Frank", "password1")
	rec := doAs(t, h, "POST", "/api/invites/"+inviteTok, nil, frank)
	if rec.Code == 200 {
		t.Errorf("an invite minted by a removed org owner is still redeemable: %d %s", rec.Code, rec.Body)
	}
	// ...and redeeming it is org membership, which is read access to every
	// project in the org.
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/file?path=secret.md", nil, frank); rec.Code == 200 {
		t.Errorf("a stranger who redeemed a removed owner's invite reads the org's files: %d %q",
			rec.Code, rec.Body.String())
	}
	if orgs.Role(p.Org, "frank@x.io") != "" {
		t.Errorf("frank joined the org on a removed owner's invite, role=%q", orgs.Role(p.Org, "frank@x.io"))
	}
}

// The same lever one level up: the ACCOUNT is gone, not just the membership.
//
// BuiltinAuth.Deny is documented as the hub's only account-removal path and is
// the one place Server.offboard is wired into. offboard drops project grants
// and evicts the address from every org — which is what makes the account's
// share links stop serving — and says nothing about the invites the account
// minted. On the default invite-only posture, redeeming one of those also
// CREATES the redeeming account, so this link is a standing account factory
// attached to an address nobody can sign in as any more.
func TestSec_Lifecycle_AnInviteDiesWithTheAccountThatMintedIt(t *testing.T) {
	h, srv, c, p := permHub(t)
	orgs := sec12rOrgs(t, srv)
	auth, ok := srv.Auth.(*BuiltinAuth)
	if !ok {
		t.Fatal("fixture has no BuiltinAuth")
	}
	if err := orgs.SetRole(p.Org, "bob@x.io", RoleOwner); err != nil {
		t.Fatal(err)
	}
	shareTok := sec12rMintShare(t, h, p.ID, "secret.md", "s3cret", c["bob"])
	inviteTok := sec12rMintInvite(t, h, p.Org, c["bob"])

	var bobID string
	for _, u := range auth.Accounts() {
		if u.Email == "bob@x.io" {
			bobID = u.ID
		}
	}
	if bobID == "" {
		t.Fatal("bob has no account row")
	}
	if err := auth.Deny(bobID); err != nil {
		t.Fatalf("remove bob's account: %v", err)
	}
	if orgs.Role(p.Org, "bob@x.io") != "" {
		t.Fatal("offboard did not evict bob from the org; the rest of this test measures nothing")
	}

	// Control: offboarding reached the share.
	if rec := doAs(t, h, "GET", "/s/"+shareTok, nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("share minted by a deleted account still serves: %d, want 404", rec.Code)
	}

	// The finding: the invite is still live, and still bootstraps accounts.
	if !orgs.ValidInvite(inviteTok) {
		return // already closed
	}
	frank := signupAndSession(t, h, "frank@x.io", "Frank", "password1")
	if rec := doAs(t, h, "POST", "/api/invites/"+inviteTok, nil, frank); rec.Code == 200 {
		t.Errorf("an invite minted by a DELETED account is still redeemable: %d %s", rec.Code, rec.Body)
	}
	if orgs.Role(p.Org, "frank@x.io") != "" {
		t.Errorf("a stranger joined the org on a deleted account's invite, role=%q",
			orgs.Role(p.Org, "frank@x.io"))
	}
}

// The rest of the matrix, as one permanent regression test: everything else a
// member accumulates, checked against removal from the org. These cells are
// expected to be clean — the point of writing them down is that no round has
// asserted them together, so a future change that reopens one is caught here
// rather than four rounds later by accident.
func TestSec_Lifecycle_ARemovedMembersOtherGrantsAreAllRefused(t *testing.T) {
	h, srv, c, p := permHub(t)
	orgs := sec12rOrgs(t, srv)
	base := "/api/p/" + p.ID + "/"
	// Read heat is a matrix cell of its own (a member accumulates ledger
	// buckets), and permHub leaves it off; turn it on so /heat is a real
	// surface here rather than a 404 for the wrong reason.
	reads, err := OpenReadLedger(filepath.Join(t.TempDir(), "reads.json"), 30)
	if err != nil {
		t.Fatal(err)
	}
	srv.Reads = reads

	// bob accumulates: an explicit project grant, published content, a device
	// row (through a journal push), and a live browser session.
	if rec := doAs(t, h, "PUT", base+"permissions/bob@x.io",
		map[string]string{"level": PermAdmin}, c["alice"]); rec.Code != 200 {
		t.Fatalf("grant bob admin: %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "PUT", base+"upload/content?path=bob.md", []byte("bob's"), c["bob"]); rec.Code != 200 {
		t.Fatalf("bob publishes: %d %s", rec.Code, rec.Body)
	}
	secRegisterDevice(t, h, p.ID, c["bob"], "bobdev", "Bob Laptop", "linux")

	// Baseline: every one of these succeeds while bob is a member.
	authorized := []struct {
		name, method, url string
		body              any
	}{
		{"read a file", "GET", base + "file?path=bob.md", nil},
		{"list the tree", "GET", base + "tree", nil},
		{"read history", "GET", base + "history?path=bob.md", nil},
		{"read heat", "GET", base + "heat", nil},
		{"list shares", "GET", base + "shares", nil},
		{"read the permission map", "GET", base + "permissions", nil},
		{"pull through the store proxy", "GET", base + "store/list?prefix=journal/", nil},
		{"publish", "PUT", base + "upload/content?path=bob2.md", []byte("more")},
		{"mint a share", "POST", base + "shares", map[string]string{"path": "bob.md"}},
	}
	for _, tc := range authorized {
		if rec := doAs(t, h, tc.method, tc.url, tc.body, c["bob"]); rec.Code != 200 {
			t.Fatalf("baseline %s: %d %s (the probe is wrong, not the server)", tc.name, rec.Code, rec.Body)
		}
	}

	// Alice removes bob from the org. His explicit project grant is deliberately
	// left in place — round 1's rule is that org membership, not the grant map,
	// decides — so this is the exact state that rule exists for.
	if rec := doAs(t, h, "DELETE", "/api/orgs/"+p.Org+"/members/bob@x.io", nil, c["alice"]); rec.Code != 200 {
		t.Fatalf("remove bob: %d %s", rec.Code, rec.Body)
	}
	if orgs.Role(p.Org, "bob@x.io") != "" {
		t.Fatal("bob is still an org member")
	}
	if got, _ := srv.Projects.Get(p.ID); got.Perms["bob@x.io"] != PermAdmin {
		t.Fatalf("the fixture no longer tests the interesting case: bob's grant is %q", got.Perms["bob@x.io"])
	}

	// Every one of them must now be refused, on the same session cookie.
	for _, tc := range authorized {
		if rec := doAs(t, h, tc.method, tc.url, tc.body, c["bob"]); rec.Code == 200 {
			t.Errorf("a removed member can still %s: %d %s", tc.name, rec.Code, rec.Body)
		}
	}
	// The project must not even be listed to him any more.
	rec := doAs(t, h, "GET", "/api/projects", nil, c["bob"])
	if rec.Code == 200 {
		var out struct {
			Projects []struct {
				ID string `json:"id"`
			} `json:"projects"`
		}
		json.Unmarshal(rec.Body.Bytes(), &out)
		for _, pr := range out.Projects {
			if pr.ID == p.ID {
				t.Errorf("a removed member still sees the project in /api/projects")
			}
		}
	}
	// And his device may no longer write the journal it owns.
	if rec := doAs(t, h, "PUT", base+"store/object?key=journal/bobdev.jsonl",
		[]byte(secaudOpLine(2, "bobdev", "put", "bob.md", shaOf("bob's"))), c["bob"]); rec.Code == 200 {
		t.Errorf("a removed member's registered device still pushes its journal: %d %s", rec.Code, rec.Body)
	}
}

// Demotion is the other revocation, and the one the hub is least explicit
// about. shares.go states its position outright ("Demotion (write -> read) is
// deliberately NOT covered: a link lives until revoked"). Nothing states a
// position on an INVITE minted while the account was an owner, and an invite
// is the owner-only capability: handleInviteCreate refuses a plain member.
//
// So an owner-only capability outlives the ownership. Asserted here as the
// secure behaviour — a token minted under a role the account no longer holds
// is not redeemable — so the day the hub takes a position, this is the
// regression test for it.
func TestSec_Lifecycle_AnInviteDiesWithTheOwnershipThatMintedIt(t *testing.T) {
	h, srv, c, p := permHub(t)
	orgs := sec12rOrgs(t, srv)
	if err := orgs.SetRole(p.Org, "bob@x.io", RoleOwner); err != nil {
		t.Fatal(err)
	}
	inviteTok := sec12rMintInvite(t, h, p.Org, c["bob"])

	// Alice demotes bob to a plain member. He can no longer mint one...
	if rec := doAs(t, h, "PATCH", "/api/orgs/"+p.Org+"/members/bob@x.io",
		map[string]string{"role": RoleMember}, c["alice"]); rec.Code != 200 {
		t.Fatalf("demote bob: %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "POST", "/api/orgs/"+p.Org+"/invites", nil, c["bob"]); rec.Code != http.StatusForbidden {
		t.Fatalf("a demoted owner can still mint an invite: %d %s", rec.Code, rec.Body)
	}
	// ...so the one he already minted must not keep doing it for him.
	frank := signupAndSession(t, h, "frank@x.io", "Frank", "password1")
	if rec := doAs(t, h, "POST", "/api/invites/"+inviteTok, nil, frank); rec.Code == 200 {
		t.Errorf("an invite minted by a since-demoted owner is still redeemable: %d %s", rec.Code, rec.Body)
	}
}

// ---- the other end of the lifecycle: what a password reset does NOT reach ----

// sec12rGrants lists the live mail grants of a kind held for an account. This
// is what an attacker sitting on the mailbox has: the links themselves.
func sec12rGrants(a *BuiltinAuth, kind, userID string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for id, g := range a.pending {
		if g.kind == kind && g.user == userID {
			out = append(out, id)
		}
	}
	return out
}

// A password reset is, in this repo's own words, "the documented recovery for
// a stolen account, so it has to end the thief's access too: every session
// cookie and device token minted under the old password dies with it"
// (pageResetConfirm). It calls revokeTokensFor and stops there — a.pending,
// the map of outstanding one-time mail grants, is untouched.
//
// Two of those grants are credentials in their own right:
//
//   - a "reset" grant sets the password without knowing the old one. A thief
//     who requested one BEFORE the victim recovers still holds it after.
//   - a "verify" grant signs its holder straight in (pageVerify's last arm
//     calls startSession and redirects to /) with no password at all, and it
//     is minted at signup with a 24-hour TTL.
//
// Both survive the one action a user is told to take when they suspect
// compromise.
func TestSec_Verify_APasswordResetEndsEveryOutstandingMailGrant(t *testing.T) {
	srv, _, _ := newHub(t, true, nil)
	auth := gatedAuth(t, func(a *BuiltinAuth) { a.RequireVerification = true })
	srv.Auth = auth
	h := srv.Handler()

	u, err := auth.signup("victim@x.io", "Victim", "oldpassword")
	if err != nil {
		t.Fatal(err)
	}
	// The verification link the signup mail carries.
	verifyTok := auth.newGrant("verify", u.ID, 24*time.Hour)
	// The account is verified and in use.
	if rec := doAs(t, h, "GET", "/auth/verify?token="+verifyTok, nil, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("baseline verify: %d %s", rec.Code, rec.Body)
	}
	staleVerify := auth.newGrant("verify", u.ID, 24*time.Hour) // a second, still in the mailbox

	// The thief, sitting on the mailbox, requests a reset link and keeps it.
	sec12rPostForm(t, h, "/auth/reset", "email=victim@x.io")
	stolen := sec12rGrants(auth, "reset", u.ID)
	if len(stolen) != 1 {
		t.Fatalf("expected one outstanding reset grant, got %d", len(stolen))
	}

	// The victim recovers: a second reset link, used to set a new password.
	sec12rPostForm(t, h, "/auth/reset", "email=victim@x.io")
	var mine string
	for _, g := range sec12rGrants(auth, "reset", u.ID) {
		if g != stolen[0] {
			mine = g
		}
	}
	if mine == "" {
		t.Fatal("could not tell the victim's reset link from the thief's")
	}
	rec := sec12rPostForm(t, h, "/auth/reset/confirm", "token="+mine+"&password=brandnewpassword")
	if !strings.Contains(rec.Body.String(), "Password updated") {
		t.Fatalf("the recovery step did not happen: %d %s", rec.Code, rec.Body)
	}

	// The thief's reset link must be dead: it sets the password without
	// knowing it, which is the whole thing the recovery just took away.
	rec = sec12rPostForm(t, h, "/auth/reset/confirm", "token="+stolen[0]+"&password=thiefspassword")
	if strings.Contains(rec.Body.String(), "Password updated") {
		t.Errorf("a reset link issued before the recovery still sets the password afterwards")
	}

	// And the stale verification link must not be a passwordless sign-in.
	rec = doAs(t, h, "GET", "/auth/verify?token="+staleVerify, nil, nil)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Errorf("a verification link minted before the password reset still starts a session (%d)", rec.Code)
		}
	}
}

// sec12rPostForm posts a urlencoded form to an auth page.
func sec12rPostForm(t *testing.T, h http.Handler, path, form string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// DELETE /api/auth/token is the hub's only credential-revocation door and the
// thinnest-covered route in the package (one mention across 260 TestSec bodies
// before this round). What it must do that its neighbours need not: end
// EXACTLY the credential presented, and be unreachable by anything that is not
// that credential — a cookie alone must not do it, or any cross-origin page
// could sign a user's devices out.
func TestSec_Token_RevokingOneCredentialEndsExactlyThatOne(t *testing.T) {
	h, _, c, p := permHub(t)
	tok := sec12rDeviceToken(t, h, p.ID, c["bob"], "bobdev", "Bob Laptop")
	other := sec12rDeviceToken(t, h, p.ID, c["bob"], "bobphone", "Bob Phone")

	bearer := func(method, url, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, url, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// Baseline: the token is a working credential.
	if rec := bearer("GET", "/api/auth/me", tok); rec.Code != 200 {
		t.Fatalf("baseline /api/auth/me: %d %s", rec.Code, rec.Body)
	}
	if rec := bearer("GET", "/api/p/"+p.ID+"/store/list?prefix=journal/", tok); rec.Code != 200 {
		t.Fatalf("baseline store pull: %d %s", rec.Code, rec.Body)
	}

	// A cookie is not enough: revocation needs the credential itself, so a
	// cross-origin page holding only the browser session cannot use it.
	if rec := doAs(t, h, "DELETE", "/api/auth/token", nil, c["bob"]); rec.Code == 200 {
		t.Errorf("a session cookie alone revoked a device token: %d %s", rec.Code, rec.Body)
	}
	// Neither is a bogus bearer.
	if rec := bearer("DELETE", "/api/auth/token", "bdt_notatoken"); rec.Code == 200 {
		t.Errorf("an invalid bearer revoked something: %d %s", rec.Code, rec.Body)
	}

	if rec := bearer("DELETE", "/api/auth/token", tok); rec.Code != 200 {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}

	// The revoked credential reaches nothing...
	for _, u := range []string{
		"/api/auth/me",
		"/api/p/" + p.ID + "/store/list?prefix=journal/",
		"/api/p/" + p.ID + "/tree",
		"/api/projects",
	} {
		if rec := bearer("GET", u, tok); rec.Code == 200 {
			t.Errorf("a revoked token still reaches %s: %d", u, rec.Code)
		}
	}
	// ...and nothing else was revoked with it: the account's other device and
	// its browser session are untouched.
	if rec := bearer("GET", "/api/auth/me", other); rec.Code != 200 {
		t.Errorf("revoking one device token killed another: %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, c["bob"]); rec.Code != 200 {
		t.Errorf("revoking a device token killed the browser session: %d %s", rec.Code, rec.Body)
	}
}

// sec12rDeviceToken runs the real loopback sign-in for a device and returns
// its long-lived token.
func sec12rDeviceToken(t *testing.T, h http.Handler, project string, c *http.Cookie, id, name string) string {
	t.Helper()
	rec := secRegisterDevice(t, h, project, c, id, name, "linux")
	if rec.Code != 200 {
		t.Fatalf("register %s: %d %s", id, rec.Code, rec.Body)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Token == "" {
		t.Fatalf("exchange body = %s", rec.Body)
	}
	return out.Token
}
