package webapp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Round 7's sabotage sweep: for each guard the scoreboard names, delete it in
// a scratch copy and see whether any TestSec_* goes red. Round 6 ran 33 such
// reversions and found five guards nothing was holding; round 7 finished the
// sweep on the rows it never reached (3, 10, 12, 14, 16) plus the three that
// would not compile as a whole-file revert.
//
// Every test in this file pins a guard that a reversion proved was NOT held —
// i.e. the guard could be deleted with all 290 TestSec_* green. Each is
// constructed so that only the guard under test can produce the refusal:
// where an earlier or later layer would also refuse, the fixture removes that
// layer rather than the assertion.

// ---- helpers (slug: secaud2) ----

// secaud2Do is doAs plus request headers, which the store and read-report
// routes need (X-Bdrive-Device is the device identity every one of them keys
// on) and which doAs cannot express.
func secaud2Do(t *testing.T, h http.Handler, method, target string, body any,
	c *http.Cookie, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		data, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, target, rd)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// secaud2ReadsHub is permHub with read telemetry on — the state /heat and
// /reads need, and which permHub does not install.
func secaud2ReadsHub(t *testing.T) (http.Handler, *Server, map[string]*http.Cookie, Project) {
	t.Helper()
	h, srv, c, p := permHub(t)
	led, err := OpenReadLedger(filepath.Join(t.TempDir(), "reads.json"), 30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { led.Close() })
	srv.Reads = led
	return h, srv, c, p
}

// secaud2Sync makes a device known to the hub the only way a device becomes
// known: its own /store/* traffic (observeDevice).
func secaud2Sync(t *testing.T, h http.Handler, projectID string, c *http.Cookie, dev, name string) {
	t.Helper()
	// Its own journal push: a read grants nothing and so claims nothing.
	rec := secRegisterDevice(t, h, projectID, c, dev, name, "darwin 26.1")
	if rec.Code != 200 {
		t.Fatalf("%s syncing as %s: %d %s", name, dev, rec.Code, rec.Body)
	}
}

// secaud2Report posts an agent read report under a chosen device id.
func secaud2Report(t *testing.T, h http.Handler, projectID string, c *http.Cookie, dev string, paths ...string) *httptest.ResponseRecorder {
	t.Helper()
	reads := make([]map[string]any, 0, len(paths))
	for _, p := range paths {
		reads = append(reads, map[string]any{"path": p})
	}
	return secaud2Do(t, h, "POST", "/api/p/"+projectID+"/reads",
		map[string]any{"reads": reads}, c, map[string]string{"X-Bdrive-Device": dev})
}

// secaud2ByDevice reads /heat?by=device and returns id → folder → count.
func secaud2ByDevice(t *testing.T, h http.Handler, projectID string, c *http.Cookie) map[string]map[string]int64 {
	t.Helper()
	rec := secaud2Do(t, h, "GET", "/api/p/"+projectID+"/heat?by=device&days=0", nil, c, nil)
	if rec.Code != 200 {
		t.Fatalf("heat?by=device: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Devices []deviceHeat `json:"devices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("heat?by=device body %q: %v", rec.Body, err)
	}
	m := map[string]map[string]int64{}
	for _, d := range out.Devices {
		m[d.ID] = d.Folders
	}
	return m
}

// secaud2Entries reads /heat and returns path → entry.
func secaud2Entries(t *testing.T, h http.Handler, projectID string, c *http.Cookie) map[string]HeatEntry {
	t.Helper()
	rec := secaud2Do(t, h, "GET", "/api/p/"+projectID+"/heat?days=0", nil, c, nil)
	if rec.Code != 200 {
		t.Fatalf("heat: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Entries map[string]HeatEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("heat body %q: %v", rec.Body, err)
	}
	return out.Entries
}

// ---------------------------------------------------------------------------
// Row 10 — DeviceRegistry.MayActAs
// ---------------------------------------------------------------------------

// TestSec_Row10_MemberCannotReportReadsUnderAPeersDeviceId
//
// SABOTAGE RESULT: deleting the refusal loop in DeviceRegistry.MayActAs —
//
//	for k := range r.byKey {
//	    if k.ID == id && k.User != "" && k.User != user { return false }
//	}
//
// leaves all 290 TestSec_* green. That loop is the whole of round 2's and
// round 3's device-identity check on read reports; it is the only reason a
// well-formed device id another account is syncing under cannot be named as
// yourself.
//
// Why the existing tests miss it: ownsDevice is
// `validDeviceID(id) && MayActAs(...)`, and every test that reaches it
// (…ReadReportCannotInjectAnIdentity, …PlantedIdentityCannotBeSelfRegistered
// ThenReported, …StoreRouteCannotMintAnArbitraryHeatActor) plants an id that
// is not a device id at all — an email, free text, a foreign-org shape — so
// validDeviceID answers first and MayActAs is never consulted. The one that
// names a real peer's id (…ReportCannotRewriteAnotherOrgsDevice) is refused a
// second time by heatByDevice's org-scoped LookupIn, which is a LATER layer:
// the reads are still recorded, only the machine's name and OS are withheld.
//
// Nothing asserted the same-org case, which is the one with a consequence:
// bob reports reads under alice's real device id, and /heat?by=device — which
// every project member can read — reports Alice's MacBook, by name, reading
// files she never opened. The read heatmap is an audit surface; this writes
// to it in a teammate's name.
//
// The fixture removes every other layer: the id is well-formed (validDeviceID
// cannot refuse), alice and bob are in the SAME org (LookupIn cannot refuse),
// bob has full read permission on the project (projectPerm cannot refuse).
// Only MayActAs is left.
func TestSec_Row10_MemberCannotReportReadsUnderAPeersDeviceId(t *testing.T) {
	h, _, c, p := secaud2ReadsHub(t)
	const aliceDev = "alice-laptop-5b73"

	// Alice's device becomes known the only way one does: her own sync.
	secaud2Sync(t, h, p.ID, c["alice"], aliceDev, "Alice's MacBook")

	// Round 12 fixture update: handleReadReport now drops a report for a path
	// that is not in the project's replayed state. Every path this test reports
	// has to be a real file, INCLUDING the ones bob names — otherwise the
	// existence check would be what refuses the attack and MayActAs, the thing
	// under test, would never be reached.
	for _, f := range []string{"notes/own.md", "payroll/salaries.md", "payroll/offers.md"} {
		secauthzUpload(t, h, p.ID, f, "x", c["alice"])
	}

	// Control 1: alice reporting under her OWN device id is counted. Without
	// this the test could pass because reporting never works at all.
	if rec := secaud2Report(t, h, p.ID, c["alice"], aliceDev, "notes/own.md"); rec.Code != 200 {
		t.Fatalf("control: alice's own read report: %d %s", rec.Code, rec.Body)
	}
	if got := secaud2ByDevice(t, h, p.ID, c["alice"])[aliceDev]["notes"]; got != 1 {
		t.Fatalf("control: alice's own report is not in her device's heat (notes=%d, want 1)", got)
	}

	// Control 2: bob may read this project, so nothing in the permission
	// layer is what refuses the attack below.
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, c["bob"]); rec.Code != 200 {
		t.Fatalf("control: bob cannot read the project: %d %s", rec.Code, rec.Body)
	}

	// The attack: bob names alice's device id as himself.
	secaud2Report(t, h, p.ID, c["bob"], aliceDev, "payroll/salaries.md", "payroll/offers.md")

	byDev := secaud2ByDevice(t, h, p.ID, c["alice"])
	if got := byDev[aliceDev]["payroll"]; got != 0 {
		t.Errorf("bob reported %d reads under alice's device id %q: /heat?by=device now says "+
			"Alice's MacBook read payroll/ %d times, and every project member sees it. "+
			"DeviceRegistry.MayActAs must refuse an id another account is already syncing under",
			got, aliceDev, got)
	}
	// Her own reads must survive the squatting attempt untouched.
	if got := byDev[aliceDev]["notes"]; got != 1 {
		t.Errorf("alice's own reads under her own device id changed to %d (want 1)", got)
	}
}

// TestSec_Row10_ReadReportRefusesAControlCharacterPath
//
// SABOTAGE RESULT: deleting `|| hasControlChars(e.Path)` from handleReadReport
// leaves all 290 TestSec_* green.
//
// Row 10 names …OneUnstorableBucketCannotWedgeTheLedger for this, but that
// test asserts the LEDGER's resilience — that one unstorable bucket does not
// stop the rest from flushing — which is a different guard (the retry in
// flushLocked) and stays true with the ingest check gone. Nothing asserted
// that the report route refuses the path in the first place, which is what
// row 14 relies on to call the Postgres NUL divergence "unreachable through
// the API": with the check gone a NUL-bearing path becomes a bucket key that
// Postgres rejects at every flush, forever, from any member who may read.
//
// The report is made under the reporter's OWN device id, so MayActAs and
// validDeviceID both pass and the control-character check is the only thing
// that can refuse the path.
func TestSec_Row10_ReadReportRefusesAControlCharacterPath(t *testing.T) {
	h, _, c, p := secaud2ReadsHub(t)
	const dev = "alice-laptop-5b73"
	secaud2Sync(t, h, p.ID, c["alice"], dev, "Alice's MacBook")
	// Round 12 fixture update, as above: the control read has to name a file
	// the project actually has. The hostile paths below are refused by
	// journal.SafePath before anything else looks at them.
	secauthzUpload(t, h, p.ID, "notes/ok.md", "x", c["alice"])

	// Control: an ordinary path from the same device, same request shape, is
	// recorded — so a missing bucket below means refusal, not a broken route.
	if rec := secaud2Report(t, h, p.ID, c["alice"], dev, "notes/ok.md"); rec.Code != 200 {
		t.Fatalf("control: ordinary read report: %d %s", rec.Code, rec.Body)
	}
	if _, ok := secaud2Entries(t, h, p.ID, c["alice"])["notes/ok.md"]; !ok {
		t.Fatalf("control: an ordinary reported path is not in /heat")
	}

	for _, hostile := range []struct{ name, path string }{
		{"NUL", "notes/laptop\x00-of-eve.md"},
		{"NUL in a directory", "note\x00s/ok.md"},
		{"ESC", "notes/\x1b[2Jok.md"},
		{"DEL", "notes/ok\x7f.md"},
		{"newline", "notes/ok.md\ninjected.md"},
	} {
		t.Run(hostile.name, func(t *testing.T) {
			secaud2Report(t, h, p.ID, c["alice"], dev, hostile.path)
			for path := range secaud2Entries(t, h, p.ID, c["alice"]) {
				if strings.ContainsFunc(path, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
					t.Errorf("a read bucket exists for %q — a control character in a reported "+
						"path became a metadata-store key, which Postgres refuses at every "+
						"flush from here on", path)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Row 16 — the frontend shell's framing headers
// ---------------------------------------------------------------------------

// TestSec_Row16_ShellCarriesBothFramingHeadersNotEitherOr
//
// SABOTAGE RESULT: deleting the `frame-ancestors 'none'` CSP leaves all 290
// green. Deleting `X-Frame-Options: DENY` leaves all 290 green. Only deleting
// BOTH is caught.
//
// Round 3's …ShellCarriesFramingAndSniffingDefenses computes
//
//	framed := csp contains "frame-ancestors" || xfo == DENY || xfo == SAMEORIGIN
//
// so it holds the disjunction, not the code. The two headers are not
// interchangeable and the hub sets both on purpose: `frame-ancestors` is the
// directive current browsers honour (and, where present, the one that
// OVERRIDES X-Frame-Options per CSP Level 2), while X-Frame-Options is what a
// pre-CSP engine understands. Dropping either silently narrows the set of
// browsers in which the signed-in hub UI — the document holding the session
// cookie and driving share creation, permission edits and project deletion —
// cannot be framed and clickjacked.
//
// Asserted per header, so neither can be deleted with the suite green.
func TestSec_Row16_ShellCarriesBothFramingHeadersNotEitherOr(t *testing.T) {
	h, _, _, _ := permHub(t)

	for _, target := range []string{"/", "/index.html", "/some-project-id/dashboard", "/some-project-id/settings"} {
		rec := seccfgRaw(t, h, target)
		if rec.Code != 200 {
			t.Fatalf("GET %s: %d (expected the app shell)", target, rec.Code)
		}
		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(strings.ToLower(csp), "frame-ancestors") {
			t.Errorf("GET %s: Content-Security-Policy = %q carries no frame-ancestors — "+
				"the directive current browsers honour, and the only one that applies "+
				"inside <object>/<embed>; X-Frame-Options is not a substitute", target, csp)
		}
		if xfo := rec.Header().Get("X-Frame-Options"); !strings.EqualFold(xfo, "DENY") {
			t.Errorf("GET %s: X-Frame-Options = %q, want DENY — the fallback for engines "+
				"that do not implement frame-ancestors", target, xfo)
		}
	}
}

// ---------------------------------------------------------------------------
// Row 14 — the account-id uniqueness invariant, on the backend production uses
// ---------------------------------------------------------------------------

// TestSec_Row14_AccountIdIsNeverReassignedOnAnyBackend
//
// SABOTAGE RESULT: deleting the `WHERE lower(accounts.email) =
// lower(excluded.email)` clause AND the RowsAffected check from
// sqlAccountRepo.PutAccount leaves all 290 TestSec_* green. Nothing in the
// repository — not db_conformance_test.go either — ever asserts it.
//
// Round 6 closed a 32-bit account-id collision (…AnIdCollisionMustNotDestroy
// ALiveAccount) and the scoreboard records the fix as landing on BOTH
// backends. It did land on both; only the FILE one is tested, because that
// test builds its hub with OpenBuiltinAuth(path, …). The SQL repo is what a
// managed deployment runs — `database: {driver: sqlite|postgres}` — so the
// backend with no coverage is the backend with the user table big enough for
// the birthday bound to matter (~9,300 accounts for a 1% chance, ~77,000 for
// even odds, with no attacker involved).
//
// Same property, every backend the hub ships, through the repo interface that
// is the layer required to refuse.
func TestSec_Row14_AccountIdIsNeverReassignedOnAnyBackend(t *testing.T) {
	for _, be := range metaBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			be.reset(t)
			store := be.open(t)
			t.Cleanup(func() { store.Close() })
			repo := store.Accounts()

			const id = "u-0f1e2d3c4b5a69788796a5b4c3d2e1f0"
			victim := &authUser{
				ID: id, Email: "victim@corp.test", Name: "Victim",
				Pass:   "$2a$10$victimhashvictimhashvictimhashvictimhashvictimhash",
				Status: statusActive, Created: time.Now().UTC().Truncate(time.Second),
			}
			if err := repo.PutAccount(victim); err != nil {
				t.Fatalf("control: storing a fresh account failed: %v", err)
			}
			// Control: the SAME account may still be updated under its id —
			// that is what every password change and rename does.
			renamed := *victim
			renamed.Name = "Victim Renamed"
			if err := repo.PutAccount(&renamed); err != nil {
				t.Fatalf("control: updating the same account under its own id failed: %v", err)
			}

			// The collision: a later signup whose id generator landed here.
			attacker := &authUser{
				ID: id, Email: "attacker@evil.test", Name: "Attacker",
				Pass:   "$2a$10$notarealhashnotarealhashnotarealhashnotarealhash",
				Status: statusActive, Created: time.Now().UTC().Truncate(time.Second),
			}
			if err := repo.PutAccount(attacker); err == nil {
				t.Errorf("the %s backend accepted %q under %q's live account id %s — "+
					"an id collision transfers the victim's device tokens, org memberships and "+
					"project grants onto the newcomer and erases the victim's password hash",
					be.name, attacker.Email, victim.Email, id)
			}

			// And the victim's row is intact after a reopen, the way a hub
			// restart reads it.
			store.Close()
			reopened := be.open(t)
			t.Cleanup(func() { reopened.Close() })
			users, _, _, err := reopened.Accounts().Load()
			if err != nil {
				t.Fatal(err)
			}
			var found *authUser
			for _, u := range users {
				if u.ID == id {
					found = u
				}
			}
			if found == nil {
				t.Fatalf("after a restart no account carries id %s at all", id)
			}
			if !strings.EqualFold(found.Email, "victim@corp.test") {
				t.Errorf("after a restart id %s belongs to %q, not %q — the victim's row is gone from disk",
					id, found.Email, "victim@corp.test")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Row 11 — the blobRe guard in RemoteSource.Files
// ---------------------------------------------------------------------------

// TestSec_Row11_AnOpWithABogusBlobDoesNotMaskTheLastGoodVersion
//
// SABOTAGE RESULT: deleting
//
//	if !blobRe.MatchString(op.Blob) { continue }
//
// from RemoteSource.Files leaves all 290 TestSec_* green.
//
// It survives on OpenBlob's own blobRe (round 2, re-pinned in round 6 by
// …OpBlobIsRefusedBeforeItReachesStorage) and on remote.Prefixed.safeKey
// (round 4) — a third layer under the same round-6 heading of "a test that
// silently changes which layer it measures". But this guard states a property
// the layers below it cannot deliver, and the comment above it says so: an op
// whose Blob is not a content address "is ignored (the path keeps its previous
// version) rather than treated as a delete". Without it the bogus op WINS
// last-writer-wins, so the path's FileInfo carries a key nothing will serve:
// /tree still lists the file, and every door to its bytes fails. The last
// good version is masked, and nothing in the journal records a delete — so
// History shows the file as present at a version that cannot be read.
//
// Asserted as the property, not as a status code: after the hostile op the
// viewer must still serve the ORIGINAL content. Neither OpenBlob's guard nor
// safeKey can produce that answer — they can only refuse — so this test can
// only be satisfied by the guard in Files.
func TestSec_Row11_AnOpWithABogusBlobDoesNotMaskTheLastGoodVersion(t *testing.T) {
	h, _, c, p := permHub(t)
	const dev = "alice-laptop-9c41"
	const content = "the last good version"

	// Alice's device claims its id the way a real one does.
	if rec := secfx4Store(t, h, "GET", "/api/p/"+p.ID+"/store/list", "", c["alice"], dev); rec.Code != 200 {
		t.Fatalf("control: alice's sync: %d %s", rec.Code, rec.Body)
	}
	sum := sha256.Sum256([]byte(content))
	sha := hex.EncodeToString(sum[:])
	if rec := secfx4Store(t, h, "PUT", "/api/p/"+p.ID+"/store/object?key=blobs/"+sha,
		content, c["alice"], dev); rec.Code != 200 {
		t.Fatalf("control: storing the blob: %d %s", rec.Code, rec.Body)
	}

	push := func(body string) {
		t.Helper()
		if rec := secfx4Store(t, h, "PUT",
			"/api/p/"+p.ID+"/store/object?key=journal/"+dev+".jsonl", body, c["alice"], dev); rec.Code != 200 {
			t.Fatalf("pushing the journal: %d %s", rec.Code, rec.Body)
		}
	}
	good := secaudOpLine(1, dev, "put", "notes.md", sha)
	push(good)

	// Control: the good version is served.
	rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/file?path=notes.md", nil, c["alice"])
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), content) {
		t.Fatalf("control: the good version is not served: %d %s", rec.Code, rec.Body)
	}

	for _, bogus := range []struct{ name, blob string }{
		{"traversal", "../../../etc/passwd"},
		{"another projects prefix", "../otherproject/blobs/" + sha},
		{"not hex", strings.Repeat("z", 64)},
		{"empty", ""},
		{"short", "ab"},
	} {
		t.Run(bogus.name, func(t *testing.T) {
			// A LATER op for the same path, so it wins last-writer-wins if it
			// is folded in at all. The journal keeps the good op ahead of it.
			push(good + secaudOpLine(2, dev, "put", "notes.md", bogus.blob))
			rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/file?path=notes.md", nil, c["alice"])
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), content) {
				t.Errorf("after an op naming blob %q the viewer no longer serves the last good "+
					"version of notes.md: %d %s — Files folded an op whose Blob is not a content "+
					"address into the winning FileInfo, so the file is listed and unreadable with "+
					"no delete anywhere in the journal", bogus.blob, rec.Code, strings.TrimSpace(rec.Body.String()))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Row 3 — POST /api/invites/{token}, the one route outside proj() whose own
// check nothing held
// ---------------------------------------------------------------------------

// TestSec_Row3_InviteAcceptRefusesAnIdentityWithNoAddress
//
// SABOTAGE RESULT: deleting
//
//	if me.Email == "" { http.Error(w, "sign in to accept an invite", 401); return }
//
// from handleInviteAccept leaves all 290 TestSec_* green — the only one of the
// 23 route-level checks outside proj() that no test holds.
//
// It survives on authGate, which 401s an anonymous /api/** request before the
// handler runs. That is the wrong layer to be relying on, because authGate
// asks `Authenticate(r)` — a bool from a swappable AuthProvider — while every
// authorization decision downstream keys on the EMAIL that provider returns,
// and the interface promises nothing about it (round 6's
// …AProviderIdentityTheHubCannotResolveReachesNothing established exactly this
// gap and covered the per-project routes; the org-join route was not among
// them). A provider that authenticates a principal it cannot name walks
// straight past authGate, and with the handler's own check gone this route is
// what it reaches: an org-membership write and the seat check in front of it.
//
// The fixture uses the same stub provider that test uses, so authGate cannot
// be what refuses — only handleInviteAccept's own check is left.
func TestSec_Row3_InviteAcceptRefusesAnIdentityWithNoAddress(t *testing.T) {
	h, srv, c, p := permHub(t)
	real := srv.Auth
	t.Cleanup(func() { srv.Auth = real })

	// Alice, the org owner, mints a live invite.
	rec := doAs(t, h, "POST", "/api/orgs/"+p.Org+"/invites", map[string]any{}, c["alice"])
	if rec.Code != 200 {
		t.Fatalf("owner could not mint an invite: %d %s", rec.Code, rec.Body)
	}
	var inv struct {
		Token string `json:"token"`
	}
	json.Unmarshal(rec.Body.Bytes(), &inv)
	if inv.Token == "" {
		t.Fatalf("no invite token in %s", rec.Body)
	}

	before, _ := srv.Dir.Get(p.Org)

	// Control: dave — a real, resolvable account from another org — accepts
	// the same invite and joins. The route and the token both work.
	if rec := doAs(t, h, "POST", "/api/invites/"+inv.Token, nil, c["dave"]); rec.Code != 200 {
		t.Fatalf("control: a signed-in account could not accept the invite: %d %s", rec.Code, rec.Body)
	}

	// The attack: a provider that authenticates (so authGate passes) but
	// resolves to no address the hub can key a decision on.
	//
	// "" is refused. "   " is NOT, because the guard is spelled `me.Email ==
	// ""` while every downstream decision — OrgDB.AddMember, Role, the grant
	// maps — normalizes first (normEmail = ToLower+TrimSpace). The two
	// disagree on exactly the values in between, and for those the request
	// runs on: Redeem resolves the token, and quota().CheckSeat is called,
	// before AddMember finally refuses on its own normalized check.
	const garbage = "0123456789abcdef0123456789abcdef"
	for _, email := range []string{"", "   ", "\t", "\n "} {
		srv.Auth = secapiStubAuth{user: User{ID: "u-stub", Email: email, Name: "Ghost"}}
		live := doAs(t, h, "POST", "/api/invites/"+inv.Token, nil, nil)
		if live.Code != http.StatusUnauthorized {
			t.Errorf("a provider identity with email %q reached the org-join route: %d %s; want 401 — "+
				"handleInviteAccept guards on me.Email == \"\" while everything downstream of it "+
				"normalizes, so an unnameable identity runs Redeem and the seat check before "+
				"anything refuses it", email, live.Code, live.Body)
		}
		// And the answer must not tell an identity the hub cannot name
		// whether an org's join token is live: that token bootstraps an
		// ACCOUNT on an invite-only hub, which is the default posture.
		dead := doAs(t, h, "POST", "/api/invites/"+garbage, nil, nil)
		if live.Code != dead.Code || live.Body.String() != dead.Body.String() {
			t.Errorf("with email %q a live invite answers %d %q and a garbage token %d %q — "+
				"an invite-token validity oracle for a principal the hub cannot name",
				email, live.Code, strings.TrimSpace(live.Body.String()),
				dead.Code, strings.TrimSpace(dead.Body.String()))
		}
	}
	srv.Auth = real

	after, _ := srv.Dir.Get(p.Org)
	for email := range after.Members {
		if _, had := before.Members[email]; !had && email != "dave@x.io" {
			t.Errorf("the org gained a member %q that no signed-in account asked for", email)
		}
	}
}

// ---------------------------------------------------------------------------
// Part 3 — POST /api/auth/device/start, a route with zero coverage after six
// rounds
// ---------------------------------------------------------------------------

// TestSec_CLIAuth_AGrantTheHubReportsDeadIsNotRetainedForever
//
// POST /api/auth/device/start is UNAUTHENTICATED (authGate opens everything
// under /api/auth/) and is not one of the three paths rateLimitAuth covers
// (/auth/login, /auth/signup, /auth/reset). Every call allocates a cliGrant
// in CLIAuth.pending holding the caller's device and os strings, read from a
// body capped at 64 KiB.
//
// Nothing ever reaps that map. take() deletes on consumption; peek() — the
// only thing an unpolled grant ever meets — returns false for an expired
// grant and LEAVES IT IN PLACE; approveDevice deletes nothing. There is no
// sweep anywhere in authcli.go. So a stranger who POSTs /start and never
// polls has bought a permanent allocation on the hub: after ten minutes the
// hub tells every caller the grant is invalid, and keeps holding it for the
// life of the process.
//
// The property asserted is the narrow, non-negotiable half of that: a grant
// the hub has already declared dead must not still be held. Expiry is
// simulated by moving the stored deadline rather than by sleeping ten
// minutes; nothing else about the flow is faked — the grants are minted
// through the real HTTP route and reported dead through the real poll route.
func TestSec_CLIAuth_AGrantTheHubReportsDeadIsNotRetainedForever(t *testing.T) {
	auth, err := OpenBuiltinAuth(filepath.Join(t.TempDir(), "auth.json"), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{Auth: auth}
	h := srv.Handler()
	c := auth.cli

	start := func(name string) string {
		t.Helper()
		rec := secaud2Do(t, h, "POST", "/api/auth/device/start",
			map[string]string{"device": name, "os": "linux"}, nil, nil)
		if rec.Code != 200 {
			t.Fatalf("device/start: %d %s", rec.Code, rec.Body)
		}
		var out struct {
			Code string `json:"code"`
		}
		json.Unmarshal(rec.Body.Bytes(), &out)
		if out.Code == "" {
			t.Fatalf("device/start returned no code: %s", rec.Body)
		}
		return out.Code
	}

	// Control: a live grant exists, polls as pending, and is retained — so a
	// zero count below would mean the fixture, not the fix.
	live := start("an-honest-device")
	if rec := secaud2Do(t, h, "POST", "/api/auth/device/poll",
		map[string]string{"code": live}, nil, nil); rec.Code != 200 ||
		!strings.Contains(rec.Body.String(), "pending") {
		t.Fatalf("control: a live grant does not poll as pending: %d %s", rec.Code, rec.Body)
	}

	// The flood: unauthenticated, unmetered, each carrying the largest
	// device name the 64 KiB body limit allows through.
	const n = 200
	big := strings.Repeat("A", 8<<10)
	codes := make([]string, 0, n)
	for i := 0; i < n; i++ {
		codes = append(codes, start(big))
	}

	// Ten minutes pass. Every one of them is now expired, and the hub says so.
	c.mu.Lock()
	for _, id := range codes {
		g := c.pending[id]
		g.expires = time.Now().Add(-time.Minute)
		c.pending[id] = g
	}
	c.mu.Unlock()
	for _, id := range codes[:5] {
		rec := secaud2Do(t, h, "POST", "/api/auth/device/poll",
			map[string]string{"code": id}, nil, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("control: an expired grant does not poll as invalid: %d %s", rec.Code, rec.Body)
		}
	}

	c.mu.Lock()
	held := 0
	for _, id := range codes {
		if _, ok := c.pending[id]; ok {
			held++
		}
	}
	stillLive := len(c.pending)
	c.mu.Unlock()

	if held != 0 {
		t.Errorf("%d of %d grants the hub reports as invalid are still held in CLIAuth.pending "+
			"(%d rows total, ~%d KiB of attacker-chosen strings) — POST /api/auth/device/start "+
			"needs no credential and is not rate limited, and nothing in authcli.go ever sweeps "+
			"the map, so every unpolled sign-in attempt is a permanent allocation",
			held, n, stillLive, held*len(big)/1024)
	}
	// The one live grant must survive: reclaiming must be about expiry, not
	// about emptying the map.
	if _, ok := c.peek("device", live); !ok {
		t.Errorf("the live grant was reclaimed too — a pending sign-in must survive")
	}
}
