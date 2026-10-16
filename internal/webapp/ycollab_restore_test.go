package webapp

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
)

/* A restore is a write the hub makes to a file whose room may be live. The
   room's document must take the restored text — once, as an edit its peers
   receive — and its base must move to the restored version, or the room's
   next snapshot writes the old text back or parks a conflict copy. */

// liveRoom opens a room the way a connecting browser does — through the
// ygo server, so OnLoadDocument seeds and watches it — and returns its doc.
func liveRoom(t *testing.T, srv *Server, room string, writer bool) *crdt.Doc {
	t.Helper()
	_ = srv.ydocs().Apply(context.Background(), room, func(*crdt.Doc, func(func(*crdt.Transaction))) {})
	doc, _, ok := srv.rooms().get(room)
	if !ok {
		t.Fatal("the room did not open")
	}
	if writer {
		srv.rooms().writer(room, User{Email: "editor@example.com", Name: "An Editor"})
	}
	t.Cleanup(func() { srv.rooms().drop(room) })
	return doc
}

type restoreOut struct {
	OK     bool `json:"ok"`
	Folded bool `json:"folded"`
}

func restoreVia(t *testing.T, h http.Handler, id, path, sha string) restoreOut {
	t.Helper()
	rec := do(t, h, "POST", "/api/p/"+id+"/restore", map[string]string{"path": path, "sha": sha})
	if rec.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	var out restoreOut
	mustJSON(t, rec, &out)
	return out
}

func conflictCopies(t *testing.T, h http.Handler, id string) int {
	return strings.Count(get(t, h, "/api/p/"+id+"/tree?slim=1").Body.String(), ".bdrive-conflict-")
}

func TestRestoreFoldsIntoALiveRoom(t *testing.T) {
	fastRoom(t, 100*time.Millisecond, time.Second)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	up := "/api/p/" + p.ID + "/upload/content?path=notes.md"
	url := "/api/p/" + p.ID + "/file?path=notes.md"
	putContent(t, h, up, "first version\n", "")
	putContent(t, h, up, "second version\n", "")
	room := p.ID + "/notes.md"
	doc := liveRoom(t, srv, room, true)

	if out := restoreVia(t, h, p.ID, "notes.md", shaOf("first version\n")); !out.Folded {
		t.Fatal("restore into a live room reported folded=false")
	}
	if got := doc.GetText("body").ToString(); got != "first version\n" {
		t.Fatalf("room document after restore = %q, want the restored text exactly once", got)
	}
	if b := srv.rooms().base(room); b != shaOf("first version\n") {
		t.Fatalf("room base = %q, want the restored sha", b)
	}

	typeInto(doc, "typed after\n")
	if !waitFile(t, h, url, "first version\ntyped after\n", 2*time.Second) {
		t.Fatalf("the room's next snapshot did not write restored+typing: file is %q", get(t, h, url).Body.String())
	}
	time.Sleep(300 * time.Millisecond)
	if n := conflictCopies(t, h, p.ID); n != 0 {
		t.Fatalf("restore into a live room parked %d conflict copies", n)
	}
	if strings.Contains(get(t, h, url).Body.String(), "second version") {
		t.Fatal("the pre-restore text came back")
	}
}

func TestRestoreWithNoRoomIsUnchanged(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	up := "/api/p/" + p.ID + "/upload/content?path=notes.md"
	putContent(t, h, up, "first version\n", "")
	putContent(t, h, up, "second version\n", "")

	if out := restoreVia(t, h, p.ID, "notes.md", shaOf("first version\n")); out.Folded {
		t.Fatal("restore with no live room reported folded=true")
	}
	if _, _, ok := srv.rooms().get(p.ID + "/notes.md"); ok {
		t.Fatal("restore created a room nobody opened")
	}
	if got := get(t, h, "/api/p/"+p.ID+"/file?path=notes.md").Body.String(); got != "first version\n" {
		t.Fatalf("file after restore = %q", got)
	}
}

// A room nobody with write access joined (readers only) still takes the
// restored text, and the hub writes nothing further on its behalf: the
// restore is the one new version.
func TestRestoreIntoAReaderOnlyRoomAddsNoWriter(t *testing.T) {
	fastRoom(t, 100*time.Millisecond, time.Second)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	up := "/api/p/" + p.ID + "/upload/content?path=notes.md"
	putContent(t, h, up, "first version\n", "")
	putContent(t, h, up, "second version\n", "")
	doc := liveRoom(t, srv, p.ID+"/notes.md", false)
	before := len(historyOf(t, h, "/api/p/"+p.ID+"/", "notes.md"))

	if out := restoreVia(t, h, p.ID, "notes.md", shaOf("first version\n")); !out.Folded {
		t.Fatal("restore into a live room reported folded=false")
	}
	if got := doc.GetText("body").ToString(); got != "first version\n" {
		t.Fatalf("room document = %q", got)
	}
	time.Sleep(500 * time.Millisecond)
	if n := len(historyOf(t, h, "/api/p/"+p.ID+"/", "notes.md")); n != before+1 {
		t.Fatalf("history grew by %d, want exactly the restore", n-before)
	}
	if n := conflictCopies(t, h, p.ID); n != 0 {
		t.Fatalf("%d conflict copies", n)
	}
	if got := get(t, h, "/api/p/"+p.ID+"/file?path=notes.md").Body.String(); got != "first version\n" {
		t.Fatalf("file = %q", got)
	}
}

// A version too large for a room to hold is never folded in as a prefix: the
// room is ended without a snapshot, and the file stays the whole restored
// blob however long the old room's document keeps changing.
func TestRestoreTooLargeForARoomEndsItInstead(t *testing.T) {
	fastRoom(t, 50*time.Millisecond, 500*time.Millisecond)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	up := "/api/p/" + p.ID + "/upload/content?path=notes.md"
	url := "/api/p/" + p.ID + "/file?path=notes.md"
	big := strings.Repeat("x", maxSeedBytes+10)
	putContent(t, h, up, big, "")
	putContent(t, h, up, "small now\n", "")
	room := p.ID + "/notes.md"
	doc := liveRoom(t, srv, room, true)

	if out := restoreVia(t, h, p.ID, "notes.md", shaOf(big)); out.Folded {
		t.Fatal("a version too large to hold reported folded=true")
	}
	if strings.Contains(doc.GetText("body").ToString(), "xxxx") {
		t.Fatal("a prefix of the restored version was folded into the room")
	}
	typeInto(doc, "late\n")
	time.Sleep(500 * time.Millisecond)
	if got := get(t, h, url).Body.String(); got != big {
		t.Fatalf("the file after restore is %d bytes, want the whole restored %d", len(got), len(big))
	}
}
