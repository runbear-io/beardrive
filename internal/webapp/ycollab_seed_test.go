package webapp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/runbear-io/beardrive/internal/remote"
)

// flakyBlobs fails every blob read while broken is set: what a room sees when
// the store times out, or a cold process cannot read the file yet.
type flakyBlobs struct {
	remote.Backend
	broken atomic.Bool
	during atomic.Pointer[func()] // runs once, inside the next blob read
}

func (b *flakyBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if b.broken.Load() && strings.Contains(key, "blobs/") {
		return nil, errors.New("storage timed out")
	}
	if strings.Contains(key, "blobs/") {
		if f := b.during.Swap(nil); f != nil {
			(*f)()
		}
	}
	return b.Backend.Get(ctx, key)
}

/*
A room that cannot read its file must not open as if the file were new.

	It used to: a failed read and a missing file both seeded an empty
	document with no base, and the room's first snapshot wrote that empty
	text unconditionally — past both the stale-write and the shrink guard,
	which only look at conditional writes. Every editor who opened the file
	in that window emptied it.
*/
func TestRoomRefusesAFileItCannotRead(t *testing.T) {
	fastRoom(t, 50*time.Millisecond, 500*time.Millisecond)
	var flaky *flakyBlobs
	srv, p, _ := newHub(t, true, func(be remote.Backend) remote.Backend {
		flaky = &flakyBlobs{Backend: be}
		return flaky
	})
	h := srv.Handler()
	notes := "# Daily\n\n- Snow: shipped the thing\n"
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", notes, "")

	flaky.broken.Store(true)
	room := p.ID + "/notes.md"
	doc := crdt.New()
	srv.rooms().load(room, doc)
	t.Cleanup(func() {
		srv.rooms().drop(room)
		srv.rooms().settle()
	})
	if err := srv.seedDoc(context.Background(), room, doc); err == nil {
		t.Fatal("a room opened over a file it could not read")
	}
	flaky.broken.Store(false)

	if got := get(t, h, "/api/p/"+p.ID+"/file?path=notes.md").Body.String(); got != notes {
		t.Fatalf("the file changed under a refused room: %q", got)
	}
}

// An empty file is still a file: the room's writes stay conditional on it,
// so an agent's write in between is merged rather than overwritten.
func TestRoomOverAnEmptyFileWritesConditionally(t *testing.T) {
	fastRoom(t, 50*time.Millisecond, 500*time.Millisecond)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	up := "/api/p/" + p.ID + "/upload/content?path=notes.md"
	putContent(t, h, up, "", "")
	room := p.ID + "/notes.md"
	doc := liveRoom(t, srv, room, true)
	if b := srv.rooms().base(room); b != shaOf("") {
		t.Fatalf("room base over an empty file = %q, want the empty blob", b)
	}

	putContent(t, h, up, "agent template\n", "")
	typeInto(doc, "typed in the room\n")
	time.Sleep(600 * time.Millisecond)
	got := get(t, h, "/api/p/"+p.ID+"/file?path=notes.md").Body.String()
	if !strings.Contains(got, "agent template") || !strings.Contains(got, "typed in the room") {
		t.Fatalf("file = %q, want the agent's template and the typing both", got)
	}
	if n := conflictCopies(t, h, p.ID); n != 0 {
		t.Fatalf("%d conflict copies for two writes into an empty file", n)
	}
}

/*
An outside write to a file somebody is editing lands in the document.

	An agent appending to meeting notes while the team types at the top is
	the ordinary case. Line edits in different places merge; the file and
	every editor's document end up holding both, and nothing is parked.
*/
func TestOutsideWriteMergesIntoALiveRoom(t *testing.T) {
	fastRoom(t, 50*time.Millisecond, 500*time.Millisecond)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	up := "/api/p/" + p.ID + "/upload/content?path=notes.md"
	url := "/api/p/" + p.ID + "/file?path=notes.md"
	putContent(t, h, up, "a\nb\nc\n", "")
	room := p.ID + "/notes.md"
	doc := liveRoom(t, srv, room, true)

	putContent(t, h, up, "a\nb\nc\nfrom the agent\n", "")
	txt := doc.GetText("body")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "typed at the top\n", nil) })

	want := "typed at the top\na\nb\nc\nfrom the agent\n"
	if !waitFile(t, h, url, want, 2*time.Second) {
		t.Fatalf("file = %q, want both edits", get(t, h, url).Body.String())
	}
	if got := txt.ToString(); got != want {
		t.Fatalf("room document = %q, want the merged text", got)
	}
	if n := conflictCopies(t, h, p.ID); n != 0 {
		t.Fatalf("%d conflict copies for edits that merge", n)
	}
}

// An empty document holds nothing to preserve. A room left empty when its
// last editor goes keeps the file as it is and parks nothing.
func TestRoomNeverParksEmptyText(t *testing.T) {
	fastRoom(t, 50*time.Millisecond, 500*time.Millisecond)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	notes := "# Daily\n\n" + strings.Repeat("- a line of notes\n", 30)
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", notes, "")
	room := p.ID + "/notes.md"
	doc := openRoom(t, srv, room)

	txt := doc.GetText("body")
	doc.Transact(func(txn *crdt.Transaction) { txt.Delete(txn, 0, txt.Len()) })
	srv.finalSnapshot(context.Background(), room)
	time.Sleep(200 * time.Millisecond)

	if got := get(t, h, "/api/p/"+p.ID+"/file?path=notes.md").Body.String(); got != notes {
		t.Fatalf("an empty room changed the file: %q", got)
	}
	if n := conflictCopies(t, h, p.ID); n != 0 {
		t.Fatalf("an empty room parked %d conflict copies", n)
	}
}

// The last editor leaving after an outside write: there is nobody left to
// receive a merged document, so the merge goes straight to the file.
func TestFinalSnapshotMergesAnOutsideWrite(t *testing.T) {
	fastRoom(t, time.Hour, time.Hour)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	up := "/api/p/" + p.ID + "/upload/content?path=notes.md"
	url := "/api/p/" + p.ID + "/file?path=notes.md"
	putContent(t, h, up, "a\nb\nc\n", "")
	room := p.ID + "/notes.md"
	doc := openRoom(t, srv, room)

	putContent(t, h, up, "a\nb\nc\nfrom the agent\n", "")
	txt := doc.GetText("body")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "typed at the top\n", nil) })
	srv.finalSnapshot(context.Background(), room)

	if got := get(t, h, url).Body.String(); got != "typed at the top\na\nb\nc\nfrom the agent\n" {
		t.Fatalf("file = %q, want both edits", got)
	}
	if n := conflictCopies(t, h, p.ID); n != 0 {
		t.Fatalf("%d conflict copies for edits that merge", n)
	}
}

// A text adds nothing to a file only if its lines are there in order, each
// as many times: a line typed twice is not held by a file that has it once.
func TestFileHoldsCountsAndOrdersLines(t *testing.T) {
	for _, c := range []struct {
		file, text string
		holds      bool
		appended   string
	}{
		{"same\n", "same\nsame\n", false, "same\nsame\n"},
		{"a\nb\nc\n", "a\nc\n", true, "a\nb\nc\n"},
		{"a\nb\n", "b\na\n", false, "a\nb\na\n"},
		{"agent template\n", "typed in the room\n", false, "agent template\ntyped in the room\n"},
		{"anything\n", "\n\n", true, "anything\n"},
	} {
		if got := fileHolds(c.file, c.text); got != c.holds {
			t.Errorf("fileHolds(%q, %q) = %v, want %v", c.file, c.text, got, c.holds)
		}
		if got := appendNew(c.file, c.text); got != c.appended {
			t.Errorf("appendNew(%q, %q) = %q, want %q", c.file, c.text, got, c.appended)
		}
	}
}

// A file past the seed bound is read only up to it. Merged as if that prefix
// were the file, the room would write it back over the tail; it parks instead.
func TestOutsideWritePastTheSeedBoundIsNeverMergedFromAPrefix(t *testing.T) {
	fastRoom(t, time.Hour, time.Hour)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	up := "/api/p/" + p.ID + "/upload/content?path=notes.md"
	url := "/api/p/" + p.ID + "/file?path=notes.md"
	putContent(t, h, up, "a\nb\nc\n", "")
	room := p.ID + "/notes.md"
	doc := openRoom(t, srv, room)

	big := "a\nb\nc\n" + strings.Repeat("an agent's appended line\n", maxSeedBytes/24+1000) + "THE TAIL\n"
	if rec := putContent(t, h, up, big, ""); rec.Code != http.StatusOK {
		t.Fatalf("outside write: %d", rec.Code)
	}
	txt := doc.GetText("body")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "typed at the top\n", nil) })
	srv.finalSnapshot(context.Background(), room)

	if got := get(t, h, url).Body.String(); got != big {
		t.Fatalf("file is %d bytes ending %q, want the agent's %d bytes intact", len(got), got[max(0, len(got)-20):], len(big))
	}
	if n := conflictCopies(t, h, p.ID); n != 1 {
		t.Fatalf("conflict copies = %d, want the room's text parked once", n)
	}
}

// A copy the store refuses is retried, not given up on: the room has already
// moved onto their version, so the copy is the only place its text lives.
func TestARefusedConflictCopyIsRetried(t *testing.T) {
	old := parkBackoff
	parkBackoff = 10 * time.Millisecond
	t.Cleanup(func() { parkBackoff = old })
	fastRoom(t, time.Hour, time.Hour)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	up := "/api/p/" + p.ID + "/upload/content?path=notes.md"
	putContent(t, h, up, "one\ntwo\n", "")
	room := p.ID + "/notes.md"
	doc := openRoom(t, srv, room)

	putContent(t, h, up, "one\nOUTSIDE\n", "")
	txt := doc.GetText("body")
	doc.Transact(func(txn *crdt.Transaction) {
		txt.Delete(txn, 0, txt.Len())
		txt.Insert(txn, 0, "one\nROOM\n", nil)
	})
	// The snapshot reaches its 409; the copy after it fails three times.
	srv.Quota = &failingQuota{skip: 1, n: 3}
	srv.finalSnapshot(context.Background(), room)

	deadline := time.Now().Add(5 * time.Second)
	for conflictCopies(t, h, p.ID) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the refused conflict copy was never written")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := get(t, h, "/api/p/"+p.ID+"/file?path=notes.md").Body.String(); got != "one\nOUTSIDE\n" {
		t.Fatalf("file = %q, want the outside write", got)
	}
}

// The last editor leaving while the store cannot be read: the room is
// dropped, but its text is not. It is written once the file reads again.
func TestFinalSnapshotThatCannotReadIsFinishedLater(t *testing.T) {
	old := parkBackoff
	parkBackoff = 10 * time.Millisecond
	t.Cleanup(func() { parkBackoff = old })
	fastRoom(t, time.Hour, time.Hour)
	var flaky *flakyBlobs
	srv, p, _ := newHub(t, true, func(be remote.Backend) remote.Backend {
		flaky = &flakyBlobs{Backend: be}
		return flaky
	})
	h := srv.Handler()
	url := "/api/p/" + p.ID + "/file?path=notes.md"
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "a\nb\n", "")
	room := p.ID + "/notes.md"
	doc := openRoom(t, srv, room)
	typeInto(doc, "typed before the outage\n")

	flaky.broken.Store(true)
	srv.finalSnapshot(context.Background(), room)
	srv.rooms().drop(room)
	time.Sleep(50 * time.Millisecond)
	flaky.broken.Store(false)

	want := "a\nb\ntyped before the outage\n"
	if !waitFile(t, h, url, want, 3*time.Second) {
		t.Fatalf("the room's text was dropped with the room: file is %q", get(t, h, url).Body.String())
	}
	if n := conflictCopies(t, h, p.ID); n != 0 {
		t.Fatalf("%d conflict copies for a write nobody else made", n)
	}
	// The write is filed with the room's state, so a browser reconnecting
	// with that state replays onto it rather than beside it.
	deadline := time.Now().Add(2 * time.Second)
	for srv.roomHead(context.Background(), p.ID, "notes.md") != shaOf(want) {
		if time.Now().After(deadline) {
			t.Fatalf("room head = %q, want the delayed write's sha", srv.roomHead(context.Background(), p.ID, "notes.md"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A file deleted while the room could not read it stays deleted: the room's
// text goes beside it rather than bringing it back.
func TestDelayedFinalSnapshotDoesNotUndoADelete(t *testing.T) {
	old := parkBackoff
	parkBackoff = 10 * time.Millisecond
	t.Cleanup(func() { parkBackoff = old })
	fastRoom(t, time.Hour, time.Hour)
	var flaky *flakyBlobs
	srv, p, _ := newHub(t, true, func(be remote.Backend) remote.Backend {
		flaky = &flakyBlobs{Backend: be}
		return flaky
	})
	h := srv.Handler()
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "a\nb\n", "")
	room := p.ID + "/notes.md"
	doc := openRoom(t, srv, room)
	typeInto(doc, "typed before the outage\n")

	flaky.broken.Store(true)
	srv.finalSnapshot(context.Background(), room)
	srv.rooms().drop(room)
	if rec := do(t, h, "POST", "/api/p/"+p.ID+"/remove", map[string]string{"path": "notes.md"}); rec.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
	flaky.broken.Store(false)

	deadline := time.Now().Add(3 * time.Second)
	for conflictCopies(t, h, p.ID) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the room's text was never parked")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rec := get(t, h, "/api/p/"+p.ID+"/file?path=notes.md"); rec.Code == http.StatusOK {
		t.Fatalf("a deleted file came back: %q", rec.Body.String())
	}
}

// Binary content is never merged with a room's lines.
func TestBinaryOutsideWriteIsNeverMerged(t *testing.T) {
	fastRoom(t, time.Hour, time.Hour)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	up := "/api/p/" + p.ID + "/upload/content?path=notes.md"
	url := "/api/p/" + p.ID + "/file?path=notes.md"
	putContent(t, h, up, "a\nb\nc\n", "")
	room := p.ID + "/notes.md"
	doc := openRoom(t, srv, room)

	bin := "a\nb\nc\n\x00\x01binary tail\n"
	putContent(t, h, up, bin, "")
	txt := doc.GetText("body")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "typed at the top\n", nil) })
	srv.finalSnapshot(context.Background(), room)

	if got := get(t, h, url).Body.String(); got != bin {
		t.Fatalf("file = %q, want the outside binary write untouched", got)
	}
	if n := conflictCopies(t, h, p.ID); n != 1 {
		t.Fatalf("conflict copies = %d, want the room's text parked once", n)
	}
}

// A final snapshot that keeps their version moves the room onto it, so an
// editor who reopens before the room unloads is not refused for a stale base.
func TestFinalSnapshotKeepingTheirsRebases(t *testing.T) {
	fastRoom(t, time.Hour, time.Hour)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	up := "/api/p/" + p.ID + "/upload/content?path=notes.md"
	putContent(t, h, up, "one\ntwo\n", "")
	room := p.ID + "/notes.md"
	doc := openRoom(t, srv, room)

	putContent(t, h, up, "OUTSIDE\none\ntwo\nthree\n", "")
	srv.finalSnapshot(context.Background(), room)
	got := get(t, h, "/api/p/"+p.ID+"/file?path=notes.md")
	if got.Body.String() != "OUTSIDE\none\ntwo\nthree\n" {
		t.Fatalf("file = %q", got.Body.String())
	}
	if b := srv.rooms().base(room); b != shaOf("OUTSIDE\none\ntwo\nthree\n") {
		t.Fatalf("room base = %q, want their version", b)
	}
	if got := doc.GetText("body").ToString(); got != "OUTSIDE\none\ntwo\nthree\n" {
		t.Fatalf("room document = %q, want it moved with the base", got)
	}
}

// A merged final write moves the held document too: a room reopened before
// it unloads must not write its pre-merge text over the merge.
func TestFinalMergeMovesTheHeldDocument(t *testing.T) {
	fastRoom(t, time.Hour, time.Hour)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	up := "/api/p/" + p.ID + "/upload/content?path=notes.md"
	putContent(t, h, up, "a\nb\nc\n", "")
	room := p.ID + "/notes.md"
	doc := openRoom(t, srv, room)

	putContent(t, h, up, "a\nb\nc\nfrom the agent\n", "")
	txt := doc.GetText("body")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "typed at the top\n", nil) })
	srv.finalSnapshot(context.Background(), room)

	want := "typed at the top\na\nb\nc\nfrom the agent\n"
	if got := txt.ToString(); got != want {
		t.Fatalf("room document = %q, want the merge it wrote", got)
	}
	if b := srv.rooms().base(room); b != shaOf(want) {
		t.Fatalf("room base = %q, want the merged version", b)
	}
	srv.snapshotRoom(context.Background(), room)
	if got := get(t, h, "/api/p/"+p.ID+"/file?path=notes.md").Body.String(); got != want {
		t.Fatalf("a later snapshot changed the merged file: %q", got)
	}
}

// A restore folded into the room while a snapshot is reading the file is not
// undone by that snapshot: the snapshot's base is the one its text was built on.
func TestSnapshotDoesNotUndoARestoreFoldedMidway(t *testing.T) {
	fastRoom(t, time.Hour, time.Hour)
	var hooked *flakyBlobs
	srv, p, _ := newHub(t, true, func(be remote.Backend) remote.Backend {
		hooked = &flakyBlobs{Backend: be}
		return hooked
	})
	h := srv.Handler()
	up := "/api/p/" + p.ID + "/upload/content?path=notes.md"
	url := "/api/p/" + p.ID + "/file?path=notes.md"
	putContent(t, h, up, "first version\n", "")
	putContent(t, h, up, "second version\n", "")
	room := p.ID + "/notes.md"
	doc := liveRoom(t, srv, room, true)
	typeInto(doc, "typed\n")

	restore := func() {
		if out := restoreVia(t, h, p.ID, "notes.md", shaOf("first version\n")); !out.Folded {
			t.Error("restore did not fold into the live room")
		}
	}
	hooked.during.Store(&restore)
	srv.snapshotRoom(context.Background(), room)
	if hooked.during.Load() != nil {
		t.Fatal("the restore never ran inside the snapshot's read")
	}

	if got := get(t, h, url).Body.String(); !strings.HasPrefix(got, "first version\n") || strings.Contains(got, "second version") {
		t.Fatalf("the snapshot undid the restore: file is %q", got)
	}
}
