package webapp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// The gate and its enabler have to have the same reach.
//
// `ownJournal` refuses a journal write unless DeviceRegistry.OwnerOf says the
// id belongs to the caller, for EVERY provider — it asks only whether
// s.Devices is nil. The only thing that ever creates that ownership row is
// DeviceRegistry.Bind, and its only caller is BuiltinAuth.finishLogin, which
// used to be wired behind `if a, ok := s.Auth.(*BuiltinAuth); ok`.
//
// So on a hub running a managed provider — the deployment the AuthProvider seam
// exists FOR — nothing ever bound a device, and every journal push from every
// device was refused forever. Everything around it looked healthy, which is why
// it took two days and a device-id rotation to find: /api/auth/me answered,
// project permissions read `write`, blobs (content-addressed, ownerless)
// uploaded fine, and only the journal PUT died. Rotating to a fresh random id
// reproduced it identically, which is what ruled out any stale row and pointed
// here.
// ---------------------------------------------------------------------------

// secprovAuth is a managed deployment's provider: it authenticates against its
// own identity system and mints its own tokens, and the hub knows nothing about
// either. What it must still do is bind the device at the moment it mints —
// which it can only do if the hub hands it the binder.
type secprovAuth struct {
	user User
	bind DeviceBinder // captured from UseDeviceBinder; nil means the hub never offered one
}

func (a *secprovAuth) CLILoginPath() string                    { return "/auth/login" }
func (a *secprovAuth) Authenticate(*http.Request) (User, bool) { return a.user, a.user.Email != "" }
func (a *secprovAuth) Register(*http.ServeMux)                 {}
func (a *secprovAuth) Accounts() []User                        { return []User{a.user} }
func (a *secprovAuth) UseDeviceBinder(b DeviceBinder)          { a.bind = b }

// mint is the provider's token-mint moment, standing in for the handler a
// managed provider serves at /api/auth/exchange. The contract is that it calls
// the hub's binder before handing the token over.
func (a *secprovAuth) mint(t *testing.T, dev string) error {
	t.Helper()
	if a.bind == nil {
		t.Fatal("the hub never handed this provider a device binder: nothing it does at mint " +
			"can bind a device, so ownJournal will refuse every push on this hub forever")
	}
	r := httptest.NewRequest("POST", "/api/auth/exchange", nil)
	r.Header.Set("X-Bdrive-Device", dev)
	r.Header.Set("X-Bdrive-Device-Name", "machine-"+dev)
	r.Header.Set("X-Bdrive-Os", "linux/amd64")
	return a.bind(a.user.Email, r)
}

// secprovHub is permHub re-served behind a managed provider: the accounts, org
// and project are real (created through BuiltinAuth), and then the hub is
// rebuilt with a provider it does not own — which is the shape of every
// managed deployment.
func secprovHub(t *testing.T) (http.Handler, *Server, *secprovAuth, Project) {
	t.Helper()
	_, srv, _, p := permHub(t)
	auth := &secprovAuth{user: User{ID: "u-bob", Email: "bob@x.io", Name: "Bob"}}
	srv.Auth = auth
	return srv.Handler(), srv, auth, p
}

func secprovPush(t *testing.T, h http.Handler, project, dev string) *httptest.ResponseRecorder {
	t.Helper()
	body := secaudOpLine(1, dev, "put", "notes.md", strings.Repeat("a", 64))
	return secfx4Store(t, h, "PUT",
		"/api/p/"+project+"/store/object?key=journal/"+dev+".jsonl", body, nil, dev)
}

// The regression itself: a provider that is not BuiltinAuth must be able to
// bind a device, and its devices must then be able to push.
func TestSec_Device_AManagedProviderCanBindAndItsDevicesCanPush(t *testing.T) {
	h, srv, auth, p := secprovHub(t)
	const dev = "ce898b5e82bf"

	// Before the binding, the gate is on and refusing — that half always worked.
	if rec := secprovPush(t, h, p.ID, dev); rec.Code != http.StatusForbidden {
		t.Fatalf("an unbound device pushed a journal: %d %s", rec.Code, rec.Body)
	}

	// The hub must have handed this provider the binder. This is the assertion
	// that fails on the old tree: the wiring was behind a type assertion on
	// *BuiltinAuth, so a managed provider got nil and no login it served could
	// ever bind anything.
	if auth.bind == nil {
		t.Fatal("Server.Handler did not hand the provider a device binder — every push on a hub " +
			"with a managed AuthProvider is refused forever, however often anyone signs in")
	}

	// AnyOwned is what tells "your device is not registered" from "no device on
	// this hub is registered, and none can be" — the difference between a user
	// who should sign in and an operator who must fix their provider. It is the
	// discriminator behind the log line ownJournal emits, so it has to actually
	// discriminate.
	if srv.Devices.AnyOwned() {
		t.Fatal("AnyOwned() is true on a hub that has bound nothing")
	}

	// The provider mints a token and binds, as the contract requires.
	if err := auth.mint(t, dev); err != nil {
		t.Fatalf("binding a fresh id at mint: %v", err)
	}
	if !srv.Devices.AnyOwned() {
		t.Fatal("AnyOwned() is still false after a successful bind — the hub would keep " +
			"telling its operator the provider is broken after it was fixed")
	}
	if owner, known := srv.Devices.OwnerOf(dev); !known || owner != "bob@x.io" {
		t.Fatalf("OwnerOf(%s) = %q,%v after the provider bound it", dev, owner, known)
	}

	// And the push that was refused now lands.
	if rec := secprovPush(t, h, p.ID, dev); rec.Code != http.StatusOK {
		t.Fatalf("a bound device still cannot push its own journal: %d %s", rec.Code, rec.Body)
	}
}

// The binding a managed provider makes is the same binding BuiltinAuth makes,
// conflict rules included: it does not become a way to take an id that is
// already somebody else's just because the provider is external.
func TestSec_Device_AManagedProviderCannotBindSomebodyElsesId(t *testing.T) {
	h, srv, auth, p := secprovHub(t)
	const dev = "aa11bb22cc33"

	if err := auth.mint(t, dev); err != nil {
		t.Fatalf("bob's own bind: %v", err)
	}
	if rec := secprovPush(t, h, p.ID, dev); rec.Code != http.StatusOK {
		t.Fatalf("bob cannot push after binding: %d %s", rec.Code, rec.Body)
	}

	// carol signs in on the same hub and names bob's id.
	auth.user = User{ID: "u-carol", Email: "carol@x.io", Name: "Carol"}
	if err := auth.mint(t, dev); err == nil {
		t.Fatal("a second account bound an id already registered to somebody else")
	}
	if owner, _ := srv.Devices.OwnerOf(dev); owner != "bob@x.io" {
		t.Fatalf("OwnerOf(%s) = %q — carol's refused bind moved the claim", dev, owner)
	}
	// And she cannot write his journal.
	if rec := secprovPush(t, h, p.ID, dev); rec.Code != http.StatusForbidden {
		t.Fatalf("carol wrote bob's journal: %d %s", rec.Code, rec.Body)
	}
}

// A device token still cannot reach a bind. The fix moved WHERE the binder is
// wired (every provider, not just BuiltinAuth); it did not add a door that a
// sync credential can push on, which is the property round 10 recorded when the
// CISO declined an automatic self-heal.
func TestSec_Device_TheProviderSeamAddsNoBindingDoorForSyncTraffic(t *testing.T) {
	h, _, auth, p := secprovHub(t)
	const unowned = "never-bound-9f21"

	// Every /store door, under a fully authenticated identity, naming an id
	// nothing has bound. None of them may create the row.
	base := "/api/p/" + p.ID + "/store/"
	for _, probe := range []struct{ method, target string }{
		{"GET", base + "list?prefix=journal/"},
		{"GET", base + "object?key=journal/" + unowned + ".jsonl"},
		{"GET", base + "exists?key=journal/" + unowned + ".jsonl"},
		{"POST", base + "sign?key=journal/" + unowned + ".jsonl&size=1"},
	} {
		secfx4Store(t, h, probe.method, probe.target, "", nil, unowned)
	}
	if rec := secprovPush(t, h, p.ID, unowned); rec.Code != http.StatusForbidden {
		t.Errorf("an unowned id became writable through the store API: %d %s", rec.Code, rec.Body)
	}
	if auth.bind == nil {
		t.Fatal("no binder handed to the provider")
	}
	// Nothing in that traffic bound anything: only the provider's mint can.
	h2, srv2, _, _ := secprovHub(t)
	_ = h2
	if owner, known := srv2.Devices.OwnerOf(unowned); known || owner != "" {
		t.Errorf("OwnerOf(%s) = %q,%v — sync traffic created an ownership row", unowned, owner, known)
	}
}

// A hub that cannot persist a device row must not report the binding as done:
// the login would hand back a token whose every push is then refused, with
// nothing anywhere saying why.
func TestSec_Device_ABindingThatDidNotReachTheStoreIsNotReported(t *testing.T) {
	_, srv, auth, _ := secprovHub(t)
	srv.Devices.repo = secprovDeadRepo{srv.Devices.repo}
	if err := auth.mint(t, "dd44ee55ff66"); err == nil {
		t.Fatal("Bind reported success while the store refused the row — the account gets a token " +
			"it cannot push with, and the claim vanishes at the next restart")
	}
}

// secprovDeadRepo accepts reads and refuses every write.
type secprovDeadRepo struct{ DeviceRepo }

func (secprovDeadRepo) Put(DeviceInfo) error { return errSecprovDead }

var errSecprovDead = &secprovErr{}

type secprovErr struct{}

func (*secprovErr) Error() string { return "disk is full" }
