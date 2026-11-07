package webapp

import (
	"context"
	"errors"
	"io"
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
}

func (b *flakyBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if b.broken.Load() && strings.Contains(key, "blobs/") {
		return nil, errors.New("storage timed out")
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
