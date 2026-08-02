package webapp

// Round 9 — the target is round 8's own fixes on the hub side:
//
//   - the sole-owner HEIR PROMOTION added to OrgDB.EvictMember, whose heir is
//     "the lowest address" (the CISO flagged this itself);
//   - PKCE on the loopback exchange;
//   - DELETE /api/auth/token, which round 8 shipped with end-to-end coverage
//     only and no direct attack.
//
// Helpers are prefixed secfx8; no existing file is touched.

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// secfx8Auth returns the hub's BuiltinAuth, which permHub always installs.
func secfx8Auth(t *testing.T, srv *Server) *BuiltinAuth {
	t.Helper()
	a, ok := srv.Auth.(*BuiltinAuth)
	if !ok {
		t.Fatalf("this fixture expects a BuiltinAuth, got %T", srv.Auth)
	}
	return a
}

// secfx8AccountID finds the account id for an address — what the hub's only
// account-removal route (POST /api/admin/pending/{id}/deny) is keyed on.
func secfx8AccountID(t *testing.T, a *BuiltinAuth, email string) string {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, u := range a.users {
		if normEmail(u.Email) == normEmail(email) {
			return id
		}
	}
	t.Fatalf("no account for %s", email)
	return ""
}

// secfx8Owners lists the addresses holding RoleOwner in an org.
func secfx8Owners(t *testing.T, srv *Server, orgID string) []string {
	t.Helper()
	db, ok := srv.Dir.(LocalDirectory)
	if !ok {
		t.Fatalf("this fixture expects a LocalDirectory, got %T", srv.Dir)
	}
	o, ok := db.Get(orgID)
	if !ok {
		t.Fatalf("no org %s", orgID)
	}
	var out []string
	for m, role := range o.Members {
		if role == RoleOwner {
			out = append(out, m)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 1. The heir is whoever picked the lowest-sorting address.
// ---------------------------------------------------------------------------

// Round 8 closed a real hole — an org whose sole owner is offboarded could
// never gain, lose or change a role again — by promoting a remaining member.
// Its own comment says which one: "The longest-standing remaining member is
// promoted instead." The implementation is `lowestMember`, a scan for the
// smallest string, because no join time is recorded.
//
// So the heir of every org is decided by the address a member typed at signup.
// A member who joins last through an ordinary invite link, holding no grant
// on anything, is the designated successor of every member who was there
// first — and the trigger is not an attacker action at all but the single most
// routine thing an operator does: removing the account of someone who left.
//
// The delta this asserts is the capability, not the role name: before the
// removal `aaa@x.io` is 403 on an admin-only project route and bob is too;
// after it, `aaa@x.io` administers a project it was never granted anything on,
// while bob — who has been in the org since before aaa existed — is still 403.
func TestSec_Org_TheOrgHeirIsNotChosenByTheAddressAMemberPicked(t *testing.T) {
	h, srv, c, p := permHub(t)
	auth := secfx8Auth(t, srv)
	db, _ := srv.Dir.(LocalDirectory)

	// A newcomer joins the org LAST, as a plain member — what redeeming an
	// invite link gets you. Its only distinguishing property is its address.
	c["aaa"] = signupAndSession(t, h, "aaa@x.io", "Aaa", "password1")
	if err := db.AddMember(p.Org, "aaa@x.io", RoleMember); err != nil {
		t.Fatal(err)
	}

	// Control: the admin-only project route refuses both plain members.
	perms := "/api/p/" + p.ID + "/permissions/carol@x.io"
	body := map[string]string{"level": PermAdmin}
	for _, who := range []string{"bob", "aaa"} {
		if rec := doAs(t, h, "PUT", perms, body, c[who]); rec.Code != http.StatusForbidden {
			t.Fatalf("control: %s could already administer the project: %d %s", who, rec.Code, rec.Body)
		}
	}

	// The routine operator action: the org's sole owner leaves the company and
	// a hub admin removes her account. Nothing here is an attacker's request.
	auth.Admins = map[string]bool{"dave@x.io": true}
	aliceID := secfx8AccountID(t, auth, "alice@x.io")
	if rec := doAs(t, h, "POST", "/api/admin/pending/"+aliceID+"/deny", nil, c["dave"]); rec.Code != 200 {
		t.Fatalf("hub admin could not remove the account: %d %s", rec.Code, rec.Body)
	}

	owners := secfx8Owners(t, srv, p.Org)
	after := doAs(t, h, "PUT", perms, body, c["aaa"])
	bobAfter := doAs(t, h, "PUT", perms, body, c["bob"])
	if after.Code == http.StatusForbidden {
		return // the newcomer inherited nothing: the secure outcome
	}
	t.Errorf("the newest member of the org now administers a project it holds no grant on.\n"+
		"  owners after the removal:              %v\n"+
		"  aaa@x.io  PUT %s -> %d (was 403)\n"+
		"  bob@x.io  PUT %s -> %d\n"+
		"aaa@x.io joined the org after bob and carol, through an ordinary member row, and holds "+
		"no project grant. EvictMember's heir is `lowestMember` — the smallest address string — "+
		"so the successor to every org is decided by what a member typed at signup, and the "+
		"trigger is the most routine operator action there is. The code's own comment says the "+
		"heir is \"the longest-standing remaining member\"; nothing records when a member joined, "+
		"so that is not what happens.",
		owners, perms, after.Code, perms, bobAfter.Code)
}

// ---------------------------------------------------------------------------
// 2. PKCE on the loopback exchange.
// ---------------------------------------------------------------------------

// Round 8's critical: `state` binds nothing (it is printed and passed to
// xdg-open as argv[1]), so a local process could complete a sign-in of ITS OWN
// account into somebody else's `bdrive login`. PKCE is the fix. These are the
// ways an attacker would try to get around it, all of them against the real
// route.
func TestSec_PKCE_ACodeMintedForAnotherFlowIsNotRedeemable(t *testing.T) {
	issued := 0
	c := NewCLIAuth(
		func(*http.Request) (User, bool) { return User{ID: "u-victim", Email: "victim@x.io"}, true },
		func(w http.ResponseWriter, userID, device string) { issued++; writeJSON(w, map[string]any{"token": "t"}) },
	)
	mux := http.NewServeMux()
	c.Register(mux)

	// mint runs the approval POST for a flow bound to `challenge` (empty =
	// a pre-PKCE CLI) and returns the one-time code the hub redirected with.
	mint := func(t *testing.T, challenge string) string {
		t.Helper()
		u := "/auth/cli?redirect=" + "http%3A%2F%2F127.0.0.1%3A9999%2Fcallback" + "&state=s"
		if challenge != "" {
			u += "&code_challenge=" + challenge + "&code_challenge_method=S256"
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", u, nil))
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("approval POST: %d %s", rec.Code, rec.Body)
		}
		loc := rec.Header().Get("Location")
		i := strings.Index(loc, "code=")
		if i < 0 {
			t.Fatalf("no code in %q", loc)
		}
		return strings.SplitN(loc[i+len("code="):], "&", 2)[0]
	}
	exchange := func(code, verifier string) int {
		rec := httptest.NewRecorder()
		body := `{"code":"` + code + `","device":"d","code_verifier":"` + verifier + `"}`
		mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/auth/exchange", strings.NewReader(body)))
		return rec.Code
	}

	verifier := "1f0e3dad99908345f7439f8ffabdffc41f0e3dad99908345f7439f8ffabdffc4"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	// Control: the honest round trip works, or every case below is vacuous.
	if got := exchange(mint(t, challenge), verifier); got != 200 {
		t.Fatalf("control: the honest PKCE round trip failed with %d", got)
	}
	if issued != 1 {
		t.Fatalf("control: %d tokens issued for one honest exchange", issued)
	}

	for _, tc := range []struct {
		name              string
		mintWith, present string
	}{
		// The round-8 attack itself: another local process approves a flow of
		// its own (it cannot know this CLI's verifier, so it binds none) and
		// the code lands on this CLI's loopback listener.
		{"a code from a flow that bound nothing", "", verifier},
		// The same, with a challenge the attacker chose.
		{"a code bound to someone else's challenge", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", verifier},
		// `plain` is not an accepted method: the verifier is not the challenge.
		{"plain instead of S256", verifier, verifier},
		// A guess at the verifier.
		{"a wrong verifier", challenge, "not-the-verifier"},
		// No verifier at all against a bound flow.
		{"no verifier against a bound flow", challenge, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := issued
			if got := exchange(mint(t, tc.mintWith), tc.present); got != http.StatusUnauthorized {
				t.Errorf("POST /api/auth/exchange = %d, want 401: %s is redeemable, so the loopback "+
					"callback still has no proof of possession and any local process that can read "+
					"the listener port off `ps` signs this CLI in as its own account", got, tc.name)
			}
			if issued != before {
				t.Errorf("a token was issued for %s", tc.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 3. DELETE /api/auth/token — round 8 shipped it with no direct attack test.
// ---------------------------------------------------------------------------

// The route revokes the credential the request presents and takes no other
// input, so "it can revoke another device's token" ought to be impossible by
// construction. Assert it anyway: the construction is one line, and a body or
// query parameter added later is exactly the kind of thing that turns a
// self-service sign-out into a hub-wide credential killer.
func TestSec_Logout_ATokenCannotEndAnotherDevicesCredential(t *testing.T) {
	h, srv, _, p := permHub(t)
	auth := secfx8Auth(t, srv)

	victimID := secfx8AccountID(t, auth, "alice@x.io")
	attackerID := secfx8AccountID(t, auth, "bob@x.io")
	victimTok, err := auth.issueToken(victimID, "alices-laptop")
	if err != nil {
		t.Fatal(err)
	}
	attackerTok, err := auth.issueToken(attackerID, "bobs-laptop")
	if err != nil {
		t.Fatal(err)
	}

	live := func(tok string) int {
		req := httptest.NewRequest("GET", "/api/p/"+p.ID+"/tree", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	revoke := func(tok string, body string) int {
		var r *http.Request
		if body == "" {
			r = httptest.NewRequest("DELETE", "/api/auth/token", nil)
		} else {
			r = httptest.NewRequest("DELETE", "/api/auth/token", strings.NewReader(body))
		}
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}

	if live(victimTok) != 200 || live(attackerTok) != 200 {
		t.Fatalf("control: the two tokens are not both live to begin with (%d, %d)",
			live(victimTok), live(attackerTok))
	}
	if got := revoke("", ""); got != http.StatusUnauthorized {
		t.Errorf("anonymous DELETE /api/auth/token = %d, want 401", got)
	}
	if got := revoke("not-a-real-token", ""); got != http.StatusUnauthorized {
		t.Errorf("forged bearer DELETE /api/auth/token = %d, want 401", got)
	}
	// The attacker names the victim's token every way a later refactor might
	// start reading it: a JSON body and a query parameter.
	if got := revoke(attackerTok, `{"token":"`+victimTok+`"}`); got != 200 {
		t.Fatalf("attacker's own revocation was refused: %d", got)
	}
	if live(victimTok) != 200 {
		t.Errorf("bob's sign-out ended ALICE's device token: a self-service logout is a hub-wide "+
			"credential killer. GET /api/p/%s/tree with alice's token = %d", p.ID, live(victimTok))
	}
	if live(attackerTok) == 200 {
		t.Errorf("bob's own token survived his own logout: DELETE /api/auth/token did not revoke "+
			"the credential it was presented with")
	}
}
