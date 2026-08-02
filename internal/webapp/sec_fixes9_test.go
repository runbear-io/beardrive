package webapp

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// ---- helpers (slug: secfx9) ----

// secfx9Store issues one /store request as `who`, with the device headers a
// real client sends. It is deliberately NOT secRegisterDevice: the point of
// several tests here is which door registers a device and which does not.
func secfx9Store(t *testing.T, h http.Handler, method, url string, body string, c *http.Cookie, dev string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, url, nil)
	} else {
		r = httptest.NewRequest(method, url, strings.NewReader(body))
	}
	r.Header.Set("X-Bdrive-Device", dev)
	r.Header.Set("X-Bdrive-Device-Name", "laptop-"+dev)
	r.Header.Set("X-Bdrive-Os", "darwin")
	if c != nil {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// secfx9SetPerm grants one account an explicit level on a project, as alice.
func secfx9SetPerm(t *testing.T, h http.Handler, p Project, admin *http.Cookie, email, level string) {
	t.Helper()
	rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions/"+email, map[string]string{"level": level}, admin)
	if rec.Code != 200 {
		t.Fatalf("set %s=%s: %d %s", email, level, rec.Code, rec.Body)
	}
}

// secfx9Op is one journal line naming a device, the shape a real client writes.
func secfx9Op(seq int, dev, path, blob string) string {
	n := strconv.Itoa(seq)
	return `{"seq":` + n + `,"lamport":` + n + `,"time":"2026-01-01T00:00:00Z","device":"` + dev +
		`","kind":"put","path":"` + path + `","blob":"` + blob + `","size":3}` + "\n"
}

// ---------------------------------------------------------------------------
// Row 5 / the design decision deferred since round 6: a device that syncs with
// READ permission everywhere can never register, so its id stays unclaimed
// hub-wide forever and the first member with write on ANY project takes it.
//
// Round 5 named the residual; round 8 made it permanent by demoting every read
// door to refreshDevice (which records only into a row the caller ALREADY
// owns). A read-only member's device therefore never reaches observeDevice —
// the only claim site is an authorized journal PUT, which PermWrite gates.
//
// The delta that proves this is the server's decision and not a fixture
// problem: carol's own journal PUT is the SAME request that succeeds the
// moment she has write, and bob's PUT of carol's journal key is refused the
// moment carol's row exists. Nothing about the request changes — only who got
// to the unclaimed id first.
// ---------------------------------------------------------------------------

func TestSec_Device_AReadOnlyMembersDeviceIdIsNotFreeForTheTaking(t *testing.T) {
	h, srv, c, p := permHub(t)
	const carolDev = "c0ffee000001"

	// carol is a read-only member of the only project on the hub.
	secfx9SetPerm(t, h, p, c["alice"], "carol@x.io", PermRead)

	// Her machine signed in — which is how it got the token its daemon syncs
	// with, and, since round 11, where the hub binds the device id it names to
	// her account (DeviceRegistry.Bind, from BuiltinAuth.finishLogin). This
	// line is the fix this test asked for, expressed in the fixture: when it
	// was written the ONLY way to create a binding was an authorized journal
	// PUT, which a read-only member can never make, which is exactly the hole
	// it reports. Every assertion below is unchanged, and both still fail
	// without the fix: without the binding OwnerOf answers nobody, and without
	// deleting ownJournal's `!known && journalNames` arm bob takes the id.
	if rec := secRegisterDevice(t, h, p.ID, c["carol"], carolDev, "carol-laptop", "darwin"); rec.Code != 200 {
		t.Fatalf("setup: carol's device could not sign in: %d %s", rec.Code, rec.Body)
	}

	// She syncs, exactly as her daemon does: list, exists, get. Every one of
	// these is a request her permission allows and every one carries her
	// device id.
	for _, u := range []string{
		"/api/p/" + p.ID + "/store/list?prefix=journal/",
		"/api/p/" + p.ID + "/store/exists?key=blobs/" + strings.Repeat("a", 64),
		"/api/p/" + p.ID + "/store/object?key=journal/" + carolDev + ".jsonl",
	} {
		if rec := secfx9Store(t, h, "GET", u, "", c["carol"], carolDev); rec.Code >= 500 {
			t.Fatalf("carol GET %s: %d %s", u, rec.Code, rec.Body)
		}
	}
	// And her agent reports reads, which her read permission also allows.
	if rec := secfx9Store(t, h, "POST", "/api/p/"+p.ID+"/reads",
		`{"paths":["notes.md"]}`, c["carol"], carolDev); rec.Code >= 500 {
		t.Fatalf("carol read report: %d %s", rec.Code, rec.Body)
	}

	// A full sync as a read-only member must leave the hub knowing this device
	// belongs to carol. If it does not, the id is unowned hub-wide and the
	// next paragraph is unstoppable.
	if owner, known := srv.Devices.OwnerOf(carolDev); !known || normEmail(owner) != "carol@x.io" {
		t.Errorf("after a full read-only sync the hub does not know carol's device: OwnerOf(%s) = (%q, %v), want (carol@x.io, true)",
			carolDev, owner, known)
	}

	// bob has ordinary write on the project — no admin anywhere. He pushes a
	// journal under carol's device id, with ops naming it (journalNames is
	// satisfied by writing the field, which costs nothing).
	body := secfx9Op(1, carolDev, "bobs-note.md", strings.Repeat("b", 64))
	rec := secfx9Store(t, h, "PUT", "/api/p/"+p.ID+"/store/object?key=journal/"+carolDev+".jsonl",
		body, c["bob"], carolDev)
	if rec.Code != http.StatusForbidden {
		t.Errorf("bob wrote carol's device journal: %d %s, want 403", rec.Code, rec.Body)
	}
	if owner, _ := srv.Devices.OwnerOf(carolDev); normEmail(owner) == "bob@x.io" {
		t.Errorf("bob's write claimed carol's device id hub-wide: OwnerOf(%s) = %q", carolDev, owner)
	}

	// The lockout is permanent: carol is promoted to write and can still not
	// push her own journal. This is the same request bob just made, from the
	// account the id actually belongs to.
	secfx9SetPerm(t, h, p, c["alice"], "carol@x.io", PermWrite)
	own := secfx9Op(1, carolDev, "carols-note.md", strings.Repeat("c", 64))
	rec = secfx9Store(t, h, "PUT", "/api/p/"+p.ID+"/store/object?key=journal/"+carolDev+".jsonl",
		own, c["carol"], carolDev)
	if rec.Code != 200 {
		t.Errorf("carol cannot push her OWN journal from her OWN device: %d %s", rec.Code, rec.Body)
	}
}

// The same hole from the other side, with no read-only member in it at all:
// bob's claim of an id he has never synced under must not survive contact with
// the device itself. Today `!known && journalNames(dev, ops)` treats "nobody
// has claimed it" as permission, and the field journalNames reads is one the
// claimant writes — so it costs one request to take any id that has not yet
// pushed a journal, including every device of every read-only member.
func TestSec_Device_AnUnclaimedIdIsNotWonByWritingItIntoAnOpsDeviceField(t *testing.T) {
	h, srv, c, p := permHub(t)
	const victimDev = "c0ffee000002"

	// carol's device exists and has been observed reading — she is a plain
	// member with default (write) permission, but her device has not pushed a
	// journal yet, which is the ordinary state of a device between `bdrive
	// init` and its first commit.
	if rec := secfx9Store(t, h, "GET", "/api/p/"+p.ID+"/store/list?prefix=journal/", "", c["carol"], victimDev); rec.Code != 200 {
		t.Fatalf("carol list: %d %s", rec.Code, rec.Body)
	}

	// bob names it and pushes ops that all declare it.
	body := secfx9Op(1, victimDev, "planted.md", strings.Repeat("d", 64))
	rec := secfx9Store(t, h, "PUT", "/api/p/"+p.ID+"/store/object?key=journal/"+victimDev+".jsonl",
		body, c["bob"], victimDev)
	if rec.Code == 200 {
		owner, _ := srv.Devices.OwnerOf(victimDev)
		t.Errorf("bob claimed an id he has never synced under (now owned by %q); "+
			"every op he plants there is attributed to that device in History", owner)
	}
}

// ---------------------------------------------------------------------------
// Row 3 / round 9's org-heir fix, on a hub that predates it.
//
// earliestMember orders by Org.Joined and breaks ties by the lowest address.
// Every member row written before round 9 carries no join time at all, so on
// an upgraded hub every legacy member ties at the zero time and the tie-break
// IS the rule again — round 8's hole, live, on exactly the hubs that have been
// running longest.
//
// The hub is not out of evidence when Joined is empty: AuthProvider.Accounts()
// is documented "oldest first" and the org migration already picks the default
// org's owner with it. The secure answer for a legacy row is that account
// order, never the address the member typed at signup.
// ---------------------------------------------------------------------------

func TestSec_Org_ALegacyOrgsHeirIsNotChosenByTheAddressAMemberTyped(t *testing.T) {
	h, srv, c, p := permHub(t)

	// A newcomer, signed up last (so: the newest account on the hub), whose
	// address happens to sort before everyone else's.
	newcomer := "aaa@x.io"
	signupAndSession(t, h, newcomer, "Aaa", "password1")
	if err := srv.Dir.AddMember(p.Org, newcomer, RoleMember); err != nil {
		t.Fatal(err)
	}
	_ = c

	// Make the org look like one written before round 9: members, no join
	// times. This is the state of every orgs.json and every org_members row on
	// disk today.
	secfx9Legacyize(t, srv, p.Org)

	// The hub admin offboards the departed owner — the most routine operator
	// action there is, and the trigger round 9 named.
	srv.offboard("alice@x.io")

	org, ok := srv.Dir.Get(p.Org)
	if !ok {
		t.Fatal("org vanished")
	}
	var owners []string
	for m, role := range org.Members {
		if role == RoleOwner {
			owners = append(owners, m)
		}
	}
	if len(owners) != 1 {
		t.Fatalf("owners after offboard = %v, want exactly one", owners)
	}
	// Oldest remaining account per Accounts() (oldest first): permHub signs up
	// alice, bob, carol, dave in that order and the newcomer after them.
	want := ""
	for _, u := range srv.Auth.Accounts() {
		if org.Members[normEmail(u.Email)] != "" {
			want = normEmail(u.Email)
			break
		}
	}
	if owners[0] != want {
		t.Errorf("legacy org's heir = %q, want the longest-standing member %q "+
			"(Accounts() is oldest-first and the org migration already uses it); "+
			"the address sorted lowest, which is round 8's hole unchanged",
			owners[0], want)
	}
}

// The same escalation on every metadata backend. zoe joins first and aaa
// joins last; the join times are then stripped, which is exactly what a row
// written before round 9 looks like when it is loaded on file, sqlite and
// postgres alike (” decodes to the zero time in tdec, and the file backend's
// `joined,omitempty` is simply absent). Every member then ties at zero and
// earliestMember's address tie-break decides — so the newest member inherits
// the org, on all three backends, which is round 8's hole unchanged.
func TestSec_Org_ALegacyOrgsHeirIsNotTheNewestMemberOnAnyBackend(t *testing.T) {
	for _, be := range metaBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			be.reset(t)
			store := be.open(t)
			db, err := NewOrgDB(store.Orgs())
			if err != nil {
				t.Fatal(err)
			}
			o, err := db.Create("legacy-"+be.name, "founder@x.io")
			if err != nil {
				t.Fatal(err)
			}
			// Order matters and is the test's ground truth: zoe has been in
			// the org since the beginning, aaa arrived last week.
			for _, m := range []string{"zoe@x.io", "aaa@x.io"} {
				if err := db.AddMember(o.ID, m, RoleMember); err != nil {
					t.Fatal(err)
				}
			}
			cur, _ := db.Get(o.ID)
			cur.Joined = nil // the state of every member row on disk today
			if err := store.Orgs().PutOrg(cur); err != nil {
				t.Fatal(err)
			}
			reloaded, err := NewOrgDB(store.Orgs())
			if err != nil {
				t.Fatal(err)
			}
			// The founder leaves the company and is offboarded.
			if err := reloaded.EvictMember(o.ID, "founder@x.io"); err != nil {
				t.Fatal(err)
			}
			got, _ := reloaded.Get(o.ID)
			if got.Members["aaa@x.io"] == RoleOwner {
				t.Errorf("%s: the newest member inherited the org (and project-admin on every "+
					"project in it) because the address sorted lowest; the longest-standing "+
					"member is zoe@x.io. members=%v joined=%v.\n"+
					"Round 9's Joined field is only consulted when the row HAS one, and no row "+
					"written before round 9 does — so on every hub that upgrades, the heir is "+
					"the address a member typed at signup, which is round 8's finding verbatim.",
					be.name, got.Members, got.Joined)
			}
		})
	}
}

// secfx9Legacyize rewrites an org through its repo with no join times, which is
// how every member row written before round 9 reads back.
func secfx9Legacyize(t *testing.T, srv *Server, orgID string) {
	t.Helper()
	dir, ok := srv.Dir.(LocalDirectory)
	if !ok {
		t.Fatalf("directory is %T, not the file-backed one", srv.Dir)
	}
	o, ok := dir.OrgDB.Get(orgID)
	if !ok {
		t.Fatal("no such org")
	}
	o.Joined = nil
	if err := dir.OrgDB.repo.PutOrg(o); err != nil {
		t.Fatal(err)
	}
	fresh, err := NewOrgDB(dir.OrgDB.repo)
	if err != nil {
		t.Fatal(err)
	}
	srv.Dir = LocalDirectory{OrgDB: fresh}
	if a, ok := srv.Auth.(*BuiltinAuth); ok {
		a.Offboard = srv.offboard
	}
}

// ---------------------------------------------------------------------------
// The PKCE compat residual, carried since round 8.
//
// /api/auth/exchange accepts a challenge-less code redeemed by a challenge-less
// exchange so a pre-PKCE binary keeps working. The hub cannot tell a pre-PKCE
// binary from a caller that simply left the parameter out, so the arm is
// reachable by any bare HTTP client: no proof of possession is required of
// anybody who asks for none.
// ---------------------------------------------------------------------------

func TestSec_CLIAuth_AGrantWithNoProofOfPossessionIsNotRedeemable(t *testing.T) {
	h, _, c, _ := permHub(t)

	// A bare HTTP client starts the loopback flow with NO code_challenge and
	// approves it with an ordinary browser session (POST, cookie — the only
	// thing the flow actually requires of the party at the keyboard).
	target := "/auth/cli?redirect=" + "http%3A%2F%2F127.0.0.1%3A45999%2Fcallback" + "&state=s1"
	req := httptest.NewRequest("POST", target, nil)
	req.AddCookie(c["bob"])
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body)
	}
	loc := rec.Header().Get("Location")
	i := strings.Index(loc, "code=")
	if i < 0 {
		t.Fatalf("no code in redirect %q", loc)
	}
	code := loc[i+len("code="):]
	if j := strings.IndexByte(code, '&'); j >= 0 {
		code = code[:j]
	}

	// Exchanging it with no verifier at all mints a permanent device token.
	ex := httptest.NewRequest("POST", "/api/auth/exchange",
		strings.NewReader(`{"code":"`+code+`","device":"attacker-dev"}`))
	ex.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	h.ServeHTTP(out, ex)
	if out.Code == 200 {
		t.Errorf("a code minted for a flow that bound nothing was redeemed by an exchange "+
			"that proved nothing: %d %s — the compat arm is reachable by any client that "+
			"omits code_challenge, which is exactly what an attacker does", out.Code, out.Body)
	}
}
