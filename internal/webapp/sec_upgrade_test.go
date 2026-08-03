package webapp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Round 11 — the upgrade path for hub-side device binding.
//
// Binding a device id at token issuance means an unowned id is nobody's to
// write. Every device already in the field has an id in device.json and no
// login-minted binding, so "what happens on the next sync" is the question that
// decides whether this fix ships inert, ships as an outage, or ships.
//
// Rounds 9 and 10 both found "inert on legacy rows" bugs (earliestMember,
// MountInfo.Dev+Ino). These are the tests for the UPGRADED hub, not the fresh
// one, so this one is not a third.
//
// Helpers are prefixed secupg.

// secupgLegacyPush is a device syncing with no binding of any kind — the state
// of every device in the field at the moment the hub restarts with this change.
func secupgLegacyPush(t *testing.T, h http.Handler, projectID, dev string, c *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	body := secfx9Op(1, dev, "notes.md", strings.Repeat("a", 64))
	req := httptest.NewRequest("PUT",
		"/api/p/"+projectID+"/store/object?key=journal/"+dev+".jsonl", strings.NewReader(body))
	req.Header.Set("X-Bdrive-Device", dev)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A device that had already pushed before the upgrade keeps syncing. Its
// ownership row was created by observeDevice on that push and is exactly what
// OwnerOf reads, so nothing about it changes — this is the population that
// must not notice the change at all.
func TestSec_Upgrade_ADeviceThatAlreadyPushedKeepsSyncing(t *testing.T) {
	h, srv, c, p := permHub(t)
	const dev = "legacy0000ab"

	// The pre-upgrade world: a row created by sync traffic, with no login
	// behind it. Written straight through the registry because the door that
	// used to create it is the one this change removes.
	srv.Devices.Observe(DeviceInfo{ID: dev, User: "bob@x.io", Name: "bob-laptop"})

	if rec := secupgLegacyPush(t, h, p.ID, dev, c["bob"]); rec.Code != 200 {
		t.Fatalf("a device that synced before the upgrade can no longer push its own journal: %d %s\n"+
			"every device in the field is in this state, and the remedy must not be a manual step",
			rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}

// A device that had a token but had NEVER pushed — a read-only member's, or one
// between `bdrive init` and its first commit — has no row at all, and its push
// is now refused. That is the deliberate cost of the fix, and the requirement is
// that the remedy is one command and the message says so.
func TestSec_Upgrade_ADeviceThatNeverPushedIsToldToSignInAgain(t *testing.T) {
	h, _, c, p := permHub(t)
	const dev = "neverpushed1"

	rec := secupgLegacyPush(t, h, p.ID, dev, c["bob"])
	if rec.Code != http.StatusForbidden {
		t.Fatalf("an id with no owner was writable: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "bdrive login") {
		t.Errorf("the refusal does not name the remedy: %q — a device that cannot sync must be "+
			"told the one command that fixes it, or the fix reads as an outage",
			strings.TrimSpace(rec.Body.String()))
	}

	// And the remedy works: signing in binds the id, and the same push lands.
	if rec := secRegisterDevice(t, h, p.ID, c["bob"], dev, "bob-laptop", "linux"); rec.Code != 200 {
		t.Fatalf("the remedy the message names was refused: %d %s", rec.Code, rec.Body)
	}
	if rec := secupgLegacyPush(t, h, p.ID, dev, c["bob"]); rec.Code != 200 {
		t.Errorf("after signing in, the device still cannot push: %d %s", rec.Code, rec.Body)
	}
}

// Signing in cannot take an id that is already another account's — the same
// refusal ownJournal makes, at the door that now creates bindings, so the mint
// point is not a way around the write gate.
func TestSec_Upgrade_SigningInCannotTakeAnotherAccountsDeviceId(t *testing.T) {
	h, srv, c, p := permHub(t)
	const dev = "carolbox0001"

	if rec := secRegisterDevice(t, h, p.ID, c["carol"], dev, "carol-laptop", "darwin"); rec.Code != 200 {
		t.Fatalf("setup: carol's device could not sign in: %d %s", rec.Code, rec.Body)
	}
	if rec := secRegisterDevice(t, h, p.ID, c["bob"], dev, "bob-laptop", "linux"); rec.Code == 200 {
		owner, _ := srv.Devices.OwnerOf(dev)
		t.Fatalf("bob signed in as carol's device id and the hub bound it to him (owner now %q); "+
			"the mint point must refuse an id another account already holds", owner)
	}
	if owner, known := srv.Devices.OwnerOf(dev); !known || normEmail(owner) != "carol@x.io" {
		t.Errorf("after bob's refused sign-in the id belongs to %q (known=%v), want carol@x.io", owner, known)
	}
	// carol keeps it.
	if rec := secupgLegacyPush(t, h, p.ID, dev, c["carol"]); rec.Code != 200 {
		t.Errorf("carol's own device lost its journal to a refused sign-in: %d %s", rec.Code, rec.Body)
	}
}
