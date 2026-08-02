package webapp

// Round 4 — the target is round 3's own fixes on the hub (02aa9a2).
//
// Round 3 attacked rounds 1 and 2's fixes and broke two of them. This file
// does the same job one round later: the (account, id) device registry rekey,
// LookupIn's org scoping on History and heat, clientIP's "last hop" rewrite,
// setContentLength, persistLocked's split retry, and the /auth/reset
// goroutine.
//
// Every test asserts the SECURE behavior, so it goes green the moment the hole
// is closed and stays as a permanent regression test. Helpers are prefixed
// secfx3 per the harness rules; no existing file is touched.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- helpers -------------------------------------------------------------

// secfx3Sync is the one request that registers a device: an ordinary store
// call carrying the X-Bdrive-Device headers remote/http.go always sends.
func secfx3Sync(t *testing.T, h http.Handler, projectID string, c *http.Cookie, id, name, os string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/p/"+projectID+"/store/list?prefix=blobs/", nil)
	req.Header.Set("X-Bdrive-Device", id)
	req.Header.Set("X-Bdrive-Device-Name", name)
	req.Header.Set("X-Bdrive-Os", os)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// secfx3PushJournal writes a journal for device dev through the public store
// API, so the op's attribution comes from a key the hub bound to the caller.
func secfx3PushJournal(t *testing.T, h http.Handler, projectID, dev string, ops []map[string]any, c *http.Cookie) {
	t.Helper()
	var b strings.Builder
	for _, op := range ops {
		line, err := json.Marshal(op)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	req := httptest.NewRequest("PUT", "/api/p/"+projectID+"/store/object?key=journal/"+dev+".jsonl",
		strings.NewReader(b.String()))
	req.Header.Set("X-Bdrive-Device", dev)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("push journal for %s: %d %s", dev, rec.Code, rec.Body)
	}
}

func secfx3PutBlob(t *testing.T, h http.Handler, projectID, body string, c *http.Cookie) string {
	t.Helper()
	sha := shaOf(body)
	req := httptest.NewRequest("PUT", "/api/p/"+projectID+"/store/object?key=blobs/"+sha, strings.NewReader(body))
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("put blob: %d %s", rec.Code, rec.Body)
	}
	return sha
}

func secfx3Op(seq int64, path, blob string, size int) map[string]any {
	return map[string]any{
		"seq": seq, "lamport": seq,
		"time": time.Now().UTC().Format(time.RFC3339Nano),
		"kind": "put", "path": path, "blob": blob, "size": size,
	}
}

func secfx3History(t *testing.T, h http.Handler, projectID string, c *http.Cookie) map[string]HistoryEntry {
	t.Helper()
	rec := doAs(t, h, "GET", "/api/p/"+projectID+"/history", nil, c)
	if rec.Code != 200 {
		t.Fatalf("history: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Entries []HistoryEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	byPath := map[string]HistoryEntry{}
	for _, e := range out.Entries {
		byPath[e.Path] = e
	}
	return byPath
}

// ---------------------------------------------------------------------------
// The (account, id) registry rekey — LookupIn resolves by LAST SEEN, not by
// owner, so the second account to name an id decides what the project shows.
// ---------------------------------------------------------------------------

// Round 3 replaced "first caller owns the id" with a per-(account, id) row,
// on the stated grounds that "two accounts naming the same id hold two
// separate rows and cannot overwrite or lock out each other". That is true of
// MayActAs. It is not true of the display join every project surface uses:
// LookupIn scans every row with a matching id, keeps the one with the newest
// LastSeen, and returns it — so a SECOND row simply has to be fresher to win.
//
// alice's laptop syncs alice's project; her ops are attributed from the
// journal key, which round 3 made the trustworthy part. bob is a plain member
// of the same org, so his rows pass deviceVisibleIn — he sends ONE ordinary
// store request naming her device id and any machine name he likes, and from
// then on her changes are credited to his string in the org's audit feed.
//
// The forgery round 3 named as the reason for the rekey ("forge History
// attribution") is therefore still reachable; it just needs a store call
// instead of a read report.
func TestSec_Devices_MemberCannotRelabelAnotherMembersDeviceInHistory(t *testing.T) {
	h, srv, c, p := permHub(t)
	reg, err := OpenDeviceRegistry(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Devices = reg

	const (
		aliceDev  = "alice-laptop-9f21"
		aliceName = "Alice's MacBook"
		aliceOS   = "darwin 26.1"
	)
	if rec := secfx3Sync(t, h, p.ID, c["alice"], aliceDev, aliceName, aliceOS); rec.Code != 200 {
		t.Fatalf("alice's own sync: %d %s", rec.Code, rec.Body)
	}
	blob := secfx3PutBlob(t, h, p.ID, "alice's notes", c["alice"])
	secfx3PushJournal(t, h, p.ID, aliceDev, []map[string]any{
		secfx3Op(1, "notes.md", blob, len("alice's notes")),
	}, c["alice"])

	// Control: History joins her real row, so the join is live.
	if got := secfx3History(t, h, p.ID, c["alice"])["notes.md"].Device; got.Name != aliceName {
		t.Fatalf("control: History did not join alice's device row (got %+v)", got)
	}

	// The attack: bob, a plain member of the same org, makes one ordinary
	// store request claiming her device id.
	time.Sleep(2 * time.Millisecond) // make "most recently observed" unambiguous
	const forged = "Alice's MacBook (COMPROMISED - call IT)"
	if rec := secfx3Sync(t, h, p.ID, c["bob"], aliceDev, forged, "windows"); rec.Code != 200 {
		t.Fatalf("bob's store call: %d %s", rec.Code, rec.Body)
	}

	got := secfx3History(t, h, p.ID, c["alice"])["notes.md"].Device
	if got.Name == forged || got.OS == "windows" {
		t.Errorf("History now credits alice's own change to a label bob chose: %+v\n"+
			"LookupIn returns the most recently OBSERVED row for an id regardless of which account owns it, "+
			"so the second account to name an id decides what the whole project sees", got)
	}
	if got.Name != aliceName {
		t.Errorf("History lost alice's real device name after bob named her id: %+v", got)
	}
}

// The same join, one surface over: /heat?by=device reports agent device ids
// with the name and OS LookupIn hands back. Asserting it separately keeps a
// fix that only patches handleHistory from looking green.
func TestSec_Devices_MemberCannotRelabelAnotherMembersDeviceInHeat(t *testing.T) {
	h, srv, c, p := permHub(t)
	secfx3Telemetry(t, srv)

	const (
		aliceDev  = "alice-laptop-4d10"
		aliceName = "Alice's MacBook"
	)
	if rec := secfx3Sync(t, h, p.ID, c["alice"], aliceDev, aliceName, "darwin 26.1"); rec.Code != 200 {
		t.Fatalf("alice's sync: %d %s", rec.Code, rec.Body)
	}
	// alice's agent reports a read, so the id becomes an actor in the ledger.
	rec := secfx3Do(t, h, "POST", "/api/p/"+p.ID+"/reads",
		map[string]any{"reads": []map[string]string{{"path": "notes.md"}}},
		c["alice"], map[string]string{"X-Bdrive-Device": aliceDev})
	if rec.Code != 200 {
		t.Fatalf("alice's read report: %d %s", rec.Code, rec.Body)
	}

	time.Sleep(2 * time.Millisecond)
	const forged = "not-alices-machine"
	if rec := secfx3Sync(t, h, p.ID, c["bob"], aliceDev, forged, "plan9"); rec.Code != 200 {
		t.Fatalf("bob's store call: %d %s", rec.Code, rec.Body)
	}

	rec = doAs(t, h, "GET", "/api/p/"+p.ID+"/heat?by=device", nil, c["alice"])
	if rec.Code != 200 {
		t.Fatalf("heat: %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, forged) || strings.Contains(body, "plan9") {
		t.Errorf("/heat?by=device reports a device label bob chose for alice's device id: %s", body)
	}
}

// secfx3Telemetry gives a permHub the two tables the identity check sits
// between.
func secfx3Telemetry(t *testing.T, srv *Server) {
	t.Helper()
	var err error
	if srv.Reads, err = OpenReadLedger(filepath.Join(t.TempDir(), "reads.json"), 0); err != nil {
		t.Fatal(err)
	}
	if srv.Devices, err = OpenDeviceRegistry(filepath.Join(t.TempDir(), "devices.json")); err != nil {
		t.Fatal(err)
	}
}

func secfx3Do(t *testing.T, h http.Handler, method, target string, body any, c *http.Cookie, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body == nil {
		rd = strings.NewReader("")
	} else {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = strings.NewReader(string(data))
	}
	req := httptest.NewRequest(method, target, rd)
	if c != nil {
		req.AddCookie(c)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// "The journal KEY is the one part the hub binds to the pushing device."
// ---------------------------------------------------------------------------

// Round 3's History fix rests on a stated premise (server.go, sourcedOp):
// "the journal KEY is the one part the hub binds to the pushing device
// (store.go's ownJournal), so From is the only trustworthy attribution."
//
// ownJournal binds the key to the X-Bdrive-Device HEADER — and nothing binds
// that header to an account. Round 1's TestSec_Store_ForeignDeviceJournalWrite
// varies the key while holding the header fixed, which is the one combination
// that is refused. Move both together and the check is satisfied by
// construction: bob sets the header to alice's device id and PUTs alice's
// journal key.
//
// Two consequences, from any project member:
//   - Alice's journal object is REPLACED. That is the repo's first invariant
//     ("each device writes only its own journal") broken at the hub, and every
//     peer replays bob's version — a delete op there unlinks the file on every
//     teammate's disk.
//   - History attributes the forged ops to sop.From = alice's device id, which
//     LookupIn then resolves to her real machine name. The audit trail says
//     Alice's MacBook did it.
//
// The primitive to fix it with already exists and is right next door:
// DeviceRegistry.MayActAs, which ownsDevice uses for exactly this question.
func TestSec_Store_MemberCannotWriteAPeersJournalByRenamingItself(t *testing.T) {
	h, srv, c, p := permHub(t)
	reg, err := OpenDeviceRegistry(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Devices = reg

	const (
		aliceDev  = "alice-laptop-5b73"
		aliceName = "Alice's MacBook"
	)
	// Alice's device is known to the hub the only way a device becomes known:
	// its own sync traffic.
	if rec := secfx3Sync(t, h, p.ID, c["alice"], aliceDev, aliceName, "darwin 26.1"); rec.Code != 200 {
		t.Fatalf("alice's sync: %d %s", rec.Code, rec.Body)
	}
	blob := secfx3PutBlob(t, h, p.ID, "alice's notes", c["alice"])
	secfx3PushJournal(t, h, p.ID, aliceDev, []map[string]any{
		secfx3Op(1, "notes.md", blob, len("alice's notes")),
	}, c["alice"])
	before := secfx3History(t, h, p.ID, c["alice"])
	if before["notes.md"].Device.Name != aliceName {
		t.Fatalf("control: alice's own op is not attributed to her device (%+v)", before["notes.md"].Device)
	}

	// Control: bob writing HIS OWN journal is legitimate and must keep working.
	bobOK := secfx3Do(t, h, "PUT", "/api/p/"+p.ID+"/store/object?key=journal/bob-laptop-0001.jsonl",
		nil, c["bob"], map[string]string{"X-Bdrive-Device": "bob-laptop-0001"})
	if bobOK.Code != 200 {
		t.Fatalf("control: bob cannot write his own journal: %d %s", bobOK.Code, bobOK.Body)
	}

	// The attack: bob renames himself to alice's device and writes her journal.
	forged := `{"seq":99,"lamport":99999,"time":"2026-12-31T00:00:00Z","kind":"delete","path":"notes.md","device":"` + aliceDev + `"}` + "\n" +
		`{"seq":100,"lamport":100000,"time":"2026-12-31T00:00:01Z","kind":"put","path":"policy.md","blob":"` + blob + `","size":13,"device":"` + aliceDev + `"}` + "\n"
	req := httptest.NewRequest("PUT", "/api/p/"+p.ID+"/store/object?key=journal/"+aliceDev+".jsonl",
		strings.NewReader(forged))
	req.Header.Set("X-Bdrive-Device", aliceDev)
	req.AddCookie(c["bob"])
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("bob wrote alice's journal by setting X-Bdrive-Device to her device id: %d %s, want 403\n"+
			"ownJournal binds the key to the header the SAME request supplies, and nothing binds that header to an account",
			rec.Code, rec.Body)
	}

	// Alice's own op must still be in the project's history, and neither
	// forged op may be.
	for _, e := range secfx3Entries(t, h, p.ID, c["alice"]) {
		if e.Path == "policy.md" || (e.Path == "notes.md" && e.Kind == "delete") {
			t.Errorf("an op bob authored survives in History as %s %s, credited to %+v — "+
				"alice's journal object was replaced, so every peer replays it too", e.Kind, e.Path, e.Device)
		}
	}
	if !secfx3Has(t, h, p.ID, c["alice"], "notes.md", "add") {
		t.Errorf("alice's own op for notes.md is gone from the project's history — " +
			"'each device writes only its own journal' is broken at the hub")
	}
}

func secfx3Entries(t *testing.T, h http.Handler, projectID string, c *http.Cookie) []HistoryEntry {
	t.Helper()
	rec := doAs(t, h, "GET", "/api/p/"+projectID+"/history", nil, c)
	if rec.Code != 200 {
		t.Fatalf("history: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Entries []HistoryEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Entries
}

func secfx3Has(t *testing.T, h http.Handler, projectID string, c *http.Cookie, path, kind string) bool {
	t.Helper()
	for _, e := range secfx3Entries(t, h, projectID, c) {
		if e.Path == path && e.Kind == kind {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The rekey is in memory only: the repo under it is still keyed by id alone.
// ---------------------------------------------------------------------------

// DeviceRegistry now holds map[devKey]DeviceInfo, but every row is persisted
// through DeviceRepo.Put(DeviceInfo) — and both backends key on the id alone
// (fileDeviceRepo.byID[d.ID]; sqlDeviceRepo's `ON CONFLICT(id) DO UPDATE`).
// Two accounts' rows for one id therefore collapse into ONE row on disk, and
// whichever wrote last is the only one NewDeviceRegistry reloads.
//
// So the whole (account, id) model lives exactly as long as the process. After
// any restart — a deploy, a crash, an `systemctl restart` — the hub believes
// alice's laptop belongs to whoever named it last, and MayActAs refuses her
// real device: her agent's read heat stops being recorded, silently and
// permanently, which is precisely the outcome round 3 says the rekey dissolved
// (TestSec_Devices_IdCannotBeSquattedBeforeItsOwnerRegisters).
//
// The registry's own comment concedes the disk row is "a display cache, not
// the authority" and that "a device re-registers on its very next sync cycle".
// Both are true — and neither helps, because the reload is what MayActAs then
// reads, and re-registering only ADDS alice's row back alongside the row that
// is still refusing her.
func TestSec_Devices_OwnershipSurvivesAHubRestart(t *testing.T) {
	h, srv, c, p := permHub(t)
	path := filepath.Join(t.TempDir(), "devices.json")
	reg, err := OpenDeviceRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	srv.Devices = reg

	const aliceDev = "alice-laptop-1c4e"
	if rec := secfx3Sync(t, h, p.ID, c["alice"], aliceDev, "Alice's MacBook", "darwin"); rec.Code != 200 {
		t.Fatalf("alice's sync: %d %s", rec.Code, rec.Body)
	}
	if !reg.MayActAs("alice@x.io", aliceDev) {
		t.Fatal("control: alice cannot act as the device she just synced")
	}

	// bob names her id once, in the same project he is a member of.
	time.Sleep(2 * time.Millisecond)
	if rec := secfx3Sync(t, h, p.ID, c["bob"], aliceDev, "bobs-label", "windows"); rec.Code != 200 {
		t.Fatalf("bob's store call: %d %s", rec.Code, rec.Body)
	}
	// In memory the rekey holds — this is round 3's fix working.
	if !reg.MayActAs("alice@x.io", aliceDev) {
		t.Fatal("control: the in-memory (account, id) rekey already failed")
	}

	// Restart the hub: same file, fresh registry.
	restarted, err := OpenDeviceRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.MayActAs("alice@x.io", aliceDev) {
		t.Errorf("after a restart the hub refuses alice's own device %q: the (account, id) rekey is in-memory only — "+
			"DeviceRepo.Put is keyed by id, so bob's row overwrote hers on disk and hers is the one that vanished", aliceDev)
	}
	info, ok := restarted.LookupIn(aliceDev, func(string) bool { return true })
	if ok && info.User != "" && info.User != "alice@x.io" {
		t.Errorf("after a restart the only surviving row for alice's device is owned by %q (name %q): "+
			"one account's store call permanently rewrote another account's device metadata", info.User, info.Name)
	}
}

// ---------------------------------------------------------------------------
// clientIP — "the last hop" is read out of the FIRST header line only.
// ---------------------------------------------------------------------------

// Round 3 fixed clientIP to take the last X-Forwarded-For element, because
// proxies append. It reads that list with r.Header.Get, which returns only the
// FIRST X-Forwarded-For field line — and a request can legitimately carry
// several. RFC 9110 says multiple field lines of the same name are equivalent
// to one comma-joined list in order, so the operator's proxy hop is in the
// LAST line whenever the proxy adds its own header rather than rewriting the
// client's.
//
// A client that sends `X-Forwarded-For: <anything>` therefore owns the whole
// key again: Get() hands back its line, Split takes its only element, and the
// login limiter gets a fresh bucket per attempt — round 3's own finding, one
// header line over.
//
// The secure read is Values() and the last element of the last line.
func TestSec_RateLimit_TrustedProxyIgnoresAnExtraForwardedForLine(t *testing.T) {
	h, srv, _, _ := permHub(t)
	srv.TrustProxy = true
	h = srv.Handler()

	const proxy = "10.1.1.1"
	blocked := false
	for i := 0; i < 40; i++ {
		form := url.Values{"email": {"nobody@x.io"}, "password": {"wrong"}}
		req := httptest.NewRequest("POST", "http://hub.test/auth/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = proxy + ":9999"
		// The client's own line, then the line the operator's proxy added.
		req.Header.Add("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i%250))
		req.Header.Add("X-Forwarded-For", proxy)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			blocked = true
			break
		}
	}
	if !blocked {
		t.Error("with trust_proxy on, 40 failed logins through one proxy hop all went through when the client " +
			"added its OWN X-Forwarded-For field line: clientIP reads Header.Get (the first line only), so a " +
			"second field line from the client is the entire key — the brute-force limiter is off again")
	}
}

// ---------------------------------------------------------------------------
// Clean assertions — round 3 fixes that held up under attack.
// ---------------------------------------------------------------------------

// LookupIn's predicate is deviceVisibleIn(project), which resolves the
// PROJECT's org and asks whether the row's owner is in it. Assert that it is
// the project's org and not the caller's: dave is in another org, so his
// device must stay invisible in alice's project even when the caller is a
// member of both orgs — and the answer for a device that does not exist must
// be identical to the answer for one that exists but belongs elsewhere.
func TestSec_Devices_LookupScopeIsTheProjectsOrgNotTheCallers(t *testing.T) {
	h, srv, c, alice := permHub(t)
	reg, err := OpenDeviceRegistry(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Devices = reg

	// dave's own org, his own project, his own device.
	rec := doAs(t, h, "POST", "/api/projects", map[string]string{"name": "dave-notes"}, c["dave"])
	if rec.Code != 200 {
		t.Fatalf("dave's project: %d %s", rec.Code, rec.Body)
	}
	var dp struct {
		Project Project `json:"project"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dp); err != nil {
		t.Fatal(err)
	}
	const daveDev = "dave-box-77aa"
	if rec := secfx3Sync(t, h, dp.Project.ID, c["dave"], daveDev, "Dave's ThinkPad", "openbsd"); rec.Code != 200 {
		t.Fatalf("dave's sync: %d %s", rec.Code, rec.Body)
	}

	// Make alice a member of dave's org too: the caller now spans both orgs.
	dir, ok := srv.Dir.(LocalDirectory)
	if !ok {
		t.Skip("fixture is not a LocalDirectory")
	}
	if err := dir.OrgDB.AddMember(dp.Project.Org, "alice@x.io", RoleMember); err != nil {
		t.Fatal(err)
	}

	// In ALICE's project, dave's device must not resolve — and must read the
	// same as an id nobody ever registered.
	visible := srv.deviceVisibleIn(alice.ID)
	real1, ok1 := reg.LookupIn(daveDev, visible)
	fake, ok2 := reg.LookupIn("no-such-device-0000", visible)
	if ok1 != ok2 || real1 != fake {
		t.Errorf("in alice's project a foreign org's device resolves to %+v (found=%v) while an unknown id resolves to %+v (found=%v): "+
			"the scope followed the CALLER's memberships, not the project's org", real1, ok1, fake, ok2)
	}
	// Control: in dave's own project the same id does resolve.
	if _, ok := reg.LookupIn(daveDev, srv.deviceVisibleIn(dp.Project.ID)); !ok {
		t.Fatal("control: dave's device does not resolve in dave's own project — the join is dead, not scoped")
	}
}

// History's fallback when the registry join finds nothing must not tell the
// caller WHY it found nothing. Round 3 made attribution come from the journal
// key (sourcedOp.From) and the name come from the op only when the op agrees
// with the key. Assert that an unregistered device and a device that exists
// but is out of scope produce byte-identical shapes.
func TestSec_Devices_HistoryFallbackDoesNotDistinguishUnknownFromDenied(t *testing.T) {
	h, srv, c, p := permHub(t)
	reg, err := OpenDeviceRegistry(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Devices = reg

	// A device that exists on the hub but belongs to another org.
	rec := doAs(t, h, "POST", "/api/projects", map[string]string{"name": "dave-notes-2"}, c["dave"])
	if rec.Code != 200 {
		t.Fatalf("dave's project: %d %s", rec.Code, rec.Body)
	}
	var dp struct {
		Project Project `json:"project"`
	}
	json.Unmarshal(rec.Body.Bytes(), &dp)
	const daveDev = "dave-box-9911"
	secfx3Sync(t, h, dp.Project.ID, c["dave"], daveDev, "Dave's ThinkPad", "openbsd")

	// alice pushes two journals into her own project under two device keys she
	// is entitled to write: one that exists elsewhere on the hub, one that
	// exists nowhere. Neither is registered in HER org.
	blob := secfx3PutBlob(t, h, p.ID, "body", c["alice"])
	secfx3PushJournal(t, h, p.ID, daveDev, []map[string]any{secfx3Op(1, "a.md", blob, 4)}, c["alice"])
	secfx3PushJournal(t, h, p.ID, "totally-unknown-3333", []map[string]any{secfx3Op(1, "b.md", blob, 4)}, c["alice"])

	hist := secfx3History(t, h, p.ID, c["alice"])
	a, b := hist["a.md"].Device, hist["b.md"].Device
	if a.Name != b.Name || a.OS != b.OS {
		t.Errorf("History distinguishes a device that exists elsewhere on the hub (%+v) from one that does not exist (%+v): "+
			"the join is an existence oracle one layer down", a, b)
	}
}

// setContentLength must never promise a length it did not measure, and must
// never contradict the bytes it then writes. Round 3 stopped echoing the
// journal's Size field; pin that the header, when present, equals the body.
func TestSec_Journal_ContentLengthAlwaysMatchesTheBodyServed(t *testing.T) {
	h, srv, c, p := permHub(t)
	_ = srv

	const body = "the real content"
	blob := secfx3PutBlob(t, h, p.ID, body, c["alice"])
	// A journal op whose Size is a lie in both directions.
	secfx3PushJournal(t, h, p.ID, "alice-dev-0001", []map[string]any{
		secfx3Op(1, "big.txt", blob, 1<<20),
		secfx3Op(2, "small.txt", blob, 1),
	}, c["alice"])

	for _, route := range []string{"file", "download"} {
		for _, name := range []string{"big.txt", "small.txt"} {
			rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/"+route+"?path="+name, nil, c["alice"])
			if rec.Code != 200 {
				t.Fatalf("%s %s: %d %s", route, name, rec.Code, rec.Body)
			}
			if got := rec.Body.String(); got != body {
				t.Errorf("%s %s served %q, want %q", route, name, got, body)
			}
			if cl := rec.Header().Get("Content-Length"); cl != "" && cl != fmt.Sprint(len(body)) {
				t.Errorf("%s %s: Content-Length %s but %d bytes written — a journal field is still deciding the header",
					route, name, cl, len(body))
			}
		}
	}
}

// The reset mail now goes out on a goroutine. Nothing about that may reach the
// caller: no token in the response, the same answer for a known and an unknown
// address, and no panic when no Mailer is configured (a panic in a goroutine
// is not recovered by net/http — it takes the whole hub down).
func TestSec_Password_ResetGoroutineLeaksNothingAndCannotCrashTheHub(t *testing.T) {
	h, _, _, _ := permHub(t)
	// permHub signs alice up, so alice@x.io exists and nobody@x.io does not.
	known := secfx3PostForm(t, h, "/auth/reset", url.Values{"email": {"alice@x.io"}})
	unknown := secfx3PostForm(t, h, "/auth/reset", url.Values{"email": {"nobody@x.io"}})
	if known.Code != unknown.Code || known.Body.String() != unknown.Body.String() {
		t.Errorf("reset answers a known address (%d) differently from an unknown one (%d)", known.Code, unknown.Code)
	}
	if strings.Contains(known.Body.String(), "/auth/reset/confirm") {
		t.Errorf("the reset response carries the grant link: %s", known.Body)
	}
	// Let the goroutine run; a nil Mailer must be handled, not dereferenced.
	time.Sleep(50 * time.Millisecond)
}

func secfx3PostForm(t *testing.T, h http.Handler, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "http://hub.test"+target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "203.0.113.77:4444"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
