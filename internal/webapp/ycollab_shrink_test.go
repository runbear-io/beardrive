package webapp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
)

/*
An editor in a live room confirms a shrink the hub refused to make on its

	own. That write spells the room's document, so the room must build its
	next snapshot on it — not send the pre-confirm base, be refused, and park
	the user's following typing as a conflict copy.
*/
func TestConfirmedShrinkInALiveRoomBecomesTheRoomBase(t *testing.T) {
	fastRoom(t, 100*time.Millisecond, time.Second)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	up := "/api/p/" + p.ID + "/upload/content?path=notes.md"
	url := "/api/p/" + p.ID + "/file?path=notes.md"
	body := "## Snow\n" + strings.Repeat("- shipped the thing\n", 60)
	putContent(t, h, up, body, "")
	doc := liveRoom(t, srv, p.ID+"/notes.md", true)

	txt := doc.GetText("body")
	doc.Transact(func(txn *crdt.Transaction) {
		txt.Delete(txn, 0, txt.Len())
		txt.Insert(txn, 0, "short", nil)
	})
	time.Sleep(400 * time.Millisecond)
	if got := get(t, h, url).Body.String(); got != body {
		t.Fatalf("the room wrote a sharp shrink nobody confirmed: %q", got)
	}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("PUT", up, strings.NewReader("short"))
	r.Header.Set("If-Match", shaOf(body))
	r.Header.Set(confirmShrinkHeader, "1")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirmed shrink: %d %s", rec.Code, rec.Body)
	}

	typeInto(doc, " and more")
	if !waitFile(t, h, url, "short and more", 2*time.Second) {
		t.Fatalf("the room's next snapshot did not build on the confirmed write: file is %q", get(t, h, url).Body.String())
	}
	if n := conflictCopies(t, h, p.ID); n != 0 {
		t.Fatalf("%d conflict copies after a confirmed shrink", n)
	}
}

// A room seeded from the first maxSeedBytes of a larger file would write that
// prefix back over the whole file on its first snapshot, so it never opens.
func TestRoomRefusesAFileTooLargeToHold(t *testing.T) {
	fastRoom(t, 50*time.Millisecond, 500*time.Millisecond)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	big := strings.Repeat("x", maxSeedBytes+10)
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=big.md", big, "")
	room := p.ID + "/big.md"
	srv.rooms().writer(room, User{Email: "editor@example.com", Name: "An Editor"})
	t.Cleanup(func() {
		srv.rooms().drop(room)
		srv.rooms().settle()
	})
	_ = srv.ydocs().Apply(context.Background(), room, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		txt := doc.GetText("body")
		transact(func(txn *crdt.Transaction) { txt.Insert(txn, txt.Len(), "!", nil) })
	})
	time.Sleep(400 * time.Millisecond)
	if n := get(t, h, "/api/p/"+p.ID+"/file?path=big.md").Body.Len(); n != len(big) {
		t.Fatalf("a room over a file larger than it can hold rewrote it: %d bytes, was %d", n, len(big))
	}
}
