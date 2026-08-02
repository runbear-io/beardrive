package webapp

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/remote"
)

// Round 13: the OFFBOARDING MATRIX — what a principal accumulates crossed with
// every route that is supposed to take it away. Rounds 1, 2, 7, 10 and 12 each
// found one grant surviving one revocation by accident; round 12 drew the
// table. These are the cells nobody had ever driven.
//
// The method every test here follows: a POSITIVE CONTROL that proves the
// capability worked, then the real revocation route, then the same request
// again. The delta is the finding.

// ---- fixture -------------------------------------------------------------

// sec13mServer builds one hub PROCESS: fresh in-memory registries over the
// metadata directory `dir` and the storage root `root`. Calling it twice with
// the same paths is two `bdrive serve` processes in front of one database —
// the deployment round 11's own comment names, and the shape of every
// second-process test below.
func sec13mServer(t *testing.T, dir, root string) *Server {
	t.Helper()
	be, err := remote.Open(context.Background(), "file://"+root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { be.Close() })
	projects, err := OpenProjectDB(filepath.Join(dir, "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := OpenBuiltinAuth(filepath.Join(dir, "auth.json"), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	orgs, err := OpenOrgDB(filepath.Join(dir, "orgs.json"))
	if err != nil {
		t.Fatal(err)
	}
	shares, err := OpenShareDB(filepath.Join(dir, "shares.json"))
	if err != nil {
		t.Fatal(err)
	}
	devices, err := OpenDeviceRegistry(filepath.Join(dir, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &Server{
		Root: be, Projects: projects, Device: webDevice,
		Upload: UploadConfig{Enabled: true},
		Auth:   auth, Dir: LocalDirectory{OrgDB: orgs}, Shares: shares, Devices: devices,
	}
}

// sec13mHub is process A plus accounts: alice owns the org and the project,
// bob and carol are plain members.
func sec13mHub(t *testing.T) (h http.Handler, srv *Server, dir, root string,
	cookies map[string]*http.Cookie, p Project) {
	t.Helper()
	dir, root = t.TempDir(), t.TempDir()
	srv = sec13mServer(t, dir, root)
	h = srv.Handler()
	cookies = map[string]*http.Cookie{}
	for _, who := range []string{"alice", "bob", "carol"} {
		cookies[who] = signupAndSession(t, h, who+"@x.io",
			strings.ToUpper(who[:1])+who[1:], "password1")
	}
	rec := doAs(t, h, "POST", "/api/projects", map[string]string{"name": "wiki"}, cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("fixture: create project: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Project Project `json:"project"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	p = out.Project
	orgs := srv.Dir.(LocalDirectory).OrgDB
	for _, who := range []string{"bob", "carol"} {
		if err := orgs.AddMember(p.Org, who+"@x.io", RoleMember); err != nil {
			t.Fatal(err)
		}
	}
	return h, srv, dir, root, cookies, p
}

// sec13mBearer runs a request authenticated by a device token.
func sec13mBearer(h http.Handler, method, url, tok string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, url, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	return doHTTP(h, req)
}

// sec13mAll is Bind's "everyone is visible" predicate.
func sec13mAll(string) bool { return true }

// ---- cell 5: device token x second hub process ---------------------------

// A device token revoked on one hub process (`bdrive logout`, DELETE
// /api/auth/token) must be dead on every process. BuiltinAuth answers
// userForToken out of a.tokens, a map loaded once at open — the exact defect
// round 11 fixed for ProjectDB's write side and round 12 fixed for its READ
// side (ProjectDB.refresh). BuiltinAuth never got either.
func TestSec_Matrix_ARevokedDeviceTokenIsDeadOnEveryHubProcess(t *testing.T) {
	h, srv, dir, root, _, p := sec13mHub(t)
	auth := srv.Auth.(*BuiltinAuth)

	auth.mu.Lock()
	uid := auth.findByEmail("bob@x.io").ID
	auth.mu.Unlock()
	tok, err := auth.issueToken(uid, "devbob13")
	if err != nil {
		t.Fatal(err)
	}

	// Process B, up before the revocation, like any second replica.
	srvB := sec13mServer(t, dir, root)
	hB := srvB.Handler()

	// Positive control: the token reads the project on BOTH processes.
	if rec := sec13mBearer(h, "GET", "/api/p/"+p.ID+"/tree", tok); rec.Code != 200 {
		t.Fatalf("fixture: token on process A: %d %s", rec.Code, rec.Body)
	}
	if rec := sec13mBearer(hB, "GET", "/api/p/"+p.ID+"/tree", tok); rec.Code != 200 {
		t.Fatalf("fixture: token on process B: %d %s", rec.Code, rec.Body)
	}

	// The real revocation route: the token authenticates its own revocation.
	if rec := sec13mBearer(h, "DELETE", "/api/auth/token", tok); rec.Code != 200 {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}
	if rec := sec13mBearer(h, "GET", "/api/p/"+p.ID+"/tree", tok); rec.Code == 200 {
		t.Fatal("fixture: the token should be dead on the process that revoked it")
	}

	if rec := sec13mBearer(hB, "GET", "/api/p/"+p.ID+"/tree", tok); rec.Code == 200 {
		t.Fatalf("a device token revoked on process A still reads the project on process B "+
			"(%d): BuiltinAuth.userForToken answers from a.tokens, loaded once at open. "+
			"`bdrive logout` on a lost laptop revokes on one replica and no other.", rec.Code)
	}
}

// The durable half of the same defect: fileAccountRepo has no reload(). Every
// other file repo grew one (projects r11, orgs/shares/devices r12); auth.json
// alone still rewrites users+tokens from a map taken at open, so ANY ordinary
// write by a second hub process — one login, one signup — puts a revoked
// token row back on disk, where the next restart loads it as live.
func TestSec_Matrix_ASecondHubProcessCannotResurrectARevokedDeviceToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	a, err := OpenBuiltinAuth(path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	alice, err := a.signup("alice@x.io", "Alice", "password1")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := a.issueToken(alice.ID, "devalice13")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.userForToken(tok); !ok {
		t.Fatal("fixture: the fresh token should authenticate")
	}

	// A second hub process, up before the revocation.
	b, err := OpenBuiltinAuth(path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.userForToken(tok); !ok {
		t.Fatal("fixture: B should see the token")
	}

	// Revoked on A, durably (killToken voids the row and deletes it).
	if err := a.revokeToken(tok); err != nil {
		t.Fatal(err)
	}

	// B does one ordinary, unrelated thing: another account signs up.
	if _, err := b.signup("bob@x.io", "Bob", "password1"); err != nil {
		t.Fatal(err)
	}

	// Next restart.
	c, err := OpenBuiltinAuth(path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.userForToken(tok); ok {
		t.Fatalf("after a restart the REVOKED device token authenticates again: an unrelated "+
			"signup on a second hub process rewrote %s from a map loaded at open. "+
			"fileAccountRepo.PutAccount/PutToken/DeleteToken never re-read the file.", path)
	}
}

// ---- account deletion x second hub process (new column x new row) --------

// The same repo defect one row up: an account DENIED by a hub admin
// (/api/admin/pending/{id}/deny -> BuiltinAuth.Deny) comes back — with its
// password hash — when any second process writes auth.json afterwards.
func TestSec_Matrix_ASecondHubProcessCannotResurrectADeletedAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	a, err := OpenBuiltinAuth(path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	mallory, err := a.signup("mallory@x.io", "Mallory", "password1")
	if err != nil {
		t.Fatal(err)
	}
	if a.verifyPassword("mallory@x.io", "password1") == nil {
		t.Fatal("fixture: the account should authenticate before it is denied")
	}

	b, err := OpenBuiltinAuth(path, true, nil) // second process, up first
	if err != nil {
		t.Fatal(err)
	}

	if err := a.Deny(mallory.ID); err != nil {
		t.Fatal(err)
	}
	if a.verifyPassword("mallory@x.io", "password1") != nil {
		t.Fatal("fixture: the denied account should be gone on the process that denied it")
	}

	// B does one ordinary, unrelated thing.
	if _, err := b.signup("newhire@x.io", "New", "password1"); err != nil {
		t.Fatal(err)
	}

	c, err := OpenBuiltinAuth(path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.verifyPassword("mallory@x.io", "password1") != nil {
		t.Fatal("after a restart the DELETED account signs in again with its old password: " +
			"a second hub process's unrelated signup rewrote auth.json from a map loaded at open")
	}
}

// ---- cell 3: org invite x second hub process -----------------------------

// Round 12 gave OrgRepo row-scoped WRITES. The read path never got the
// ProjectDB.refresh treatment, so an invite revoked on one process is still
// redeemable on another — and redeeming one is org membership, which is read
// access to every project in the org (and, on the default invite-only posture,
// bootstraps the account that holds it).
func TestSec_Matrix_ARevokedOrgInviteIsDeadOnEveryHubProcess(t *testing.T) {
	h, _, dir, root, cookies, p := sec13mHub(t)

	rec := doAs(t, h, "POST", "/api/orgs/"+p.Org+"/invites", map[string]string{"expires_in": "24h"}, cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("fixture: mint invite: %d %s", rec.Code, rec.Body)
	}
	var inv struct {
		Token string `json:"token"`
	}
	json.Unmarshal(rec.Body.Bytes(), &inv)

	// Process B, up before the revocation.
	srvB := sec13mServer(t, dir, root)
	hB := srvB.Handler()
	// An outsider with an account on this hub, holding the link.
	outsider := signupAndSession(t, hB, "dave@x.io", "Dave", "password1")

	// Positive control: B honours the invite while it is live.
	if !srvB.Dir.(LocalDirectory).OrgDB.ValidInvite(inv.Token) {
		t.Fatal("fixture: process B should see the live invite")
	}

	// The real revocation route.
	if rec := doAs(t, h, "DELETE", "/api/orgs/"+p.Org+"/invites/"+inv.Token, nil, cookies["alice"]); rec.Code != 200 {
		t.Fatalf("revoke invite: %d %s", rec.Code, rec.Body)
	}

	rec = doAs(t, hB, "POST", "/api/invites/"+inv.Token, nil, outsider)
	if rec.Code == 200 {
		t.Fatalf("a REVOKED invite still joins the org on a second hub process (%d %s): "+
			"OrgDB.Redeem answers from db.invites, loaded once at open. "+
			"dave is now a member and reads every project in the org.", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if role := srvB.Dir.Role(p.Org, "dave@x.io"); role != "" {
		t.Fatalf("dave holds role %q in the org through a revoked invite", role)
	}
}

// ---- org membership x second hub process (the outer wall) ----------------

// The same staleness on membership itself, which is the wall every per-project
// route 403s on. An offboarding that takes effect on one replica and no other
// is not an offboarding.
func TestSec_Matrix_RemovedOrgMembershipIsGoneOnEveryHubProcess(t *testing.T) {
	h, _, dir, root, cookies, p := sec13mHub(t)

	srvB := sec13mServer(t, dir, root)
	hB := srvB.Handler()

	// Positive control: bob reads the project on both processes.
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, cookies["bob"]); rec.Code != 200 {
		t.Fatalf("fixture: bob on A: %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, hB, "GET", "/api/p/"+p.ID+"/tree", nil, cookies["bob"]); rec.Code != 200 {
		t.Fatalf("fixture: bob on B: %d %s", rec.Code, rec.Body)
	}

	// The real revocation route.
	if rec := doAs(t, h, "DELETE", "/api/orgs/"+p.Org+"/members/bob@x.io", nil, cookies["alice"]); rec.Code != 200 {
		t.Fatalf("remove member: %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, cookies["bob"]); rec.Code == 200 {
		t.Fatal("fixture: bob should be out on the process that removed him")
	}

	if rec := doAs(t, hB, "GET", "/api/p/"+p.ID+"/tree", nil, cookies["bob"]); rec.Code == 200 {
		t.Fatalf("a removed member still reads the project on a second hub process (%d): "+
			"OrgDB.Role answers from db.byID, loaded once at open. ProjectDB got refresh() "+
			"in round 12; the org wall in front of it did not.", rec.Code)
	}
}

// ---- the staleness is in the SERVICE, not the file backend ---------------

// The two revocations above are not a file-backend bug. BuiltinAuth and OrgDB
// keep in-memory maps loaded at open and never re-read them, whatever repo is
// underneath — so the same revocation is stale on a hub running sqlite or
// Postgres too. Named separately because a fix aimed at db_file.go would leave
// exactly the managed deployments (two replicas, one Postgres) that the
// second-process shape describes still broken.
//
// Set BDRIVE_TEST_POSTGRES to run the same body against Postgres too.
func TestSec_Matrix_RevocationIsHonouredByEverySQLBackedProcess(t *testing.T) {
	backends := []struct{ driver, dsn string }{{"sqlite", filepath.Join(t.TempDir(), "meta.db")}}
	if dsn := os.Getenv("BDRIVE_TEST_POSTGRES"); dsn != "" {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		db.Exec(`DROP TABLE IF EXISTS accounts, tokens, auth_policy, projects, orgs, org_members, invites, shares, devices, read_stats`)
		db.Close()
		backends = append(backends, struct{ driver, dsn string }{"pgx", dsn})
	} else {
		t.Log("BDRIVE_TEST_POSTGRES not set — postgres UNTESTED in this run")
	}
	for _, be := range backends {
		t.Run(be.driver, func(t *testing.T) { sec13mStaleRevocation(t, be.driver, be.dsn) })
	}
}

func sec13mStaleRevocation(t *testing.T, driver, dsn string) {
	stA, err := OpenSQLStore(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer stA.Close()

	authA, err := NewBuiltinAuth(stA.Accounts(), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	orgsA, err := NewOrgDB(stA.Orgs())
	if err != nil {
		t.Fatal(err)
	}
	alice, err := authA.signup("alice@x.io", "Alice", "password1")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := authA.issueToken(alice.ID, "devalice13")
	if err != nil {
		t.Fatal(err)
	}
	o, err := orgsA.Create("acme", "alice@x.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := orgsA.AddMember(o.ID, "bob@x.io", RoleMember); err != nil {
		t.Fatal(err)
	}

	// Process B over the SAME database, up before the revocations.
	stB, err := OpenSQLStore(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer stB.Close()
	authB, err := NewBuiltinAuth(stB.Accounts(), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	orgsB, err := NewOrgDB(stB.Orgs())
	if err != nil {
		t.Fatal(err)
	}
	// Positive control.
	if _, ok := authB.userForToken(tok); !ok {
		t.Fatal("fixture: B should honour the live token")
	}
	if orgsB.Role(o.ID, "bob@x.io") != RoleMember {
		t.Fatal("fixture: B should see bob's membership")
	}

	if err := authA.revokeToken(tok); err != nil {
		t.Fatal(err)
	}
	if err := orgsA.RemoveMember(o.ID, "bob@x.io"); err != nil {
		t.Fatal(err)
	}

	if _, ok := authB.userForToken(tok); ok {
		t.Error("a token revoked on process A still authenticates on a SQL-backed process B: " +
			"BuiltinAuth.tokens is loaded at open and never re-read")
	}
	if role := orgsB.Role(o.ID, "bob@x.io"); role != "" {
		t.Errorf("a removed member still holds role %q on a SQL-backed process B: "+
			"OrgDB.byID is loaded at open and never re-read", role)
	}
}

// ---- cell 4: device binding x account deletion ---------------------------

// Server.offboard drops project grants and org roles. It never touches
// DeviceRegistry, and OwnerOf is what decides who may write a journal — so a
// deleted account keeps a hub-wide claim on its device id forever.
//
// The consequence is not theoretical: the offboarding scenario IS "the laptop
// goes to the next hire". Bind refuses an id owned by another account, and
// when that account is invisible (it no longer exists, so it shares no org)
// Bind silently binds NOTHING and lets the login succeed — after which every
// push from that machine 403s with "run `bdrive login`", which is what the
// user just did. The id is unusable by anyone, permanently.
func TestSec_Matrix_AccountDeletionReleasesTheDeviceBinding(t *testing.T) {
	h, srv, _, _, _, _ := sec13mHub(t)
	_ = h
	auth := srv.Auth.(*BuiltinAuth)

	const id = "devlaptop13"
	bind := func(email string) error {
		req := httptest.NewRequest("POST", "/auth/cli", nil)
		req.Header.Set("X-Bdrive-Device", id)
		req.Header.Set("X-Bdrive-Device-Name", "the-laptop")
		return srv.bindDevice(email, req) // exactly what finishLogin calls
	}

	// Alice's machine signs in: the id becomes hers, hub-wide.
	if err := bind("alice@x.io"); err != nil {
		t.Fatal(err)
	}
	if owner, _ := srv.Devices.OwnerOf(id); owner != "alice@x.io" {
		t.Fatalf("fixture: OwnerOf = %q", owner)
	}
	// Positive control: while alice's account lives, the claim is hers and
	// bob cannot take it. That refusal is correct.
	if err := bind("bob@x.io"); err == nil {
		t.Fatal("fixture: bob should not take a live account's device id")
	}

	// The real revocation route: alice's ACCOUNT is deleted.
	auth.mu.Lock()
	aliceID := auth.findByEmail("alice@x.io").ID
	auth.mu.Unlock()
	if err := auth.Deny(aliceID); err != nil {
		t.Fatal(err)
	}
	if auth.verifyPassword("alice@x.io", "password1") != nil {
		t.Fatal("fixture: alice's account should be gone")
	}

	if owner, _ := srv.Devices.OwnerOf(id); owner != "" {
		t.Fatalf("the device id is still claimed by %q, an account this hub deleted: "+
			"Server.offboard never touches DeviceRegistry, and OwnerOf is the WRITE gate "+
			"(store.go ownJournal). The grant outlived the account.", owner)
	}
	// And the machine must be usable by whoever inherits it.
	if err := bind("bob@x.io"); err != nil {
		t.Fatalf("bob cannot bind the reassigned laptop after its previous owner's account "+
			"was deleted: %v", err)
	}
	if owner, _ := srv.Devices.OwnerOf(id); owner != "bob@x.io" {
		t.Fatalf("after a successful bind OwnerOf = %q, want bob@x.io — a login that binds "+
			"nothing hands back a token that cannot push, forever", owner)
	}
}

// ---- cell 9: read-ledger buckets x second hub process --------------------

// fileReadRepo is the last file repo with no reload(): PutBatch and
// DeleteBatch rewrite reads.json from a map taken at open. A second hub
// process's routine flush therefore erases whatever the first recorded since —
// the read heatmap is the surface an operator reads to decide what is stale
// and who is consuming what, so this is audit data that silently disappears.
func TestSec_Matrix_ASecondHubProcessDoesNotEraseReadBuckets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reads.json")
	a, err := OpenReadLedger(path, 400)
	if err != nil {
		t.Fatal(err)
	}
	a.Record("proj13", "wiki/old.md", ReadKindHuman, "alice@x.io")
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	// Second hub process, up now — it loads the bucket above.
	b, err := OpenReadLedger(path, 400)
	if err != nil {
		t.Fatal(err)
	}

	// A records a new read and flushes it.
	a.Record("proj13", "wiki/new.md", ReadKindHuman, "alice@x.io")
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	// B does one ordinary, unrelated thing: it flushes its own read.
	b.Record("proj13", "wiki/other.md", ReadKindHuman, "bob@x.io")
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	// Next restart.
	c, err := OpenReadLedger(path, 400)
	if err != nil {
		t.Fatal(err)
	}
	heat := c.Heat("proj13", "", time.Time{})
	for _, p := range []string{"wiki/old.md", "wiki/new.md", "wiki/other.md"} {
		if heat[p].Human == 0 {
			t.Errorf("read telemetry for %q is gone after a restart: fileReadRepo.PutBatch "+
				"rewrites reads.json from a map loaded at open, so a second hub process's "+
				"flush drops every bucket the first recorded since. heat = %+v", p, heat)
		}
	}
}

// ---- cell 1: project grant x demotion (owner -> member) ------------------

// An org owner is implicitly admin on every project in the org and holds no
// explicit grant (grantable refuses to write one). Demotion must therefore
// drop the admin capability outright.
func TestSec_Matrix_DemotionDropsImplicitProjectAdmin(t *testing.T) {
	h, srv, _, _, cookies, p := sec13mHub(t)
	orgs := srv.Dir.(LocalDirectory).OrgDB
	// A second owner, so the demotion is not the last-owner refusal.
	if err := orgs.SetRole(p.Org, "carol@x.io", RoleOwner); err != nil {
		t.Fatal(err)
	}

	// Positive control: alice, an owner, can edit the project's permissions.
	probe := func(c *http.Cookie) int {
		return doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions/bob@x.io",
			map[string]string{"level": PermRead}, c).Code
	}
	if code := probe(cookies["alice"]); code != 200 {
		t.Fatalf("fixture: owner alice should edit permissions: %d", code)
	}

	// The real revocation route: carol demotes alice to plain member.
	if rec := doAs(t, h, "PATCH", "/api/orgs/"+p.Org+"/members/alice@x.io",
		map[string]string{"role": RoleMember}, cookies["carol"]); rec.Code != 200 {
		t.Fatalf("demote: %d %s", rec.Code, rec.Body)
	}

	if code := probe(cookies["alice"]); code == 200 {
		t.Fatal("a demoted org owner still edits the project's permission map: " +
			"the admin capability outlived the role that granted it")
	}
	if lvl := srv.projectPerm(sec13mAs(cookies["alice"]), p.ID); lvl == PermAdmin {
		t.Fatalf("projectPerm still answers %q for the demoted owner", lvl)
	}
}

// sec13mAs builds a bare request carrying one session cookie, for calling a
// resolver directly.
func sec13mAs(c *http.Cookie) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(c)
	return r
}

// ---- cell 6: mail grants x account deletion ------------------------------

// A "verify" grant signs its holder straight in with no password, and a
// "reset" grant sets one without knowing the old. Deleting the account has to
// take both with it, or the deletion is undone by an email already in an
// inbox.
func TestSec_Matrix_AccountDeletionKillsOutstandingMailGrants(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	a, err := OpenBuiltinAuth(path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := a.signup("mallory@x.io", "Mallory", "password1")
	if err != nil {
		t.Fatal(err)
	}
	verify := a.newGrant("verify", u.ID, time.Hour)
	reset := a.newGrant("reset", u.ID, time.Hour)

	// Positive control: the grants are real capabilities before the deletion.
	a.mu.Lock()
	_, hasV := a.pending[verify]
	_, hasR := a.pending[reset]
	a.mu.Unlock()
	if !hasV || !hasR {
		t.Fatal("fixture: both grants should be outstanding")
	}

	if err := a.Deny(u.ID); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ kind, id string }{{"verify", verify}, {"reset", reset}} {
		if _, ok := a.takeGrant(tc.kind, tc.id); ok {
			t.Errorf("a %s mail grant for a DELETED account is still redeemable: "+
				"the link in that inbox outlived the account", tc.kind)
		}
	}
}

// ---- cell 11: mail grants x second hub process ---------------------------

// Mail grants live only in a.pending and are never persisted, so a grant
// minted on one process cannot be redeemed on another. That is the
// fail-closed direction; this pins it so a future "persist the grants" change
// cannot quietly make a revoked link redeemable on a replica.
func TestSec_Matrix_MailGrantsDoNotCrossHubProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	a, err := OpenBuiltinAuth(path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := a.signup("alice@x.io", "Alice", "password1")
	if err != nil {
		t.Fatal(err)
	}
	reset := a.newGrant("reset", u.ID, time.Hour)

	b, err := OpenBuiltinAuth(path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Positive control: A itself honours it.
	if _, ok := a.takeGrant("reset", reset); !ok {
		t.Fatal("fixture: the minting process should honour its own grant")
	}
	if _, ok := b.takeGrant("reset", reset); ok {
		t.Fatal("a password-reset grant minted on one hub process is redeemable on another; " +
			"revoking it on the minter would then not revoke it")
	}
}

// ---- cells 2 and 10: share link / org invite x password reset ------------

// A password reset is THE action a user takes after a compromise. It must
// leave the thief's session unable to mint anything new — a share link is a
// public URL onto live org content, an invite is org membership plus an
// account bootstrap.
//
// (What it deliberately does NOT do is revoke links the account minted
// earlier; that is a policy call, not a boundary, and is filed as a lead.)
func TestSec_Matrix_PasswordResetLeavesNoSessionAbleToMintSharesOrInvites(t *testing.T) {
	h, srv, _, root, cookies, p := sec13mHub(t)
	auth := srv.Auth.(*BuiltinAuth)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.put("dev1", "wiki/report.html", "<h1>Q3</h1>")
	f.put("dev1", "wiki/notes.md", "# Notes")

	// Positive control: alice's session mints both.
	if rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/shares",
		map[string]string{"path": "wiki/report.html"}, cookies["alice"]); rec.Code != 200 {
		t.Fatalf("fixture: mint share: %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "POST", "/api/orgs/"+p.Org+"/invites", nil, cookies["alice"]); rec.Code != 200 {
		t.Fatalf("fixture: mint invite: %d %s", rec.Code, rec.Body)
	}

	// The real recovery route: a password reset through the confirm page.
	auth.mu.Lock()
	uid := auth.findByEmail("alice@x.io").ID
	auth.mu.Unlock()
	grant := auth.newGrant("reset", uid, time.Hour)
	form := "token=" + grant + "&password=password2"
	req := httptest.NewRequest("POST", "/auth/reset/confirm", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rec := doHTTP(h, req); rec.Code != 200 && rec.Code != http.StatusSeeOther {
		t.Fatalf("reset confirm: %d %s", rec.Code, rec.Body)
	}

	if rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/shares",
		map[string]string{"path": "wiki/notes.md"}, cookies["alice"]); rec.Code == 200 {
		t.Fatal("the pre-reset session still mints public share links after the password reset")
	}
	if rec := doAs(t, h, "POST", "/api/orgs/"+p.Org+"/invites", nil, cookies["alice"]); rec.Code == 200 {
		t.Fatal("the pre-reset session still mints org invites after the password reset")
	}
}

// ---- cells 7 and 8: read-ledger buckets x removal / account deletion -----

// Buckets carry the actor (email, device id, share token) and nothing removes
// them when a member leaves or an account is deleted. That is a retention
// question; the BOUNDARY question is whether any of it reaches an API
// response. It must not, in either heat shape, for a member who is gone.
func TestSec_Matrix_HeatNeverNamesADepartedMember(t *testing.T) {
	h, srv, _, _, cookies, p := sec13mHub(t)
	ledger, err := OpenReadLedger(filepath.Join(t.TempDir(), "reads.json"), 400)
	if err != nil {
		t.Fatal(err)
	}
	srv.Reads = ledger

	// Bob reads, as a human and (via his device) as an agent.
	ledger.Record(p.ID, "wiki/report.html", ReadKindHuman, "bob@x.io")
	ledger.Record(p.ID, "wiki/report.html", ReadKindAgent, "devbob13")
	ledger.Record(p.ID, "wiki/report.html", ReadKindShare, "sometoken/10.0.0.1")

	// Positive control: heat answers at all.
	rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/heat?days=0", nil, cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("fixture: heat: %d %s", rec.Code, rec.Body)
	}

	// The real revocation routes: removed from the org, then deleted.
	if rec := doAs(t, h, "DELETE", "/api/orgs/"+p.Org+"/members/bob@x.io", nil, cookies["alice"]); rec.Code != 200 {
		t.Fatalf("remove member: %d %s", rec.Code, rec.Body)
	}
	auth := srv.Auth.(*BuiltinAuth)
	auth.mu.Lock()
	bobID := auth.findByEmail("bob@x.io").ID
	auth.mu.Unlock()
	if err := auth.Deny(bobID); err != nil {
		t.Fatal(err)
	}

	for _, u := range []string{"/heat?days=0", "/heat?days=0&by=device", "/heat?days=0&prefix=wiki"} {
		rec := doAs(t, h, "GET", "/api/p/"+p.ID+u, nil, cookies["carol"])
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", u, rec.Code, rec.Body)
		}
		body := rec.Body.String()
		for _, secret := range []string{"bob@x.io", "sometoken"} {
			if strings.Contains(body, secret) {
				t.Errorf("GET %s reports %q — a departed account's identity out of a read bucket: %s",
					u, secret, body)
			}
		}
	}
}

// ---- new column: project delete ------------------------------------------

// Deleting a project is a revocation too, and the share rows for it stay in
// shares.json (nothing sweeps them). The public URL must stop serving all the
// same — the blobs and journals are deliberately left in storage, so a link
// that still resolved would serve the deleted project's content to anyone.
func TestSec_Matrix_ProjectDeleteKillsItsPublicShareLinks(t *testing.T) {
	h, _, _, root, cookies, p := sec13mHub(t)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.put("dev1", "wiki/notes.md", "# Notes\n\nsecret")

	rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/shares", map[string]string{"path": "wiki/notes.md"}, cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("fixture: mint share: %d %s", rec.Code, rec.Body)
	}
	var sh struct {
		Token string `json:"token"`
	}
	json.Unmarshal(rec.Body.Bytes(), &sh)

	// Positive control: the link serves to an anonymous stranger.
	if rec := doHTTP(h, httptest.NewRequest("GET", "/s/"+sh.Token, nil)); rec.Code != 200 {
		t.Fatalf("fixture: the fresh link should serve: %d %s", rec.Code, rec.Body)
	}

	// The real revocation route.
	if rec := doAs(t, h, "DELETE", "/api/projects/"+p.ID, nil, cookies["alice"]); rec.Code != 200 {
		t.Fatalf("delete project: %d %s", rec.Code, rec.Body)
	}

	if rec := doHTTP(h, httptest.NewRequest("GET", "/s/"+sh.Token, nil)); rec.Code == 200 {
		t.Fatalf("GET /s/%s still serves the deleted project's file to an anonymous stranger", sh.Token)
	}
}
