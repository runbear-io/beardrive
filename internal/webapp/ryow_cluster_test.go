package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

// Read-your-own-writes across hub processes with a NONZERO refresh: the shape
// production runs in. newHubPair uses Refresh 0, which refolds on every read
// and so can never show a sibling serving its cached fold.

// listCountingBackend counts journal listings, the storage round trip a
// receipt refold costs, on top of the CAS backend the hub pair shares.
type listCountingBackend struct {
	*casBackend
	journalLists atomic.Int64
}

func (b *listCountingBackend) List(ctx context.Context, prefix string) ([]remote.Object, error) {
	if strings.HasSuffix(prefix, "journal/") {
		b.journalLists.Add(1)
	}
	return b.casBackend.List(ctx, prefix)
}

type ryowHub struct {
	s *Server
	h http.Handler
}

func newRYOWHubPair(t *testing.T) (a, b ryowHub, p Project, be *listCountingBackend) {
	t.Helper()
	fs, err := remote.Open(context.Background(), "file://"+t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	be = &listCountingBackend{casBackend: &casBackend{Backend: fs, ver: map[string]int{}}}
	db, err := OpenProjectDB(filepath.Join(t.TempDir(), "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, _, err = db.GetOrCreate("proj", "")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(id string) ryowHub {
		s := &Server{
			Root: be, Projects: db, Device: Identity{ID: id, Name: id, Author: id + "@test"},
			Refresh: 10 * time.Second, Upload: UploadConfig{Enabled: true},
		}
		h := s.Handler()
		t.Cleanup(func() {
			s.Close(context.Background())
			s.rooms().settle()
		})
		return ryowHub{s: s, h: h}
	}
	return mk("hub-a"), mk("hub-b"), p, be
}

func (hub ryowHub) call(t *testing.T, method, url string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, url, bytes.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	hub.h.ServeHTTP(rec, req)
	return rec
}

// writeReceipt asserts a write answered 200 with the same receipt in its
// header and body, and returns it.
func writeReceipt(t *testing.T, rec *httptest.ResponseRecorder, what string) string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", what, rec.Code, rec.Body)
	}
	var out struct {
		Receipt string `json:"receipt"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: %v (%s)", what, err, rec.Body)
	}
	if out.Receipt == "" || rec.Header().Get(receiptHeader) != out.Receipt {
		t.Fatalf("%s: receipt header %q, body %q", what, rec.Header().Get(receiptHeader), out.Receipt)
	}
	return out.Receipt
}

// UX-02: a file created through one process is in the sibling's tree and
// readable there at once, though the sibling's fold is seconds younger than
// its refresh.
func TestRYOWNewFileAcrossProcesses(t *testing.T) {
	a, b, p, _ := newRYOWHubPair(t)
	base := "/api/p/" + p.ID

	if rec := a.call(t, "PUT", base+"/upload/content?path=seed.md", []byte("seed"), nil); rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	if rec := b.call(t, "GET", base+"/tree", nil, nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "seed.md") {
		t.Fatalf("prime B: %d %s", rec.Code, rec.Body)
	}

	receipt := writeReceipt(t, a.call(t, "PUT", base+"/upload/content?path=ui-final.md", nil, nil), "create ui-final.md")
	if !strings.HasPrefix(receipt, "hub-a:") {
		t.Fatalf("receipt %q does not name the writing process's journal", receipt)
	}

	// The control: without the receipt B still serves its cached fold. This
	// is what made the browser 404 its own new file.
	if rec := b.call(t, "GET", base+"/tree", nil, nil); strings.Contains(rec.Body.String(), "ui-final.md") {
		t.Fatal("B's cached tree already holds the new file; the test cannot show anything")
	}

	rh := map[string]string{receiptHeader: receipt}
	if rec := b.call(t, "GET", base+"/tree", nil, rh); rec.Code != 200 || !strings.Contains(rec.Body.String(), "ui-final.md") {
		t.Fatalf("B tree with receipt: %d, missing ui-final.md: %s", rec.Code, rec.Body)
	}
	rec := b.call(t, "GET", base+"/file?path=ui-final.md", nil, rh)
	if rec.Code != 200 || rec.Body.Len() != 0 {
		t.Fatalf("B file with receipt: %d %q", rec.Code, rec.Body)
	}
	if !strings.Contains(strings.Join(rec.Header().Values("Vary"), ","), receiptHeader) {
		t.Fatalf("/file must Vary on %s, got %q", receiptHeader, rec.Header().Values("Vary"))
	}
	// /resolve answers only for paths that MOVED; for a live one the honest
	// answer is "nothing moved". With the receipt it must at least answer
	// from the fold that holds the file, never a storage error.
	if rec := b.call(t, "GET", base+"/resolve?path=ui-final.md", nil, rh); rec.Code >= 500 {
		t.Fatalf("B resolve with receipt: %d %s", rec.Code, rec.Body)
	}

	// Saving the same bytes appends nothing, yet still answers a receipt: the
	// op that holds them.
	again := a.call(t, "PUT", base+"/upload/content?path=ui-final.md", nil, nil)
	if got := writeReceipt(t, again, "unchanged save"); got != receipt {
		t.Fatalf("unchanged save receipt = %q, want the creating op %q", got, receipt)
	}
	if !strings.Contains(again.Body.String(), `"unchanged":true`) {
		t.Fatalf("expected unchanged: %s", again.Body)
	}
}

// UX-03: a restore through one process is what the sibling serves next — and
// the sibling must not answer a 304 for the version the restore replaced.
func TestRYOWRestoreAcrossProcessesNeverServesStale304(t *testing.T) {
	a, b, p, _ := newRYOWHubPair(t)
	base := "/api/p/" + p.ID
	for _, v := range []string{"version one", "version two", "version three"} {
		if rec := a.call(t, "PUT", base+"/upload/content?path=doc.md", []byte(v), nil); rec.Code != 200 {
			t.Fatalf("write %q: %d %s", v, rec.Code, rec.Body)
		}
	}
	v3 := `"` + shaOf("version three") + `"`
	prime := b.call(t, "GET", base+"/file?path=doc.md", nil, nil)
	if prime.Code != 200 || prime.Body.String() != "version three" || prime.Header().Get("ETag") != v3 {
		t.Fatalf("prime B: %d %q etag %q", prime.Code, prime.Body, prime.Header().Get("ETag"))
	}

	body, _ := json.Marshal(map[string]string{"path": "doc.md", "sha": shaOf("version one")})
	receipt := writeReceipt(t, a.call(t, "POST", base+"/restore", body, nil), "restore v1")

	// The control: B's cached fold still says v3, so the browser's cached v3
	// is "not modified" — the stale page this fixes.
	if rec := b.call(t, "GET", base+"/file?path=doc.md", nil, map[string]string{"If-None-Match": v3}); rec.Code != http.StatusNotModified {
		t.Fatalf("control: B without a receipt answered %d, expected its stale 304", rec.Code)
	}

	rec := b.call(t, "GET", base+"/file?path=doc.md", nil, map[string]string{
		"If-None-Match": v3, receiptHeader: receipt,
	})
	if rec.Code != http.StatusOK || rec.Body.String() != "version one" {
		t.Fatalf("B with the restore receipt: %d %q, want 200 \"version one\"", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("ETag"); got != `"`+shaOf("version one")+`"` {
		t.Fatalf("ETag = %q, want v1's", got)
	}
}

// UX-04: a thread opened through one process can be replied to and resolved
// through the other at once, and stays resolved wherever it is read.
func TestRYOWCommentThreadAcrossProcesses(t *testing.T) {
	a, b, p, _ := newRYOWHubPair(t)
	base := "/api/p/" + p.ID

	wrote := writeReceipt(t, a.call(t, "PUT", base+"/upload/content?path=note.md", []byte("hello"), nil), "create note.md")
	// The page B serves the reader is opened with the write's receipt.
	if rec := b.call(t, "GET", base+"/file?path=note.md", nil, map[string]string{receiptHeader: wrote}); rec.Code != 200 {
		t.Fatalf("B file: %d %s", rec.Code, rec.Body)
	}
	// B lists comments now: its listing is trusted for commentListTTL.
	if rec := b.call(t, "GET", base+"/comments?status=all", nil, nil); rec.Code != 200 {
		t.Fatalf("prime B comments: %d %s", rec.Code, rec.Body)
	}

	type answer struct {
		Comment CommentThread `json:"comment"`
		Event   string        `json:"event"`
	}
	decode := func(rec *httptest.ResponseRecorder, what string) answer {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", what, rec.Code, rec.Body)
		}
		var out answer
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if out.Event == "" || rec.Header().Get(commentReceiptHeader) != out.Event {
			t.Fatalf("%s: comment receipt header %q, body %q", what, rec.Header().Get(commentReceiptHeader), out.Event)
		}
		return out
	}

	body, _ := json.Marshal(map[string]any{"path": "note.md", "body": "is this right?"})
	created := decode(a.call(t, "POST", base+"/comments", body, nil), "create on A")
	id := created.Comment.ID
	receipts := []string{created.Event}
	hdr := func() map[string]string {
		return map[string]string{commentReceiptHeader: strings.Join(receipts, ",")}
	}

	reply, _ := json.Marshal(map[string]string{"action": "reply", "body": "yes"})
	replied := decode(b.call(t, "POST", base+"/comments/"+id, reply, hdr()), "reply on B")
	receipts = append(receipts, replied.Event)
	if n := len(replied.Comment.Messages); n != 2 {
		t.Fatalf("reply on B sees %d messages, want 2", n)
	}

	resolve, _ := json.Marshal(map[string]string{"action": "resolve"})
	resolved := decode(b.call(t, "POST", base+"/comments/"+id, resolve, hdr()), "resolve on B")
	receipts = append(receipts, resolved.Event)
	if !resolved.Comment.Resolved {
		t.Fatal("resolve on B answered an open thread")
	}

	for _, hub := range []struct {
		name string
		h    ryowHub
	}{{"A", a}, {"B", b}} {
		rec := hub.h.call(t, "GET", base+"/comments?status=all", nil, hdr())
		if rec.Code != 200 {
			t.Fatalf("list on %s: %d %s", hub.name, rec.Code, rec.Body)
		}
		var list struct {
			Comments []CommentThread `json:"comments"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		if len(list.Comments) != 1 || list.Comments[0].ID != id {
			t.Fatalf("list on %s: %s", hub.name, rec.Body)
		}
		if got := list.Comments[0]; !got.Resolved || len(got.Messages) != 2 {
			t.Fatalf("list on %s: resolved=%v messages=%d, want resolved with 2", hub.name, got.Resolved, len(got.Messages))
		}
	}

	// Deleting answers the tombstone as its receipt, on a bodiless 204.
	del := a.call(t, "DELETE", base+"/comments/"+id, nil, hdr())
	if del.Code != http.StatusNoContent {
		t.Fatalf("delete on A: %d %s", del.Code, del.Body)
	}
	if got := del.Header().Get(commentReceiptHeader); got != commentsPrefix+id+"/deleted.json" {
		t.Fatalf("delete receipt = %q", got)
	}
	receipts = append(receipts, del.Header().Get(commentReceiptHeader))
	rec := b.call(t, "GET", base+"/comments?status=all", nil, hdr())
	if rec.Code != 200 || strings.Contains(rec.Body.String(), id) {
		t.Fatalf("B still lists the deleted thread: %d %s", rec.Code, rec.Body)
	}
}

// A receipt no fold will ever contain costs one refold, then nothing for the
// cooldown: it cannot be turned into a storage List per request.
func TestRYOWForgedReceiptCostsOneList(t *testing.T) {
	_, b, p, be := newRYOWHubPair(t)
	base := "/api/p/" + p.ID
	if rec := b.call(t, "GET", base+"/tree", nil, nil); rec.Code != 200 {
		t.Fatalf("prime: %d %s", rec.Code, rec.Body)
	}

	be.journalLists.Store(0)
	for range 10 {
		if rec := b.call(t, "GET", base+"/tree", nil, map[string]string{receiptHeader: "not a receipt, ::1, a/b:5, x:0"}); rec.Code != 200 {
			t.Fatalf("malformed receipt: %d %s", rec.Code, rec.Body)
		}
	}
	if n := be.journalLists.Load(); n != 0 {
		t.Fatalf("malformed receipts listed journals %d times, want 0", n)
	}

	for range 10 {
		if rec := b.call(t, "GET", base+"/tree", nil, map[string]string{receiptHeader: "fake00000001:999999"}); rec.Code != 200 {
			t.Fatalf("forged receipt: %d %s", rec.Code, rec.Body)
		}
	}
	if n := be.journalLists.Load(); n > 1 {
		t.Fatalf("a forged receipt listed journals %d times within the cooldown, want at most 1", n)
	}
}

// The negative receipt caches are bounded: expired entries go on every
// insert, and a flood of distinct forged receipts within one cooldown evicts
// the oldest rather than growing the map.
func TestRYOWUnseenReceiptCacheIsBounded(t *testing.T) {
	now := time.Now()
	var m map[string]time.Time
	for i := range maxUnseenReceipts * 4 {
		m = insertUnseen(m, "fake:"+strconv.Itoa(i+1), now.Add(time.Duration(i)*time.Microsecond), receiptMissCooldown)
	}
	if len(m) != maxUnseenReceipts {
		t.Fatalf("cache holds %d entries, want the cap %d", len(m), maxUnseenReceipts)
	}
	last := "fake:" + strconv.Itoa(maxUnseenReceipts*4)
	if _, ok := m[last]; !ok {
		t.Fatal("the newest entry was evicted instead of the oldest")
	}
	if _, ok := m["fake:1"]; ok {
		t.Fatal("the oldest entry survived a full cache")
	}

	// Past the cooldown, one insert prunes every expired entry.
	m = insertUnseen(m, "fresh:1", now.Add(receiptMissCooldown+time.Second), receiptMissCooldown)
	if len(m) != 1 {
		t.Fatalf("after the cooldown the cache holds %d entries, want 1", len(m))
	}

	// The volume and the comment cache both go through it.
	v := &volume{}
	for i := range maxUnseenReceipts * 2 {
		v.markUnseen("dev:"+strconv.Itoa(i+1), now)
	}
	if len(v.unseen) > maxUnseenReceipts {
		t.Fatalf("volume negative cache holds %d entries, cap %d", len(v.unseen), maxUnseenReceipts)
	}
}

// A fold's receipt coverage is published together with its files, by every
// fold path: an incremental fold replaces the exact fold's coverage in the
// same section that replaces its files, and a cache hit reads both at once.
func TestRYOWFoldCoveragePublishedWithItsFiles(t *testing.T) {
	ctx := context.Background()
	fs, err := remote.Open(ctx, "file://"+t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	r := &RemoteSource{Backend: fs, Device: Identity{ID: "hub-a", Name: "hub-a"}}
	put := func(content string) {
		t.Helper()
		if err := r.appendOps(ctx, []journal.Op{{Kind: journal.KindPut, Path: "a.md", Blob: shaOf(content), Size: int64(len(content))}}); err != nil {
			t.Fatal(err)
		}
	}
	fpNow := func() string {
		t.Helper()
		keep, err := r.listJournals(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return journalFingerprint(keep)
	}
	covers := func(cov *seqCoverage, seq int64) bool { return cov.covers(journalReceipt{dev: "hub-a", seq: seq}) }

	put("one")
	put("two")
	files, _, cov, err := r.filesMovesCoverage(ctx) // a full fold: exact coverage
	if err != nil {
		t.Fatal(err)
	}
	if !covers(cov, 2) || covers(cov, 3) || files["a.md"].Blob != shaOf("two") {
		t.Fatalf("exact fold: covers 2=%v 3=%v blob=%s", covers(cov, 2), covers(cov, 3), files["a.md"].Blob)
	}
	exact := cov

	put("three") // an overwrite of a known path: the incremental fold takes it
	files, _, cov, err = r.filesMovesCoverage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if files["a.md"].Blob != shaOf("three") || !covers(cov, 3) {
		t.Fatalf("incremental fold: blob=%s covers 3=%v", files["a.md"].Blob, covers(cov, 3))
	}
	cf, _, ccov, ok := r.cachedFold(fpNow())
	if !ok {
		t.Fatal("the incremental fold was not published")
	}
	if ccov == exact || !covers(ccov, 3) || cf["a.md"].Blob != shaOf("three") {
		t.Fatal("the published fold pairs its files with another fold's coverage")
	}

	// A fold landing late with OLDER files must carry its own coverage. The
	// publishers set all of it in one section; mimic one with the exact
	// fold's files and check the cache hit never answers newer coverage.
	r.foldMu.Lock()
	r.foldFP, r.foldFiles, r.foldTombstones = "older", map[string]FileInfo{"a.md": {Blob: shaOf("two"), DeviceID: "hub-a", Seq: 2}}, nil
	r.foldCov = lazyCoverage(r.foldFiles, nil)
	r.foldMu.Unlock()
	_, _, ocov, ok := r.cachedFold("older")
	if !ok || covers(ocov, 3) || !covers(ocov, 2) {
		t.Fatalf("late older fold: covers 2=%v 3=%v", covers(ocov, 2), covers(ocov, 3))
	}
}

// An ok that acknowledges a version on a journaled volume always carries that
// version's receipt; one it cannot name is a 503 to retry, never an unfenced
// 200. A plain folder has no journal, so its ok carries none and is still ok.
func TestRYOWUnchangedAckIsFencedOrRefused(t *testing.T) {
	ctx := context.Background()
	fs, err := remote.Open(ctx, "file://"+t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	rs := &RemoteSource{Backend: fs, Device: Identity{ID: "hub-a", Name: "hub-a"}}
	if err := rs.appendOps(ctx, []journal.Op{{Kind: journal.KindPut, Path: "a.md", Blob: shaOf("one"), Size: 3}}); err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	v := &volume{source: rs, refresh: 10 * time.Second}

	rec := httptest.NewRecorder()
	s.writeWriteOK(ctx, rec, v, map[string]any{"ok": true, "path": "a.md", "sha": shaOf("one"), "unchanged": true})
	if rec.Code != http.StatusOK || rec.Header().Get(receiptHeader) != "hub-a:1" {
		t.Fatalf("matching head: %d receipt %q", rec.Code, rec.Header().Get(receiptHeader))
	}

	rec = httptest.NewRecorder()
	s.writeWriteOK(ctx, rec, v, map[string]any{"ok": true, "path": "a.md", "sha": shaOf("elsewhere"), "unchanged": true})
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" || rec.Header().Get(receiptHeader) != "" {
		t.Fatalf("unnameable head: %d retry-after %q receipt %q, want an unfenced-free 503", rec.Code, rec.Header().Get("Retry-After"), rec.Header().Get(receiptHeader))
	}

	root := t.TempDir()
	dv := &volume{source: &DirSource{Root: root}}
	rec = httptest.NewRecorder()
	s.writeWriteOK(ctx, rec, dv, map[string]any{"ok": true, "path": "a.md", "sha": "dir-1-3", "unchanged": true})
	if rec.Code != http.StatusOK || rec.Header().Get(receiptHeader) != "" {
		t.Fatalf("plain folder: %d receipt %q, want a bare 200", rec.Code, rec.Header().Get(receiptHeader))
	}
}

// UX-SSE: a change frame published on one hub carries the write's journal
// receipt, so a teammate's SSE-driven /tree or /file refetch that lands on a
// sibling hub fences that sibling's stale snapshot immediately.
func TestRYOWSSEChangeFrameFencesSiblingTreeRead(t *testing.T) {
	a, b, p, _ := newRYOWHubPair(t)
	base := "/api/p/" + p.ID

	if rec := a.call(t, "PUT", base+"/upload/content?path=seed.md", []byte("seed"), nil); rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	if rec := b.call(t, "GET", base+"/tree", nil, nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "seed.md") {
		t.Fatalf("prime B: %d %s", rec.Code, rec.Body)
	}

	sub, ok := a.s.events().subscribe(p.ID, "peer@example.com")
	if !ok {
		t.Fatal("subscribe failed")
	}
	defer a.s.events().unsubscribe(p.ID, sub)

	if rec := a.call(t, "PUT", base+"/upload/content?path=live-peer.md", []byte("from peer"), nil); rec.Code != 200 {
		t.Fatalf("peer write: %d %s", rec.Code, rec.Body)
	}

	var ev changeEvent
	select {
	case raw := <-sub.ch:
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("unmarshal frame: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SSE change frame")
	}
	if ev.Receipt == "" || !strings.HasPrefix(ev.Receipt, "hub-a:") {
		t.Fatalf("SSE change frame missing causal receipt: %+v", ev)
	}

	// Without the SSE receipt, sibling B still serves its cached pre-event tree.
	if rec := b.call(t, "GET", base+"/tree", nil, nil); strings.Contains(rec.Body.String(), "live-peer.md") {
		t.Fatal("B cached tree unexpectedly already had live-peer.md")
	}
	// With the receipt learned from the SSE frame alone, B refolds and serves live-peer.md.
	if rec := b.call(t, "GET", base+"/tree", nil, map[string]string{receiptHeader: ev.Receipt}); rec.Code != 200 || !strings.Contains(rec.Body.String(), "live-peer.md") {
		t.Fatalf("B tree with SSE receipt %q: %d %s", ev.Receipt, rec.Code, rec.Body)
	}
}
