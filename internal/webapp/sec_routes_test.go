package webapp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Round 8, the routes with permission-gate coverage and no route-specific
// attack test: /store/exists, /download, /remove — plus the two exported
// functions no TestSec_* test referenced at all, Endpoint and AgentHeat.
//
// Helpers here are prefixed sec8. doAs cannot set headers and every attack on
// this page turns on X-Bdrive-Device, so sec8Do is doAs with a header map.

func sec8Do(t *testing.T, h http.Handler, method, url string, body any, c *http.Cookie, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case []byte:
		rd = bytes.NewReader(b)
	case string:
		rd = bytes.NewReader([]byte(b))
	default:
		data, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, url, rd)
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

// sec8SetPerm gives someone an explicit project level, as alice the admin.
func sec8SetPerm(t *testing.T, h http.Handler, p Project, admin *http.Cookie, email, level string) {
	t.Helper()
	rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions/"+email, map[string]string{"level": level}, admin)
	if rec.Code != 200 {
		t.Fatalf("set %s=%s: %d %s", email, level, rec.Code, rec.Body)
	}
}

const sec8Sha = "0000000000000000000000000000000000000000000000000000000000000000"

// ---------------------------------------------------------------------------
// 1. /store/exists — a READ route that first-claims a device id
// ---------------------------------------------------------------------------

// Round 5 established the rule the write doors follow: a request must not
// register the device it claims to be, because the row it creates is the row
// ownJournal then consults, and OwnerOf is hub-wide first-claim. The fix was
// applied to handleStorePut only — it observes AFTER ownJournal returns, with
// a comment saying exactly why.
//
// handleStoreExists, handleStoreList and handleStoreGet are all PermRead and
// all call s.observeDevice(r) unconditionally, before they do anything else.
// So the claim a write door refuses to make is made for free by a READ.
//
// The attack needs nothing but read permission on any one project on the hub:
//
//	GET /api/p/<any project I can read>/store/exists?key=blobs/<64 hex>
//	X-Bdrive-Device: <the id of a device that has not synced yet>
//
// The registry now says that id belongs to the reader. When the real device
// makes its first journal push, OwnerOf answers somebody else, journalNames'
// first-claim arm is dead (known == true), and the push is 403 with a message
// telling its owner to delete device.json — i.e. to abandon its journal and
// its entire local history. OwnerOf is hub-wide, so the target device need not
// be in the attacker's org, or in any project the attacker can see.
//
// The secure behaviour is the one the write door already implements: a route
// that grants nothing must claim nothing. A read may refresh a row its own
// account already owns; it must not create the FIRST row for an id.
func TestSec_Row5_AReadRouteCannotFirstClaimADeviceId(t *testing.T) {
	h, srv, c, p := permHub(t)
	sec8SetPerm(t, h, p, c["alice"], "bob@x.io", PermRead)

	const victimDev = "carol-laptop-01"

	// bob, read-only, touches a read route naming carol's device.
	rec := sec8Do(t, h, "GET", "/api/p/"+p.ID+"/store/exists?key=blobs/"+sec8Sha, nil, c["bob"],
		map[string]string{"X-Bdrive-Device": victimDev, "X-Bdrive-Device-Name": "Bobs Squat"})
	if rec.Code != 200 {
		t.Fatalf("read-only member could not call /store/exists at all: %d %s", rec.Code, rec.Body)
	}

	if owner, known := srv.Devices.OwnerOf(victimDev); known {
		t.Fatalf("a PermRead route first-claimed device %q for %q\n"+
			"(handleStoreExists calls observeDevice before any decision; OwnerOf is hub-wide\n"+
			"first-claim, so this is the write door's own refusal reached through a read)",
			victimDev, owner)
	}
}

// The same claim, shown as what it actually costs: carol's own device can no
// longer push its journal. This is round 5's C2 lockout, reached from a route
// that grants no write anywhere.
//
// The control comes first and is the whole point of the test: on a clean hub
// carol's first push is accepted. The only thing that changes between the two
// halves is one GET by a read-only member.
func TestSec_Row5_AReadOnlyMemberCannotLockADeviceOutOfItsOwnJournal(t *testing.T) {
	const victimDev = "carol-laptop-02"
	body := secaudOpLine(1, victimDev, "put", "notes.md", sec8Sha)
	url := func(p Project) string {
		return "/api/p/" + p.ID + "/store/object?key=journal/" + victimDev + ".jsonl"
	}

	// Control: nobody squatted, carol's first claim lands.
	{
		h, _, c, p := permHub(t)
		rec := sec8Do(t, h, "PUT", url(p), body, c["carol"], map[string]string{"X-Bdrive-Device": victimDev})
		if rec.Code != 200 {
			t.Fatalf("control: carol's own first journal claim was refused: %d %s", rec.Code, rec.Body)
		}
	}

	// Attack: bob, read-only, names carol's device on a read route first.
	h, _, c, p := permHub(t)
	sec8SetPerm(t, h, p, c["alice"], "bob@x.io", PermRead)
	if rec := sec8Do(t, h, "GET", "/api/p/"+p.ID+"/store/exists?key=blobs/"+sec8Sha, nil, c["bob"],
		map[string]string{"X-Bdrive-Device": victimDev}); rec.Code != 200 {
		t.Fatalf("setup: /store/exists as bob: %d %s", rec.Code, rec.Body)
	}

	rec := sec8Do(t, h, "PUT", url(p), body, c["carol"], map[string]string{"X-Bdrive-Device": victimDev})
	if rec.Code != 200 {
		t.Fatalf("carol's own device was locked out of its own journal by one read-only GET: %d %s\n"+
			"(bob called /store/exists with X-Bdrive-Device: %s; observeDevice on a PermRead route\n"+
			"recorded the first claim, so OwnerOf now answers bob and ownJournal 403s the real owner)",
			rec.Code, strings.TrimSpace(rec.Body.String()), victimDev)
	}
}

// The other two read doors carry the same call. Same property, so it belongs
// in the same regression test rather than three copies of the reasoning.
func TestSec_Row5_NoReadRouteRegistersADeviceItHasNeverSeen(t *testing.T) {
	for _, tc := range []struct{ name, url string }{
		{"exists", "/store/exists?key=blobs/" + sec8Sha},
		{"list", "/store/list?prefix=blobs/"},
		{"object", "/store/object?key=blobs/" + sec8Sha},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, srv, c, p := permHub(t)
			sec8SetPerm(t, h, p, c["alice"], "bob@x.io", PermRead)
			dev := "unclaimed-" + tc.name
			sec8Do(t, h, "GET", "/api/p/"+p.ID+tc.url, nil, c["bob"],
				map[string]string{"X-Bdrive-Device": dev})
			if _, known := srv.Devices.OwnerOf(dev); known {
				t.Fatalf("GET %s registered device %q for a read-only member", tc.url, dev)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 2. /remove — route-specific
// ---------------------------------------------------------------------------

// sec8Seed puts one file into the project through the relayed browser upload
// door (one call, no presigning), so the snapshot /remove and /download
// consult actually holds it.
func sec8Seed(t *testing.T, h http.Handler, p Project, c *http.Cookie, path, content string) {
	t.Helper()
	rec := sec8Do(t, h, "PUT", "/api/p/"+p.ID+"/upload/content?path="+url.QueryEscape(path), content, c, nil)
	if rec.Code != 200 {
		t.Fatalf("upload/content %q: %d %s", path, rec.Code, rec.Body)
	}
}

// /remove writes a delete op the whole fleet replays, so it is a destructive
// route with only a project-level gate in front of it. These are the things it
// must refuse regardless of who is asking:
//
//   - a path that is not in the project's tree (nothing to delete)
//   - a path that leaves the project (traversal, absolute, reserved dir)
//   - a spelling that is not the snapshot key it would delete
//
// The last one is the one that matters: cleanUploadPath NORMALIZES, and the
// snapshot is keyed by the exact journal path. If "a/../notes.md" cleaned to
// "notes.md" and the handler then deleted the snapshot's "notes.md", a caller
// could aim a delete with a spelling that never appears in the tree. That is
// only safe because the handler looks the CLEANED path up. Pin it.
func TestSec_Row6_RemoveOnlyEverDeletesAPathTheProjectActuallyHolds(t *testing.T) {
	h, _, c, p := permHub(t)
	sec8Seed(t, h, p, c["alice"], "notes.md", "hello")

	refused := []string{
		"../../etc/passwd",
		"/etc/passwd",
		"..",
		".",
		"",
		".git/config",
		".bdrive/config.json",
		"notes.md/",
		"./notes.md/../../notes.md",
		"notes.md\x00",
		"nope.md",
	}
	for _, path := range refused {
		rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/remove", map[string]string{"path": path}, c["alice"])
		if rec.Code == 200 {
			t.Errorf("remove %q was accepted (%s)", path, strings.TrimSpace(rec.Body.String()))
		}
	}
	// The tree is untouched by every refusal above.
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/file?path=notes.md", nil, c["alice"]); rec.Code != 200 {
		t.Fatalf("a refused remove destroyed the file anyway: %d %s", rec.Code, rec.Body)
	}
	// And the honest remove works, so the refusals above are not vacuous.
	if rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/remove", map[string]string{"path": "notes.md"}, c["alice"]); rec.Code != 200 {
		t.Fatalf("control: honest remove refused: %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/file?path=notes.md", nil, c["alice"]); rec.Code == 200 {
		t.Fatal("control: the file survived its own removal")
	}
}

// A delete op is authored by the hub's own device, not by the caller's — the
// caller never gets to choose which journal slot a removal occupies, which is
// what keeps /remove out of the "each device writes only its own journal"
// invariant. A caller that sends X-Bdrive-Device must not move the op.
func TestSec_Row6_RemoveCannotAuthorItselfIntoAnotherDevicesJournal(t *testing.T) {
	h, srv, c, p := permHub(t)
	sec8Seed(t, h, p, c["alice"], "doomed.md", "bye")

	rec := sec8Do(t, h, "POST", "/api/p/"+p.ID+"/remove", map[string]string{"path": "doomed.md"},
		c["alice"], map[string]string{"X-Bdrive-Device": "carol-laptop-03"})
	if rec.Code != 200 {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
	if _, known := srv.Devices.OwnerOf("carol-laptop-03"); known {
		t.Fatal("POST /remove registered the device id in its header")
	}
	// The delete landed in the hub's journal, so History credits the hub's
	// device and no peer's log was rewritten.
	rec = doAs(t, h, "GET", "/api/p/"+p.ID+"/history?path=doomed.md", nil, c["alice"])
	if rec.Code != 200 {
		t.Fatalf("history: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "carol-laptop-03") {
		t.Fatalf("the removal was journaled under the caller's chosen device id: %s", rec.Body)
	}
}

// ---------------------------------------------------------------------------
// 3. /download — route-specific
// ---------------------------------------------------------------------------

// /download is the one blob door that does NOT call sandboxInline: handleFile
// passes attach=false and gets the "sandbox allow-scripts" CSP for active
// types, handleDownload passes attach=true and gets a Content-Disposition
// instead. That is a real defence, but it is the ONLY one, so it has to
// actually be on every response — including for the types that would execute
// on the hub's own origin if a browser ever rendered them.
//
// The path is also a journal string, so the filename parameter is peer-chosen.
// A raw CR/LF there would be header injection; SafePath refuses control
// characters at ingest, and %q would escape them anyway — this pins both.
func TestSec_Row11_DownloadNeverServesActiveContentWithoutADisposition(t *testing.T) {
	h, _, c, p := permHub(t)
	for _, name := range []string{"evil.html", "evil.svg", "evil.xhtml", "notes.md", `weird".html`} {
		sec8Seed(t, h, p, c["alice"], name, "<script>alert(1)</script>")
	}
	for _, name := range []string{"evil.html", "evil.svg", "evil.xhtml", "notes.md", `weird".html`} {
		rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/download?path="+url.QueryEscape(name), nil, c["alice"])
		if rec.Code != 200 {
			t.Fatalf("download %q: %d %s", name, rec.Code, rec.Body)
		}
		cd := rec.Header().Get("Content-Disposition")
		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.HasPrefix(cd, "attachment") && !strings.Contains(csp, "sandbox") {
			t.Errorf("download %q: Content-Type %q with neither an attachment disposition nor a sandbox CSP",
				name, rec.Header().Get("Content-Type"))
		}
		for _, h := range []string{cd, rec.Header().Get("Content-Type"), rec.Header().Get("ETag")} {
			if strings.ContainsAny(h, "\r\n") {
				t.Errorf("download %q: header carries a bare CR/LF: %q", name, h)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 4. AgentHeat — the actor column must not leave the server
// ---------------------------------------------------------------------------

// AgentHeat is the only ledger accessor that returns the ACTOR as a map key,
// and heatByDevice serves those keys straight out as deviceHeat.ID. Its whole
// safety argument is one line — "key.Kind != ReadKindAgent: continue" — on the
// premise that agent actors are device ids and nothing else. Human actors are
// emails and share actors are tokens; either one reaching this map is a direct
// identity leak to every project member.
//
// This drives the ledger through its real ingest paths (a viewer read = human,
// a /s/ hit = share, a device report = agent) and then reads AgentHeat back,
// so the filter is measured rather than assumed. It is the test that goes red
// if the Kind clause is ever relaxed.
func TestSec_Row10_AgentHeatNeverCarriesAHumanOrShareActor(t *testing.T) {
	h, srv, c, p := permHub(t)
	ledger, err := OpenReadLedger(t.TempDir()+"/reads.json", 0)
	if err != nil {
		t.Fatal(err)
	}
	srv.Reads = ledger
	sec8Seed(t, h, p, c["alice"], "notes.md", "hello")

	// human: alice reads the file in the viewer.
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/file?path=notes.md", nil, c["alice"]); rec.Code != 200 {
		t.Fatalf("viewer read: %d %s", rec.Code, rec.Body)
	}
	// agent: carol's device reports a read.
	if rec := sec8Do(t, h, "POST", "/api/p/"+p.ID+"/reads",
		map[string]any{"reads": []map[string]string{{"path": "notes.md"}}}, c["carol"],
		map[string]string{"X-Bdrive-Device": "carol-laptop-04"}); rec.Code != 200 {
		t.Fatalf("read report: %d %s", rec.Code, rec.Body)
	}
	// share: a public hit on a minted link.
	rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/shares", map[string]string{"path": "notes.md"}, c["alice"])
	if rec.Code != 200 {
		t.Fatalf("mint share: %d %s", rec.Code, rec.Body)
	}
	var sh struct {
		Token string `json:"token"`
	}
	json.Unmarshal(rec.Body.Bytes(), &sh)
	if sh.Token != "" {
		sec8Do(t, h, "GET", "/s/"+sh.Token, nil, nil, nil)
	}

	for actor := range srv.Reads.AgentHeat(p.ID, time.Time{}) {
		if strings.Contains(actor, "@") || !validDeviceID(actor) {
			t.Fatalf("AgentHeat returned actor %q — not a device id; heatByDevice serves this "+
				"key verbatim as deviceHeat.ID to every project member", actor)
		}
		if sh.Token != "" && actor == sh.Token {
			t.Fatalf("AgentHeat returned a share token as an actor: %q", actor)
		}
	}

	// And the wire form carries no identity either, for the same reason.
	rec = doAs(t, h, "GET", "/api/p/"+p.ID+"/heat?by=device", nil, c["bob"])
	if rec.Code != 200 {
		t.Fatalf("heat?by=device: %d %s", rec.Code, rec.Body)
	}
	for _, needle := range []string{"alice@x.io", "carol@x.io", "bob@x.io", sh.Token} {
		if needle != "" && strings.Contains(rec.Body.String(), needle) {
			t.Fatalf("/heat?by=device leaked %q: %s", needle, rec.Body)
		}
	}
}

// ---------------------------------------------------------------------------
// 5. Endpoint — the analytics block on the anonymous config
// ---------------------------------------------------------------------------

// AnalyticsConfig.Endpoint feeds /api/config, which authGate serves to
// signed-OUT visitors on purpose. That makes it the one operator-configured
// string on the anonymous surface, so what it may carry is a real question:
// the block must appear only when the hub configured one, must be exactly
// {key, host}, and must never gain a field from the rest of the config.
func TestSec_Row12_AnonymousConfigCarriesNoAnalyticsUntilOneIsConfigured(t *testing.T) {
	h, srv, _, _ := permHub(t)

	rec := sec8Do(t, h, "GET", "/api/config", nil, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("anonymous config: %d %s", rec.Code, rec.Body)
	}
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if _, ok := out["analytics"]; ok {
		t.Fatalf("an unconfigured hub served an analytics block to an anonymous visitor: %s", rec.Body)
	}

	// Configured: exactly two fields, the host defaulted by Endpoint.
	srv.Analytics = AnalyticsConfig{Key: "phc_public"}
	rec = sec8Do(t, h, "GET", "/api/config", nil, nil, nil)
	json.Unmarshal(rec.Body.Bytes(), &out)
	block, ok := out["analytics"].(map[string]any)
	if !ok {
		t.Fatalf("configured hub served no analytics block: %s", rec.Body)
	}
	if len(block) != 2 || block["key"] != "phc_public" || block["host"] != DefaultAnalyticsHost {
		t.Fatalf("analytics block is not exactly {key, host}: %#v", block)
	}
	if srv.Analytics.Endpoint() != DefaultAnalyticsHost {
		t.Fatalf("Endpoint() did not default: %q", srv.Analytics.Endpoint())
	}
	srv.Analytics.Host = "https://eu.i.posthog.com"
	if srv.Analytics.Endpoint() != "https://eu.i.posthog.com" {
		t.Fatalf("Endpoint() dropped the configured host: %q", srv.Analytics.Endpoint())
	}
}

// /store/exists is a one-bit answer about the storage backend, reachable by
// any read member, so what it may answer ABOUT is the whole question. Two
// properties: the key never leaves the project's own prefix (a blob present in
// another org's project must read as absent), and a key that is not a store
// key is refused before it reaches storage at all.
func TestSec_Row5_StoreExistsAnswersOnlyAboutItsOwnProject(t *testing.T) {
	h, _, c, p := permHub(t)

	// alice puts a file in her project; the content address is now real.
	content := "a file only this project has"
	sec8Seed(t, h, p, c["alice"], "only-here.md", content)
	sum := sha256.Sum256([]byte(content))
	sha := hex.EncodeToString(sum[:])

	if rec := sec8Do(t, h, "GET", "/api/p/"+p.ID+"/store/exists?key=blobs/"+sha, nil, c["alice"], nil); rec.Code != 200 ||
		!strings.Contains(rec.Body.String(), "true") {
		t.Fatalf("control: the project's own blob did not read as present: %d %s", rec.Code, rec.Body)
	}

	// dave, in a different org, makes a project of his own. The same content
	// address must read as absent there — Prefixed is the only thing between
	// the two, and this route asks storage directly.
	rec := doAs(t, h, "POST", "/api/projects", map[string]string{"name": "daves"}, c["dave"])
	if rec.Code != 200 {
		t.Fatalf("dave create: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Project Project `json:"project"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)

	rec = sec8Do(t, h, "GET", "/api/p/"+out.Project.ID+"/store/exists?key=blobs/"+sha, nil, c["dave"], nil)
	if rec.Code != 200 {
		t.Fatalf("dave's own project: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "true") {
		t.Fatalf("/store/exists answered about another org's content: %s", rec.Body)
	}

	// Keys that are not store keys never reach the backend.
	for _, key := range []string{
		"", "blobs/", "blobs/../../etc/passwd", "../" + p.ID + "/blobs/" + sha,
		"journal/../../x.jsonl", "blobs/" + strings.ToUpper(sha), "blobs/" + sha + "/x",
		"journal/" + strings.Repeat("a", 65) + ".jsonl", "journal/a b.jsonl",
	} {
		rec := sec8Do(t, h, "GET", "/api/p/"+p.ID+"/store/exists?key="+url.QueryEscape(key), nil, c["alice"], nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("/store/exists?key=%q: %d %s, want 400", key, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}

	// And an outsider gets nothing at all.
	if rec := sec8Do(t, h, "GET", "/api/p/"+p.ID+"/store/exists?key=blobs/"+sha, nil, c["dave"], nil); rec.Code != http.StatusForbidden {
		t.Fatalf("outsider on /store/exists: %d, want 403", rec.Code)
	}
}
