package webapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Round 11 attacks round 10's device-binding rewrite: DeviceRegistry.Bind,
// BuiltinAuth.finishLogin, and the deleted `!known && journalNames(dev, ops)`
// arm in ownJournal.
//
// Helper prefix: secfx10.

// secfx10Project makes a project for `who` and returns its id. A plain member
// who creates a project is its ADMIN (TestCreatorBecomesAdmin), which is the
// only permission these attacks need.
func secfx10Project(t *testing.T, h http.Handler, c *http.Cookie, org, name string) string {
	t.Helper()
	body := map[string]any{"name": name}
	if org != "" {
		body["org"] = org
	}
	rec := doAs(t, h, "POST", "/api/projects", body, c)
	if rec.Code != 200 {
		t.Fatalf("create project %q: %d %s", name, rec.Code, rec.Body)
	}
	var out struct {
		Project Project `json:"project"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Project.ID
}

// secfx10Bearer sends a store request authenticated by a DEVICE TOKEN (the
// credential a daemon actually holds), not a browser cookie.
func secfx10Bearer(t *testing.T, h http.Handler, method, target, body, token, dev string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if dev != "" {
		req.Header.Set("X-Bdrive-Device", dev)
		req.Header.Set("X-Bdrive-Device-Name", "machine-"+dev)
		req.Header.Set("X-Bdrive-Os", "darwin/arm64")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// secfx10Token pulls the device token out of a successful sign-in response.
func secfx10Token(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("sign-in response is not JSON: %s", rec.Body)
	}
	if out.Token == "" {
		t.Fatalf("sign-in handed back no token: %s", rec.Body)
	}
	return out.Token
}

// ---------------------------------------------------------------------------
// 1. ownJournal's admin RECOVERY arm creates a competing ownership row, and one
//    competing row is enough to refuse the real owner's next sign-in forever.
// ---------------------------------------------------------------------------

// TestSec_Device_ARecoveryPushDoesNotLockTheOwnerOutOfSigningIn.
//
// Round 10 made a device identity a hub-wide binding created at token issuance,
// and made a login that cannot bind a 409 — "a credential is never handed to a
// machine that cannot then use it". Two rules meet badly:
//
//   - Bind refuses when ANY row for the id names a different account.
//   - handleStorePut calls observeDevice AFTER ownJournal passes, and one arm
//     of ownJournal is "project admin is the recovery path". So an admin's
//     journal PUT for somebody else's device id creates a row keyed
//     {the admin's email, that id} — a second, competing row.
//
// Anyone who can create a project is an admin of it (TestCreatorBecomesAdmin),
// and ownJournal asks projectPerm on the project in the URL — so the attacker
// supplies the project too. dave is in a DIFFERENT ORG from bob and shares
// nothing with him.
//
// After one PUT into dave's own project, bob's device — still the OwnerOf
// answer, still able to push — can never sign in again: every `bdrive login`
// on that machine answers 409 "already registered to another account". The
// documented remedy for a lost binding IS `bdrive login`, so the recovery path
// bricks the recovery path, and it does so across the org wall.
func TestSec_Device_ARecoveryPushDoesNotLockTheOwnerOutOfSigningIn(t *testing.T) {
	h, srv, c, p := permHub(t)
	const bobDev = "bob-laptop-77"

	// Control 1: bob's machine signs in and binds its id.
	if rec := secRegisterDevice(t, h, p.ID, c["bob"], bobDev, "bobs-box", "darwin"); rec.Code != 200 {
		t.Fatalf("control: bob's first sign-in: %d %s", rec.Code, rec.Body)
	}
	// Control 2: signing in again from the same machine is an ordinary thing to
	// do (a new token, `bdrive init` in a second folder, a re-login after
	// logout) and it works.
	if rec := secRegisterDevice(t, h, p.ID, c["bob"], bobDev, "bobs-box", "darwin"); rec.Code != 200 {
		t.Fatalf("control: bob signs in twice from the same machine: %d %s", rec.Code, rec.Body)
	}
	if owner, known := srv.Devices.OwnerOf(bobDev); !known || owner != "bob@x.io" {
		t.Fatalf("fixture: OwnerOf(%s) = %q,%v, want bob@x.io", bobDev, owner, known)
	}

	// dave is in another org. He makes his own project, where he is admin.
	dp := secfx10Project(t, h, c["dave"], "", "daves-wiki")

	// One journal PUT naming bob's device id, into dave's own project. The
	// admin arm lets it through; observeDevice then records {dave, bobDev}.
	body := secaudOpLine(1, bobDev, "put", "notes.md", strings.Repeat("b", 64))
	push := secfx4Store(t, h, "PUT",
		"/api/p/"+dp+"/store/object?key=journal/"+bobDev+".jsonl", body, c["dave"], bobDev)
	t.Logf("dave's PUT of journal/%s.jsonl into his own project: %d %s",
		bobDev, push.Code, strings.TrimSpace(push.Body.String()))

	// The property under attack: bob's machine can still sign in. Nothing an
	// account outside bob's org does may take that away.
	rec := secRegisterDevice(t, h, p.ID, c["bob"], bobDev, "bobs-box", "darwin")
	if rec.Code != 200 {
		t.Errorf("bob's machine can no longer sign in: %d %s\n"+
			"one journal PUT from dave — a stranger in another org, into a project of his own — "+
			"made observeDevice write a second row for bob's device id, and Bind refuses any id "+
			"that has a row under another account. bob is still OwnerOf and can still push, but "+
			"`bdrive login` on his own machine is refused forever, which is the documented remedy "+
			"for every other device problem.",
			rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}

// ---------------------------------------------------------------------------
// 2. The 409 is a hub-wide device-existence oracle, across the org wall.
// ---------------------------------------------------------------------------

// TestSec_Device_LoginIsNotAHubWideDeviceExistenceOracle.
//
// This is the class round 3 closed for History
// (TestSec_Journal_IsNotAnExistenceOracle), round 4 re-closed for the registry
// join, and round 5 closed for /store/sign
// (TestSec_Journal_OwnershipIsNotAHubWideDeviceExistenceOracle): OwnerOf is
// deliberately hub-wide, so any route that turns its answer into a status code
// answers questions about tenants the caller cannot see through any other
// surface.
//
// finishLogin is a new such route. Bind's refusal is 409 and its BODY says
// "device id X is already registered to another account on this hub". bob, a
// plain member of alice's org, asks about a device belonging to dave's org —
// a separate tenant whose projects, members and devices he cannot reach — and
// the sign-in answers, in words.
//
// The fix does not have to keep the 409: the push door already refuses an id
// owned by somebody else and an id owned by nobody with the SAME 403 and the
// same message, so a login that binds nothing and hands back the token leaks
// nothing and loses nothing.
func TestSec_Device_LoginIsNotAHubWideDeviceExistenceOracle(t *testing.T) {
	h, srv, c, p := permHub(t)

	// dave is in a different org, with his own project, and his device signs in
	// the ordinary way.
	dp := secfx10Project(t, h, c["dave"], "", "daves-wiki")
	const daveDev = "dave-laptop-9f21"
	if rec := secRegisterDevice(t, h, dp, c["dave"], daveDev, "daves-box", "linux"); rec.Code != 200 {
		t.Fatalf("dave's sign-in: %d %s", rec.Code, rec.Body)
	}
	if owner, known := srv.Devices.OwnerOf(daveDev); !known || owner != "dave@x.io" {
		t.Fatalf("fixture: OwnerOf(%s) = %q,%v, want dave@x.io", daveDev, owner, known)
	}

	// bob has no way to learn anything about dave's org through any other route.
	probeUnknown := secRegisterDevice(t, h, p.ID, c["bob"], "no-such-device-0001", "probe", "linux")
	probeKnown := secRegisterDevice(t, h, p.ID, c["bob"], daveDev, "probe", "linux")

	// Control: the probe is a request bob is allowed to make at all.
	if probeUnknown.Code != 200 {
		t.Fatalf("control failed: bob cannot sign in at all: %d %s", probeUnknown.Code, probeUnknown.Body)
	}
	if probeKnown.Code != probeUnknown.Code {
		t.Errorf("a device id owned by another ORG answers %d %q while an id nothing on this hub "+
			"has ever seen answers %d %q — one `bdrive login` is a hub-wide device-existence "+
			"oracle across the org wall, and the 409 body names the id back to the caller",
			probeKnown.Code, strings.TrimSpace(probeKnown.Body.String()),
			probeUnknown.Code, strings.TrimSpace(probeUnknown.Body.String()))
	}
}

// ---------------------------------------------------------------------------
// 2b. The hub-side twin of round 10's critical: ownership is decided on an
//     exact-case id, and the object store folds case.
// ---------------------------------------------------------------------------

// TestSec_Device_ADeviceIdSpelledInAnotherCaseIsNotASecondJournal.
//
// Round 10's critical (TestSec_HostileHub_CannotOverwriteThisDevicesOwnJournal)
// was that syncer.pull skipped a listed journal only on an exact string
// compare, so on a case-insensitive filesystem journal/DEVA.jsonl and
// journal/deva.jsonl are two keys and ONE FILE. The fix was made on the device.
//
// The hub has the same shape and did not get the same fix. Every ownership
// decision here is exact-case:
//
//	DeviceRegistry.Bind   `if k.ID != d.ID`         (byte compare)
//	DeviceRegistry.OwnerOf`if k.ID != id`           (byte compare)
//	ownJournal            `key != "journal/"+dev+".jsonl"`
//
// so "DEVA" is simply a different id from "deva": nobody owns it, any account
// may bind it at login, and ownJournal then agrees the caller owns that
// journal. The object store underneath does not agree. `bdrive serve` with a
// `file://` store on macOS (APFS) or Windows (NTFS) — the default on both, and
// what every self-hoster gets from the quickstart — writes both keys to one
// file.
//
// Result: one login and one PUT and a plain member has REPLACED another
// account's journal on the hub. That is the one-writer invariant the whole
// concurrency design rests on, broken at the hub, with no hostile hub and no
// filesystem trick beyond the filesystem the hub is already on.
func TestSec_Device_ADeviceIdSpelledInAnotherCaseIsNotASecondJournal(t *testing.T) {
	h, _, c, p := permHub(t)
	const aliceDev = "deva9f21"
	const shouted = "DEVA9F21"

	if rec := secRegisterDevice(t, h, p.ID, c["alice"], aliceDev, "alices-box", "darwin"); rec.Code != 200 {
		t.Fatalf("alice's sign-in: %d %s", rec.Code, rec.Body)
	}
	mine := secaudOpLine(1, aliceDev, "put", "quarterly-plan.md", strings.Repeat("a", 64))
	if rec := secfx4Store(t, h, "PUT",
		"/api/p/"+p.ID+"/store/object?key=journal/"+aliceDev+".jsonl", mine, c["alice"], aliceDev); rec.Code != 200 {
		t.Fatalf("control: alice writes her own journal: %d %s", rec.Code, rec.Body)
	}

	// bob is a plain member. He signs in naming the same id in another case.
	bind := secRegisterDevice(t, h, p.ID, c["bob"], shouted, "bobs-box", "linux")
	t.Logf("bob's sign-in naming %q (alice owns %q): %d", shouted, aliceDev, bind.Code)
	forged := secaudOpLine(1, shouted, "delete", "quarterly-plan.md", "")
	push := secfx4Store(t, h, "PUT",
		"/api/p/"+p.ID+"/store/object?key=journal/"+shouted+".jsonl", forged, c["bob"], shouted)
	t.Logf("bob's PUT of journal/%s.jsonl: %d %s", shouted, push.Code, strings.TrimSpace(push.Body.String()))

	// Whatever the hub decided about ownership, alice's journal object must
	// still hold alice's ops.
	got := secfx4Store(t, h, "GET",
		"/api/p/"+p.ID+"/store/object?key=journal/"+aliceDev+".jsonl", "", c["alice"], aliceDev)
	if got.Code != 200 {
		t.Fatalf("alice can no longer read her own journal: %d %s", got.Code, got.Body)
	}
	if !strings.Contains(got.Body.String(), `"quarterly-plan.md"`) ||
		!strings.Contains(got.Body.String(), `"put"`) {
		t.Errorf("bob replaced alice's journal object by spelling her device id %q as %q: "+
			"journal/%s.jsonl now reads %q.\n"+
			"Nothing on the hub compares a device id case-insensitively (Bind, OwnerOf and "+
			"ownJournal are all byte compares) while the store underneath folds case — which is "+
			"round 10's device-side critical with the hub holding the lever.",
			aliceDev, shouted, aliceDev, strings.TrimSpace(got.Body.String()))
	}
}

// ---------------------------------------------------------------------------
// 3. The CISO declined an automatic self-heal (bind on any device-token
//    request) because it would reopen round 7's hole one credential class over.
//    This measures that no such path exists.
// ---------------------------------------------------------------------------

// TestSec_Device_ADeviceTokenCannotBindAnUnownedId asserts the decision holds:
// a member holding a valid device token, hitting every /store/* door under an
// id nothing has ever bound, creates no ownership row and gains no write.
func TestSec_Device_ADeviceTokenCannotBindAnUnownedId(t *testing.T) {
	h, srv, c, p := permHub(t)

	// bob's real machine signs in; he now holds a genuine device token.
	rec := secRegisterDevice(t, h, p.ID, c["bob"], "bob-real-01", "bobs-box", "darwin")
	if rec.Code != 200 {
		t.Fatalf("bob's sign-in: %d %s", rec.Code, rec.Body)
	}
	token := secfx10Token(t, rec)

	// He now points that credential at an id nobody has bound.
	const unowned = "never-bound-4242"
	base := "/api/p/" + p.ID + "/store/"
	for _, probe := range []struct{ method, target, body string }{
		{"GET", base + "list?prefix=journal/", ""},
		{"GET", base + "object?key=journal/" + unowned + ".jsonl", ""},
		{"GET", base + "exists?key=journal/" + unowned + ".jsonl", ""},
		{"POST", base + "sign?key=journal/" + unowned + ".jsonl&size=1", ""},
	} {
		r := secfx10Bearer(t, h, probe.method, probe.target, probe.body, token, unowned)
		t.Logf("%s %s -> %d", probe.method, probe.target, r.Code)
	}
	if owner, known := srv.Devices.OwnerOf(unowned); known || owner != "" {
		t.Errorf("a device token reached a binding through a read door: OwnerOf(%s) = %q,%v — "+
			"round 7's property is that a read door creates nothing", unowned, owner, known)
	}

	// And the write door still refuses it, so there is no self-heal there either.
	body := secaudOpLine(1, unowned, "put", "x.md", strings.Repeat("c", 64))
	push := secfx4Store(t, h, "PUT",
		"/api/p/"+p.ID+"/store/object?key=journal/"+unowned+".jsonl", body, c["bob"], unowned)
	if push.Code != http.StatusForbidden {
		t.Errorf("an unowned id is writable without a binding: %d %s", push.Code, push.Body)
	}
	if owner, known := srv.Devices.OwnerOf(unowned); known || owner != "" {
		t.Errorf("the refused write registered the id it refused: OwnerOf(%s) = %q,%v", unowned, owner, known)
	}
}

// ---------------------------------------------------------------------------
// 4. Two logins racing for one id.
// ---------------------------------------------------------------------------

// TestSec_Device_TwoLoginsRacingForOneIdProduceExactlyOneOwner runs the claim
// under -race: Bind's check and write are one critical section on purpose, so
// two accounts naming the same fresh id must not both believe they won.
func TestSec_Device_TwoLoginsRacingForOneIdProduceExactlyOneOwner(t *testing.T) {
	h, srv, c, p := permHub(t)
	const contested = "contested-id-01"

	var wg sync.WaitGroup
	codes := make([]int, 2)
	who := []string{"bob", "carol"}
	for i := range who {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = secRegisterDevice(t, h, p.ID, c[who[i]], contested, "box", "linux").Code
		}(i)
	}
	wg.Wait()

	owner, known := srv.Devices.OwnerOf(contested)
	if !known {
		t.Fatalf("neither login bound the id: codes=%v", codes)
	}
	winners := 0
	for i, code := range codes {
		if code == 200 && who[i]+"@x.io" == owner {
			winners++
		}
		if code == 200 && who[i]+"@x.io" != owner {
			t.Errorf("%s's login returned 200 for %s but OwnerOf says %q — a login that was told "+
				"it succeeded holds a token its device cannot push with", who[i], contested, owner)
		}
	}
	if winners != 1 {
		t.Errorf("codes=%v owner=%q: exactly one racing login may win the id", codes, owner)
	}
}

// ---------------------------------------------------------------------------
// 5. A refused bind must leave nothing behind.
// ---------------------------------------------------------------------------

// TestSec_Device_ARefusedBindIssuesNoTokenAndRecordsNothing checks the other
// half of "before the token, not after": the 409 must not hand back a
// credential, and it must not record the caller against the id it refused —
// which is how round 2's ownsDevice turned out to be a one-request speed bump.
func TestSec_Device_ARefusedBindIssuesNoTokenAndRecordsNothing(t *testing.T) {
	h, srv, c, p := permHub(t)
	const carolDev = "carol-laptop-31"

	if rec := secRegisterDevice(t, h, p.ID, c["carol"], carolDev, "carols-box", "linux"); rec.Code != 200 {
		t.Fatalf("carol's sign-in: %d %s", rec.Code, rec.Body)
	}
	rec := secRegisterDevice(t, h, p.ID, c["bob"], carolDev, "bobs-box", "linux")
	if rec.Code == 200 {
		t.Fatalf("bob took carol's bound device id at login: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), `"token"`) {
		t.Errorf("a refused sign-in still handed back a credential: %s", rec.Body)
	}
	if owner, _ := srv.Devices.OwnerOf(carolDev); owner != "carol@x.io" {
		t.Errorf("the refused sign-in changed ownership of %s to %q", carolDev, owner)
	}
	// And the refusal is not a speed bump: a second attempt is refused too.
	if again := secRegisterDevice(t, h, p.ID, c["bob"], carolDev, "bobs-box", "linux"); again.Code == 200 {
		t.Errorf("the second attempt succeeded — the refused request registered what it refused: %s", again.Body)
	}
}
