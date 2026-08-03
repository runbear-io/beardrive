package webapp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Round 12, attacking round 11's own fixes.
//
// Everything here is prefixed secfx11 so it cannot collide with the other
// sec_* files in this package.

// secfx11Op builds one journal line from an explicit field map, so a test can
// set the fields secfx4OpLine/secaudOpLine do not expose — author, user,
// user_name, note — which is exactly where round 11's attribution fix stops.
func secfx11Op(extra map[string]any) string {
	m := map[string]any{
		"seq": 1, "lamport": 1, "time": time.Now().UTC().Format(time.RFC3339Nano),
		"kind": "put", "path": "plan.md", "size": 1,
		"blob": strings.Repeat("a", 64),
	}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b) + "\n"
}

// secfx11BoundDevice signs a device in for one account the way the hub does
// (secRegisterDevice) and returns its id, so ownJournal resolves an owner and
// opsNameTheirAuthor is actually reachable.
func secfx11BoundDevice(t *testing.T, h http.Handler, p Project, c *http.Cookie, id string) string {
	t.Helper()
	if rec := secRegisterDevice(t, h, p.ID, c, id, "machine-"+id, "darwin/arm64"); rec.Code != 200 {
		t.Fatalf("registering device %s: %d %s", id, rec.Code, rec.Body)
	}
	return id
}

// secfx11History returns the history feed for a project as the given user.
func secfx11History(t *testing.T, h http.Handler, p Project, c *http.Cookie) []HistoryEntry {
	t.Helper()
	rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/history?prefix=", nil, c)
	if rec.Code != 200 {
		t.Fatalf("history: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Entries []HistoryEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("history body %q: %v", rec.Body.String(), err)
	}
	return out.Entries
}

// ---------------------------------------------------------------------------
// 1. opsNameTheirAuthor binds Op.User. History displays Op.Author.
// ---------------------------------------------------------------------------

// Round 11 closed "/history named whoever the pushing device typed" by making
// a push refuse an op whose User is not the account the pushing device is
// registered to. It refuses ONLY on User: an op that names nobody is allowed
// through explicitly ("journals from before accounts existed have no User and
// History falls back to Author").
//
// Author is peer-written free text and the frontend's whoChanged() renders it
// as THE answer to "who changed this file?" whenever user/user_name are empty
// (util.ts: `e.user_name ? ... : e.user || e.author || "unknown"`). So the
// attack is one field over: leave user and user_name empty, put the victim in
// author, and the audit surface credits the victim for the attacker's change.
//
// The secure behaviour asserted here: when the hub KNOWS which account owns
// the pushing device, the row it serves must not attribute the change to a
// different account of this hub. Refusing the push or dropping the forged
// author both satisfy it.
func TestSec_History_APushCannotCreditAnotherAccountThroughTheAuthorField(t *testing.T) {
	h, _, c, p := permHub(t)
	bobDev := secfx11BoundDevice(t, h, p, c["bob"], "devbob01")

	// Control: bob's own honest push, naming his own account, is accepted.
	ok := secfx11Op(map[string]any{
		"device": bobDev, "path": "bob-plan.md",
		"user": "bob@x.io", "user_name": "Bob",
	})
	if rec := secfx4PushJournal(t, h, p.ID, bobDev, ok, c["bob"]); rec.Code != 200 {
		t.Fatalf("control: bob's honest push: %d %s", rec.Code, rec.Body)
	}

	// The attack: same device, same account, nobody named in user/user_name —
	// which round 11 lets through — and alice named in author.
	forged := secfx11Op(map[string]any{
		"seq": 2, "lamport": 2, "device": bobDev, "path": "quarterly-plan.md",
		"author": "Alice <alice@x.io>",
	})
	rec := secfx4PushJournal(t, h, p.ID, bobDev, forged, c["bob"])
	if rec.Code == http.StatusForbidden || rec.Code == http.StatusBadRequest {
		return // refused at the door: secure
	}
	if rec.Code != 200 {
		t.Fatalf("unexpected push result: %d %s", rec.Code, rec.Body)
	}
	for _, e := range secfx11History(t, h, p, c["alice"]) {
		if e.Path != "quarterly-plan.md" {
			continue
		}
		// whoChanged(e) = user_name ? `${user_name} <${user}>` : user || author
		shown := e.UserName
		if shown == "" {
			shown = e.User
		}
		if shown == "" {
			shown = e.Author
		}
		if strings.Contains(strings.ToLower(shown), "alice") {
			t.Fatalf("history credits %q for a change pushed by bob's device %s.\n"+
				"opsNameTheirAuthor compares Op.User only, and explicitly allows an op that names\n"+
				"nobody; Op.Author is unchecked peer text and whoChanged() falls back to it, so the\n"+
				"round-11 attribution fix is bypassed by leaving user/user_name empty.\n"+
				"entry: user=%q user_name=%q author=%q", shown, bobDev, e.User, e.UserName, e.Author)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. journalOps checks Path and Note with SafeText. Not author/user_name.
// ---------------------------------------------------------------------------

// Round 11 split journal.SafeText out of SafePath and applied it to Op.Note,
// on the stated grounds that "the note is the other peer-written free text
// History renders, right next to the path".
//
// It is not the other one, it is one of three. Op.Author and Op.UserName are
// peer-written, are rendered in the same row by the same helper (whoChanged),
// and are checked by nothing. A right-to-left override in user_name reorders
// the whole rendered row — the Trojan Source shape SafeText exists to refuse
// — and a C0 run in author is the "renders as nothing" shape its own doc
// comment names.
func TestSec_Store_AJournalsAuthorFieldsAreCheckedLikeItsNote(t *testing.T) {
	h, _, c, p := permHub(t)
	bobDev := secfx11BoundDevice(t, h, p, c["bob"], "devbob02")

	// Control: the same character in the note IS refused, so the door works
	// and the difference below is the field, not the fixture.
	note := secfx11Op(map[string]any{
		"device": bobDev, "path": "n.md", "note": "conflict\u202ecopy",
	})
	if rec := secfx4PushJournal(t, h, p.ID, bobDev, note, c["bob"]); rec.Code != http.StatusBadRequest {
		t.Fatalf("control: a bidi override in the NOTE should be refused: %d %s", rec.Code, rec.Body)
	}

	for _, tc := range []struct {
		field string
		value string
		why   string
	}{
		{"author", "Alice\u202eeslaF", "bidi override in author"},
		{"author", "Alice\x1b[2Kx", "C0 escape in author"},
		{"user_name", "Bob\u202egnp.exe", "bidi override in user_name"},
		{"user_name", "Bob\u0085\u009bx", "C1 control in user_name"},
	} {
		op := map[string]any{
			"device": bobDev, "path": "row-" + tc.field + ".md",
			tc.field: tc.value,
		}
		if tc.field == "user_name" {
			op["user"] = "bob@x.io" // so opsNameTheirAuthor is not the thing refusing
		}
		rec := secfx4PushJournal(t, h, p.ID, bobDev, secfx11Op(op), c["bob"])
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s accepted: %d %s\n"+
				"journalOps applies journal.SafeText to op.Note and journal.SafePath to op.Path and\n"+
				"nothing to op.Author / op.UserName — which HistoryRow renders in the same line.",
				tc.why, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
}

// ---------------------------------------------------------------------------
// 3. "nosniff goes on every stored-bytes door" — two doors it did not go on.
// ---------------------------------------------------------------------------

// Round 11's commit message states the rule: nosniff on every door that
// streams stored bytes. It landed on serveBlob (both arms) and on history's
// /blob when the request carries ?name=. It did not land on:
//
//   - GET /api/p/<id>/blob with no ?name= — the SAME bytes, same handler,
//     the else arm two lines below the one that got the header;
//   - GET /api/p/<id>/store/object?key=blobs/<sha> — the sync proxy, which is a
//     cookie-authenticated GET a member can hand another member as a URL.
//
// Both answer with a Content-Type the hub chose for bytes the attacker wrote,
// which is precisely the condition the header exists for.
func TestSec_Store_EveryStoredBytesDoorRefusesMIMESniffing(t *testing.T) {
	h, _, c, p := permHub(t)
	dev := secfx11BoundDevice(t, h, p, c["alice"], "devalice1")

	// Put a real blob through the store door so every reader below has bytes.
	body := "<html><body><script>fetch('/api/projects')</script></body></html>"
	sha := secfx11PutBlob(t, h, p, c["alice"], dev, body)

	// Control: the door round 11 did fix answers with nosniff.
	ctl := doAs(t, h, "GET", "/api/p/"+p.ID+"/blob?sha="+sha+"&name=x.html", nil, c["alice"])
	if ctl.Code != 200 || !strings.EqualFold(ctl.Header().Get("X-Content-Type-Options"), "nosniff") {
		t.Fatalf("control: /blob?name= should be 200 + nosniff, got %d %q",
			ctl.Code, ctl.Header().Get("X-Content-Type-Options"))
	}

	for _, tc := range []struct{ name, url string }{
		{"history /blob with no name", "/api/p/" + p.ID + "/blob?sha=" + sha},
		{"the sync proxy GET /store/object", "/api/p/" + p.ID + "/store/object?key=blobs/" + sha},
	} {
		rec := doAs(t, h, "GET", tc.url, nil, c["alice"])
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", tc.name, rec.Code, rec.Body)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); !strings.EqualFold(got, "nosniff") {
			t.Errorf("%s (%s): X-Content-Type-Options = %q, want nosniff — "+
				"a stored-bytes door serving attacker-written content with no sniffing wall",
				tc.name, tc.url, got)
		}
	}
}

func secfx11Sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// secfx11PutBlob stores one blob through the store proxy and returns its sha.
func secfx11PutBlob(t *testing.T, h http.Handler, p Project, c *http.Cookie, dev, content string) string {
	t.Helper()
	sum := secfx11Sha(content)
	rec := secfx4Store(t, h, "PUT", "/api/p/"+p.ID+"/store/object?key=blobs/"+sum, content, c, dev)
	if rec.Code != 200 {
		t.Fatalf("store blob: %d %s", rec.Code, rec.Body)
	}
	return sum
}

// ---------------------------------------------------------------------------
// 4/5. The row-scoped write landed on projects only.
// ---------------------------------------------------------------------------

// Round 11's finding: "a revoked grant was restored by any unrelated write
// from a second hub process". The fix is rowScopedProjectRepo plus a re-read
// before every project write — ProjectRepo only.
//
// OrgRepo has the identical shape and did not move: fileOrgRepo.PutOrg writes
// the caller's whole Members map with no re-read, and sqlOrgRepo.PutOrg
// DELETEs every org_members row for the org and re-inserts from the same
// stale map. Org membership is the outer wall — every per-project route 403s
// for a non-member — so this resurrects more than a grant does.
func TestSec_Meta_ASecondHubProcessCannotResurrectARevokedOrgMembership(t *testing.T) {
	dir := t.TempDir()
	t.Run("file", func(t *testing.T) {
		secfx11OrgResurrection(t, func() *OrgDB {
			db, err := OpenOrgDB(filepath.Join(dir, "orgs.json"))
			if err != nil {
				t.Fatal(err)
			}
			return db
		})
	})
	// Not a file-backend quirk: sqlOrgRepo.PutOrg DELETEs every org_members row
	// for the org and re-inserts the caller's whole map, so Postgres — the
	// backend you configure precisely because you run more than one hub
	// process — does the same thing.
	t.Run("sqlite", func(t *testing.T) {
		p := filepath.Join(dir, "meta.db")
		secfx11OrgResurrection(t, func() *OrgDB {
			s, err := OpenSQLStore("sqlite", p)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			db, err := NewOrgDB(s.Orgs())
			if err != nil {
				t.Fatal(err)
			}
			return db
		})
	})
}

func secfx11OrgResurrection(t *testing.T, open func() *OrgDB) {
	t.Helper()

	// Hub process A creates the org and the membership.
	a := open()
	org, err := a.Create("acme", "alice@x.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AddMember(org.ID, "bob@x.io", RoleMember); err != nil {
		t.Fatal(err)
	}

	// Hub process B starts and loads the same store — bob is a member.
	b := open()
	if b.Role(org.ID, "bob@x.io") != RoleMember {
		t.Fatal("fixture: B should see bob as a member")
	}

	// An admin removes bob on A.
	if err := a.RemoveMember(org.ID, "bob@x.io"); err != nil {
		t.Fatal(err)
	}

	// B then makes one unrelated org write — a rename, the most routine
	// operator action there is.
	if err := b.Rename(org.ID, "Acme Inc"); err != nil {
		t.Fatal(err)
	}

	// A third process (or the next restart) reads what is in the store.
	c := open()
	if got := c.Role(org.ID, "bob@x.io"); got != "" {
		t.Fatalf("bob is a %q of the org he was removed from: an unrelated rename by a second "+
			"hub process rewrote the whole member set from its stale copy.\n"+
			"Round 11 gave ProjectRepo PutPerm + a re-read before every write; OrgRepo has the "+
			"same whole-record shape on all three backends and did not move.", got)
	}
	// And the membership wall really is what this decides.
	if len(c.OrgsFor("bob@x.io")) != 0 {
		t.Errorf("OrgsFor still lists the org for a removed member — every project in it is "+
			"readable again: %v", c.OrgsFor("bob@x.io"))
	}
}

// Round 11's fix is a write-side fix only. rowScopedProjectRepo stops a second
// hub process from UNDOING a revocation on disk; nothing makes that process
// HONOUR it. ProjectDB loads byID once at open and never re-reads, and
// projectPerm answers straight out of that map (perms.go: `p, ok :=
// s.Projects.Get(projectID)` … `p.Perms[email]`).
//
// So in the deployment the fix's own comment names — "two hub processes in
// front of one database, which is the entire reason to configure Postgres" —
// an admin's revocation takes effect on the process that served the request
// and on no other, for as long as those processes live. The grant is gone from
// the store and still authorizes.
func TestSec_Perms_ASecondHubProcessHonoursARevokedGrant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projects.json")
	a, err := OpenProjectDB(path)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := a.GetOrCreate("wiki", "org1")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetPerm(p.ID, "alice@x.io", PermAdmin); err != nil {
		t.Fatal(err) // so bob is never the last admin and the guard is not the thing refusing
	}
	if err := a.SetPerm(p.ID, "bob@x.io", PermAdmin); err != nil {
		t.Fatal(err)
	}

	// Process B comes up and sees the grant, as it should.
	b, err := OpenProjectDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Get(p.ID); got.Perms["bob@x.io"] != PermAdmin {
		t.Fatalf("fixture: B should see bob's grant, got %q", got.Perms["bob@x.io"])
	}

	// An admin revokes it on A. This is the whole operation: "bob is no longer
	// an admin of this project."
	if err := a.ClearPerm(p.ID, "bob@x.io"); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Get(p.ID); got.Perms["bob@x.io"] != "" {
		t.Fatalf("fixture: the revoke did not take on A")
	}

	if got, _ := b.Get(p.ID); got.Perms["bob@x.io"] != "" {
		t.Fatalf("the second hub process still answers %q for a grant that was revoked: "+
			"ProjectDB.byID is loaded once at open and projectPerm reads it directly, so the "+
			"row-scoped WRITE round 11 added keeps the store correct while every other process "+
			"keeps authorizing from a copy taken at boot.", got.Perms["bob@x.io"])
	}
}

// And on the device registry, where the row that vanishes is the ONE ownership
// fact ownJournal consults. fileDeviceRepo.Put rewrites devices.json from a map
// loaded at open, so a second hub process's ordinary observation erases a
// binding the first one minted — and an id with no owning row is an id
// DeviceRegistry.Bind will hand to the next account that asks for it. That is
// the one-writer invariant ("each device writes only its own journal") lost to
// a routine write by a second process.
func TestSec_Devices_ASecondHubProcessCannotEraseADeviceBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	all := func(string) bool { return true }

	a, err := OpenDeviceRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	// A second hub process, up before the binding is minted.
	b, err := OpenDeviceRegistry(path)
	if err != nil {
		t.Fatal(err)
	}

	// Alice's machine signs in on process A: the id becomes hers, hub-wide.
	if err := a.Bind("alice@x.io", DeviceInfo{ID: "devalice9", Name: "alice-mbp",
		FirstSeen: time.Now().UTC(), LastSeen: time.Now().UTC()}, all); err != nil {
		t.Fatal(err)
	}
	if owner, _ := a.OwnerOf("devalice9"); owner != "alice@x.io" {
		t.Fatalf("fixture: OwnerOf = %q", owner)
	}

	// Process B does one ordinary, unrelated thing: it records its own,
	// different device — a /store/* refresh, the most common write there is.
	b.Observe(DeviceInfo{ID: "devbob9", User: "bob@x.io", Name: "bob-pc",
		FirstSeen: time.Now().UTC(), LastSeen: time.Now().UTC()})

	// Next restart.
	c, err := OpenDeviceRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if owner, _ := c.OwnerOf("devalice9"); owner != "alice@x.io" {
		t.Fatalf("after a restart alice's device is owned by %q, not her: an unrelated Observe "+
			"by a second hub process rewrote devices.json from a map loaded at open.", owner)
	}
	// The consequence: an unowned id is one Bind takes from her.
	if err := c.Bind("bob@x.io", DeviceInfo{ID: "devalice9", Name: "bob-pc"}, all); err == nil {
		if owner, _ := c.OwnerOf("devalice9"); owner == "bob@x.io" {
			t.Fatalf("bob now owns alice's device id, and with it her journal on every project")
		}
	}
}

// The same defect on shares, where the resurrected row is an UNAUTHENTICATED
// public URL: fileShareRepo.Put/Delete rewrite shares.json from an in-memory
// map loaded once at open. A revoked /s/<token> comes back the moment any
// second hub process mints any unrelated share.
func TestSec_Share_ASecondHubProcessCannotResurrectARevokedLink(t *testing.T) {
	srv, p, sharesPath, _, h := shareHub(t)

	tok, _ := authedShare(t, srv, h, p.ID, "wiki/report.html")
	if rec := doHTTP(h, httptestNewRequestBody("GET", "/s/"+tok, nil)); rec.Code != 200 {
		t.Fatalf("fixture: the fresh link should serve: %d", rec.Code)
	}

	// A second hub process in front of the same metadata.
	b, err := OpenShareDB(sharesPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Get(tok); !ok {
		t.Fatal("fixture: B should see the share")
	}

	// Revoked on this process.
	if !srv.Shares.Revoke(tok) {
		t.Fatal("revoke failed")
	}
	if rec := doHTTP(h, httptestNewRequestBody("GET", "/s/"+tok, nil)); rec.Code == 200 {
		t.Fatal("fixture: the link should be dead on this process")
	}

	// B mints an unrelated share of a different file.
	if _, err := b.Create(p.ID, "wiki/notes.md", "s@x.io", 0); err != nil {
		t.Fatal(err)
	}

	// Restart: reload the registry the way a hub does at boot.
	reloaded, err := OpenShareDB(sharesPath)
	if err != nil {
		t.Fatal(err)
	}
	srv.Shares = reloaded
	rec := doHTTP(h, httptestNewRequestBody("GET", "/s/"+tok, nil))
	if rec.Code == 200 {
		t.Fatalf("GET /s/%s serves the revoked file again to an anonymous stranger after a "+
			"restart: an unrelated Create by a second hub process rewrote shares.json from a map "+
			"loaded at open. fileShareRepo.Put/Delete never re-read; only ProjectRepo did in r11.",
			tok)
	}
	if _, ok := reloaded.Get(tok); ok {
		t.Fatalf("the revoked token is back in the registry on disk")
	}
}
