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
	// Dropped, then settled: a snapshot already running on its timer would
	// otherwise write ydoc/ while the test's temp dir is being removed.
	t.Cleanup(func() {
		srv.rooms().drop(room)
		srv.rooms().settle()
	})
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
An outside write is folded into the room, never parked and then overwritten.

	With the hub holding the pen, an If-Match refusal cannot come from a
	co-editor — everyone in the room shares this document. It can only be an
	agent, the CLI, or another device writing the file underneath the room.
	Parking the room's text beside the file and rebasing looked safe, but the
	room's next snapshot then wrote over the outsider's text: a copy every
	round and the outside change reverted. The hub folds the outside change
	into the document instead, as an edit every peer receives, and both
	sides' work ends up in the one file.
*/
func TestRoomFoldsAWriteFromOutside(t *testing.T) {
	fastRoom(t, 100*time.Millisecond, time.Second)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	url := "/api/p/" + p.ID + "/file?path=notes.md"
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "title\nbefore\nend\n", "")
	doc := openRoom(t, srv, p.ID+"/notes.md")

	// An agent rewrites one line. The room knows nothing of it.
	if rec := putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "title\nREWRITTEN BY AN AGENT\nend\n", ""); rec.Code != http.StatusOK {
		t.Fatalf("outside write: %d %s", rec.Code, rec.Body.String())
	}
	typeInto(doc, "typed in the room\n")

	want := "title\nREWRITTEN BY AN AGENT\nend\ntyped in the room\n"
	if !waitFile(t, h, url, want, 2*time.Second) {
		t.Fatalf("file is %q, want both the agent's line and the room's typing", get(t, h, url).Body.String())
	}
	if got := doc.GetText("body").ToString(); got != want {
		t.Fatalf("the room's document is %q; the outside write never reached the people in it", got)
	}
	tree := get(t, h, "/api/p/"+p.ID+"/tree?slim=1").Body.String()
	if strings.Contains(tree, "bdrive-conflict") {
		t.Fatal("a write the room could fold was parked as a conflict copy")
	}

	typeInto(doc, "more\n")
	if !waitFile(t, h, url, want+"more\n", 2*time.Second) {
		t.Fatalf("after folding, the room never wrote again: file is %q", get(t, h, url).Body.String())
	}
}

/*
A writer that worked from an older version takes nothing the room wrote since.

	A device or an agent edits the copy it last synced, which can be a few of
	the room's snapshots behind. Folded against the room's LATEST version,
	everything typed between the two reads as a deletion the writer made, and
	the fold would take it out of the room. The fold is made against the
	room's version the outside text is closest to instead.
*/
func TestFoldStartsFromTheVersionTheWriterHad(t *testing.T) {
	fastRoom(t, 50*time.Millisecond, time.Second)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	url := "/api/p/" + p.ID + "/file?path=notes.md"
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "a\n", "")
	doc := openRoom(t, srv, p.ID+"/notes.md")
	typeInto(doc, "b\n")
	if !waitFile(t, h, url, "a\nb\n", 2*time.Second) {
		t.Fatal("the room never wrote b")
	}
	typeInto(doc, "c\n")
	if !waitFile(t, h, url, "a\nb\nc\n", 2*time.Second) {
		t.Fatal("the room never wrote c")
	}

	// A device that last synced "a\nb\n" adds a line to it.
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "a\nb\nfrom a device\n", "")
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := doc.GetText("body").ToString()
		ok := len(got) == len("a\nb\nc\nfrom a device\n")
		for _, line := range []string{"a\n", "b\n", "c\n", "from a device\n"} {
			ok = ok && strings.Count(got, line) == 1
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the room says %q; want every line once, c included", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A room opened on a file that did not exist yet still folds the write that
// creates it, rather than writing over it unconditionally.
func TestRoomFoldsTheWriteThatCreatesItsFile(t *testing.T) {
	fastRoom(t, 100*time.Millisecond, time.Second)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	url := "/api/p/" + p.ID + "/file?path=new.md"
	doc := openRoom(t, srv, p.ID+"/new.md")

	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=new.md", "from an agent\n", "")
	typeInto(doc, "typed\n")
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := get(t, h, url).Body.String()
		if strings.Contains(got, "from an agent\n") && strings.Contains(got, "typed\n") && len(got) == len("from an agent\ntyped\n") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("file is %q, want the agent's line and the room's typing, once each", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A room that cannot fold an outside write — no stored history for the
// version it was built on — still never drops either side: the outsider's
// write stays the file and the room's text is parked beside it.
func TestRoomParksWhatItCannotFold(t *testing.T) {
	fastRoom(t, 100*time.Millisecond, time.Second)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	url := "/api/p/" + p.ID + "/file?path=notes.md"
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "before\n", "")
	room := p.ID + "/notes.md"
	doc := openRoom(t, srv, room)
	// A base with no stored history, and no other version to fold against.
	bogus := strings.Repeat("0", 64)
	r := srv.rooms()
	r.rebase(room, bogus)
	r.mu.Lock()
	r.versions[room] = []string{bogus}
	r.mu.Unlock()

	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "REWRITTEN BY AN AGENT\n", "")
	typeInto(doc, "typed in the room\n")
	time.Sleep(600 * time.Millisecond)
	if got := get(t, h, url).Body.String(); got != "REWRITTEN BY AN AGENT\n" {
		t.Fatalf("an unfoldable room overwrote the outside write: file is %q", got)
	}
	if tree := get(t, h, "/api/p/"+p.ID+"/tree?slim=1").Body.String(); !strings.Contains(tree, "notes.md.bdrive-conflict-An-Editor-") {
		t.Fatal("the room's version was refused and then dropped: no conflict copy beside the file")
	}
}

// Nobody has to type for an outside write to reach the people in a room: the
// change it raises is enough.
func TestQuietRoomFoldsAWriteFromOutside(t *testing.T) {
	fastRoom(t, 100*time.Millisecond, time.Second)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "one\n", "")
	doc := openRoom(t, srv, p.ID+"/notes.md")

	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "one\ntwo\n", "")
	deadline := time.Now().Add(2 * time.Second)
	for doc.GetText("body").ToString() != "one\ntwo\n" {
		if time.Now().After(deadline) {
			t.Fatalf("the room still says %q; an idle room never heard about the write", doc.GetText("body").ToString())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

/*
A room's own snapshots never conflict with each other.

	A snapshot on the debounce timer and the one the last peer's departure
	fires can overlap. Unserialized, the second is refused for the base the
	first just moved, and the room took its own write for an outsider's.
*/
func TestOverlappingSnapshotsAreOneWriter(t *testing.T) {
	fastRoom(t, 100*time.Millisecond, time.Second)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	url := "/api/p/" + p.ID + "/file?path=notes.md"
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "one\n", "")
	room := p.ID + "/notes.md"
	doc := openRoom(t, srv, room)
	typeInto(doc, "two\n")

	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func() { srv.finalSnapshot(context.Background(), room); done <- struct{}{} }()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	if got := get(t, h, url).Body.String(); got != "one\ntwo\n" {
		t.Fatalf("file is %q", got)
	}
	if tree := get(t, h, "/api/p/"+p.ID+"/tree?slim=1").Body.String(); strings.Contains(tree, "bdrive-conflict") {
		t.Fatal("the room parked its own text as a conflict with itself")
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
