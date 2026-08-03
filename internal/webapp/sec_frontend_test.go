package webapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Round 11 — the hub's React frontend, and the server data it renders.
//
// Ten rounds have attacked the hub's HTTP boundaries. Row 16 covers the
// SHELL (framing, sniffing, cache), never the application. These tests come
// at the frontend from the server side: for every place the browser turns
// server bytes into a document, an identity, or a filename, the assertion is
// on the door that produced them — because that is where a fix belongs and
// where a Go test can hold it forever.
//
// Everything here is written from a payload the frontend actually reaches:
//   - FileView opens a same-origin URL for anything the app can't render
//     itself, and markdown links to /api/... are followed natively
//     (handleLinkClick leaves any href starting with "/" to the browser).
//   - HistoryRow renders `user`/`user_name` through whoChanged() as THE
//     answer to "who changed this file?".
//   - FolderListing / FileTree / Breadcrumbs / HistoryRow render `path` as
//     text, exactly as a terminal renders it — and `cmd/bdrive`'s safeField
//     already refuses to.
//
// helper prefix: sec11fe

// sec11feBlob stores content and returns its sha.
func sec11feBlob(t *testing.T, h http.Handler, id, content string, c *http.Cookie) string {
	t.Helper()
	return secpathStoreBlob(t, h, id, content, c)
}

// sec11feGet performs an authenticated GET.
func sec11feGet(t *testing.T, h http.Handler, url string, c *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", url, nil)
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// sec11feMarkup reports whether a Content-Type names a document format the
// browser parses as MARKUP in a top-level navigation — the property that
// makes it a script-execution vehicle on whatever origin served it. The XML
// family qualifies: an XML document carries <?xml-stylesheet type="text/xsl"?>
// and the XSLT result is HTML in the SAME origin, so an attacker-controlled
// .xml is an attacker-controlled HTML document on the hub.
func sec11feMarkup(ct string) bool {
	ct = strings.ToLower(ct)
	for _, m := range []string{"text/html", "xhtml", "svg", "/xml", "+xml"} {
		if strings.Contains(ct, m) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 1. Inline markup the wall does not cover: the XML family.
// ---------------------------------------------------------------------------

// sandboxInline walls off "text/html", "image/svg" and anything containing
// "xhtml". Round 10's test loops over .html/.htm/.svg/.xhtml only, so the
// rule it measures is the list, not the property — and the XML family is
// outside the list while having exactly the property.
//
// An .xml file is a document. A document may carry
//
//	<?xml-stylesheet type="text/xsl" href="theme.xml"?>
//
// and the browser applies that XSLT (the stylesheet is same-origin — the
// attacker uploads it to the same project) and renders its OUTPUT, which is
// HTML, in the hub's own origin, with the reader's session cookie. That is
// the identical capability round 10 already refused for .html and .svg: "any
// member who can write one .html file has script running on the hub origin".
//
// Reachable from the app without leaving it: a synced markdown document may
// link to /api/p/<id>/file?path=report.xml, and FileView's handleLinkClick
// leaves every href starting with "/" to the browser — a same-tab, top-level
// navigation to the hub origin.
func TestSec_Frontend_InlineXMLIsWalledOffLikeEveryOtherMarkup(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	const doc = `<?xml version="1.0"?><?xml-stylesheet type="text/xsl" href="theme.xml"?><r/>`
	sha := sec11feBlob(t, h, p.ID, doc, nil)
	if rec := secpathPushOp(t, h, p.ID, "dev1", secpathPutOp("report.xml", sha, len(doc)), nil); rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}

	// Control: the door DOES know how to wall markup off — the same bytes
	// under a .html name come back sandboxed. So a bare CSP below is a
	// decision about the content type, not a route that never sandboxes.
	ctl := sec11feGet(t, h, "/api/p/"+p.ID+"/blob?sha="+sha+"&name=x.html", nil)
	if ctl.Code != 200 || ctl.Header().Get("Content-Security-Policy") != "sandbox allow-scripts" {
		t.Fatalf("control x.html: %d CSP=%q — expected the wall to be present here",
			ctl.Code, ctl.Header().Get("Content-Security-Policy"))
	}

	// Every door that serves stored bytes inline.
	urls := map[string]string{
		"live file":        "/api/p/" + p.ID + "/file?path=report.xml",
		"historical blob":  "/api/p/" + p.ID + "/blob?sha=" + sha + "&name=report.xml",
		"blob as .xsl":     "/api/p/" + p.ID + "/blob?sha=" + sha + "&name=theme.xsl",
		"blob as .rss":     "/api/p/" + p.ID + "/blob?sha=" + sha + "&name=feed.rss",
		"blob as .atom":    "/api/p/" + p.ID + "/blob?sha=" + sha + "&name=feed.atom",
		"blob as .rdf":     "/api/p/" + p.ID + "/blob?sha=" + sha + "&name=meta.rdf",
		"blob as .xslt":    "/api/p/" + p.ID + "/blob?sha=" + sha + "&name=t.xslt",
		"blob as .svg":     "/api/p/" + p.ID + "/blob?sha=" + sha + "&name=d.svg",
		"blob as .xhtml":   "/api/p/" + p.ID + "/blob?sha=" + sha + "&name=d.xhtml",
		"blob as .plist":   "/api/p/" + p.ID + "/blob?sha=" + sha + "&name=d.plist",
		"blob as .xml.txt": "/api/p/" + p.ID + "/blob?sha=" + sha + "&name=d.xml.txt",
	}
	for name, u := range urls {
		t.Run(name, func(t *testing.T) {
			rec := sec11feGet(t, h, u, nil)
			if rec.Code != 200 {
				return // not served inline at all — fine
			}
			ct := rec.Header().Get("Content-Type")
			if !sec11feMarkup(ct) {
				return // the browser will not parse it as a document
			}
			if csp := rec.Header().Get("Content-Security-Policy"); csp != "sandbox allow-scripts" {
				t.Errorf("%s serves attacker-written bytes as %s on the hub's own origin with "+
					"CSP %q — an XML document applies its own <?xml-stylesheet type=\"text/xsl\"?> "+
					"and the XSLT output is HTML in THIS origin, with the reader's session cookie. "+
					"sandboxInline's list covers text/html, image/svg and *xhtml* only; the "+
					"property it is walling off is \"the browser parses this as a document\", "+
					"which the whole XML family has.", u, ct, csp)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 2. History's answer to "who changed this file?" is the client's own claim.
// ---------------------------------------------------------------------------

// A journal op carries User/UserName. The hub knows, on every push, exactly
// which account is pushing (the session) and which account owns the device id
// in the key (DeviceRegistry — round 8 made that binding load-bearing, and it
// is checked on this very request). It nonetheless serves the op's OWN
// User/UserName back through /history, and the frontend renders them as the
// actor: whoChanged() prints `${user_name} <${user}>` in every HistoryRow,
// the run card header, and the file view's meta line.
//
// So bob writes a file and history tells every teammate alice wrote it. That
// is the audit surface — the one place a hub can answer "who did this" — and
// the answer is written by the party being audited.
func TestSec_History_AnOpCannotNameAnotherAccountAsItsAuthor(t *testing.T) {
	h, _, ck, p := permHub(t)
	secRegisterDevice(t, h, p.ID, ck["bob"], "bobdev", "bobs-mac", "darwin")
	sha := sec11feBlob(t, h, p.ID, "payload", ck["bob"])

	// Control: bob's own push is attributed to bob — so a wrong answer below
	// is the forgery landing, not history failing to attribute anything.
	if rec := secpathPushOp(t, h, p.ID, "bobdev", map[string]any{
		"seq": 1, "lamport": 1, "time": "2026-01-01T00:00:00Z",
		"kind": "put", "path": "honest.md", "blob": sha, "size": 7,
		"user": "bob@x.io", "user_name": "Bob",
	}, ck["bob"]); rec.Code != 200 {
		t.Fatalf("control push: %d %s", rec.Code, rec.Body)
	}
	if got := sec11feAuthorOf(t, h, p.ID, "honest.md", ck["alice"]); got.User != "bob@x.io" {
		t.Fatalf("control: history attributes bob's own change to %q, want bob@x.io", got.User)
	}

	// The attack: the same device, the same session, an op that names alice.
	if rec := secpathPushOp(t, h, p.ID, "bobdev", map[string]any{
		"seq": 2, "lamport": 2, "time": "2026-01-02T00:00:00Z",
		"kind": "put", "path": "policy.md", "blob": sha, "size": 7,
		"user": "alice@x.io", "user_name": "Alice", "author": "alice",
		"note": "signed off by alice",
	}, ck["bob"]); rec.Code != 200 {
		// Refusing the push outright is a perfectly good answer.
		return
	}
	got := sec11feAuthorOf(t, h, p.ID, "policy.md", ck["alice"])
	if got.User == "alice@x.io" || got.UserName == "Alice" {
		t.Errorf("bob pushed the op and /history reports the author as %q <%q>: the hub knows the "+
			"pushing session (bob@x.io) and the account that owns device %q, and serves the op's "+
			"own claim anyway. whoChanged() renders exactly these two fields as the actor in every "+
			"HistoryRow, so the audit log names whoever the audited party typed.",
			got.UserName, got.User, "bobdev")
	}
}

type sec11feEntry struct {
	Path     string `json:"path"`
	User     string `json:"user"`
	UserName string `json:"user_name"`
	Author   string `json:"author"`
	Note     string `json:"note"`
	Device   struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"device"`
}

// sec11feAuthorOf reads the history entry for one path.
func sec11feAuthorOf(t *testing.T, h http.Handler, project, path string, c *http.Cookie) sec11feEntry {
	t.Helper()
	rec := sec11feGet(t, h, "/api/p/"+project+"/history?path="+path, c)
	if rec.Code != 200 {
		t.Fatalf("history %s: %d %s", path, rec.Code, rec.Body)
	}
	var out struct {
		Entries []sec11feEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) == 0 {
		t.Fatalf("history %s: no entries", path)
	}
	return out.Entries[0]
}

// ---------------------------------------------------------------------------
// 3. The row-reordering characters, at the door the web UI reads from.
// ---------------------------------------------------------------------------

// journal.SafePath refuses C0 and DEL because "DEL and the C0s render as
// nothing, so notes\x7f.md and notes.md are two indistinguishable entries in
// one tree". Two other families do exactly that and are not refused:
//
//   - the bidi format controls (U+202A..U+202E, U+2066..U+2069, U+200E/F,
//     U+061C) — Trojan Source, CVE-2021-42574. "invoice<RLO>gnp.exe" renders
//     as "invoiceexe.png" in every FolderListing row, FileTree node,
//     Breadcrumb and HistoryRow, and downloads as an .exe.
//   - C1 (U+0080..U+009F), which every C0 filter misses.
//
// Both are already refused for a PROJECT NAME (projects.go trimName, "the
// bidi overrides that reorder a rendered row") and scrubbed out of `bdrive
// log` (cmd/bdrive safeField, round 10). A path travels further than either:
// it is a row in the web UI, a filename on every synced device, and the
// subject line of every history entry.
func TestSec_Frontend_APathCannotCarryTheControlsThatReorderARow(t *testing.T) {
	h, _, ck, p := permHub(t)
	secRegisterDevice(t, h, p.ID, ck["bob"], "bobdev", "bobs-mac", "darwin")
	sha := sec11feBlob(t, h, p.ID, "payload", ck["bob"])

	// Control: a perfectly ordinary path is accepted on both doors, so a
	// refusal below is about the character.
	if rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/upload/commit",
		map[string]any{"path": "ok.md", "sha256": sha, "size": 7}, ck["bob"]); rec.Code != 200 {
		t.Fatalf("control commit: %d %s", rec.Code, rec.Body)
	}

	bad := []struct{ name, path string }{
		{"RLO U+202E", "invoice\u202egnp.exe"},
		{"LRO U+202D", "invoice\u202dgnp.exe"},
		{"RLI U+2067", "invoice\u2067gnp.exe"},
		{"PDI U+2069", "invoice\u2069gnp.exe"},
		{"LRM U+200E", "invoice\u200egnp.exe"},
		{"ALM U+061C", "invoice\u061cgnp.exe"},
		{"C1 CSI U+009B", "notes\u009b2Jx.md"},
		{"C1 NEL U+0085", "notes\u0085x.md"},
	}
	for i, b := range bad {
		t.Run("commit/"+b.name, func(t *testing.T) {
			rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/upload/commit",
				map[string]any{"path": b.path, "sha256": sha, "size": 7}, ck["bob"])
			if rec.Code == 200 {
				t.Errorf("upload/commit accepted %q: it renders in the file listing as something "+
					"else entirely and downloads under a different extension. The same rune is "+
					"already stripped from a project name (trimName) and from `bdrive log` "+
					"(safeField) — the path is the field that reaches the most surfaces and the "+
					"only one that never checks.", b.path)
			}
		})
		t.Run("journal/"+b.name, func(t *testing.T) {
			rec := secpathPushOp(t, h, p.ID, "bobdev", map[string]any{
				"seq": i + 2, "lamport": i + 2, "time": "2026-01-01T00:00:00Z",
				"kind": "put", "path": b.path, "blob": sha, "size": 7,
			}, ck["bob"])
			if rec.Code == 200 {
				t.Errorf("/store/object journalled %q: journal.SafePath refuses C0 and DEL for "+
					"exactly this reason (\"two indistinguishable entries in one tree\") and lets "+
					"the bidi overrides and every C1 through. Every device materializes this "+
					"filename on disk.", b.path)
			}
		})
	}
}

// The note is the other free-text field history renders (HistoryRow's
// NoteText, and the run-card header). `bdrive log` scrubs C0/C1/bidi out of
// it before printing — "the audit tool an operator uses to catch a peer must
// not be renderable BY that peer" — and the web History view, which is the
// audit tool everybody actually uses, gets it raw.
func TestSec_Frontend_ANoteCannotCarryTheControlsThatReorderARow(t *testing.T) {
	h, _, ck, p := permHub(t)
	secRegisterDevice(t, h, p.ID, ck["bob"], "bobdev", "bobs-mac", "darwin")
	sha := sec11feBlob(t, h, p.ID, "payload", ck["bob"])

	// Control: an ordinary note survives the round trip, so an absent one
	// below means it was refused or scrubbed, not that notes are dropped.
	if rec := secpathPushOp(t, h, p.ID, "bobdev", map[string]any{
		"seq": 1, "lamport": 1, "time": "2026-01-01T00:00:00Z",
		"kind": "put", "path": "a.md", "blob": sha, "size": 7, "note": "ordinary note",
	}, ck["bob"]); rec.Code != 200 {
		t.Fatalf("control push: %d %s", rec.Code, rec.Body)
	}
	if got := sec11feAuthorOf(t, h, p.ID, "a.md", ck["alice"]); got.Note != "ordinary note" {
		t.Fatalf("control: note came back %q", got.Note)
	}

	const evil = "cleanup‮ 2K deleted nothing"
	if rec := secpathPushOp(t, h, p.ID, "bobdev", map[string]any{
		"seq": 2, "lamport": 2, "time": "2026-01-02T00:00:00Z",
		"kind": "put", "path": "b.md", "blob": sha, "size": 7, "note": evil,
	}, ck["bob"]); rec.Code != 200 {
		return // refusing the push is a fine answer
	}
	if got := sec11feAuthorOf(t, h, p.ID, "b.md", ck["alice"]); strings.ContainsAny(got.Note, "‮") {
		t.Errorf("/history served the note %q verbatim: the bidi override reorders the rest of "+
			"the row in the History view and the C1 is CSI to any terminal reading the same JSON. "+
			"cmd/bdrive's safeField already refuses both on the way to a terminal; the hub serves "+
			"them to every browser.", got.Note)
	}
}
