package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
)

func headSha(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct{ SHA string }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.SHA == "" {
		t.Fatalf("%d %s: no sha", rec.Code, rec.Body.String())
	}
	return out.SHA
}

/*
An editor that attached before its document had content saved 0 bytes over

	four meeting notes, each with the right If-Match. The base check cannot
	see that; this one does.
*/
func TestEditorSaveThatEmptiesAFileIsRefused(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	url := "/api/p/" + p.ID + "/upload/content?path=notes.md"
	full := strings.Repeat("a standup line\n", 100)

	base := headSha(t, putContent(t, h, url, full, ""))

	empty := putContent(t, h, url, "", base)
	if empty.Code != http.StatusUnprocessableEntity {
		t.Fatalf("emptying save = %d, want 422: %s", empty.Code, empty.Body.String())
	}
	var refusal struct {
		Shrink bool
		SHA    string
		Size   int64
	}
	if err := json.Unmarshal(empty.Body.Bytes(), &refusal); err != nil || !refusal.Shrink || refusal.SHA != base || refusal.Size != int64(len(full)) {
		t.Fatalf("refusal body = %s", empty.Body.String())
	}
	if rec := get(t, h, "/api/p/"+p.ID+"/file?path=notes.md"); rec.Body.String() != full {
		t.Fatalf("file changed after a refused save: %q", rec.Body.String())
	}

	shrunk := putContent(t, h, url, "a standup line\n", base)
	if shrunk.Code != http.StatusUnprocessableEntity {
		t.Fatalf("save keeping under a fifth = %d, want 422", shrunk.Code)
	}

	// The user meant it.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("PUT", url, strings.NewReader(""))
	r.Header.Set("If-Match", base)
	r.Header.Set(confirmShrinkHeader, "1")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirmed emptying save = %d: %s", rec.Code, rec.Body.String())
	}
}

// Unconditional writes (uploads, MCP, older clients) replace files wholesale
// and are not editor saves.
func TestUnconditionalWriteMayEmptyAFile(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	url := "/api/p/" + p.ID + "/upload/content?path=notes.md"
	putContent(t, h, url, strings.Repeat("x", 1000), "")
	if rec := putContent(t, h, url, "", ""); rec.Code != http.StatusOK {
		t.Fatalf("unconditional empty write = %d", rec.Code)
	}
}

func TestSharpShrink(t *testing.T) {
	for _, c := range []struct {
		was, now int64
		want     bool
	}{
		{0, 0, false},
		{10, 0, true},
		{10, 1, false},
		{1000, 199, true},
		{1000, 200, false},
		{1000, 2000, false},
		{shrinkRatioFloor - 1, 1, false},
	} {
		if got := sharpShrink(c.was, c.now); got != c.want {
			t.Errorf("sharpShrink(%d, %d) = %v, want %v", c.was, c.now, got, c.want)
		}
	}
}

/*
A room cleared on purpose cannot confirm it — nobody in a live room writes

	for themselves — so when the room empties, its text is parked beside the
	file rather than dropped with the room.
*/
func TestFinalSnapshotParksARefusedShrink(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	room := p.ID + "/notes.md"
	full := strings.Repeat("a standup line\n", 100)
	if rec := putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", full, ""); rec.Code != http.StatusOK {
		t.Fatalf("seed write: %d", rec.Code)
	}
	doc := crdt.New()
	if err := srv.seedDoc(room, doc); err != nil {
		t.Fatal(err)
	}
	srv.rooms().load(room, doc)
	srv.rooms().writer(room, User{Email: "editor@example.com", Name: "An Editor"})
	txt := doc.GetText("body")
	doc.Transact(func(txn *crdt.Transaction) { txt.Delete(txn, 0, txt.Len()) })

	conflicts := func() int {
		var out struct {
			Entries []struct{ Path string } `json:"entries"`
		}
		rec := get(t, h, "/api/p/"+p.ID+"/history?n=50")
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("history: %d %s", rec.Code, rec.Body.String())
		}
		n := 0
		for _, e := range out.Entries {
			if strings.HasPrefix(e.Path, "notes.md.bdrive-conflict-") {
				n++
			}
		}
		return n
	}

	srv.snapshotRoom(context.Background(), room)
	if got := get(t, h, "/api/p/"+p.ID+"/file?path=notes.md").Body.String(); got != full {
		t.Fatal("an idle snapshot emptied the file")
	}
	if n := conflicts(); n != 0 {
		t.Fatalf("an idle snapshot parked %d copies; editors are still in the room", n)
	}

	srv.finalSnapshot(context.Background(), room)
	srv.finalSnapshot(context.Background(), room)
	if got := get(t, h, "/api/p/"+p.ID+"/file?path=notes.md").Body.String(); got != full {
		t.Fatal("the final snapshot emptied the file")
	}
	if n := conflicts(); n != 1 {
		t.Fatalf("conflict copies after the room emptied = %d, want 1", n)
	}
}

// failingQuota admits `skip` writes, then refuses the next n, standing in for
// a store that goes briefly down.
type failingQuota struct {
	UnlimitedQuota
	mu      sync.Mutex
	skip, n int
}

func (q *failingQuota) CheckWrite(string, int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.skip > 0 {
		q.skip--
		return nil
	}
	if q.n > 0 {
		q.n--
		return errors.New("store unavailable")
	}
	return nil
}

// The room is gone by the time a failed copy could be retried, so the retry
// cannot depend on it.
func TestRefusedShrinkCopyIsRetriedAfterTheRoomCloses(t *testing.T) {
	old := parkBackoff
	parkBackoff = 10 * time.Millisecond
	t.Cleanup(func() { parkBackoff = old })

	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	room := p.ID + "/notes.md"
	full := strings.Repeat("a standup line\n", 100)
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", full, "")
	doc := crdt.New()
	if err := srv.seedDoc(room, doc); err != nil {
		t.Fatal(err)
	}
	srv.rooms().load(room, doc)
	srv.rooms().writer(room, User{Email: "editor@example.com", Name: "An Editor"})
	txt := doc.GetText("body")
	doc.Transact(func(txn *crdt.Transaction) { txt.Delete(txn, 0, txt.Len()) })

	// The snapshot itself gets through to its 422; every write after it fails
	// until the copy has been refused more than once.
	srv.Quota = &failingQuota{skip: 1, n: 3}
	srv.finalSnapshot(context.Background(), room)
	srv.finalSnapshot(context.Background(), room)
	srv.rooms().drop(room)

	deadline := time.Now().Add(5 * time.Second)
	for {
		var out struct {
			Entries []struct{ Path string } `json:"entries"`
		}
		_ = json.Unmarshal(get(t, h, "/api/p/"+p.ID+"/history?n=50").Body.Bytes(), &out)
		n := 0
		for _, e := range out.Entries {
			if strings.HasPrefix(e.Path, "notes.md.bdrive-conflict-") {
				n++
			}
		}
		if n == 1 {
			return
		}
		if n > 1 || time.Now().After(deadline) {
			t.Fatalf("conflict copies = %d, want exactly 1 after retries", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
