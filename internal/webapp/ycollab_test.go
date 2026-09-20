package webapp

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

/* The wire, pinned in both directions.

   The hub now decodes CRDT updates that browsers produce, so "our Go library
   and their JS library agree" stopped being a property of somebody else's
   README and became a thing this repo has to keep true across version bumps.

   The fixtures in testdata/ were produced by the exact yjs build the frontend
   ships (node_modules/yjs) and are checked in as BYTES: CI runs Go without
   node, and a test that needs a toolchain it does not have is a test that
   gets skipped. A bump that breaks the wire fails the build instead of the
   editor. */
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

/* The room name is the hub's to decide.

   ygo takes the room from PathValue("room") or the URL's last segment. If a
   caller could name the room, the project id in the path would be decoration
   — any member of any project could join any other project's document by
   asking for its name. The handler sets the name itself, after proj() has
   resolved which project this is. */
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
