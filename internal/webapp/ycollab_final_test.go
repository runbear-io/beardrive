package webapp

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
)

// A room that empties on top of an outside append writes the two together
// (reconcile), and the state it stores must be the one that spells that file:
// built instead from an older state plus a splice of the bytes, the next room
// holds the room's own lines as new items, and a browser coming back with the
// items it typed merges them in a second time.
func TestYCollabFinalReconcileStoresTheStateItWrote(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	room := p.ID + "/daily.md"
	who := User{Email: "editor@example.com", Name: "An Editor"}
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=daily.md", "# daily\n- a\n", "")
	open := func() *crdt.Doc {
		doc := crdt.New()
		if err := srv.seedDoc(context.Background(), room, doc); err != nil {
			t.Fatal(err)
		}
		srv.rooms().load(room, doc)
		srv.rooms().writer(room, who)
		return doc
	}
	sync := func(from *crdt.Doc) *crdt.Doc {
		d := crdt.New()
		if err := d.ApplyUpdate(from.EncodeStateAsUpdate()); err != nil {
			t.Fatal(err)
		}
		return d
	}
	typeAtEnd := func(doc *crdt.Doc, s string) {
		txt := doc.GetText("body")
		doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, txt.Len(), s, nil) })
	}
	hub := open()
	typeAtEnd(hub, "- b\n")
	srv.snapshotRoom(context.Background(), room)
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=daily.md", "# daily\n- a\n- b\n- agent\n", "")
	typeAtEnd(hub, "- c\n- d\n")
	browser := sync(hub)
	srv.finalSnapshot(context.Background(), room)
	srv.rooms().drop(room)
	if file := get(t, h, "/api/p/"+p.ID+"/file?path=daily.md").Body.String(); file != "# daily\n- a\n- b\n- agent\n- c\n- d\n" {
		t.Fatalf("file after the room emptied = %q", file)
	}

	fresh := open()
	if err := fresh.ApplyUpdate(browser.EncodeStateAsUpdate()); err != nil {
		t.Fatal(err)
	}
	got := fresh.GetText("body").ToString()
	for _, want := range []string{"- b\n", "- agent\n", "- c\n", "- d\n"} {
		if n := strings.Count(got, want); n != 1 {
			t.Errorf("%q appears %d times in %q, want once", want, n, got)
		}
	}
}

// One room's snapshots run one at a time: a pause and the last peer leaving
// fire on different goroutines, and two in flight could store history and
// rebase the room in either order, the slower naming an older version.
func TestYCollabSnapshotsOfOneRoomDoNotOverlap(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	room := p.ID + "/notes.md"
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "before\n", "")
	doc := liveRoom(t, srv, room, true)
	typeInto(doc, "typed\n")

	var inside atomic.Int32
	release := make(chan struct{})
	snapshotSeam = func(string) {
		inside.Add(1)
		<-release
	}
	t.Cleanup(func() { snapshotSeam = nil })
	done := make(chan struct{}, 2)
	for range 2 {
		go func() {
			srv.snapshotRoom(context.Background(), room)
			done <- struct{}{}
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for inside.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if n := inside.Load(); n != 1 {
		t.Fatalf("%d snapshots of one room ran at once", n)
	}
	close(release)
	<-done
	<-done
	if n := inside.Load(); n != 2 {
		t.Fatalf("the second snapshot never ran: %d", n)
	}
}
