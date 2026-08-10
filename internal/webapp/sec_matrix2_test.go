package webapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Round 14: the cells of the offboarding matrix round 13 left open, plus the
// columns that had no column at all — org rename, "the org is gone", project
// delete beyond its share links, and device re-registration.
//
// Same method as round 13: a POSITIVE CONTROL that proves the capability was
// real, then the revocation route, then the same request again. All helpers
// here are prefixed sec14m; the fixtures (sec13mHub, sec13mServer, permHub,
// doAs, signupAndSession) are reused, never copied.

// ---- helpers -------------------------------------------------------------

// sec14mBindReq is the request finishLogin hands bindDevice: a machine naming
// itself while its account is being authenticated.
func sec14mBindReq(id, name string) *http.Request {
	r := httptest.NewRequest("POST", "/auth/cli", nil)
	r.Header.Set("X-Bdrive-Device", id)
	r.Header.Set("X-Bdrive-Device-Name", name)
	return r
}

// sec14mDeleteAccount drives the hub's ONE account-removal path (Deny →
// Server.offboard), which is what `bdrive` offboarding actually is.
func sec14mDeleteAccount(t *testing.T, srv *Server, email string) {
	t.Helper()
	a := srv.Auth.(*BuiltinAuth)
	a.mu.Lock()
	u := a.findByEmail(email)
	a.mu.Unlock()
	if u == nil {
		t.Fatalf("sec14mDeleteAccount: no account %s", email)
	}
	if err := a.Deny(u.ID); err != nil {
		t.Fatalf("sec14mDeleteAccount %s: %v", email, err)
	}
}

// sec14mLogin signs an EXISTING account in through the real login page — what
// a second hub process gives a browser whose account was created on the first.
func sec14mLogin(t *testing.T, h http.Handler, email, pass string) *http.Cookie {
	t.Helper()
	form := url.Values{"email": {email}, "password": {pass}}
	req := httptest.NewRequest("POST", "/auth/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := doHTTP(h, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("sec14mLogin %s: %d %s", email, rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatalf("sec14mLogin %s: no session cookie", email)
	return nil
}

// sec14mStorePut writes a store key as one signed-in browser session naming a
// device — the shape of every /store/* write.
func sec14mStorePut(t *testing.T, h http.Handler, project, key, device string, body []byte, c *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptestNewRequestBody("PUT", "/api/p/"+project+"/store/object?key="+key, body)
	req.Header.Set("X-Bdrive-Device", device)
	req.AddCookie(c)
	return doHTTP(h, req)
}

// ---- cell: device binding x account deletion x SECOND HUB PROCESS --------

// Round 13 closed "a device claim must not outlive its account"
// (TestSec_Matrix_AccountDeletionReleasesTheDeviceBinding) inside ONE process.
// DeviceRegistry was then recorded as "verified-not-applicable" for the
// second-process class on the strength of the BIND-AWAY direction alone
// (TestSec_Devices_ASecondHubProcessCannotBindAwayAnExistingDeviceID). The
// RELEASE direction was never driven, and DeviceRegistry has no refresh() at
// all: byKey is loaded at open and only ever mutated by this process.
//
// So on a hub running two processes in front of one devices.json — the
// deployment the SQL backend exists for — offboarding an account releases its
// device claims on whichever process served the deletion and on no other. Both
// halves of Release's own doc comment come back:
//
//   - the next hire is silently locked out: the departed owner is invisible to
//     her (no shared org any more), so Bind's invisible-conflict arm binds
//     NOTHING and lets the login succeed, and every push then 403s telling her
//     to run `bdrive login`, which is what she just did;
//   - and the mirror, which is the authorization direction: re-create the
//     address and the new account inherits the departed employee's device row
//     and write access to that device's journal.
func TestSec_Matrix_ADeviceReleaseIsHonouredByEveryHubProcess(t *testing.T) {
	_, srvA, dir, root, _, _ := sec13mHub(t)
	const id = "devlaptop14"

	// Bob's machine signs in on process A: the id becomes his, hub-wide.
	if err := srvA.bindDevice("bob@x.io", sec14mBindReq(id, "the-laptop")); err != nil {
		t.Fatal(err)
	}
	if owner, _ := srvA.Devices.OwnerOf(id); owner != "bob@x.io" {
		t.Fatalf("fixture: OwnerOf = %q", owner)
	}

	// A second hub process, up now, in front of the same metadata.
	srvB := sec13mServer(t, dir, root)
	_ = srvB.Handler()

	// Positive control: while bob's account lives, process B holds the same
	// claim and refuses to hand the id to carol. That refusal is correct, and
	// it proves B is really reading this row.
	if err := srvB.bindDevice("carol@x.io", sec14mBindReq(id, "carol-pc")); err == nil {
		t.Fatal("fixture: process B should refuse carol a live account's device id")
	}

	// The real revocation route: bob's ACCOUNT is deleted on process A.
	sec14mDeleteAccount(t, srvA, "bob@x.io")
	if owner, _ := srvA.Devices.OwnerOf(id); owner != "" {
		t.Fatalf("fixture: process A should have released the claim, got %q", owner)
	}

	if owner, _ := srvB.Devices.OwnerOf(id); owner != "" {
		t.Errorf("process B still says %q owns the reassigned laptop after that account was "+
			"DELETED on process A: DeviceRegistry has no read-path refresh, so Release only ever "+
			"took effect on the process that served the offboarding.", owner)
	}
	// The lockout half: the next hire's login must actually take the id.
	if err := srvB.bindDevice("carol@x.io", sec14mBindReq(id, "carol-pc")); err != nil {
		t.Fatalf("process B refused the next hire the reassigned laptop: %v", err)
	}
	if owner, _ := srvB.Devices.OwnerOf(id); owner != "carol@x.io" {
		t.Errorf("after a successful bind on process B, OwnerOf = %q, want carol@x.io — "+
			"a login that binds nothing hands back a token whose every push 403s, forever", owner)
	}
}

// The mirror of the same stale map, and this one is authorization, not
// availability: the address is signed up again (an owner re-invites a
// contractor, a re-hire), and on the second hub process the NEW account walks
// straight into the departed one's device row — which is the WRITE gate for
// that device's journal on every project on the hub.
func TestSec_Matrix_ARecreatedAddressDoesNotInheritAReleasedDeviceOnAnyProcess(t *testing.T) {
	hA, srvA, dir, root, _, _ := sec13mHub(t)
	const id = "devlaptop14b"

	if err := srvA.bindDevice("bob@x.io", sec14mBindReq(id, "the-laptop")); err != nil {
		t.Fatal(err)
	}
	srvB := sec13mServer(t, dir, root)
	hB := srvB.Handler()
	if owner, _ := srvB.Devices.OwnerOf(id); owner != "bob@x.io" {
		t.Fatalf("fixture: process B should see the claim, got %q", owner)
	}

	sec14mDeleteAccount(t, srvA, "bob@x.io")

	// The address is signed up again. Same string, different account.
	if c := signupAndSession(t, hA, "bob@x.io", "Bob Two", "password2"); c == nil {
		t.Fatal("fixture: re-signup failed")
	}
	if owner, _ := srvA.Devices.OwnerOf(id); owner != "" {
		t.Fatalf("fixture: process A should hold no claim, got %q", owner)
	}
	_ = hB
	if owner, _ := srvB.Devices.OwnerOf(id); owner == "bob@x.io" {
		t.Errorf("on process B a BRAND NEW account on the departed employee's address owns "+
			"device %s and may rewrite its journal: the release never reached this process", id)
	}
}

// ---- cell: explicit project grant x demotion, on every route -------------

// Round 13 covered the IMPLICIT project-admin an org owner carries
// (TestSec_Matrix_DemotionDropsImplicitProjectAdmin). The EXPLICIT
// Project.Perms grant — the one an admin actually edits — was never demoted
// and re-probed. Every level a route can declare is probed here, including
// ownJournal's project-admin RECOVERY arm, which is the one place PermAdmin
// buys write access to somebody else's journal object.
func TestSec_Matrix_ExplicitProjectGrantDemotionIsHonouredOnEveryRoute(t *testing.T) {
	h, srv, cookies, p := permHub(t)
	bob := cookies["bob"]

	// Alice grants bob an explicit admin, and carol one too so the last-admin
	// guard is not what refuses the demotion later.
	for _, who := range []string{"bob", "carol"} {
		if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions/"+who+"@x.io",
			map[string]string{"level": PermAdmin}, cookies["alice"]); rec.Code != 200 {
			t.Fatalf("fixture: grant %s admin: %d %s", who, rec.Code, rec.Body)
		}
	}

	adminRoutes := []struct {
		name, method, url string
		body              any
	}{
		{"rename the project", "PATCH", "/api/projects/" + p.ID, map[string]string{"name": "renamed"}},
		{"set the default level", "PUT", "/api/p/" + p.ID + "/permissions", map[string]string{"default": PermRead}},
		{"grant a level", "PUT", "/api/p/" + p.ID + "/permissions/carol@x.io", map[string]string{"level": PermWrite}},
		{"clear a grant", "DELETE", "/api/p/" + p.ID + "/permissions/carol@x.io", nil},
		{"delete the project", "DELETE", "/api/projects/" + p.ID, nil},
	}
	writeRoutes := []struct {
		name, method, url string
		body              any
	}{
		{"mint a share", "POST", "/api/p/" + p.ID + "/shares", map[string]string{"path": "wiki/a.md"}},
		{"start an upload", "POST", "/api/p/" + p.ID + "/upload/init", map[string]any{"path": "x.md", "size": 1}},
		{"ask for a signed put", "POST", "/api/p/" + p.ID + "/store/sign", map[string]any{"key": "blobs/" + strings.Repeat("a", 64), "size": 1}},
		{"restore a version", "POST", "/api/p/" + p.ID + "/restore", map[string]string{"path": "wiki/a.md", "sha": strings.Repeat("b", 64)}},
		{"remove a path", "POST", "/api/p/" + p.ID + "/remove", map[string]string{"path": "wiki/a.md"}},
	}
	readRoutes := []struct{ name, url string }{
		{"tree", "/api/p/" + p.ID + "/tree"},
		{"file", "/api/p/" + p.ID + "/file?path=wiki/a.md"},
		{"download", "/api/p/" + p.ID + "/download?path=wiki/a.md"},
		{"render", "/api/p/" + p.ID + "/render?path=wiki/a.md"},
		{"history", "/api/p/" + p.ID + "/history"},
		{"share list", "/api/p/" + p.ID + "/shares"},
		{"permissions", "/api/p/" + p.ID + "/permissions"},
		{"store list", "/api/p/" + p.ID + "/store/list"},
		{"store exists", "/api/p/" + p.ID + "/store/exists?key=blobs/" + strings.Repeat("a", 64)},
	}

	// Positive control: with the explicit admin grant bob reaches every one of
	// them. "delete the project" is exercised only in the demoted state — the
	// control for it is the four siblings gated by the identical PermAdmin.
	for _, rt := range adminRoutes[:len(adminRoutes)-1] {
		if rec := doAs(t, h, rt.method, rt.url, rt.body, bob); rec.Code == http.StatusForbidden {
			t.Fatalf("fixture: explicit project admin bob cannot %s: %d %s", rt.name, rec.Code, rec.Body)
		}
	}
	for _, rt := range writeRoutes {
		if rec := doAs(t, h, rt.method, rt.url, rt.body, bob); rec.Code == http.StatusForbidden {
			t.Fatalf("fixture: explicit project admin bob cannot %s: %d %s", rt.name, rec.Code, rec.Body)
		}
	}
	// ownJournal's recovery arm: PermAdmin is what lets a caller write a
	// journal object bound to no account of theirs.
	if rec := sec14mStorePut(t, h, p.ID, "journal/sec14mfree.jsonl", "sec14mfree", []byte("\n"), bob); rec.Code == http.StatusForbidden {
		t.Fatalf("fixture: explicit project admin bob cannot use the journal recovery arm: %s", rec.Body)
	}

	// The control sweep cleared carol's grant; put it back so the demotion
	// below is refused by nothing but the thing under test.
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions/carol@x.io",
		map[string]string{"level": PermAdmin}, cookies["alice"]); rec.Code != 200 {
		t.Fatalf("fixture: restore carol's admin grant: %d %s", rec.Code, rec.Body)
	}

	// The real revocation route: alice demotes bob's explicit grant to read.
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions/bob@x.io",
		map[string]string{"level": PermRead}, cookies["alice"]); rec.Code != 200 {
		t.Fatalf("demote bob to read: %d %s", rec.Code, rec.Body)
	}

	for _, rt := range adminRoutes {
		if rec := doAs(t, h, rt.method, rt.url, rt.body, bob); rec.Code != http.StatusForbidden {
			t.Errorf("after demotion to read, bob can still %s: %d %s", rt.name, rec.Code, rec.Body)
		}
	}
	for _, rt := range writeRoutes {
		if rec := doAs(t, h, rt.method, rt.url, rt.body, bob); rec.Code != http.StatusForbidden {
			t.Errorf("after demotion to read, bob can still %s: %d %s", rt.name, rec.Code, rec.Body)
		}
	}
	if rec := sec14mStorePut(t, h, p.ID, "journal/sec14mfree2.jsonl", "sec14mfree2", []byte("\n"), bob); rec.Code != http.StatusForbidden {
		t.Errorf("after demotion to read, bob still writes an unowned device's journal: %d %s", rec.Code, rec.Body)
	}
	// Read still works — the demotion is to read, not to none.
	for _, rt := range readRoutes {
		if rec := doAs(t, h, "GET", rt.url, nil, bob); rec.Code == http.StatusForbidden {
			t.Errorf("demotion to read also took %s away: %d %s", rt.name, rec.Code, rec.Body)
		}
	}

	// And the whole way down: read -> none must close the read routes too, and
	// take the project out of the list.
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions/bob@x.io",
		map[string]string{"level": PermNone}, cookies["alice"]); rec.Code != 200 {
		t.Fatalf("demote bob to none: %d %s", rec.Code, rec.Body)
	}
	for _, rt := range readRoutes {
		if rec := doAs(t, h, "GET", rt.url, nil, bob); rec.Code != http.StatusForbidden {
			t.Errorf("after demotion to none, bob can still read %s: %d %s", rt.name, rec.Code, rec.Body)
		}
	}
	if rec := doAs(t, h, "GET", "/api/projects", nil, bob); strings.Contains(rec.Body.String(), p.ID) {
		t.Errorf("a project bob is denied is still listed to him: %s", rec.Body)
	}
	// Create-or-join by name must not hand the id back either.
	if rec := doAs(t, h, "POST", "/api/projects", map[string]string{"name": "renamed"}, bob); rec.Code != http.StatusForbidden {
		t.Errorf("create-or-join by name hands a denied project back: %d %s", rec.Code, rec.Body)
	}
	_ = srv
}

// ---- cell: device binding x demotion, and x removal from the org ---------

// OwnerOf is deliberately hub-wide, so neither a demotion nor a removal from
// the org may release a device claim — releasing it would hand a departing
// member's journal to whoever is left in the org, and History would keep
// crediting her. Nobody had ever asserted either direction; "it probably
// should" is how five cells of this matrix were missed.
//
// What removal MUST do is stop the row being joined into that org's surfaces.
func TestSec_Matrix_DeviceBindingOutlivesDemotionAndOrgRemoval(t *testing.T) {
	h, srv, dir, root, cookies, p := sec13mHub(t)
	_, _ = dir, root
	orgs := srv.Dir.(LocalDirectory).OrgDB
	const id = "devbob14c"

	if err := srv.bindDevice("bob@x.io", sec14mBindReq(id, "bob-laptop")); err != nil {
		t.Fatal(err)
	}
	// A second owner so alice can be demoted at all.
	if err := orgs.SetRole(p.Org, "carol@x.io", RoleOwner); err != nil {
		t.Fatal(err)
	}

	// --- demotion (owner -> member) ---
	const aliceDev = "devalice14c"
	if err := srv.bindDevice("alice@x.io", sec14mBindReq(aliceDev, "alice-mbp")); err != nil {
		t.Fatal(err)
	}
	if rec := doAs(t, h, "PATCH", "/api/orgs/"+p.Org+"/members/alice@x.io",
		map[string]string{"role": RoleMember}, cookies["carol"]); rec.Code != 200 {
		t.Fatalf("demote alice: %d %s", rec.Code, rec.Body)
	}
	if owner, _ := srv.Devices.OwnerOf(aliceDev); owner != "alice@x.io" {
		t.Errorf("demoting alice released her device claim (owner now %q): a claim that "+
			"evaporates on a role change is a claim the rest of the org can take", owner)
	}
	if err := srv.bindDevice("carol@x.io", sec14mBindReq(aliceDev, "carol-pc")); err == nil {
		if owner, _ := srv.Devices.OwnerOf(aliceDev); owner == "carol@x.io" {
			t.Errorf("carol took a demoted member's device id, and with it her journal")
		}
	}

	// --- removal from the org ---
	if rec := doAs(t, h, "DELETE", "/api/orgs/"+p.Org+"/members/bob@x.io", nil, cookies["carol"]); rec.Code != 200 {
		t.Fatalf("remove bob: %d %s", rec.Code, rec.Body)
	}
	if owner, _ := srv.Devices.OwnerOf(id); owner != "bob@x.io" {
		t.Errorf("removing bob from the org released his device claim (owner now %q): "+
			"OwnerOf is hub-wide on purpose — offboarding a teammate must not hand her "+
			"journal to the org she left", owner)
	}
	if err := srv.bindDevice("carol@x.io", sec14mBindReq(id, "carol-pc")); err == nil {
		if owner, _ := srv.Devices.OwnerOf(id); owner == "carol@x.io" {
			t.Errorf("carol took a removed member's device id: the claim did not survive removal")
		}
	}
	// The row must stop being VISIBLE in the org bob left.
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.put(id, "wiki/bob.md", "bob wrote this")
	rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/history", nil, cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("history: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "bob-laptop") {
		t.Errorf("history still joins a removed member's device row into this org's feed: %s", rec.Body)
	}
}

// ---- new column: the org is gone -----------------------------------------

// There is no DeleteOrg route — but an org CAN be emptied, and it is the one
// routine operator action that gets there: offboard the last member and
// EvictMember drops the row without promoting anyone (there is nobody left).
// Nothing had ever probed that state, and every fail-OPEN in it is a project's
// whole content handed to whoever asks: orgOf() still answers a non-empty id,
// so the "org is missing" guards (projectPerm, shareCreatorStillBelongs,
// grantable) are NOT the code path this takes.
func TestSec_Matrix_AnEmptiedOrgGrantsNothingToAnybody(t *testing.T) {
	h, srv, _, root, cookies, p := sec13mHub(t)
	orgs := srv.Dir.(LocalDirectory).OrgDB
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.put("dev14d", "wiki/secret.md", "the org's content")

	// Positive control: while the org has members, everything works.
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, cookies["bob"]); rec.Code != 200 {
		t.Fatalf("fixture: member read: %d %s", rec.Code, rec.Body)
	}
	rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/shares", map[string]string{"path": "wiki/secret.md"}, cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("fixture: mint share: %d %s", rec.Code, rec.Body)
	}
	var sh struct {
		Token string `json:"token"`
	}
	json.Unmarshal(rec.Body.Bytes(), &sh)
	if r := doHTTP(h, httptest.NewRequest("GET", "/s/"+sh.Token, nil)); r.Code != 200 {
		t.Fatalf("fixture: the fresh link should serve: %d", r.Code)
	}
	rec = doAs(t, h, "POST", "/api/orgs/"+p.Org+"/invites", nil, cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("fixture: mint invite: %d %s", rec.Code, rec.Body)
	}
	var inv struct {
		Token string `json:"token"`
	}
	json.Unmarshal(rec.Body.Bytes(), &inv)

	// Empty the org the only way the hub allows: remove the plain members,
	// then delete the sole owner's account (offboard -> EvictMember, and with
	// nobody left there is no heir).
	for _, who := range []string{"bob", "carol"} {
		if r := doAs(t, h, "DELETE", "/api/orgs/"+p.Org+"/members/"+who+"@x.io", nil, cookies["alice"]); r.Code != 200 {
			t.Fatalf("remove %s: %d %s", who, r.Code, r.Body)
		}
	}
	sec14mDeleteAccount(t, srv, "alice@x.io")
	if o, ok := orgs.Get(p.Org); !ok || len(o.Members) != 0 {
		t.Fatalf("fixture: the org should be empty, got ok=%v members=%v", ok, o.Members)
	}

	// Nothing in it may be reachable by anyone.
	for _, who := range []string{"bob", "carol"} {
		for _, u := range []string{"/api/p/" + p.ID + "/tree", "/api/p/" + p.ID + "/history",
			"/api/p/" + p.ID + "/store/list", "/api/p/" + p.ID + "/permissions"} {
			if r := doAs(t, h, "GET", u, nil, cookies[who]); r.Code == 200 {
				t.Errorf("%s still reads %s of a project in an org with no members: %s", who, u, r.Body)
			}
		}
		if r := doAs(t, h, "POST", "/api/projects",
			map[string]string{"name": "wiki", "org": p.Org}, cookies[who]); r.Code != http.StatusForbidden {
			t.Errorf("%s can still create/join a project in an org with no members: %d %s", who, r.Code, r.Body)
		}
		if r := doAs(t, h, "POST", "/api/orgs/"+p.Org+"/invites", nil, cookies[who]); r.Code != http.StatusForbidden {
			t.Errorf("%s can still mint an invite to an org with no members: %d %s", who, r.Code, r.Body)
		}
		if r := doAs(t, h, "GET", "/api/orgs", nil, cookies[who]); strings.Contains(r.Body.String(), p.Org) {
			t.Errorf("%s is still shown an org they do not belong to: %s", who, r.Body)
		}
	}
	// The public link the departed owner left behind must be dead.
	if r := doHTTP(h, httptest.NewRequest("GET", "/s/"+sh.Token, nil)); r.Code == 200 {
		t.Errorf("a public share link still serves an emptied org's content to anonymous strangers")
	}
	// And so must the invite, which on the default invite-only posture also
	// bootstraps an account.
	if r := doAs(t, h, "POST", "/api/invites/"+inv.Token, nil, cookies["carol"]); r.Code == 200 {
		t.Errorf("an invite minted by the departed owner still joins strangers to the emptied org: %s", r.Body)
	}
}

// ---- new column: project delete, beyond its share links ------------------

// Round 13 proved /s/ links die when the project is deleted, and named the
// mechanism: projectVolume fails the lookup, NOT a sweep of the share rows.
// Everything else the delete leaves behind was never probed. This drives the
// whole surface as the project's own ADMIN — the strongest principal there is
// — so a residue that answers is not "an outsider guessed an id", it is a
// deleted project still being a project.
func TestSec_Matrix_ProjectDeleteLeavesNoReachableResidue(t *testing.T) {
	h, srv, _, root, cookies, p := sec13mHub(t)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.put("dev14e", "wiki/secret.md", "the project's content")

	rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/shares", map[string]string{"path": "wiki/secret.md"}, cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("fixture: mint share: %d %s", rec.Code, rec.Body)
	}
	var sh struct {
		Token string `json:"token"`
	}
	json.Unmarshal(rec.Body.Bytes(), &sh)

	reads := []string{
		"/api/p/" + p.ID + "/tree",
		"/api/p/" + p.ID + "/file?path=wiki/secret.md",
		"/api/p/" + p.ID + "/download?path=wiki/secret.md",
		"/api/p/" + p.ID + "/render?path=wiki/secret.md",
		"/api/p/" + p.ID + "/history",
		"/api/p/" + p.ID + "/shares",
		"/api/p/" + p.ID + "/permissions",
		"/api/p/" + p.ID + "/store/list",
		"/api/projects/" + p.ID,
	}
	// Positive control: alice reaches all of it.
	for _, u := range reads {
		if r := doAs(t, h, "GET", u, nil, cookies["alice"]); r.Code != 200 {
			t.Fatalf("fixture: alice cannot GET %s: %d %s", u, r.Code, r.Body)
		}
	}
	if r := doAs(t, h, "GET", "/api/orgs/"+p.Org+"/shares", nil, cookies["alice"]); !strings.Contains(r.Body.String(), sh.Token) {
		t.Fatalf("fixture: the org share audit should list the link: %s", r.Body)
	}

	if r := doAs(t, h, "DELETE", "/api/projects/"+p.ID, nil, cookies["alice"]); r.Code != 200 {
		t.Fatalf("delete project: %d %s", r.Code, r.Body)
	}

	for _, u := range reads {
		if r := doAs(t, h, "GET", u, nil, cookies["alice"]); r.Code == 200 {
			t.Errorf("a DELETED project still answers GET %s to its former admin: %s", u, r.Body)
		}
	}
	if r := doAs(t, h, "GET", "/api/projects", nil, cookies["alice"]); strings.Contains(r.Body.String(), p.ID) {
		t.Errorf("a deleted project is still in the project list: %s", r.Body)
	}
	if r := doAs(t, h, "GET", "/api/orgs/"+p.Org+"/shares", nil, cookies["alice"]); strings.Contains(r.Body.String(), sh.Token) {
		t.Errorf("the org-wide share audit still lists a deleted project's link: %s", r.Body)
	}
	if r := doHTTP(h, httptest.NewRequest("GET", "/s/"+sh.Token, nil)); r.Code == 200 {
		t.Errorf("a deleted project's public link still serves its content")
	}
	// Write doors too — the storage prefix is deliberately left in place, so
	// nothing may still be able to add to it.
	if r := sec14mStorePut(t, h, p.ID, "journal/dev14e.jsonl", "dev14e", []byte("\n"), cookies["alice"]); r.Code == 200 {
		t.Errorf("a deleted project still accepts a journal push: %s", r.Body)
	}
	if r := doAs(t, h, "POST", "/api/p/"+p.ID+"/upload/init",
		map[string]any{"path": "x.md", "size": 1}, cookies["alice"]); r.Code == 200 {
		t.Errorf("a deleted project still accepts an upload: %s", r.Body)
	}
	// Re-creating the same NAME must not re-open the old storage prefix.
	rec = doAs(t, h, "POST", "/api/projects", map[string]string{"name": p.Name}, cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("recreate: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Project Project `json:"project"`
		Created bool    `json:"created"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.Created || out.Project.ID == p.ID {
		t.Errorf("re-creating a deleted project's name reused its id (%s): the retired "+
			"storage prefix comes back with it", out.Project.ID)
	}
	if r := doAs(t, h, "GET", "/api/p/"+out.Project.ID+"/file?path=wiki/secret.md", nil, cookies["alice"]); r.Code == 200 {
		t.Errorf("the new project serves the deleted one's content: %s", r.Body)
	}
	_ = srv
}

// ---- new column: org rename ----------------------------------------------

// Nothing on the hub is supposed to be keyed on an org's NAME — create-or-join
// is name-scoped per org ID, membership is by id, share liveness resolves the
// id. There was no column for it, so nothing said so. A rename that moved any
// of those would be a silent re-authorization triggered by a label edit.
func TestSec_Matrix_OrgRenameMovesNoGrant(t *testing.T) {
	h, srv, _, root, cookies, p := sec13mHub(t)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.put("dev14f", "wiki/secret.md", "content")

	rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/shares", map[string]string{"path": "wiki/secret.md"}, cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("fixture: mint share: %d %s", rec.Code, rec.Body)
	}
	var sh struct {
		Token string `json:"token"`
	}
	json.Unmarshal(rec.Body.Bytes(), &sh)
	// Carol is denied this project explicitly.
	if r := doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions/carol@x.io",
		map[string]string{"level": PermNone}, cookies["alice"]); r.Code != 200 {
		t.Fatalf("fixture: deny carol: %d %s", r.Code, r.Body)
	}

	// Only an owner may rename.
	for _, who := range []string{"bob", "carol"} {
		if r := doAs(t, h, "PATCH", "/api/orgs/"+p.Org, map[string]string{"name": "pwned"}, cookies[who]); r.Code != http.StatusForbidden {
			t.Errorf("plain member %s renamed the org: %d %s", who, r.Code, r.Body)
		}
	}
	if r := doAs(t, h, "PATCH", "/api/orgs/"+p.Org, map[string]string{"name": "Renamed Org"}, cookies["alice"]); r.Code != 200 {
		t.Fatalf("owner rename: %d %s", r.Code, r.Body)
	}

	// Everything the rename must not have touched.
	if lvl := srv.projectPerm(sec13mAs(cookies["carol"]), p.ID); lvl != PermNone {
		t.Errorf("after the org rename carol's level is %q, not the %q she was denied", lvl, PermNone)
	}
	if r := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, cookies["carol"]); r.Code != http.StatusForbidden {
		t.Errorf("an org rename gave a denied member read access back: %d %s", r.Code, r.Body)
	}
	if r := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, cookies["bob"]); r.Code != 200 {
		t.Errorf("an org rename took an ordinary member's access away: %d %s", r.Code, r.Body)
	}
	if r := doHTTP(h, httptest.NewRequest("GET", "/s/"+sh.Token, nil)); r.Code != 200 {
		t.Errorf("an org rename killed a live share link: %d", r.Code)
	}
	// Create-or-join by name still resolves the SAME project (names are scoped
	// by org id, not by org name) — and still refuses the denied member.
	rec = doAs(t, h, "POST", "/api/projects", map[string]string{"name": p.Name}, cookies["alice"])
	var out struct {
		Project Project `json:"project"`
		Created bool    `json:"created"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Created || out.Project.ID != p.ID {
		t.Errorf("after the org rename, create-or-join by name minted a NEW project %s (created=%v) "+
			"instead of resolving %s: project names are scoped by org id, and a second row with the "+
			"same name splits the team", out.Project.ID, out.Created, p.ID)
	}
	if r := doAs(t, h, "POST", "/api/projects", map[string]string{"name": p.Name}, cookies["carol"]); r.Code != http.StatusForbidden {
		t.Errorf("after the org rename create-or-join handed the denied member the project id: %d %s", r.Code, r.Body)
	}
}

// ---- cell: template seeding x revocation ---------------------------------

// seedTemplate's writes had never been touched by a revocation test in
// fourteen rounds. It is reachable only from handleProjectCreate's `created`
// branch, so the questions are (a) can a principal who has been cut off reach
// it at all, and (b) can the create-or-join path be used to seed INTO an
// existing project — which would be a write door with no PermWrite on it.
func TestSec_Matrix_TemplateSeedingIsNotAWriteDoorIntoAnExistingProject(t *testing.T) {
	h, srv, _, _, cookies, p := sec13mHub(t)

	// Alice seeds a real template into a project of her own: the positive
	// control that seeding works at all.
	rec := doAs(t, h, "POST", "/api/projects",
		map[string]string{"name": "seeded", "template": "docs"}, cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("fixture: seed a new project: %d %s", rec.Code, rec.Body)
	}
	var seeded struct {
		Project Project `json:"project"`
	}
	json.Unmarshal(rec.Body.Bytes(), &seeded)
	before := doAs(t, h, "GET", "/api/p/"+seeded.Project.ID+"/tree", nil, cookies["alice"]).Body.String()
	if !strings.Contains(before, ".md") {
		t.Fatalf("fixture: the template seeded nothing: %s", before)
	}

	// (a) A member removed from the org cannot create-and-seed in it.
	if r := doAs(t, h, "DELETE", "/api/orgs/"+p.Org+"/members/bob@x.io", nil, cookies["alice"]); r.Code != 200 {
		t.Fatalf("remove bob: %d %s", r.Code, r.Body)
	}
	if r := doAs(t, h, "POST", "/api/projects",
		map[string]string{"name": "bobs-seed", "org": p.Org, "template": "docs"}, cookies["bob"]); r.Code == 200 {
		t.Errorf("a removed org member created and seeded a project in the org he was removed from: %s", r.Body)
	}

	// (b) Create-or-join with a template against an EXISTING project must
	// neither seed nor be an unpermissioned write. Carol is denied it.
	if r := doAs(t, h, "PUT", "/api/p/"+seeded.Project.ID+"/permissions/carol@x.io",
		map[string]string{"level": PermNone}, cookies["alice"]); r.Code != 200 {
		t.Fatalf("deny carol: %d %s", r.Code, r.Body)
	}
	if r := doAs(t, h, "POST", "/api/projects",
		map[string]string{"name": "seeded", "org": p.Org, "template": "para"}, cookies["carol"]); r.Code != http.StatusForbidden {
		t.Errorf("a denied member reached an existing project through create-or-join with a template: %d %s",
			r.Code, r.Body)
	}
	after := doAs(t, h, "GET", "/api/p/"+seeded.Project.ID+"/tree", nil, cookies["alice"]).Body.String()
	if after != before {
		t.Errorf("create-or-join with a template CHANGED an existing project's tree — a write door "+
			"with no PermWrite on it.\nbefore: %s\nafter:  %s", before, after)
	}
	// And a read-only member gets the same answer: the project, unchanged.
	if r := doAs(t, h, "PUT", "/api/p/"+seeded.Project.ID+"/permissions/carol@x.io",
		map[string]string{"level": PermRead}, cookies["alice"]); r.Code != 200 {
		t.Fatalf("grant carol read: %d %s", r.Code, r.Body)
	}
	if r := doAs(t, h, "POST", "/api/projects",
		map[string]string{"name": "seeded", "org": p.Org, "template": "para"}, cookies["carol"]); r.Code != 200 {
		t.Fatalf("read-only create-or-join: %d %s", r.Code, r.Body)
	}
	if now := doAs(t, h, "GET", "/api/p/"+seeded.Project.ID+"/tree", nil, cookies["alice"]).Body.String(); now != before {
		t.Errorf("a read-only member re-seeded an existing project through create-or-join:\n%s", now)
	}
	_ = srv
}

// ---- cell: storage reservations x revocation -----------------------------

// reserve.go's grants had never been crossed with any revocation. The grant
// itself is accounting, but the CAPABILITY it accompanies is a presigned URL
// that writes straight into the object store past every check this package
// makes — so the door that mints one has to close the moment write does, and a
// grant booked for one project must never be settled by a request naming
// another.
func TestSec_Matrix_ASignedUploadGrantDiesWithTheWritePermission(t *testing.T) {
	h, srv, _, _, cookies, p := sec13mHub(t)
	key := "blobs/" + strings.Repeat("c", 64)
	org := p.Org

	// Positive control: bob has write, so the sign door answers him.
	if r := doAs(t, h, "POST", "/api/p/"+p.ID+"/store/sign",
		map[string]any{"key": key, "size": 4096}, cookies["bob"]); r.Code != 200 {
		t.Fatalf("fixture: bob should reach the sign door: %d %s", r.Code, r.Body)
	}
	beforeReserved := srv.reservedBytes(org)

	// Demote bob to read on the project.
	if r := doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions/bob@x.io",
		map[string]string{"level": PermRead}, cookies["alice"]); r.Code != 200 {
		t.Fatalf("demote bob: %d %s", r.Code, r.Body)
	}
	if r := doAs(t, h, "POST", "/api/p/"+p.ID+"/store/sign",
		map[string]any{"key": key, "size": 4096}, cookies["bob"]); r.Code != http.StatusForbidden {
		t.Errorf("a read-only member still mints a direct-to-storage upload grant: %d %s", r.Code, r.Body)
	}
	// Removed from the org entirely: same answer.
	if r := doAs(t, h, "DELETE", "/api/orgs/"+p.Org+"/members/bob@x.io", nil, cookies["alice"]); r.Code != 200 {
		t.Fatalf("remove bob: %d %s", r.Code, r.Body)
	}
	if r := doAs(t, h, "POST", "/api/p/"+p.ID+"/store/sign",
		map[string]any{"key": key, "size": 4096}, cookies["bob"]); r.Code != http.StatusForbidden {
		t.Errorf("a removed org member still mints a direct-to-storage upload grant: %d %s", r.Code, r.Body)
	}
	// The revocation must not have SETTLED the outstanding grant either: bytes
	// that arrive through a URL already handed out still have to be charged.
	if now := srv.reservedBytes(org); now < beforeReserved {
		t.Errorf("revoking the minter's write released its outstanding reservation "+
			"(%d -> %d): the presigned URL is still live, so those bytes become free", beforeReserved, now)
	}
	// A grant is keyed by (project, key): another project must not settle it.
	rec := doAs(t, h, "POST", "/api/projects", map[string]string{"name": "other", "org": p.Org}, cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("second project: %d %s", rec.Code, rec.Body)
	}
	var other struct {
		Project Project `json:"project"`
	}
	json.Unmarshal(rec.Body.Bytes(), &other)
	if !srv.claimGrant(other.Project.ID, key) {
		// The desired outcome: nothing to claim over there.
	} else {
		t.Errorf("a grant booked for project %s was settled by naming project %s", p.ID, other.Project.ID)
	}
	if now := srv.reservedBytes(org); now != beforeReserved {
		t.Errorf("a foreign project's claim moved the org's reserved total: %d -> %d", beforeReserved, now)
	}
}

// ---- cell: read-ledger buckets x offboarding -----------------------------

// Round 13 asserted /heat never NAMES a departed member on the default query.
// The two paths it did not take are the ones an actor id survives longest in:
// the all-time fold (?days=0, the rows retention keeps forever) and the
// ?by=device axis. Both are re-checked here after the account is deleted AND
// after a hub restart that reloads the buckets from disk, because the buckets
// themselves are never swept.
func TestSec_Matrix_NoHeatQueryNamesADepartedMemberAfterARestart(t *testing.T) {
	dir, root := t.TempDir(), t.TempDir()
	srv := sec13mServer(t, dir, root)
	readsPath := filepath.Join(dir, "reads.json")
	ledger, err := OpenReadLedger(readsPath, 400)
	if err != nil {
		t.Fatal(err)
	}
	srv.Reads = ledger
	h := srv.Handler()
	cookies := map[string]*http.Cookie{}
	for _, who := range []string{"alice", "bob"} {
		cookies[who] = signupAndSession(t, h, who+"@x.io", strings.ToUpper(who[:1])+who[1:], "password1")
	}
	rec := doAs(t, h, "POST", "/api/projects", map[string]string{"name": "wiki"}, cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("fixture: create project: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Project Project `json:"project"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	p := out.Project
	orgs := srv.Dir.(LocalDirectory).OrgDB
	if err := orgs.AddMember(p.Org, "bob@x.io", RoleMember); err != nil {
		t.Fatal(err)
	}
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.put("dev14i", "wiki/secret.md", "content")

	// Bob reads the file (human bucket, actor = his email) and reports an
	// agent read from his own device (agent bucket, actor = the device id).
	if err := srv.bindDevice("bob@x.io", sec14mBindReq("devbob14i", "bob-laptop")); err != nil {
		t.Fatal(err)
	}
	if r := doAs(t, h, "GET", "/api/p/"+p.ID+"/file?path=wiki/secret.md", nil, cookies["bob"]); r.Code != 200 {
		t.Fatalf("fixture: bob read: %d %s", r.Code, r.Body)
	}
	rr := httptestNewRequestBody("POST", "/api/p/"+p.ID+"/reads",
		[]byte(`{"reads":[{"path":"wiki/secret.md"}]}`))
	rr.Header.Set("X-Bdrive-Device", "devbob14i")
	rr.AddCookie(cookies["bob"])
	if r := doHTTP(h, rr); r.Code != 200 {
		t.Fatalf("fixture: agent read report: %d %s", r.Code, r.Body)
	}
	// Age one bucket past retention so the all-time fold is exercised for real.
	ledger.mu.Lock()
	for k, st := range ledger.byKey {
		if k.Kind != ReadKindHuman {
			continue
		}
		fold := k
		fold.Day = ""
		st.Day = ""
		ledger.byKey[fold] = st
		ledger.dirty[fold] = true
		delete(ledger.byKey, k)
	}
	ledger.mu.Unlock()
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	sec14mDeleteAccount(t, srv, "bob@x.io")

	// A fresh hub process over the same reads.json: the buckets are never
	// swept, so this is the state an operator actually queries months later.
	srv2 := sec13mServer(t, dir, root)
	l2, err := OpenReadLedger(readsPath, 400)
	if err != nil {
		t.Fatal(err)
	}
	srv2.Reads = l2
	h2 := srv2.Handler()
	c2 := sec14mLogin(t, h2, "alice@x.io", "password1")
	for _, u := range []string{
		"/api/p/" + p.ID + "/heat",
		"/api/p/" + p.ID + "/heat?days=0",
		"/api/p/" + p.ID + "/heat?prefix=wiki&days=0",
		"/api/p/" + p.ID + "/heat?by=device&days=0",
	} {
		r := doAs(t, h2, "GET", u, nil, c2)
		if r.Code != 200 {
			t.Fatalf("heat %s: %d %s", u, r.Code, r.Body)
		}
		if strings.Contains(strings.ToLower(r.Body.String()), "bob@x.io") {
			t.Errorf("GET %s names a DELETED account as a reader: %s", u, r.Body)
		}
	}
}

// ---- new column: create-or-join by name, on a second hub process ---------

// ProjectDB got refresh() in round 12 and OrgDB's MUTATORS got it in round 13,
// on the stated rule that a decision made from a map taken at boot is not a
// decision at all. GetOrCreate is the one mutator inside the struct that got
// the fix which still scans db.byID with no refresh — and it is the decision
// behind create-or-JOIN: `bdrive init --project`, the Connect guide, and the
// hub's own create dialog all reach a teammate's project by NAME through it.
//
// On two hub processes in front of one registry, the process that has not seen
// the project answers "no such name" and mints a SECOND row with the same name
// in the same org, with the caller as its creator and admin. The team is then
// silently split across two projects with one name, and the row that decides
// who may join which is whichever process served the request.
func TestSec_Matrix_CreateOrJoinByNameIsHonouredOnEveryHubProcess(t *testing.T) {
	hA, _, dir, root, cookiesA, p := sec13mHub(t)
	_ = hA

	// A second hub process, up now: it has bob's membership and alice's
	// project, so a join by name must resolve to exactly the same id.
	srvB := sec13mServer(t, dir, root)
	hB := srvB.Handler()
	bobB := sec14mLogin(t, hB, "bob@x.io", "password1")

	// Positive control: process B resolves the project fine when asked by id.
	if r := doAs(t, hB, "GET", "/api/p/"+p.ID+"/tree", nil, bobB); r.Code != 200 {
		t.Fatalf("fixture: process B cannot read the project: %d %s", r.Code, r.Body)
	}

	// Alice creates a SECOND project on process A. B has never seen it.
	rec := doAs(t, hA, "POST", "/api/projects", map[string]string{"name": "handbook"}, cookiesA["alice"])
	if rec.Code != 200 {
		t.Fatalf("fixture: create handbook: %d %s", rec.Code, rec.Body)
	}
	var made struct {
		Project Project `json:"project"`
	}
	json.Unmarshal(rec.Body.Bytes(), &made)

	// Bob joins it by name on process B — the real onboarding path.
	rec = doAs(t, hB, "POST", "/api/projects",
		map[string]string{"name": "handbook", "org": p.Org}, bobB)
	if rec.Code != 200 {
		t.Fatalf("join by name: %d %s", rec.Code, rec.Body)
	}
	var joined struct {
		Project Project `json:"project"`
		Created bool    `json:"created"`
	}
	json.Unmarshal(rec.Body.Bytes(), &joined)
	if joined.Created || joined.Project.ID != made.Project.ID {
		t.Errorf("joining %q by name on a second hub process minted a NEW project %s "+
			"(created=%v) instead of resolving %s: GetOrCreate scans a registry map taken at "+
			"boot, so create-or-join answers differently on every replica and the team is split "+
			"across two projects with one name.", "handbook", joined.Project.ID, joined.Created, made.Project.ID)
	}
}

// Both second-process defects above live in the SERVICE struct, not in a repo,
// so they are backend-independent — the shape round 13 said a fix aimed at
// db_file.go would miss. Pinned across every backend the run can reach so a
// fix cannot land in one of them and read as done.
func TestSec_Matrix_StaleServiceMapsAreNotHonouredOnAnySQLBackend(t *testing.T) {
	for _, be := range metaBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			be.reset(t)
			s := be.open(t)
			t.Cleanup(func() { s.Close() })

			// --- device release ---
			a, err := NewDeviceRegistry(s.Devices())
			if err != nil {
				t.Fatal(err)
			}
			if err := a.Bind("bob@x.io", DeviceInfo{ID: "devbob14s", Name: "bob-laptop"}, sec13mAll); err != nil {
				t.Fatal(err)
			}
			b, err := NewDeviceRegistry(s.Devices()) // the second hub process
			if err != nil {
				t.Fatal(err)
			}
			if owner, _ := b.OwnerOf("devbob14s"); owner != "bob@x.io" {
				t.Fatalf("fixture: process B should see the claim, got %q", owner)
			}
			a.Release("bob@x.io")
			if owner, _ := b.OwnerOf("devbob14s"); owner != "" {
				t.Errorf("after Release on process A, process B still says %q owns the device: "+
					"DeviceRegistry.byKey is loaded at open and never re-read", owner)
			}

			// --- create-or-join by name ---
			pa, err := NewProjectDB(s.Projects())
			if err != nil {
				t.Fatal(err)
			}
			pb, err := NewProjectDB(s.Projects()) // the second hub process
			if err != nil {
				t.Fatal(err)
			}
			made, created, err := pa.GetOrCreate("handbook14", "org14")
			if err != nil || !created {
				t.Fatalf("fixture: create: %v created=%v", err, created)
			}
			joined, createdB, err := pb.GetOrCreate("handbook14", "org14")
			if err != nil {
				t.Fatal(err)
			}
			if createdB || joined.ID != made.ID {
				t.Errorf("create-or-join by name minted a second %q in the same org on process B "+
					"(%s vs %s): GetOrCreate is the one mutator in the struct that got refresh() "+
					"which still scans a map taken at boot", "handbook14", joined.ID, made.ID)
			}
		})
	}
}

// ---- new column: reopening the store (migrations re-run, populated) ------

// A hub restart re-runs the schema migration against populated tables, and
// every registry rebuilds its in-memory map from what it finds. Nothing had
// ever asserted that a REVOCATION survives that — only that grants do
// (TestMetaStoreConformance). A revocation that does not survive a restart is
// the same hole as one that does not cross a process, arriving on a schedule
// instead of by accident, and it is the one an operator will never notice.
//
// Every registry, every backend the run can reach.
func TestSec_Matrix_ReopeningTheStoreResurrectsNoRevocation(t *testing.T) {
	for _, be := range metaBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			be.reset(t)
			s1 := be.open(t)

			projects, err := NewProjectDB(s1.Projects())
			if err != nil {
				t.Fatal(err)
			}
			orgs, err := NewOrgDB(s1.Orgs())
			if err != nil {
				t.Fatal(err)
			}
			shares, err := NewShareDB(s1.Shares())
			if err != nil {
				t.Fatal(err)
			}
			devices, err := NewDeviceRegistry(s1.Devices())
			if err != nil {
				t.Fatal(err)
			}
			auth, err := NewBuiltinAuth(s1.Accounts(), true, nil)
			if err != nil {
				t.Fatal(err)
			}

			org, err := orgs.Create("acme14", "alice@x.io")
			if err != nil {
				t.Fatal(err)
			}
			if err := orgs.AddMember(org.ID, "bob@x.io", RoleMember); err != nil {
				t.Fatal(err)
			}
			p, _, err := projects.GetOrCreate("wiki14", org.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := projects.SetPerm(p.ID, "bob@x.io", PermAdmin); err != nil {
				t.Fatal(err)
			}
			if err := projects.SetPerm(p.ID, "carol@x.io", PermAdmin); err != nil {
				t.Fatal(err)
			}
			inv, err := orgs.CreateInvite(org.ID, "alice@x.io", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			sh, err := shares.Create(p.ID, "wiki/secret.md", "alice@x.io", 0, FileInfo{})
			if err != nil {
				t.Fatal(err)
			}
			if err := devices.Bind("bob@x.io", DeviceInfo{ID: "devbob14r", Name: "bob-laptop"}, sec13mAll); err != nil {
				t.Fatal(err)
			}
			u, err := auth.signup("bob@x.io", "Bob", "password1")
			if err != nil {
				t.Fatal(err)
			}
			tok, err := auth.issueToken(u.ID, "devbob14r")
			if err != nil {
				t.Fatal(err)
			}

			// Positive control: every grant is real before the revocations.
			if projects.byIDLevel(p.ID, "bob@x.io") != PermAdmin ||
				orgs.Role(org.ID, "bob@x.io") != RoleMember ||
				!orgs.ValidInvite(inv.Token) {
				t.Fatal("fixture: grants did not take")
			}
			if _, ok := shares.Get(sh.Token); !ok {
				t.Fatal("fixture: the share should be live")
			}
			if owner, _ := devices.OwnerOf("devbob14r"); owner != "bob@x.io" {
				t.Fatalf("fixture: device owner %q", owner)
			}
			if _, ok := auth.userForToken(tok); !ok {
				t.Fatal("fixture: the token should authenticate")
			}

			// Every revocation route this hub has.
			if err := projects.SetPerm(p.ID, "bob@x.io", PermNone); err != nil {
				t.Fatal(err)
			}
			if err := orgs.RemoveMember(org.ID, "bob@x.io"); err != nil {
				t.Fatal(err)
			}
			if !orgs.RevokeInvite(inv.Token) {
				t.Fatal("revoke invite")
			}
			if !shares.Revoke(sh.Token) {
				t.Fatal("revoke share")
			}
			devices.Release("bob@x.io")
			if err := auth.revokeToken(tok); err != nil {
				t.Fatal(err)
			}
			if err := s1.Close(); err != nil {
				t.Fatal(err)
			}

			// The restart: migrations re-run against populated tables, and
			// every registry reloads.
			s2 := be.open(t)
			t.Cleanup(func() { s2.Close() })
			projects2, err := NewProjectDB(s2.Projects())
			if err != nil {
				t.Fatal(err)
			}
			orgs2, err := NewOrgDB(s2.Orgs())
			if err != nil {
				t.Fatal(err)
			}
			shares2, err := NewShareDB(s2.Shares())
			if err != nil {
				t.Fatal(err)
			}
			devices2, err := NewDeviceRegistry(s2.Devices())
			if err != nil {
				t.Fatal(err)
			}
			auth2, err := NewBuiltinAuth(s2.Accounts(), true, nil)
			if err != nil {
				t.Fatal(err)
			}

			if lvl := projects2.byIDLevel(p.ID, "bob@x.io"); lvl != PermNone {
				t.Errorf("after the restart bob's project grant is %q, not the %q he was demoted to",
					lvl, PermNone)
			}
			if role := orgs2.Role(org.ID, "bob@x.io"); role != "" {
				t.Errorf("after the restart bob is back in the org as %q", role)
			}
			if orgs2.ValidInvite(inv.Token) {
				t.Errorf("a revoked org invite is live again after a restart — on the default "+
					"invite-only posture that link also bootstraps accounts (%s)", inv.Token)
			}
			if _, ok := shares2.Get(sh.Token); ok {
				t.Errorf("a revoked public /s/ link is live again after a restart (%s)", sh.Token)
			}
			if owner, _ := devices2.OwnerOf("devbob14r"); owner != "" {
				t.Errorf("a released device claim is back after a restart, owned by %q", owner)
			}
			if _, ok := auth2.userForToken(tok); ok {
				t.Errorf("a revoked device token authenticates again after a restart")
			}
		})
	}
}

// ---- new column: device re-registration ----------------------------------

// A journal is an APPEND-ONLY log — that is the repo's stated data model and
// the reason History can answer "who changed this file?" at all. The hub never
// enforces it: /store/object is a plain object PUT, so the writer of a journal
// can replace it with a SHORTER one and every op it held is gone from replay,
// from every peer, and from the hub's only audit surface.
//
// Two principals reach that, and the second is the device-re-registration cell
// this round was asked to open:
//
//   - the device's own account, erasing its own trail;
//   - and, after offboarding releases the id and the machine is reassigned,
//     whoever inherits it — deleting the record of what the departed member
//     did, with ordinary permissions and one request.
func TestSec_Matrix_AJournalPushCannotEraseOpsTheHubAlreadyHolds(t *testing.T) {
	h, srv, _, _, cookies, p := sec13mHub(t)
	const id = "devbob14h"
	blob := strings.Repeat("a", 64)

	if err := srv.bindDevice("bob@x.io", sec14mBindReq(id, "bob-laptop")); err != nil {
		t.Fatal(err)
	}
	full := secaudOpLine(1, id, "put", "wiki/one.md", blob) +
		secaudOpLine(2, id, "put", "wiki/two.md", blob) +
		secaudOpLine(3, id, "put", "wiki/three.md", blob)
	if r := sec14mStorePut(t, h, p.ID, "journal/"+id+".jsonl", id, []byte(full), cookies["bob"]); r.Code != 200 {
		t.Fatalf("fixture: bob's journal push: %d %s", r.Code, r.Body)
	}
	// Positive control: all three changes are in the audit surface.
	count := func(who string) int {
		r := doAs(t, h, "GET", "/api/p/"+p.ID+"/history", nil, cookies[who])
		if r.Code != 200 {
			t.Fatalf("history: %d %s", r.Code, r.Body)
		}
		var out struct {
			Entries []HistoryEntry `json:"entries"`
		}
		json.Unmarshal(r.Body.Bytes(), &out)
		return len(out.Entries)
	}
	if n := count("alice"); n != 3 {
		t.Fatalf("fixture: history has %d entries, want 3", n)
	}

	// The device's own account rewinds its log to one op.
	short := secaudOpLine(1, id, "put", "wiki/one.md", blob)
	rec := sec14mStorePut(t, h, p.ID, "journal/"+id+".jsonl", id, []byte(short), cookies["bob"])
	if rec.Code == 200 && count("alice") < 3 {
		t.Errorf("a member erased %d of his own changes from the hub's audit trail with one "+
			"object PUT: the journal is append-only in the data model and nowhere else, so "+
			"/store/object accepts a shorter log and every peer replays the truncated one",
			3-count("alice"))
	}

	// The re-registration case: the account is offboarded, the machine is
	// reassigned, and the new holder rewinds the DEPARTED member's log.
	// Put the full log back first, so this half measures its own delta
	// whichever way the half above resolved.
	if r := sec14mStorePut(t, h, p.ID, "journal/"+id+".jsonl", id, []byte(full), cookies["bob"]); r.Code != 200 {
		t.Fatalf("fixture: restoring bob's journal: %d %s", r.Code, r.Body)
	}
	sec14mDeleteAccount(t, srv, "bob@x.io")
	if err := srv.bindDevice("carol@x.io", sec14mBindReq(id, "carol-pc")); err != nil {
		t.Fatalf("the reassigned laptop must be bindable: %v", err)
	}
	before := count("alice")
	rec = sec14mStorePut(t, h, p.ID, "journal/"+id+".jsonl", id,
		[]byte(secaudOpLine(1, id, "put", "wiki/carol.md", blob)), cookies["carol"])
	if rec.Code == 200 && count("alice") < before {
		t.Errorf("after inheriting a departed member's device id, carol deleted %d of that "+
			"member's changes from History with one journal PUT", before-count("alice"))
	}
}

// ---- new column: `bdrive logout`, end to end through the real CLI --------

// Only the HTTP route (DELETE /api/auth/token) had ever been driven. The
// command is what an operator actually runs on a laptop that is going back to
// IT, and the whole point of it is that the credential dies ON THE HUB, not
// just in $BDRIVE_HOME — a local-only clear leaves a live bearer token in
// whatever backup, shell history or disk image the file was already in.
func TestSec_Matrix_CLILogoutKillsTheTokenOnTheHubNotJustOnDisk(t *testing.T) {
	e := newCLIEnv(t)
	settings := filepath.Join(e.home, ".bdrive", "settings.json")
	raw, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("settings.json after login: %v", err)
	}
	var before struct {
		Token string `json:"token"`
		Email string `json:"email"`
	}
	if err := json.Unmarshal(raw, &before); err != nil || before.Token == "" {
		t.Fatalf("login stored no token: %v %s", err, raw)
	}

	bearer := func(tok string) int {
		req, err := http.NewRequest("GET", e.hub.URL+"/api/projects", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// Positive control: the token the CLI holds really is a hub credential.
	if code := bearer(before.Token); code != 200 {
		t.Fatalf("fixture: the logged-in token should reach the hub: %d", code)
	}

	out, err := e.run(t.TempDir(), "logout")
	if err != nil {
		t.Fatalf("bdrive logout: %v\n%s", err, out)
	}

	if code := bearer(before.Token); code == 200 {
		t.Errorf("`bdrive logout` left the device token LIVE on the hub (%d): the command "+
			"cleared $BDRIVE_HOME and the credential in every backup of that file still "+
			"syncs.\noutput:\n%s", code, out)
	}
	after, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("settings.json after logout: %v", err)
	}
	if strings.Contains(string(after), before.Token) {
		t.Errorf("`bdrive logout` left the token in settings.json:\n%s", after)
	}
	if before.Email != "" && strings.Contains(string(after), before.Email) {
		t.Errorf("`bdrive logout` left the signed-in account in settings.json:\n%s", after)
	}
}

// byIDLevel is the explicit grant a project holds for an address, or "" for
// none — read straight out of the registry so the assertion is about what was
// persisted, not about a route's resolution.
func (db *ProjectDB) byIDLevel(id, email string) string {
	p, ok := db.Get(id)
	if !ok {
		return ""
	}
	return p.Perms[normEmail(email)]
}
