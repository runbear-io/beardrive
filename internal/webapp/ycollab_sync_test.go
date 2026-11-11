package webapp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
	ygoclient "github.com/reearth/ygo/provider/client"

	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/textmerge"
)

func syncAs(t *testing.T, h http.Handler, id, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/p/"+id+"/ycollab/sync?path="+path, bytes.NewReader(data))
	req = withUser(req, User{Email: "editor@example.com", Name: "An Editor"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func roomText(s *Server, room string) string {
	if d := s.ydocs().GetDoc(room); d != nil {
		return d.GetText("body").ToString()
	}
	return ""
}

func conflictTexts(t *testing.T, s *Server, id, path string) map[string]string {
	t.Helper()
	_, v, _ := s.projectVolume(id)
	snap, err := v.current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for p := range snap.files {
		if strings.HasPrefix(p, path+".bdrive-conflict-") {
			out[p] = get(t, s.Handler(), "/api/p/"+id+"/file?path="+p).Body.String()
		}
	}
	return out
}

func waitUntil(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return cond()
}

// A sync whose snapshot the upload door refuses must say so. Answering ok
// would have the browser drop its dirty state for a write that never landed;
// relaying the door's 422 is what makes it ask the user, and the confirmed
// retry then lands.
func TestYCollabSyncRelaysTheDoorsRefusal(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	path := "notes.md"
	room := p.ID + "/" + path
	t.Cleanup(func() { srv.dropRoom(room) }) // before newHub's settle: a resident room keeps its lease renewing
	big := strings.Repeat("a line somebody wrote\n", 100)
	sha := shaOf(big)
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path="+path, big, "")

	rec := syncAs(t, h, p.ID, path, map[string]any{"base": sha, "base_text": big, "text": "tiny\n"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unconfirmed shrink: %d %s, want 422", rec.Code, rec.Body)
	}
	var refusal struct {
		Shrink bool `json:"shrink"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &refusal); err != nil {
		t.Fatal(err)
	}
	if !refusal.Shrink {
		t.Fatalf("the refusal does not say shrink: %s", rec.Body)
	}
	if f := get(t, h, "/api/p/"+p.ID+"/file?path="+path).Body.String(); f != big {
		t.Fatalf("the file changed under a refused write: %q", f)
	}
	// Not closed on a refusal: a final snapshot now would park the
	// unconfirmed text as a conflict copy before anyone could confirm it.
	if _, _, ok := srv.rooms().get(room); !ok {
		t.Fatal("the refused room was closed")
	}

	rec = syncAs(t, h, p.ID, path, map[string]any{"base": sha, "base_text": big, "text": "tiny\n", "confirm_shrink": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("confirmed shrink: %d %s", rec.Code, rec.Body)
	}
	if f := get(t, h, "/api/p/"+p.ID+"/file?path="+path).Body.String(); f != "tiny\n" {
		t.Fatalf("file = %q after the confirmed shrink", f)
	}
	if slices.Contains(srv.ydocs().Rooms(), room) {
		t.Fatal("a room only this sync used is still resident after it was written")
	}
	if n := len(conflictTexts(t, srv, p.ID, path)); n != 0 {
		t.Fatalf("%d conflict copies, want 0", n)
	}
}

// A truncated read that handed the editor "" alongside a non-empty file's ETag
// must hit the 422 shrink guard (asking the user) rather than 409-parking "x".
func TestYCollabSyncTruncatedEmptyBaseTextHitsShrinkGuard(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	path := "daily.md"
	room := p.ID + "/" + path
	t.Cleanup(func() { srv.dropRoom(room) })
	big := "## Snow\n" + strings.Repeat("- shipped the thing\n", 60)
	sha := shaOf(big)
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path="+path, big, "")

	rec := syncAs(t, h, p.ID, path, map[string]any{"base": sha, "base_text": "", "text": "x"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("truncated empty base_text: %d %s, want 422 shrink refusal", rec.Code, rec.Body)
	}
	if f := get(t, h, "/api/p/"+p.ID+"/file?path="+path).Body.String(); f != big {
		t.Fatalf("file changed under truncated-read shrink refusal: %q", f)
	}
}

// A room a sync opened is closed on the way out — but never from under a
// socket that is in it or joining it (handleYCollab holds the room for its
// whole connection).
func TestYCollabSyncLeavesAJoinedRoomOpen(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	path := "notes.md"
	room := p.ID + "/" + path
	t.Cleanup(func() { srv.dropRoom(room) })
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path="+path, "one\n", "")

	leave := srv.rooms().enter(room) // a socket mid-join
	rec := syncAs(t, h, p.ID, path, map[string]any{"base": shaOf("one\n"), "base_text": "one\n", "text": "one\ntwo\n"})
	if rec.Code != http.StatusOK {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body)
	}
	if !slices.Contains(srv.ydocs().Rooms(), room) {
		t.Fatal("the sync closed a room a socket was joining")
	}
	leave()

	rec = syncAs(t, h, p.ID, path, map[string]any{"base": shaOf("one\ntwo\n"), "base_text": "one\ntwo\n", "text": "one\ntwo\nthree\n"})
	if rec.Code != http.StatusOK {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body)
	}
	if slices.Contains(srv.ydocs().Rooms(), room) {
		t.Fatal("an unused room stayed resident after the sync")
	}
	if f := get(t, h, "/api/p/"+p.ID+"/file?path="+path).Body.String(); f != "one\ntwo\nthree\n" {
		t.Fatalf("file = %q", f)
	}
}

// base_text present but empty is an ancestor that was a blank file — never
// "absent, read the blob named by base instead".
func TestYCollabSyncEmptyBaseTextIsTheAncestor(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	path := "notes.md"
	room := p.ID + "/" + path
	t.Cleanup(func() { srv.dropRoom(room) })
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path="+path, "a\n", "")

	// Against the blob ("a\n" == the room) this would overwrite to "hello\n";
	// against "" both sides added, so the room's "a\n" must survive.
	merged, ok := textmerge.Merge(nil, []byte("a\n"), []byte("hello\n"))
	rec := syncAs(t, h, p.ID, path, map[string]any{"base": shaOf("a\n"), "base_text": "", "text": "hello\n"})
	file := get(t, h, "/api/p/"+p.ID+"/file?path="+path).Body.String()
	if !ok {
		if rec.Code != http.StatusConflict {
			t.Fatalf("sync: %d %s, want 409 (merged against \"\")", rec.Code, rec.Body)
		}
		if file != "a\n" {
			t.Fatalf("file = %q after a refused merge", file)
		}
		return
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body)
	}
	if file != string(merged) || file == "hello\n" {
		t.Fatalf("file = %q, want %q (merged against \"\", not the base blob)", file, merged)
	}
}

// retryBlobs fails blob writes while fail says so, keeping compare-and-swap.
type retryBlobs struct {
	remote.Backend
	cond remote.ConditionalPutter
	fail func() bool
}

func (b *retryBlobs) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if strings.Contains(key, "blobs/") && b.fail() {
		return errors.New("storage unavailable")
	}
	return b.Backend.Put(ctx, key, r, size)
}

func (b *retryBlobs) GetVersioned(ctx context.Context, key string) (io.ReadCloser, remote.Version, error) {
	return b.cond.GetVersioned(ctx, key)
}

func (b *retryBlobs) PutIf(ctx context.Context, key string, r io.Reader, size int64, expect remote.Version) (remote.Version, error) {
	if strings.Contains(key, "blobs/") && b.fail() {
		return remote.VersionAbsent, errors.New("storage unavailable")
	}
	return b.cond.PutIf(ctx, key, r, size, expect)
}

// countingRelay counts the whole-state republishes a failing room makes.
type countingRelay struct {
	cluster.Relay
	syncs atomic.Int32
}

func (c *countingRelay) Publish(ctx context.Context, out cluster.Outbound) error {
	if out.Kind == cluster.KindSync {
		c.syncs.Add(1)
	}
	return c.Relay.Publish(ctx, out)
}

// A room whose writes keep failing keeps trying — at a capped interval and
// republishing its state each time — for as long as it lives unwritten:
// past the eight attempts that used to be the end of it.
func TestYCollabRoomRetriesPastEightFailuresUntilStorageRecovers(t *testing.T) {
	fastRoom(t, 5*time.Millisecond, 50*time.Millisecond)
	oldMax := snapshotRetryMax
	snapshotRetryMax = 20 * time.Millisecond
	t.Cleanup(func() { snapshotRetryMax = oldMax })
	var attempts, broken atomic.Int32
	snapshotSeam = func(string) { attempts.Add(1) }
	t.Cleanup(func() { snapshotSeam = nil })
	var flaky *retryBlobs
	srv, p, _ := newHub(t, true, func(be remote.Backend) remote.Backend {
		flaky = &retryBlobs{Backend: be, cond: be.(remote.ConditionalPutter), fail: func() bool { return false }}
		return flaky
	})
	relay := &countingRelay{Relay: cluster.NewMemRelay()}
	if err := srv.useCollabRelay(relay); err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	path := "notes.md"
	url := "/api/p/" + p.ID + "/file?path=" + path
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path="+path, "base\n", "")
	// Attempts 1–9 fail; storage recovers on attempt 10.
	flaky.fail = func() bool {
		if attempts.Load() < 10 {
			broken.Add(1)
			return true
		}
		return false
	}
	doc := openRoom(t, srv, p.ID+"/"+path)
	typeInto(doc, "typed\n")
	if !waitFile(t, h, url, "base\ntyped\n", 5*time.Second) {
		t.Fatalf("never written: %d attempts, file %q", attempts.Load(), get(t, h, url).Body.String())
	}
	if n := attempts.Load(); n < 10 {
		t.Fatalf("written after %d attempts, want ≥10", n)
	}
	if broken.Load() < 9 {
		t.Fatalf("%d failed writes, want ≥9", broken.Load())
	}
	if n := relay.syncs.Load(); n < 9 {
		t.Fatalf("republished %d times across 9 failures", n)
	}
	// Written: the chain ends.
	time.Sleep(100 * time.Millisecond)
	before := attempts.Load()
	time.Sleep(200 * time.Millisecond)
	if after := attempts.Load(); after != before {
		t.Fatalf("still snapshotting after the write landed: %d → %d", before, after)
	}
}

// A sync landing on a process that does not hold the room's lease is ok only
// once the holder's write of it is on storage — and never when the holder
// cannot write it.
func TestYCollabSyncNonHolderWaitsForDurableWriteAndRefusesWhenHolderBlocked(t *testing.T) {
	fastRoom(t, 50*time.Millisecond, 500*time.Millisecond)
	oldWait := ycollabSyncDurableWait
	ycollabSyncDurableWait = 1500 * time.Millisecond
	t.Cleanup(func() { ycollabSyncDurableWait = oldWait })
	a, b, p := newHubPair(t)
	relay := cluster.NewMemRelay()
	for _, s := range []*Server{a, b} {
		if err := s.useCollabRelay(relay); err != nil {
			t.Fatal(err)
		}
	}
	ha, hb := a.Handler(), b.Handler()
	path := "notes.md"
	room := p.ID + "/" + path
	url := "/api/p/" + p.ID + "/file?path=" + path
	putContent(t, ha, "/api/p/"+p.ID+"/upload/content?path="+path, "base\n", "")
	a.rooms().writer(room, coEditor)
	// A live peer on A: A writes the room and takes the lease.
	if err := a.ydocs().Apply(context.Background(), room, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		txt := doc.GetText("body")
		transact(func(txn *crdt.Transaction) { txt.Insert(txn, txt.Len(), "from A\n", nil) })
	}); err != nil {
		t.Fatal(err)
	}
	if !waitFile(t, ha, url, "base\nfrom A\n", 5*time.Second) {
		t.Fatal("A never wrote its edit")
	}
	if b.holdLease(context.Background(), room) {
		t.Fatal("B took the lease A holds")
	}

	t.Run("holder writes", func(t *testing.T) {
		want := "base\nfrom A\nfrom B\n"
		rec := syncAs(t, hb, p.ID, path, map[string]any{"base": shaOf("base\nfrom A\n"), "base_text": "base\nfrom A\n", "text": want})
		if rec.Code != http.StatusOK {
			t.Fatalf("sync via B: %d %s", rec.Code, rec.Body)
		}
		var out struct {
			SHA string `json:"sha"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.SHA != shaOf(want) {
			t.Fatalf("B answered sha %s, want the durable %s", out.SHA, shaOf(want))
		}
		if f := get(t, ha, url).Body.String(); f != want {
			t.Fatalf("ok answered but the file says %q", f)
		}
	})

	t.Run("holder blocked", func(t *testing.T) {
		a.Quota = fullQuota{}
		t.Cleanup(func() { a.Quota = nil })
		before := get(t, ha, url).Body.String()
		rec := syncAs(t, hb, p.ID, path, map[string]any{"base": shaOf(before), "base_text": before, "text": before + "unwritten\n"})
		if rec.Code < 300 {
			t.Fatalf("sync via B acknowledged a write A cannot make: %d %s", rec.Code, rec.Body)
		}
		if rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusForbidden &&
			rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusBadGateway {
			t.Fatalf("sync via B: %d %s", rec.Code, rec.Body)
		}
		if f := get(t, ha, url).Body.String(); f != before {
			t.Fatalf("file = %q under a refused write", f)
		}
	})

	for _, s := range []*Server{a, b} {
		s.Close(context.Background())
		s.rooms().settle()
	}
}

// Without a relay the holder never hears this process's update: refused at
// once, never acknowledged.
func TestYCollabSyncNonHolderWithoutRelayRefuses(t *testing.T) {
	a, _, p := newHubPair(t)
	room := p.ID + "/notes.md"
	if _, fail := a.awaitDurable(context.Background(), room, "x\n", nil, true); fail == nil || fail.code != http.StatusServiceUnavailable {
		t.Fatalf("awaitDurable without a relay = %+v, want 503", fail)
	}
}

// An outside write racing a sync is merged into the live document by
// reconcile, which leaves writing the merge to the room's next pause. The
// sync's ok means durable, so it answers only once the merge is on storage —
// and refuses when that write fails.
func TestYCollabSyncOkOnlyOnceTheReconciledMergeIsWritten(t *testing.T) {
	base := "l1\nl2\nl3\nl4\nl5\n"
	room := "L1 from the room\nl2\nl3\nl4\nl5\n"
	outside := "l1\nl2\nl3\nl4\nL5 from outside\n"
	merged := "L1 from the room\nl2\nl3\nl4\nL5 from outside\n"

	run := func(t *testing.T, failAfterReconcile bool) {
		fastRoom(t, time.Hour, time.Hour)
		var flaky *retryBlobs
		var broken atomic.Bool
		srv, p, _ := newHub(t, true, func(be remote.Backend) remote.Backend {
			flaky = &retryBlobs{Backend: be, cond: be.(remote.ConditionalPutter), fail: broken.Load}
			return flaky
		})
		h := srv.Handler()
		path := "notes.md"
		up := "/api/p/" + p.ID + "/upload/content?path=" + path
		file := "/api/p/" + p.ID + "/file?path=" + path
		t.Cleanup(func() { srv.dropRoom(p.ID + "/" + path) })
		putContent(t, h, up, base, "")

		var calls atomic.Int32
		snapshotSeam = func(string) {
			switch calls.Add(1) {
			case 1: // an agent writes the file while the sync snapshots
				if rec := putContent(t, h, up, outside, shaOf(base)); rec.Code != http.StatusOK {
					t.Errorf("outside write: %d %s", rec.Code, rec.Body)
				}
			case 2:
				broken.Store(failAfterReconcile)
			}
		}
		t.Cleanup(func() { snapshotSeam = nil; broken.Store(false) })

		rec := syncAs(t, h, p.ID, path, map[string]any{"base": shaOf(base), "base_text": base, "text": room})
		got := get(t, h, file).Body.String()
		if failAfterReconcile {
			if rec.Code/100 == 2 {
				t.Fatalf("sync answered %d with the merge unwritten (file %q)", rec.Code, got)
			}
			if got == merged {
				t.Fatalf("the merge was written through a failing store")
			}
			return
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("sync: %d %s", rec.Code, rec.Body)
		}
		if got != merged {
			t.Fatalf("sync answered ok while the file holds %q, want the merge %q", got, merged)
		}
		var out struct{ SHA, Text string }
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.SHA != shaOf(merged) || out.Text != merged {
			t.Fatalf("answered sha %s text %q, want the merge's", out.SHA, out.Text)
		}
	}
	t.Run("written", func(t *testing.T) { run(t, false) })
	t.Run("storage fails", func(t *testing.T) { run(t, true) })
}

// A sync that fails on storage may already have put its text into the live
// room. A peek tells the browser what the room holds — text and Yjs state —
// without writing, so it answers 200 while blob writes still fail.
func TestYCollabSyncPeekReadsTheRoomWhileStorageFails(t *testing.T) {
	fastRoom(t, time.Hour, time.Hour)
	var broken atomic.Bool
	srv, p, _ := newHub(t, true, func(be remote.Backend) remote.Backend {
		return &retryBlobs{Backend: be, cond: be.(remote.ConditionalPutter), fail: broken.Load}
	})
	h := srv.Handler()
	path := "notes.md"
	file := "/api/p/" + p.ID + "/file?path=" + path
	t.Cleanup(func() { srv.dropRoom(p.ID + "/" + path); broken.Store(false) })
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path="+path, "log\n", "")

	broken.Store(true)
	rec := syncAs(t, h, p.ID, path, map[string]any{"base": shaOf("log\n"), "base_text": "log\n", "text": "log\nx\n"})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("sync through a failing store: %d %s, want 502", rec.Code, rec.Body)
	}
	if f := get(t, h, file).Body.String(); f != "log\n" {
		t.Fatalf("file = %q under a failing store", f)
	}

	rec = syncAs(t, h, p.ID, path, map[string]any{"peek": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("peek: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		OK     bool   `json:"ok"`
		SHA    string `json:"sha"`
		Text   string `json:"text"`
		Update string `json:"update"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.Text != "log\nx\n" {
		t.Fatalf("peek = %+v, want the room's text with the failed sync in it", out)
	}
	if out.SHA != shaOf("log\n") {
		t.Fatalf("peek sha = %s, want the room's unchanged base %s", out.SHA, shaOf("log\n"))
	}
	raw, err := base64.StdEncoding.DecodeString(out.Update)
	if err != nil {
		t.Fatal(err)
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, raw, nil); err != nil {
		t.Fatal(err)
	}
	if got := doc.GetText("body").ToString(); got != "log\nx\n" {
		t.Fatalf("peek update holds %q", got)
	}
	if f := get(t, h, file).Body.String(); f != "log\n" {
		t.Fatalf("a peek wrote the file: %q", f)
	}
	if _, _, held := srv.rooms().get(p.ID + "/" + path); !held {
		t.Fatal("a peek closed the room holding a failed write")
	}
}

// A keystroke landing while reconcile merges an outside write used to make
// moveDoc refuse the move; it now re-merges the document as it stands, inside
// the Apply that moves it, so the keystroke and the outside write both land in
// the room. The sync's ok must still rest on storage holding the edit, never
// on the room looking settled — so a store that fails after the reconcile is
// a refusal, and one that works answers the sha that holds it.
func TestYCollabSyncDurableDespiteMoveDocRefusal(t *testing.T) {
	base := "l1\nl2\nl3\nl4\nl5\n"
	room := "L1 from the room\nl2\nl3\nl4\nl5\n"
	outside := "l1\nl2\nl3\nl4\nL5 from outside\n"
	final := "L1 from the room\nl2\nl3 typed mid-reconcile\nl4\nL5 from outside\n"

	run := func(t *testing.T, failAfterReconcile bool) {
		fastRoom(t, time.Hour, time.Hour)
		oldWait := ycollabSyncDurableWait
		ycollabSyncDurableWait = 300 * time.Millisecond
		t.Cleanup(func() { ycollabSyncDurableWait = oldWait })
		var broken atomic.Bool
		srv, p, _ := newHub(t, true, func(be remote.Backend) remote.Backend {
			return &retryBlobs{Backend: be, cond: be.(remote.ConditionalPutter), fail: broken.Load}
		})
		h := srv.Handler()
		path := "notes.md"
		roomID := p.ID + "/" + path
		up := "/api/p/" + p.ID + "/upload/content?path=" + path
		file := "/api/p/" + p.ID + "/file?path=" + path
		t.Cleanup(func() { srv.dropRoom(roomID) })
		putContent(t, h, up, base, "")

		var calls atomic.Int32
		snapshotSeam = func(string) {
			switch calls.Add(1) {
			case 1:
				// An agent writes the file, and a co-editor types, after this
				// snapshot read the document: reconcile merged against text
				// the document no longer spells, and has to merge again.
				if rec := putContent(t, h, up, outside, shaOf(base)); rec.Code != http.StatusOK {
					t.Errorf("outside write: %d %s", rec.Code, rec.Body)
				}
				if err := srv.ydocs().Apply(context.Background(), roomID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
					txt := doc.GetText("body")
					cur := txt.ToString()
					spliceVia(transact, txt, cur, strings.Replace(cur, "l3\n", "l3 typed mid-reconcile\n", 1))
				}); err != nil {
					t.Errorf("concurrent keystroke: %v", err)
				}
			case 2:
				broken.Store(failAfterReconcile)
			}
		}
		t.Cleanup(func() { snapshotSeam = nil; broken.Store(false) })

		rec := syncAs(t, h, p.ID, path, map[string]any{"base": shaOf(base), "base_text": base, "text": room})
		got := get(t, h, file).Body.String()
		if failAfterReconcile {
			if rec.Code/100 == 2 {
				t.Fatalf("sync answered %d with the edit only in memory (file %q)", rec.Code, got)
			}
			if strings.Contains(got, "L1 from the room") {
				t.Fatalf("the edit was written through a failing store: %q", got)
			}
			return
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("sync: %d %s", rec.Code, rec.Body)
		}
		if got != final {
			t.Fatalf("sync answered ok while the file holds %q, want %q", got, final)
		}
		var out struct{ SHA string }
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.SHA != shaOf(final) {
			t.Fatalf("answered sha %s, want the durable %s", out.SHA, shaOf(final))
		}
		// one snapshot reconciles (keystroke and outside write together), the
		// next writes the merge: one snapshot could not have held it
		if n := calls.Load(); n < 2 {
			t.Fatalf("%d snapshots: the mid-reconcile keystroke was not exercised", n)
		}
	}
	t.Run("written", func(t *testing.T) { run(t, false) })
	t.Run("storage fails", func(t *testing.T) { run(t, true) })
}

// A keystroke landing after moveDocLive read the room and before its move
// lands must survive the move, and the move must land where it was meant to.
// ygo's Apply holds the document's lock only inside transact, so a move made
// in character offsets of the text read before it would be shifted by that
// keystroke — here, by eleven characters, deleting "CONCURRENT " text or the
// wrong line. The move is an update anchored to items, so it is not.
func TestMoveDocLiveConcurrentKeystrokeDuringRecompute(t *testing.T) {
	fastRoom(t, time.Hour, time.Hour)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	path := "notes.md"
	room := p.ID + "/" + path
	const base = "l1\nl2\nl3\n"
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path="+path, base, "")
	t.Cleanup(func() { srv.dropRoom(room) })
	insertAt := func(i int, s string) error {
		return srv.ydocs().Apply(context.Background(), room, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			txt := doc.GetText("body")
			if i < 0 {
				i = txt.Len()
			}
			transact(func(txn *crdt.Transaction) { txt.Insert(txn, i, s, nil) })
		})
	}
	// Typed after the snapshot read base: the move has to be recomputed.
	if err := insertAt(-1, "- early\n"); err != nil {
		t.Fatal(err)
	}
	if got := roomText(srv, room); got != base+"- early\n" {
		t.Fatalf("room = %q", got)
	}

	var recomputed string
	recompute := func(cur string) (string, bool, bool) {
		recomputed = cur
		// Another peer types at the very start while the move is between
		// reading the room and landing in it.
		done := make(chan error, 1)
		go func() { done <- insertAt(0, "CONCURRENT ") }()
		if err := <-done; err != nil {
			t.Errorf("concurrent keystroke: %v", err)
		}
		return strings.Replace(cur, "l3\n", "L3 reconciled\n", 1), false, true
	}
	theirs := shaOf("theirs")
	from, landed, moved := srv.moveDocLive(context.Background(), room, base, strings.Replace(base, "l3\n", "L3 reconciled\n", 1), theirs, false, recompute)
	if !moved {
		t.Fatal("the move was refused")
	}
	if recomputed != base+"- early\n" || from != recomputed {
		t.Fatalf("recomputed from %q (from %q), want the room as it stood", recomputed, from)
	}
	if landed != "l1\nl2\nL3 reconciled\n- early\n" {
		t.Fatalf("landed = %q", landed)
	}
	const want = "CONCURRENT l1\nl2\nL3 reconciled\n- early\n"
	if got := roomText(srv, room); got != want {
		t.Fatalf("room after the move = %q, want %q: the move landed at shifted offsets", got, want)
	}
	if got := srv.rooms().base(room); got != theirs {
		t.Fatalf("room base = %s, want the sha it moved onto", got)
	}
}

// A peer typing into the very line the move replaces, between the move
// reading the room and landing in it, merges with the move the way two
// peers' keystrokes merge: the move's delete set names only the line's old
// items, so the peer's characters survive beside the new line, and the
// outside version's line is in too. Nothing is lost, so the room rebases onto
// the version it moved to — its next pause writes both on top of it — rather
// than staying on a base it no longer stands on and parking a copy of text
// the file already holds.
func TestMoveDocLiveSameLineConcurrentKeystrokeKeepsBothEdits(t *testing.T) {
	fastRoom(t, time.Hour, time.Hour)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	path := "notes.md"
	room := p.ID + "/" + path
	const base = "l1\nl2\nl3\n"
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path="+path, base, "")
	t.Cleanup(func() { srv.dropRoom(room) })
	edit := func(fn func(txt *crdt.YText, txn *crdt.Transaction, cur string)) error {
		return srv.ydocs().Apply(context.Background(), room, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			txt := doc.GetText("body")
			cur := txt.ToString()
			transact(func(txn *crdt.Transaction) { fn(txt, txn, cur) })
		})
	}
	if err := edit(func(txt *crdt.YText, txn *crdt.Transaction, cur string) { txt.Insert(txn, len(cur), "- early\n", nil) }); err != nil {
		t.Fatal(err)
	}

	const outside = "l1\nl2\nL3 from outside\n"
	recompute := func(cur string) (string, bool, bool) {
		done := make(chan error, 1)
		go func() {
			done <- edit(func(txt *crdt.YText, txn *crdt.Transaction, cur string) {
				txt.Insert(txn, strings.Index(cur, "l3")+len("l3"), " from peer", nil)
			})
		}()
		if err := <-done; err != nil {
			t.Errorf("concurrent keystroke: %v", err)
		}
		return strings.Replace(cur, "l3\n", "L3 from outside\n", 1), false, true
	}
	theirs := shaOf(outside)
	_, landed, moved := srv.moveDocLive(context.Background(), room, base, outside, theirs, true, recompute)
	if !moved || landed != "l1\nl2\nL3 from outside\n- early\n" {
		t.Fatalf("moved %v to %q", moved, landed)
	}
	got := roomText(srv, room)
	for _, want := range []string{"l1\nl2\n", "L3 from outside", " from peer", "- early\n"} {
		if strings.Count(got, want) != 1 {
			t.Fatalf("room = %q: want %q in it exactly once", got, want)
		}
	}
	if strings.Contains(got, "l3") {
		t.Fatalf("room = %q: the replaced line came back", got)
	}
	if b := srv.rooms().base(room); b != theirs {
		t.Fatalf("room base = %s, want %s: the document holds their version", b, theirs)
	}
}

// A closing tab's last checkpoint that clashes with the live room is kept as a
// conflict copy by the hub, in that one request — the page is gone before it
// could answer a 409 with a second one. The room keeps its own text, the copy
// holds the caller's, and quota still decides whether the copy is stored.
func TestYCollabSyncParkOnConflictWritesCopyInOneRequest(t *testing.T) {
	fastRoom(t, time.Hour, time.Hour)
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	path := "notes.md"
	room := p.ID + "/" + path
	t.Cleanup(func() { srv.dropRoom(room) })
	const base = "l1\nl2\nl3\n"
	const live = "l1 from the room\nl2\nl3\n"
	const mine = "l1 from the closing tab\nl2\nl3\n"
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path="+path, base, "")

	leave := srv.rooms().enter(room) // a socket in the room: it stays open
	t.Cleanup(leave)
	if err := srv.ydocs().Apply(context.Background(), room, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		txt := doc.GetText("body")
		spliceVia(transact, txt, txt.ToString(), live)
	}); err != nil {
		t.Fatal(err)
	}
	checkpoint := func(park bool) *httptest.ResponseRecorder {
		return syncAs(t, h, p.ID, path, map[string]any{"base": shaOf(base), "base_text": base, "text": mine, "park_on_conflict": park})
	}

	if rec := checkpoint(false); rec.Code != http.StatusConflict {
		t.Fatalf("clash without park_on_conflict: %d %s, want 409", rec.Code, rec.Body)
	}
	if cc := conflictTexts(t, srv, p.ID, path); len(cc) != 0 {
		t.Fatalf("a 409 parked %d copies", len(cc))
	}

	rec := checkpoint(true)
	if rec.Code != http.StatusOK {
		t.Fatalf("clash with park_on_conflict: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		OK     bool   `json:"ok"`
		Text   string `json:"text"`
		Parked string `json:"parked"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || !strings.HasPrefix(out.Parked, path+".bdrive-conflict-") {
		t.Fatalf("answer %s: want ok naming the conflict copy", rec.Body)
	}
	if out.Text != live {
		t.Fatalf("answered text %q, want the room's %q", out.Text, live)
	}
	if got := get(t, h, "/api/p/"+p.ID+"/file?path="+out.Parked).Body.String(); got != mine {
		t.Fatalf("copy %s = %q, want the caller's %q", out.Parked, got, mine)
	}
	if got := roomText(srv, room); got != live {
		t.Fatalf("room = %q after the park, want it untouched", got)
	}
	if got := get(t, h, "/api/p/"+p.ID+"/file?path="+path).Body.String(); got != base {
		t.Fatalf("file = %q after the park, want it untouched", got)
	}
	if cc := conflictTexts(t, srv, p.ID, path); len(cc) != 1 {
		t.Fatalf("%d conflict copies, want the one parked", len(cc))
	}

	// A second tab of the same account closes in the same second with a
	// different clash: its copy takes the next free name and the first tab's
	// copy is not replaced.
	const mine2 = "l1 from the second closing tab\nl2\nl3\n"
	rec = syncAs(t, h, p.ID, path, map[string]any{"base": shaOf(base), "base_text": base, "text": mine2, "park_on_conflict": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("second tab's park: %d %s", rec.Code, rec.Body)
	}
	var out2 struct {
		Parked string `json:"parked"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out2); err != nil {
		t.Fatal(err)
	}
	if out2.Parked == out.Parked {
		t.Fatalf("both tabs parked at %s", out.Parked)
	}
	conflictStamp(t, path, out.Parked)
	conflictStamp(t, path, out2.Parked)
	cc := conflictTexts(t, srv, p.ID, path)
	if len(cc) != 2 || cc[out.Parked] != mine || cc[out2.Parked] != mine2 {
		t.Fatalf("conflict copies %v: want the first tab's at %s and the second's at %s", cc, out.Parked, out2.Parked)
	}

	// A full org: the copy is refused like any write, and nothing is stored.
	srv.Quota = fullQuota{}
	t.Cleanup(func() { srv.Quota = nil })
	const again = "l1 from a tab over quota\nl2\nl3\n"
	rec = syncAs(t, h, p.ID, path, map[string]any{"base": shaOf(base), "base_text": base, "text": again, "park_on_conflict": true})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("park over quota: %d %s, want 403", rec.Code, rec.Body)
	}
	for cp, txt := range conflictTexts(t, srv, p.ID, path) {
		if txt == again {
			t.Fatalf("%s holds the over-quota text", cp)
		}
	}
}

// An update whose result is past maxSeedBytes is refused before the live room
// takes it: an applied update cannot be taken back, and a room that large
// could be neither snapshotted whole nor seeded again.
func TestYCollabSyncRejectsUpdateExceedingMaxSeedBytes(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	path := "notes.md"
	room := p.ID + "/" + path
	t.Cleanup(func() { srv.dropRoom(room) })
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path="+path, "a\n", "")

	big := crdt.New()
	typeInto(big, strings.Repeat("x", maxSeedBytes))
	update := big.EncodeStateAsUpdate()
	if len(update) > maxHistoryBytes {
		t.Fatalf("update is %d bytes: refused by the wire bound, not the text bound", len(update))
	}
	leave := srv.rooms().enter(room) // keeps the room resident to look at afterwards
	defer leave()
	rec := syncAs(t, h, p.ID, path, map[string]any{"update": base64.StdEncoding.EncodeToString(update)})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("sync: %d %s, want 413", rec.Code, rec.Body)
	}
	var refusal struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &refusal); err != nil {
		t.Fatal(err)
	}
	if refusal.Text != "a\n" {
		t.Fatalf("refusal text = %q, want the room's unchanged \"a\\n\"", refusal.Text)
	}
	doc, _, held := srv.rooms().get(room)
	if !held {
		t.Fatal("the room is not resident")
	}
	if body := doc.GetText("body").ToString(); body != "a\n" {
		t.Fatalf("the live room took the refused update: %d bytes of body", len(body))
	}
	if f := get(t, h, "/api/p/"+p.ID+"/file?path="+path).Body.String(); f != "a\n" {
		t.Fatalf("file = %d bytes after a refused update", len(f))
	}
}

// slowBlobs holds, once armed, every blob write carrying marker until release
// is closed — a storage write that hangs.
type slowBlobs struct {
	remote.Backend
	cond    remote.ConditionalPutter
	marker  []byte
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *slowBlobs) hold(key string, r io.Reader) (io.Reader, error) {
	if !b.armed.Load() || !strings.Contains(key, "blobs/") {
		return r, nil
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if bytes.Contains(data, b.marker) {
		b.once.Do(func() { close(b.entered) })
		<-b.release
	}
	return bytes.NewReader(data), nil
}

func (b *slowBlobs) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	r, err := b.hold(key, r)
	if err != nil {
		return err
	}
	return b.Backend.Put(ctx, key, r, size)
}

func (b *slowBlobs) GetVersioned(ctx context.Context, key string) (io.ReadCloser, remote.Version, error) {
	return b.cond.GetVersioned(ctx, key)
}

func (b *slowBlobs) PutIf(ctx context.Context, key string, r io.Reader, size int64, expect remote.Version) (remote.Version, error) {
	r, err := b.hold(key, r)
	if err != nil {
		return remote.VersionAbsent, err
	}
	return b.cond.PutIf(ctx, key, r, size, expect)
}

// A room's close is a final snapshot, a storage round-trip. While it hangs,
// only an enter of THAT room waits: every other room still joins, leaves and
// syncs.
func TestYCollabCloseRoomDoesNotBlockOtherRooms(t *testing.T) {
	fastRoom(t, time.Hour, time.Hour) // only the close writes room A
	slow := &slowBlobs{marker: []byte("held-on-storage"), entered: make(chan struct{}), release: make(chan struct{})}
	srv, p, _ := newHub(t, true, func(be remote.Backend) remote.Backend {
		slow.Backend, slow.cond = be, be.(remote.ConditionalPutter)
		return slow
	})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(slow.release) }) }
	h := srv.Handler()
	roomA, roomB := p.ID+"/a.md", p.ID+"/b.md"
	t.Cleanup(func() { srv.dropRoom(roomA); srv.dropRoom(roomB) })
	t.Cleanup(unblock) // runs first: nothing is left hanging on storage
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=a.md", "a\n", "")
	putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=b.md", "b\n", "")

	// Room A, edited with nobody in it: its close owes a final snapshot.
	if err := srv.ydocs().Apply(context.Background(), roomA, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		txt := doc.GetText("body")
		transact(func(txn *crdt.Transaction) { txt.Insert(txn, txt.Len(), "held-on-storage\n", nil) })
	}); err != nil {
		t.Fatal(err)
	}
	// A snapshot with no recorded writer is skipped; name the editor so the
	// close really owes room A's final write.
	srv.rooms().writer(roomA, User{Email: "alice@example.com", Name: "Alice"})
	slow.armed.Store(true)
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		srv.rooms().closeIfUnused(roomA, func() { _ = srv.ydocs().CloseRoom(roomA, false) })
	}()
	select {
	case <-slow.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("room A's close never reached its final write")
	}

	within := func(what string, f func()) {
		t.Helper()
		done := make(chan struct{})
		go func() { defer close(done); f() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			unblock()
			<-done
			t.Fatalf("%s waited on room A's close", what)
		}
	}
	within("an enter and leave of room B", func() { srv.rooms().enter(roomB)() })
	data, err := json.Marshal(map[string]any{"base": shaOf("b\n"), "base_text": "b\n", "text": "b\nmore\n"})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	within("a sync of room B", func() {
		req := httptest.NewRequest("POST", "/api/p/"+p.ID+"/ycollab/sync?path=b.md", bytes.NewReader(data))
		h.ServeHTTP(rec, withUser(req, User{Email: "editor@example.com", Name: "An Editor"}))
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("sync of room B: %d %s", rec.Code, rec.Body)
	}
	if f := get(t, h, "/api/p/"+p.ID+"/file?path=b.md").Body.String(); f != "b\nmore\n" {
		t.Fatalf("b.md = %q", f)
	}

	// Room A itself is held: an enter waits for its close, then gets in.
	joined := make(chan struct{})
	go func() {
		leave := srv.rooms().enter(roomA)
		close(joined)
		leave()
	}()
	select {
	case <-joined:
		t.Fatal("an enter of room A ran during its close")
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	for what, ch := range map[string]chan struct{}{"room A's close": closed, "the enter of room A": joined} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s never finished once storage answered", what)
		}
	}
	if !waitFile(t, h, "/api/p/"+p.ID+"/file?path=a.md", "a\nheld-on-storage\n", 2*time.Second) {
		t.Fatalf("a.md = %q after its close", get(t, h, "/api/p/"+p.ID+"/file?path=a.md").Body.String())
	}
}

// meetingAuth is a provider for a hub whose every member is a known teammate:
// a request names who it is with a bearer token, so each editor's websocket,
// sync and read reaches the hub as that person — the room's writer is a real
// account, as it is in production, never the anonymous User{} a hub without
// auth hands out (which the snapshot refuses to attribute, so the room would
// never write nor take its lease). An in-process request already carrying a
// user (syncAs's withUser) keeps it.
type meetingAuth struct{ users map[string]User }

func (a *meetingAuth) CLILoginPath() string         { return "/auth/cli" }
func (a *meetingAuth) Register(*http.ServeMux)      {}
func (a *meetingAuth) UseDeviceBinder(DeviceBinder) {}
func (a *meetingAuth) Accounts() []User {
	out := make([]User, 0, len(a.users))
	for _, u := range a.users {
		out = append(out, u)
	}
	return out
}

func (a *meetingAuth) Authenticate(r *http.Request) (User, bool) {
	if u, ok := userFrom(r.Context()); ok {
		return u, true
	}
	u, ok := a.users[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	return u, ok
}

func meetingToken(u User) string { return "meeting-" + u.Email }

func meetingHeader(u User) http.Header {
	return http.Header{"Authorization": {"Bearer " + meetingToken(u)}}
}

// meetingDo is one HTTP request to a hub, made as u.
func meetingDo(t *testing.T, h http.Handler, u User, method, url string, body io.Reader, hdr http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, url, body)
	for k, v := range meetingHeader(u) {
		req.Header[k] = v
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// meetingEditor is one browser tab on the meeting note: a real y-websocket
// client (ygo's provider/client) holding its own document, signed in as one
// teammate.
type meetingEditor struct {
	name string
	user User
	doc  *crdt.Doc
	conn *ygoclient.Client
	done chan struct{}
}

func joinMeeting(t *testing.T, ts *httptest.Server, projectID, path string, u User) *meetingEditor {
	t.Helper()
	doc := crdt.New()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/p/" + projectID + "/ycollab/meeting.md?path=" + path
	c, err := ygoclient.New(ygoclient.Options{URL: url, Doc: doc, Header: meetingHeader(u)})
	if err != nil {
		t.Fatal(err)
	}
	e := &meetingEditor{name: u.Name, user: u, doc: doc, conn: c, done: make(chan struct{})}
	go func() {
		defer close(e.done)
		_ = c.Connect(context.Background())
	}()
	t.Cleanup(e.leave)
	select {
	case <-c.Synced():
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never synced with the hub", u.Name)
	}
	return e
}

// leave closes the tab's socket and waits for its dial loop to end.
func (e *meetingEditor) leave() {
	_ = e.conn.Close()
	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
	}
}

func (e *meetingEditor) text() string { return e.doc.GetText("body").ToString() }

// write inserts line where at says, against the text it was computed from:
// a remote update landing between the read and the transaction would shift
// the index into the middle of somebody else's line, so the transaction only
// commits on an unchanged length (every edit here changes the length) and the
// edit is recomputed otherwise.
func (e *meetingEditor) write(line string, at func(cur string) int) {
	txt := e.doc.GetText("body")
	for {
		cur := txt.ToString()
		i := at(cur)
		wrote := false
		e.doc.Transact(func(txn *crdt.Transaction) {
			if txt.Len() != len(cur) {
				return
			}
			txt.Insert(txn, i, line, nil)
			wrote = true
		})
		if wrote {
			return
		}
	}
}

// meetingSectionEnd is the start of the heading after name's section: a line
// inserted there becomes the section's last line.
func meetingSectionEnd(name string) func(string) int {
	return func(cur string) int {
		h := strings.Index(cur, "\n## "+name+"\n")
		if h < 0 {
			return len(cur)
		}
		start := h + 1
		if j := strings.Index(cur[start:], "\n## "); j >= 0 {
			return start + j + 1
		}
		return len(cur)
	}
}

func meetingDocEnd(cur string) int { return len(cur) }

/*
Eight people in one meeting note, split across two hub processes, while every
way of leaving the room and every outside writer happens mid-meeting.

	Each teammate is signed in as their own account. Alice, Bob, Carol and
	Dave are on hub A, which takes the room's lease with its first write;
	Erin, Frank, Grace and Heidi are on hub B. Grace never opens a socket: she
	is the slow editor staging checkpoints over HTTP. Everyone else types into
	their own section and appends shared action items without pausing, so
	the holder's idle snapshot never fires and only snapshotMax writes.

	(a) Carol presses Done: a /ycollab/sync and a closed socket, after which
	    the file and its rendering already hold her last line.
	(b) Erin's tab dies offline (pagehide keepalive): three lines typed with
	    no socket reach the room through ONE sync sent to the process that
	    does not hold the lease, while everyone keeps typing. That sync waits
	    for the holder's write, which continuous typing defers to
	    snapshotMax — the 503 this pins was ycollabSyncDurableWait at 4s
	    against a 10s snapshotMax.
	(c) Grace stages two checkpoints on a stale base; the second names the
	    first in prior_sent, so its bullet is not inserted twice.
	(d) An agent writes the header with a stale If-Match: merged by the
	    hub, reconciled into the live room.

	The timings are production's, halved, so the relationship between them —
	the thing under test — is the one that ships.
*/
func TestCollabEightPeersMeetingNoteExitChaos(t *testing.T) {
	if ycollabSyncDurableWait <= snapshotMax {
		t.Fatalf("ycollabSyncDurableWait %v must exceed snapshotMax %v: a non-holder's sync during continuous typing would 503", ycollabSyncDurableWait, snapshotMax)
	}
	fastRoom(t, snapshotIdle/2, snapshotMax/2)
	oldWait := ycollabSyncDurableWait
	ycollabSyncDurableWait = oldWait / 2
	t.Cleanup(func() { ycollabSyncDurableWait = oldWait })
	oldRoomIdle := collabRoomIdleTimeout
	collabRoomIdleTimeout = 500 * time.Millisecond
	t.Cleanup(func() { collabRoomIdleTimeout = oldRoomIdle })

	names := []string{"Alice", "Bob", "Carol", "Dave", "Erin", "Frank", "Grace", "Heidi"}
	people := map[string]User{}
	auth := &meetingAuth{users: map[string]User{}}
	for _, n := range append(slices.Clone(names), "Agent") {
		u := User{Email: strings.ToLower(n) + "@example.com", Name: n}
		people[n] = u
		auth.users[meetingToken(u)] = u
	}
	agent := people["Agent"]

	a, b, p := newHubPair(t)
	relay := cluster.NewMemRelay()
	for _, s := range []*Server{a, b} {
		// Before Handler: the auth gate is wired when the handler is built.
		s.Auth = auth
		if err := s.useCollabRelay(relay); err != nil {
			t.Fatal(err)
		}
	}
	ha, hb := a.Handler(), b.Handler()
	tsA, tsB := httptest.NewServer(ha), httptest.NewServer(hb)
	// Registered after newHubPair, so these close before the hubs do.
	t.Cleanup(tsA.Close)
	t.Cleanup(tsB.Close)

	const path = "areas/team/daily-meetings/2026-11-11.md"
	const oldHeader = "# Daily meeting 2026-11-11\n"
	const newHeader = "# Daily meeting 2026-11-11 (release: v0.18.10)\n"
	var seed strings.Builder
	seed.WriteString(oldHeader)
	seed.WriteString("Attendees: " + strings.Join(names, ", ") + "\n")
	for _, n := range names {
		fmt.Fprintf(&seed, "## %s\n- [ ] %s: action items\n", n, n)
	}
	seed.WriteString("## Shared action items\n- [ ] Ship v0.18.10\n")
	room := p.ID + "/" + path
	fileURL := "/api/p/" + p.ID + "/file?path=" + path
	renderURL := "/api/p/" + p.ID + "/render?path=" + path
	uploadURL := "/api/p/" + p.ID + "/upload/content?path=" + path

	read := func(h http.Handler, u User, url string) *httptest.ResponseRecorder {
		t.Helper()
		return meetingDo(t, h, u, "GET", url, nil, nil)
	}
	file := func(h http.Handler) string { return read(h, agent, fileURL).Body.String() }

	if rec := meetingDo(t, ha, agent, "PUT", uploadURL, strings.NewReader(seed.String()), nil); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	if rec := read(ha, agent, fileURL); rec.Code != http.StatusOK || rec.Body.String() != seed.String() {
		t.Fatalf("seeded file reads %d %q", rec.Code, rec.Body)
	}

	var wantMu sync.Mutex
	var want []string
	record := func(line string) {
		wantMu.Lock()
		want = append(want, line)
		wantMu.Unlock()
	}

	// Hub A opens the room first and writes it, which takes the lease.
	alice := joinMeeting(t, tsA, p.ID, path, people["Alice"])
	if got := alice.text(); got != seed.String() {
		t.Fatalf("Alice joined to %q, want the seed", got)
	}
	bob := joinMeeting(t, tsA, p.ID, path, people["Bob"])
	carol := joinMeeting(t, tsA, p.ID, path, people["Carol"])
	dave := joinMeeting(t, tsA, p.ID, path, people["Dave"])
	if _, who, ok := a.rooms().get(room); !ok || who.Email == "" {
		t.Fatalf("A's room has no signed-in writer (%+v): it would never snapshot", who)
	}
	const warm = "- Alice note 0\n"
	alice.write(warm, meetingSectionEnd("Alice"))
	record(warm)
	if !waitUntil(10*time.Second, func() bool { return strings.Contains(file(ha), warm) }) {
		t.Fatalf("A never wrote the first edit: %q", file(ha))
	}
	if !a.holdsLease(room) {
		t.Fatal("A wrote the room without holding its lease")
	}
	erin := joinMeeting(t, tsB, p.ID, path, people["Erin"])
	frank := joinMeeting(t, tsB, p.ID, path, people["Frank"])
	heidi := joinMeeting(t, tsB, p.ID, path, people["Heidi"])
	for _, e := range []*meetingEditor{erin, frank, heidi} {
		if !waitUntil(5*time.Second, func() bool { return strings.Contains(e.text(), warm) }) {
			t.Fatalf("%s on B never caught up: %q", e.name, e.text())
		}
	}
	// holdLease TAKES a free lease, so it is asked only once A is known to
	// hold it: a false here is A's stored, unexpired record refusing B.
	if !a.holdsLease(room) || b.holdLease(context.Background(), room) {
		t.Fatalf("lease: A holds %v, B could take it: the non-holder cases below would test nothing", a.holdsLease(room))
	}

	// Everyone with a socket types without pausing.
	tick := snapshotIdle / 8
	type typing struct{ stop, done chan struct{} }
	startTyping := func(e *meetingEditor, offset int) *typing {
		ty := &typing{stop: make(chan struct{}), done: make(chan struct{})}
		go func() {
			defer close(ty.done)
			for i := 1; ; i++ {
				select {
				case <-ty.stop:
					return
				case <-time.After(tick + time.Duration(offset)*time.Millisecond):
				}
				line := fmt.Sprintf("- %s note %d\n", e.name, i)
				e.write(line, meetingSectionEnd(e.name))
				record(line)
				if i%4 == 0 {
					item := fmt.Sprintf("- [ ] %s follow-up %d\n", e.name, i)
					e.write(item, meetingDocEnd)
					record(item)
				}
			}
		}()
		return ty
	}
	halt := func(ty *typing) {
		close(ty.stop)
		<-ty.done
	}
	typists := map[*meetingEditor]*typing{}
	for i, e := range []*meetingEditor{alice, bob, carol, dave, erin, frank, heidi} {
		typists[e] = startTyping(e, 7*i)
	}
	// A typist still running when the test fails is stopped before the
	// editors' own cleanups close their sockets.
	t.Cleanup(func() {
		for _, ty := range typists {
			halt(ty)
		}
	})
	stopTyping := func(e *meetingEditor) {
		halt(typists[e])
		delete(typists, e)
	}
	time.Sleep(2 * snapshotIdle)

	type syncAnswer struct {
		OK     bool   `json:"ok"`
		Text   string `json:"text"`
		Merged bool   `json:"merged"`
	}
	// syncOK retries only the room's "unintegrated updates" busy answer — a
	// relay frame still in flight — and fails on every other refusal,
	// including the durable-wait 503 this test is about.
	syncOK := func(u User, h http.Handler, body map[string]any) syncAnswer {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		for attempt := 0; ; attempt++ {
			rec := meetingDo(t, h, u, "POST", "/api/p/"+p.ID+"/ycollab/sync?path="+path, bytes.NewReader(data), nil)
			if rec.Code == http.StatusServiceUnavailable && strings.Contains(rec.Body.String(), "unintegrated") && attempt < 10 {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("%s's sync: %d %s", u.Name, rec.Code, rec.Body)
			}
			var out syncAnswer
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if !out.OK {
				t.Fatalf("%s's sync answered %s", u.Name, rec.Body)
			}
			return out
		}
	}

	// (a) Carol: Done. Her last line, a sync, the socket closed — and the
	// very next read, on either process, already has it.
	stopTyping(carol)
	const decision = "- Carol decision: ship Friday\n"
	carol.write(decision, meetingSectionEnd("Carol"))
	record(decision)
	syncOK(carol.user, ha, map[string]any{"update": base64.StdEncoding.EncodeToString(carol.doc.EncodeStateAsUpdate())})
	carol.leave()
	for _, hub := range []struct {
		name string
		h    http.Handler
	}{{"A", ha}, {"B", hb}} {
		rec := read(hub.h, carol.user, fileURL)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), decision) {
			t.Fatalf("file on %s right after Carol's Done: %d, has her line: %v", hub.name, rec.Code, strings.Contains(rec.Body.String(), decision))
		}
		rec = read(hub.h, carol.user, renderURL)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Carol decision: ship Friday") {
			t.Fatalf("render on %s right after Carol's Done: %d %.300s", hub.name, rec.Code, rec.Body)
		}
	}

	// (b) Erin: the tab dies offline. Three lines with no socket, then one
	// keepalive sync to B — which does not hold the lease — while the others
	// keep A's room busy past the old wait.
	stopTyping(erin)
	erin.leave()
	var offline []string
	for i := 1; i <= 3; i++ {
		line := fmt.Sprintf("- Erin offline %d\n", i)
		erin.write(line, meetingSectionEnd("Erin"))
		record(line)
		offline = append(offline, line)
	}
	if !a.holdsLease(room) || b.holdsLease(room) {
		t.Fatalf("lease moved before Erin's sync: A %v, B %v", a.holdsLease(room), b.holdsLease(room))
	}
	started := time.Now()
	syncOK(erin.user, hb, map[string]any{"update": base64.StdEncoding.EncodeToString(erin.doc.EncodeStateAsUpdate())})
	t.Logf("Erin's pagehide sync on the non-holder answered in %v (wait %v, snapshotMax %v)", time.Since(started), ycollabSyncDurableWait, snapshotMax)
	live := []*meetingEditor{alice, bob, dave, frank, heidi}
	for _, e := range live {
		if !waitUntil(5*time.Second, func() bool {
			txt := e.text()
			for _, l := range offline {
				if !strings.Contains(txt, l) {
					return false
				}
			}
			return true
		}) {
			t.Fatalf("Erin's offline lines never reached %s", e.name)
		}
	}

	// (c) Grace: two staged checkpoints over HTTP on the base she read once.
	grace := people["Grace"]
	graceBase := read(hb, grace, fileURL).Body.String()
	const bullet1, bullet2 = "- [ ] Grace staged 1\n", "- [ ] Grace staged 2\n"
	at := meetingSectionEnd("Grace")(graceBase)
	text1 := graceBase[:at] + bullet1 + graceBase[at:]
	text2 := graceBase[:at] + bullet1 + bullet2 + graceBase[at:]
	record(bullet1)
	record(bullet2)
	syncOK(grace, hb, map[string]any{"base_text": graceBase, "text": text1})
	out := syncOK(grace, hb, map[string]any{"base_text": graceBase, "text": text2, "prior_sent": []string{text1}})
	for _, l := range []string{bullet1, bullet2} {
		if n := strings.Count(out.Text, "\n"+l); n != 1 {
			t.Fatalf("Grace's %q is in the room %d times after her second checkpoint", l, n)
		}
	}

	// (d) An agent writes the header on a version the room has since moved
	// past (but not so far that its body reads as a sharp shrink).
	stale := file(hb)
	if !waitUntil(10*time.Second, func() bool { return file(hb) != stale }) {
		t.Fatal("the room never wrote past the agent's base")
	}
	rec := meetingDo(t, hb, agent, "PUT", uploadURL, strings.NewReader(strings.Replace(stale, oldHeader, newHeader, 1)),
		http.Header{"If-Match": {shaOf(stale)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("agent's stale-base write: %d %s", rec.Code, rec.Body)
	}
	var put struct {
		Merged bool `json:"merged"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &put); err != nil || !put.Merged {
		t.Fatalf("agent's stale-base write was not merged: %s", rec.Body)
	}
	record(newHeader)
	for _, e := range live {
		if !waitUntil(10*time.Second, func() bool { return strings.HasPrefix(e.text(), newHeader) }) {
			t.Fatalf("the agent's header never reached %s: %.200q", e.name, e.text())
		}
	}
	time.Sleep(snapshotIdle)

	// Everyone stops, converges, and leaves.
	for e := range typists {
		stopTyping(e)
	}
	var final string
	if !waitUntil(10*time.Second, func() bool {
		final = alice.text()
		for _, e := range live {
			if e.text() != final {
				return false
			}
		}
		return roomText(a, room) == final && roomText(b, room) == final
	}) {
		t.Fatal("the live documents never converged")
	}
	for _, e := range live {
		e.leave()
	}
	if !waitUntil(10*time.Second, func() bool { return file(ha) == final }) {
		t.Fatalf("the stored file never reached the converged document:\nfile %q\nroom %q", file(ha), final)
	}
	for _, s := range []*Server{a, b} {
		if !waitUntil(10*time.Second, func() bool { return !slices.Contains(s.ydocs().Rooms(), room) }) {
			t.Fatal("a room stayed resident after every peer left")
		}
	}
	if got := file(ha); got != final {
		t.Fatalf("the final snapshot changed the file:\nfile %q\nroom %q", got, final)
	}

	// conflictTexts lists from the fold; its unauthenticated reads of each
	// copy's text are not needed to count them.
	if cc := conflictTexts(t, a, p.ID, path); len(cc) != 0 {
		t.Fatalf("%d conflict copies: %v", len(cc), slices.Collect(maps.Keys(cc)))
	}
	for _, u := range []string{"/api/p/" + p.ID + "/tree", "/api/p/" + p.ID + "/history?prefix=areas/&n=1000"} {
		for _, h := range []http.Handler{ha, hb} {
			rec := read(h, agent, u)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", u, rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), ".bdrive-conflict-") {
				t.Fatalf("%s lists a conflict copy", u)
			}
		}
	}

	if !strings.HasPrefix(final, newHeader) || strings.Contains(final, oldHeader) {
		t.Fatalf("header in the final file: %.120q", final)
	}
	wantMu.Lock()
	defer wantMu.Unlock()
	for _, l := range want {
		if l == newHeader {
			continue
		}
		if n := strings.Count(final, "\n"+l); n != 1 {
			t.Errorf("%q is in the final file %d times, want exactly once", strings.TrimSuffix(l, "\n"), n)
		}
	}
	t.Logf("%d edits from 8 editors and an agent, each stored exactly once", len(want))
}
