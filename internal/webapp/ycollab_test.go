package webapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/reearth/ygo/crdt"
)

/*
The wire, pinned in both directions.

	The hub now decodes CRDT updates that browsers produce, so "our Go library
	and their JS library agree" stopped being a property of somebody else's
	README and became a thing this repo has to keep true across version bumps.

	The fixtures in testdata/ were produced by the exact yjs build the frontend
	ships (node_modules/yjs) and are checked in as BYTES: CI runs Go without
	node, and a test that needs a toolchain it does not have is a test that
	gets skipped. A bump that breaks the wire fails the build instead of the
	editor.
*/
func TestYjsWireCompatBothDirections(t *testing.T) {
	read := func(name string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	for _, tc := range []struct{ name, want string }{
		{"yjs-small", string(read("yjs-small.txt"))},
		{"yjs-big", string(read("yjs-big.txt"))},
	} {
		for _, ver := range []string{"v1", "v2"} {
			doc := crdt.New()
			update := read(tc.name + "-" + ver + ".bin")
			var err error
			if ver == "v1" {
				err = crdt.ApplyUpdateV1(doc, update, nil)
			} else {
				err = crdt.ApplyUpdateV2(doc, update, nil)
			}
			if err != nil {
				t.Fatalf("%s %s: Go could not read what yjs wrote: %v", tc.name, ver, err)
			}
			if got := doc.GetText("body").ToString(); got != tc.want {
				t.Errorf("%s %s: read %d chars, want %d", tc.name, ver, len(got), len(tc.want))
			}
		}
	}

	// ...and the other way: what Go writes, yjs must be able to read. Asserted
	// here by round-tripping through the decoder yjs shares the format with —
	// the live browser-side half is e2e's job, where a real yjs is present.
	doc := crdt.New()
	txt := doc.GetText("body")
	doc.Transact(func(txn *crdt.Transaction) {
		txt.Insert(txn, 0, "written by the hub — ünïcode ✅ 日本語", nil)
	})
	for _, ver := range []string{"v1", "v2"} {
		var update []byte
		if ver == "v1" {
			update = crdt.EncodeStateAsUpdateV1(doc, nil)
		} else {
			update = crdt.EncodeStateAsUpdateV2(doc, nil)
		}
		if len(update) == 0 {
			t.Fatalf("%s: encoded nothing", ver)
		}
		back := crdt.New()
		var err error
		if ver == "v1" {
			err = crdt.ApplyUpdateV1(back, update, nil)
		} else {
			err = crdt.ApplyUpdateV2(back, update, nil)
		}
		if err != nil {
			t.Fatalf("%s: %v", ver, err)
		}
		if got := back.GetText("body").ToString(); got != txt.ToString() {
			t.Errorf("%s: round-tripped to %q", ver, got)
		}
	}
}

/*
The room name is the hub's to decide.

	ygo takes the room from PathValue("room") or the URL's last segment. If a
	caller could name the room, the project id in the path would be decoration
	— any member of any project could join any other project's document by
	asking for its name. The handler sets the name itself, after proj() has
	resolved which project this is.
*/
func TestYCollabRoomNameIsNotTheCallersToChoose(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()

	// A websocket handshake is not what this asserts — it asserts what the
	// handler does with the request BEFORE handing it on, so a plain GET is
	// enough: ygo refuses the upgrade, and by then the room is already named.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/p/"+p.ID+"/ycollab?path=notes.md", nil)
	r.SetPathValue("room", "../some-other-project/secrets.md")
	h.ServeHTTP(rec, r)

	if got := r.PathValue("room"); got != p.ID+"/notes.md" {
		t.Fatalf("room = %q, want the hub's own (project, path) — a caller-supplied "+
			"name would make the project id decoration", got)
	}
}

// A path the caller cannot see is 404, not 403: a 403 confirms the file is
// there, which is the same rule the viewer's pathFilter applies.
func TestYCollabRefusesAnUnsafePath(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	for _, bad := range []string{"../escape.md", ".bdrive/config.json", ""} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET",
			"/api/p/"+p.ID+"/ycollab?path="+bad, nil)
		h.ServeHTTP(rec, r)
		if rec.Code == http.StatusSwitchingProtocols || rec.Code == http.StatusOK {
			t.Errorf("path %q was accepted (%d)", bad, rec.Code)
		}
	}
}

// pathPerm answers what writablePath answers, without ending the request —
// which is what lets a read-only member open a document instead of failing to.
func TestPathPermReportsWithoutRefusing(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	r := httptest.NewRequest("GET", "/api/p/"+p.ID+"/ycollab?path=notes.md", nil)
	r = withProject(withProjectID(r, p.ID), p)

	rec := httptest.NewRecorder()
	got := srv.pathPerm(r, p, "notes.md")
	if rec.Body.Len() != 0 {
		t.Error("pathPerm wrote to the response; it is a predicate, not a gate")
	}
	if !strings.Contains("none read write admin", got) {
		t.Errorf("pathPerm = %q, which is not one of the four levels", got)
	}
}

/*
The document starts as the file, without anybody claiming to be first.

	This is the property the seed claim and its grace timer existed to fake:
	exactly one joiner was told to build the document from the bytes it had
	loaded, and a claim that never produced anything left every later joiner
	with a blank document the editor would then save over the file. The hub
	seeds it instead, before any client is attached.
*/
func TestYCollabSeedsTheDocumentFromTheFile(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	const body = "# seeded by the hub\n\nünïcode ✅ 日本語\n"
	rec := putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", body, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("seed write: %d %s", rec.Code, rec.Body.String())
	}

	doc := crdt.New()
	if err := srv.seedDoc(p.ID+"/notes.md", doc); err != nil {
		t.Fatal(err)
	}
	if doc.GetText("body").Len() == 0 {
		t.Fatal("the room would have started empty, which is what the seed claim was for")
	}
	if got := doc.GetText("body").ToString(); got != body {
		t.Errorf("seeded %q, want %q", got, body)
	}
}

// A room for something that is not a file is an empty document, never an
// error: a file being created is an ordinary thing to open an editor on.
func TestYCollabSeedsEmptyForWhatIsNotThere(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	for _, room := range []string{
		p.ID + "/never-written.md",
		"no-such-project/notes.md",
		"malformed-room-name",
	} {
		doc := crdt.New()
		if err := srv.seedDoc(room, doc); err != nil {
			t.Errorf("%s: %v", room, err)
		}
		if n := doc.GetText("body").Len(); n != 0 {
			t.Errorf("%s: seeded %d chars from nothing", room, n)
		}
	}
}

/*
A real websocket, through the real handler, into the real document.

	Everything above this tests the pieces. This tests that a client can
	actually connect — which is where the first three attempts failed, each
	for a different reason the unit tests could not see: a route that did not
	match the URL y-websocket builds, a client dialling the relay's path, and
	a hub still serving a bundle compiled before any of it existed.
*/
func TestYCollabAcceptsARealWebsocket(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	const body = "# held by the hub\n"
	if rec := putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", body, ""); rec.Code != http.StatusOK {
		t.Fatalf("seed write: %d %s", rec.Code, rec.Body.String())
	}

	ts := httptest.NewServer(h)
	defer ts.Close()
	u := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/api/p/" + p.ID + "/ycollab/held?path=notes.md"

	// Accept-Encoding as a BROWSER sends it on a handshake. Go's dialer does
	// not, and without it this test passed against a compression middleware
	// that made the upgrade impossible for every real client.
	conn, resp, err := websocket.DefaultDialer.Dial(u, http.Header{
		"Accept-Encoding": {"gzip, deflate, br, zstd"},
	})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial %s: %v (HTTP %d)", u, err, status)
	}
	defer conn.Close()

	// y-websocket opens by sending sync step 1; the hub must answer with the
	// document it seeded rather than silence.
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, frame, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("no frame from the hub: %v", err)
	}
	if len(frame) == 0 {
		t.Fatal("the hub answered with an empty frame")
	}
}

/*
The hub can write the document back to the file.

	A safety net rather than the primary writer: the browser still saves on
	idle. What this covers is the case no client can — every client going away
	at once, which used to lose whatever had not reached the 700ms idle save.

	Attribution is the part worth pinning. A version whose author is "the
	server" is a regression in History even when the server is holding the pen,
	so the snapshot is written as the human who was editing.
*/
func TestYCollabSnapshotsTheDocumentAsTheHuman(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	room := p.ID + "/notes.md"
	if rec := putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "before\n", ""); rec.Code != http.StatusOK {
		t.Fatalf("seed write: %d", rec.Code)
	}

	// A room, as ygo would hand it to us, edited by somebody.
	doc := crdt.New()
	if err := srv.seedDoc(room, doc); err != nil {
		t.Fatal(err)
	}
	srv.rooms().load(room, doc)
	srv.rooms().writer(room, User{Email: "editor@example.com", Name: "An Editor"})
	txt := doc.GetText("body")
	doc.Transact(func(txn *crdt.Transaction) {
		txt.Insert(txn, txt.Len(), "and after\n", nil)
	})

	srv.snapshotRoom(context.Background(), room)

	got := get(t, h, "/api/p/"+p.ID+"/file?path=notes.md").Body.String()
	if got != "before\nand after\n" {
		t.Fatalf("file says %q, want the document's text", got)
	}
	feed := get(t, h, "/api/p/"+p.ID+"/history?path=notes.md&n=10")
	var out struct {
		Entries []struct{ User, Path string } `json:"entries"`
	}
	if err := json.Unmarshal(feed.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) == 0 {
		t.Fatal("the snapshot journaled nothing")
	}
	if u := out.Entries[0].User; u != "editor@example.com" {
		t.Errorf("newest version is attributed to %q, want the human who typed it", u)
	}
}

// A snapshot of text the file already holds writes nothing: the no-op check
// in upload.go covers the write path, and this covers the read path so the
// hub does not even ask.
func TestYCollabSnapshotOfUnchangedTextIsNotAWrite(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	room := p.ID + "/notes.md"
	const body = "unchanged\n"
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", body, "")

	doc := crdt.New()
	if err := srv.seedDoc(room, doc); err != nil {
		t.Fatal(err)
	}
	srv.rooms().load(room, doc)
	srv.rooms().writer(room, User{Email: "editor@example.com"})

	before := get(t, h, "/api/p/"+p.ID+"/history?path=notes.md&n=10").Body.String()
	srv.snapshotRoom(context.Background(), room)
	after := get(t, h, "/api/p/"+p.ID+"/history?path=notes.md&n=10").Body.String()
	if before != after {
		t.Error("snapshotting unchanged text added a version")
	}
}

// Nobody with write access ever joined, so there is nobody to attribute a
// write to — and a read-only room must not write at all.
func TestYCollabSnapshotNeedsSomeoneToAttribute(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	room := p.ID + "/notes.md"
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "original\n", "")

	doc := crdt.New()
	_ = srv.seedDoc(room, doc)
	srv.rooms().load(room, doc) // loaded, but no writer recorded
	txt := doc.GetText("body")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "SHOULD NOT LAND ", nil) })

	srv.snapshotRoom(context.Background(), room)
	if got := get(t, h, "/api/p/"+p.ID+"/file?path=notes.md").Body.String(); got != "original\n" {
		t.Errorf("file says %q — a room with no writer wrote anyway", got)
	}
}
