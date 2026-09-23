package webapp

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
)

/* The hub is the only writer while a room is live.

   Every co-editor used to write the file themselves, each against whatever
   If-Match they last saw — N writers racing for one file, and every lost
   race parking somebody's text beside the file as a "conflict" that was the
   file against itself. Two rounds of client-side retry logic did not stop it,
   because the race is between reading the base and the write arriving.

   These pin the replacement: the hub sees every update, writes once per
   pause, never later than a deadline, and a refusal can only ever mean
   somebody OUTSIDE the room wrote — which gets the conflict copy that case
   deserves instead of a silent overwrite. */

// fastRoom shrinks the debounce so a test measures the mechanism, not the clock.
func fastRoom(t *testing.T, idle, max time.Duration) {
	t.Helper()
	oi, om := snapshotIdle, snapshotMax
	snapshotIdle, snapshotMax = idle, max
	t.Cleanup(func() { snapshotIdle, snapshotMax = oi, om })
}

// openRoom does what OnLoadDocument does, in its order: load, seed, watch.
func openRoom(t *testing.T, srv *Server, room string) *crdt.Doc {
	t.Helper()
	doc := crdt.New()
	srv.rooms().load(room, doc)
	if err := srv.seedDoc(room, doc); err != nil {
		t.Fatal(err)
	}
	srv.rooms().writer(room, User{Email: "editor@example.com", Name: "An Editor"})
	srv.watchRoom(room, doc)
	t.Cleanup(func() { srv.rooms().drop(room) })
	return doc
}

func typeInto(doc *crdt.Doc, s string) {
	txt := doc.GetText("body")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, txt.Len(), s, nil) })
}

func waitFile(t *testing.T, h http.Handler, url, want string, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if get(t, h, url).Body.String() == want {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestRoomWritesItselfAfterAPause(t *testing.T) {
	fastRoom(t, 150*time.Millisecond, 2*time.Second)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	url := "/api/p/" + p.ID + "/file?path=notes.md"
	if rec := putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "before\n", ""); rec.Code != http.StatusOK {
		t.Fatalf("seed write: %d", rec.Code)
	}
	doc := openRoom(t, srv, p.ID+"/notes.md")

	// Nobody calls snapshotRoom. The edit alone has to reach the file.
	typeInto(doc, "typed\n")

	// Not yet: the pause has not happened.
	time.Sleep(50 * time.Millisecond)
	if got := get(t, h, url).Body.String(); got != "before\n" {
		t.Fatalf("the file moved %q before the idle window elapsed; that is a write per keystroke", got)
	}
	if !waitFile(t, h, url, "before\ntyped\n", 2*time.Second) {
		t.Fatal("the room paused and the hub never wrote the file; with no client saving any more, " +
			"the edit would be lost the moment the room closed")
	}
}

func TestRoomWritesByTheDeadlineWhileTypingNeverPauses(t *testing.T) {
	fastRoom(t, 150*time.Millisecond, 600*time.Millisecond)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	url := "/api/p/" + p.ID + "/file?path=notes.md"
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "", "")
	doc := openRoom(t, srv, p.ID+"/notes.md")

	// Faster than the idle window, for longer than the deadline: an idle-only
	// debounce would never fire, and a long paragraph would sit unwritten
	// for as long as the author kept going.
	stop := time.Now().Add(1500 * time.Millisecond)
	wrote := false
	for time.Now().Before(stop) {
		typeInto(doc, "a")
		time.Sleep(60 * time.Millisecond)
		if !wrote && get(t, h, url).Body.Len() > 0 {
			wrote = true
		}
	}
	if !wrote {
		t.Fatal("typing continuously for 2.5x the deadline never produced a write; " +
			"the deadline is not being honoured")
	}
	// And it converges once the typing stops.
	if !waitFile(t, h, url, doc.GetText("body").ToString(), 2*time.Second) {
		t.Fatal("the file never caught up with the document after typing stopped")
	}
}

/*
An outside writer is never silently overwritten.

	With the hub holding the pen, an If-Match refusal cannot come from a
	co-editor — everyone in the room shares this document. It can only be an
	agent, the CLI, or another device writing the file underneath the room.
	Their write stays the file; the room's version is parked beside it under
	the conflict name the sync path has always used; and the room rebases so
	it is not refused for the same reason forever.
*/
func TestRoomParksItsVersionWhenSomeoneOutsideWrote(t *testing.T) {
	fastRoom(t, 100*time.Millisecond, time.Second)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	url := "/api/p/" + p.ID + "/file?path=notes.md"
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "before\n", "")
	doc := openRoom(t, srv, p.ID+"/notes.md")

	// An agent rewrites the file. The room knows nothing of it.
	if rec := putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "REWRITTEN BY AN AGENT\n", ""); rec.Code != http.StatusOK {
		t.Fatalf("outside write: %d %s", rec.Code, rec.Body.String())
	}
	typeInto(doc, "typed in the room\n")

	// The agent's text must survive — and the room's must not vanish.
	time.Sleep(600 * time.Millisecond)
	if got := get(t, h, url).Body.String(); got != "REWRITTEN BY AN AGENT\n" {
		t.Fatalf("the room's snapshot overwrote an outside write: file is %q", got)
	}
	tree := get(t, h, "/api/p/"+p.ID+"/tree?slim=1").Body.String()
	if !strings.Contains(tree, "notes.md.bdrive-conflict-An-Editor-") {
		t.Fatal("the room's version was refused and then dropped: no conflict copy beside the file")
	}

	// Rebased: the next snapshot is not refused for the same stale base.
	typeInto(doc, "more\n")
	if !waitFile(t, h, url, "before\ntyped in the room\nmore\n", 2*time.Second) {
		t.Fatalf("after parking, the room never recovered the pen: file is %q", get(t, h, url).Body.String())
	}
}

// Seeding must not count as an edit, or every room would write the file it
// was built from the moment it opened — one journal version per open.
func TestSeedingARoomIsNotAWrite(t *testing.T) {
	fastRoom(t, 50*time.Millisecond, 500*time.Millisecond)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "seed\n", "")
	before := get(t, h, "/api/p/"+p.ID+"/history?path=notes.md&n=10").Body.String()
	openRoom(t, srv, p.ID+"/notes.md")
	time.Sleep(300 * time.Millisecond)
	after := get(t, h, "/api/p/"+p.ID+"/history?path=notes.md&n=10").Body.String()
	if before != after {
		t.Fatal("opening a room wrote the file it was seeded from")
	}
}

// Dropping a room disarms its timer and unsubscribes; a snapshot must not fire
// for a document nothing holds any more.
func TestDroppedRoomDoesNotWrite(t *testing.T) {
	fastRoom(t, 100*time.Millisecond, time.Second)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	url := "/api/p/" + p.ID + "/file?path=notes.md"
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "before\n", "")
	room := p.ID + "/notes.md"
	doc := openRoom(t, srv, room)
	typeInto(doc, "late\n")
	srv.rooms().drop(room)
	time.Sleep(400 * time.Millisecond)
	if got := get(t, h, url).Body.String(); got != "before\n" {
		t.Fatalf("a dropped room still wrote: %q", got)
	}
	_ = context.Background()
}
