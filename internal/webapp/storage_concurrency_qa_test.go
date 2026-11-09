package webapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

func qaRequest(t *testing.T, h http.Handler, method, url string, body []byte, headers http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, url, bytes.NewReader(body))
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func qaJournalLine(seq int64, path string) []byte {
	return []byte(fmt.Sprintf(`{"seq":%d,"kind":"put","path":%q}
`, seq, path))
}

// qaSizedJournal is deliberately JSON with an ignored field. The decoded Op is
// stable while the wire body has an exact size, which makes quota deltas exact.
func qaSizedJournal(t *testing.T, size int) []byte {
	t.Helper()
	prefix := `{"seq":1,"kind":"put","path":"a","x":"`
	const suffix = `"}` + "\n"
	if size < len(prefix)+len(suffix) {
		t.Fatalf("journal size %d is smaller than its JSON envelope", size)
	}
	return []byte(prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix)
}

func qaHexSHA(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestQA_C11_V2StoreAppendAndTailGetAndIdempotentRetry(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	q := &recQuota{}
	srv.Quota = q
	first := qaJournalLine(1, "first.md")
	journalDir := filepath.Join(root, p.ID, "journal")
	if err := os.MkdirAll(journalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(journalDir, "qa-c11.jsonl"), first, 0o644); err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/store/"
	key := "journal/qa-c11.jsonl"
	tail := qaJournalLine(2, "second.md")
	prefixSHA := qaHexSHA(first)
	url := base + "append?key=" + key + "&prev_size=" + fmt.Sprint(len(first)) + "&prev_sha256=" + prefixSHA
	headers := http.Header{"X-Bdrive-Device": []string{"qa-c11"}}

	if rec := qaRequest(t, h, http.MethodPost, url, tail, headers); rec.Code != http.StatusOK {
		t.Fatalf("initial append: %d %s", rec.Code, rec.Body)
	}
	q.denyW = true
	if rec := qaRequest(t, h, http.MethodPost, url, tail, headers); rec.Code != http.StatusOK {
		t.Fatalf("idempotent retry under full quota: %d %s", rec.Code, rec.Body)
	}
	q.denyW = false

	storedURL := base + "object?key=" + key
	stored := qaRequest(t, h, http.MethodGet, storedURL, nil, nil)
	want := append(append([]byte(nil), first...), tail...)
	if stored.Code != http.StatusOK || !bytes.Equal(stored.Body.Bytes(), want) {
		t.Fatalf("stored journal = (%d) %q, want %q", stored.Code, stored.Body, want)
	}
	if got := len(q.usage); got != 1 || q.usage[0].bytes != int64(len(tail)) {
		t.Fatalf("retry quota usage = %+v, want one tail charge of %d", q.usage, len(tail))
	}

	badSize := base + "append?key=" + key + "&prev_size=" + fmt.Sprint(len(first)+1) + "&prev_sha256=" + prefixSHA
	if rec := qaRequest(t, h, http.MethodPost, badSize, []byte("not-a-new-op"), headers); rec.Code != http.StatusConflict {
		t.Fatalf("mismatched prefix size: %d %s, want 409", rec.Code, rec.Body)
	}
	badSHA := base + "append?key=" + key + "&prev_size=" + fmt.Sprint(len(first)) + "&prev_sha256=" + strings.Repeat("0", 64)
	if rec := qaRequest(t, h, http.MethodPost, badSHA, qaJournalLine(3, "third.md"), headers); rec.Code != http.StatusConflict {
		t.Fatalf("mismatched prefix hash: %d %s, want 409", rec.Code, rec.Body)
	}
	dupSeqURL := base + "append?key=" + key + "&prev_size=" + fmt.Sprint(len(want)) + "&prev_sha256=" + qaHexSHA(want)
	if rec := qaRequest(t, h, http.MethodPost, dupSeqURL, qaJournalLine(1, "redefined-seq1.md"), headers); rec.Code != http.StatusConflict {
		t.Fatalf("append redefining existing Seq 1: %d %s, want 409", rec.Code, rec.Body)
	}

	offsetURL := storedURL + "&offset=" + fmt.Sprint(len(first)) + "&prefix_sha256=" + prefixSHA
	tailRec := qaRequest(t, h, http.MethodGet, offsetURL, nil, nil)
	if tailRec.Code != http.StatusOK || tailRec.Body.String() != string(tail) {
		t.Fatalf("verified journal tail = (%d) %q, want %q", tailRec.Code, tailRec.Body, tail)
	}
	if got := tailRec.Header().Get("X-Bdrive-Tail-Offset"); got != fmt.Sprint(len(first)) {
		t.Fatalf("tail offset = %q, want %d", got, len(first))
	}

	fallbackURL := storedURL + "&offset=" + fmt.Sprint(len(first)) + "&prefix_sha256=" + strings.Repeat("f", 64)
	fallback := qaRequest(t, h, http.MethodGet, fallbackURL, nil, nil)
	if fallback.Code != http.StatusOK || !bytes.Equal(fallback.Body.Bytes(), want) {
		t.Fatalf("prefix mismatch fallback = (%d) %q, want full %q", fallback.Code, fallback.Body, want)
	}
	if got := fallback.Header().Get("X-Bdrive-Tail-Offset"); got != "" {
		t.Fatalf("fallback unexpectedly advertised tail offset %q", got)
	}

	// Missing prev_sha256 when prev_size > 0 must be rejected (400 Bad Request).
	noSHAURL := base + "append?key=" + key + "&prev_size=" + fmt.Sprint(len(want))
	if rec := qaRequest(t, h, http.MethodPost, noSHAURL, qaJournalLine(3, "third.md"), headers); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing prev_sha256 with prev_size > 0: got %d %s, want 400", rec.Code, rec.Body)
	}

	// Unterminated prefix append + idempotent retry: the hub inserts '\n' on
	// the first append and must still recognize a retry of the same tail.
	untermKey := "journal/qa-c11-unterm.jsonl"
	untermFirst := bytes.TrimSuffix(qaJournalLine(1, "unterm-first.md"), []byte("\n"))
	untermPath := filepath.Join(journalDir, "qa-c11-unterm.jsonl")
	if err := os.WriteFile(untermPath, untermFirst, 0o644); err != nil {
		t.Fatal(err)
	}
	untermSHA := qaHexSHA(untermFirst)
	untermURL := base + "append?key=" + untermKey + "&prev_size=" + fmt.Sprint(len(untermFirst)) + "&prev_sha256=" + untermSHA
	untermHeaders := http.Header{"X-Bdrive-Device": []string{"qa-c11-unterm"}}
	untermTail := qaJournalLine(2, "unterm-second.md")
	if rec := qaRequest(t, h, http.MethodPost, untermURL, untermTail, untermHeaders); rec.Code != http.StatusOK {
		t.Fatalf("initial append on unterminated prefix: %d %s", rec.Code, rec.Body)
	}
	q.denyW = true
	if rec := qaRequest(t, h, http.MethodPost, untermURL, untermTail, untermHeaders); rec.Code != http.StatusOK {
		t.Fatalf("idempotent retry on unterminated prefix: %d %s", rec.Code, rec.Body)
	}
	q.denyW = false
}

func TestQA_C12_C13_ManifestWriteOnceAndHubSeqAcrossInstances(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/store/"
	chunk := []byte("manifest chunk")
	chunkSHA := qaHexSHA(chunk)
	if rec := do(t, h, http.MethodPut, base+"object?key=chunks/"+chunkSHA, chunk); rec.Code != http.StatusOK {
		t.Fatalf("chunk seed: %d %s", rec.Code, rec.Body)
	}
	key := "manifests/" + shaOf("manifest whole")
	manifest := []byte(fmt.Sprintf(`{"v":1,"size":%d,"chunks":[{"h":%q,"n":%d}]}`, len(chunk), chunkSHA, len(chunk)))
	if rec := do(t, h, http.MethodPut, base+"object?key="+key, manifest); rec.Code != http.StatusOK {
		t.Fatalf("manifest seed: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, http.MethodPut, base+"object?key="+key, manifest); rec.Code != http.StatusOK {
		t.Fatalf("identical manifest retry: %d %s", rec.Code, rec.Body)
	}
	other := []byte(fmt.Sprintf(`{"v":1,"size":%d,"chunks":[]}`, len(chunk)))
	if rec := do(t, h, http.MethodPut, base+"object?key="+key, other); rec.Code != http.StatusConflict {
		t.Fatalf("different manifest body: %d %s, want 409", rec.Code, rec.Body)
	}

	a, b, _ := newHubPair(t)
	rsA := &RemoteSource{Backend: a.Root, Device: Identity{ID: "shared-hub", Name: "A", Author: "a@test"}}
	rsB := &RemoteSource{Backend: b.Root, Device: Identity{ID: "shared-hub", Name: "B", Author: "b@test"}}
	errs := make(chan error, 2)
	go func() {
		errs <- rsA.appendOp(context.Background(), journal.Op{Kind: journal.KindPut, Path: "from-a.md", Blob: qaHexSHA([]byte("a")), Size: 1})
	}()
	go func() {
		errs <- rsB.appendOp(context.Background(), journal.Op{Kind: journal.KindPut, Path: "from-b.md", Blob: qaHexSHA([]byte("b")), Size: 1})
	}()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent hub append: %v", err)
		}
	}
	rc, err := a.Root.Get(context.Background(), "journal/shared-hub.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	ops, err := journal.Parse(body)
	if err != nil || len(ops) != 2 {
		t.Fatalf("shared hub journal = %d ops, parse error %v", len(ops), err)
	}
	seen := map[int64]bool{}
	for _, op := range ops {
		if seen[op.Seq] {
			t.Fatalf("duplicate hub Seq %d in %+v", op.Seq, ops)
		}
		seen[op.Seq] = true
	}
	if !seen[1] || !seen[2] {
		t.Fatalf("hub Seq set = %#v, want {1,2}", seen)
	}
}

func TestQA_C14_RestoreRemoveUndoStaleBaseConflict(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	dir := filepath.Join(root, p.ID)
	oldSHA := placeChunked(t, dir, "chunk one|", "chunk two")
	currentBody := []byte("current version")
	currentSHA := qaHexSHA(currentBody)
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blobs", currentSHA), currentBody, 0o644); err != nil {
		t.Fatal(err)
	}
	seed := newFakeRemoteAt(t, dir)
	seed.append("device-c14", journal.Op{Kind: journal.KindPut, Path: "chunked.md", Blob: oldSHA, Size: 19})
	seed.append("device-c14", journal.Op{Kind: journal.KindPut, Path: "chunked.md", Blob: currentSHA, Size: int64(len(currentBody))})
	h := srv.Handler()
	restore := do(t, h, http.MethodPost, "/api/p/"+p.ID+"/restore", map[string]string{"path": "chunked.md", "sha": oldSHA})
	if restore.Code != http.StatusOK {
		t.Fatalf("chunked restore: %d %s", restore.Code, restore.Body)
	}
	_, v, err := srv.projectVolume(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	rs := v.source.(*RemoteSource)
	rc, err := rs.OpenBlob(context.Background(), oldSHA)
	if err != nil {
		t.Fatalf("OpenBlob manifest fallback: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "chunk one|chunk two" {
		t.Fatalf("reassembled restored bytes = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "blobs", oldSHA)); !os.IsNotExist(err) {
		t.Fatalf("restore/reassembly created blobs/%s: err=%v", oldSHA, err)
	}

	// Both restore's conditional commit and removeIf use commitBase inside the
	// append CAS loop. A peer write after the caller captured its base must win
	// the check and leave no hub op behind.
	peer := &RemoteSource{Backend: rs.Backend, Device: Identity{ID: "peer-c14", Name: "peer", Author: "peer@test"}}
	raceOld := []byte("restore old")
	raceCurrent := []byte("restore current")
	for _, body := range [][]byte{raceOld, raceCurrent} {
		sha := qaHexSHA(body)
		if err := rs.Backend.Put(context.Background(), "blobs/"+sha, bytes.NewReader(body), int64(len(body))); err != nil {
			t.Fatal(err)
		}
	}
	if err := rs.appendOp(context.Background(), journal.Op{Kind: journal.KindPut, Path: "restore-race.md", Blob: qaHexSHA(raceOld), Size: int64(len(raceOld))}); err != nil {
		t.Fatal(err)
	}
	if err := rs.appendOp(context.Background(), journal.Op{Kind: journal.KindPut, Path: "restore-race.md", Blob: qaHexSHA(raceCurrent), Size: int64(len(raceCurrent))}); err != nil {
		t.Fatal(err)
	}
	external := []byte("external restore")
	externalSHA := qaHexSHA(external)
	if err := rs.Backend.Put(context.Background(), "blobs/"+externalSHA, bytes.NewReader(external), int64(len(external))); err != nil {
		t.Fatal(err)
	}
	if err := peer.appendOp(context.Background(), journal.Op{Kind: journal.KindPut, Path: "restore-race.md", Blob: externalSHA, Size: int64(len(external))}); err != nil {
		t.Fatal(err)
	}
	if err := rs.commitIf(context.Background(), "restore-race.md", qaHexSHA(raceOld), int64(len(raceOld)), User{}, "", qaHexSHA(raceCurrent)); err == nil {
		t.Fatal("restore conditional commit ignored concurrent modification")
	} else {
		var stale *staleWriteError
		if !errors.As(err, &stale) {
			t.Fatalf("restore conflict error = %v, want staleWriteError", err)
		}
	}

	removeBody := []byte("remove current")
	removeSHA := qaHexSHA(removeBody)
	if err := rs.Backend.Put(context.Background(), "blobs/"+removeSHA, bytes.NewReader(removeBody), int64(len(removeBody))); err != nil {
		t.Fatal(err)
	}
	if err := rs.appendOp(context.Background(), journal.Op{Kind: journal.KindPut, Path: "remove-race.md", Blob: removeSHA, Size: int64(len(removeBody))}); err != nil {
		t.Fatal(err)
	}
	removeExternal := []byte("remove external")
	removeExternalSHA := qaHexSHA(removeExternal)
	if err := rs.Backend.Put(context.Background(), "blobs/"+removeExternalSHA, bytes.NewReader(removeExternal), int64(len(removeExternal))); err != nil {
		t.Fatal(err)
	}
	if err := peer.appendOp(context.Background(), journal.Op{Kind: journal.KindPut, Path: "remove-race.md", Blob: removeExternalSHA, Size: int64(len(removeExternal))}); err != nil {
		t.Fatal(err)
	}
	if err := rs.removeIf(context.Background(), "remove-race.md", removeSHA, User{}, "remove"); err == nil {
		t.Fatal("removeIf ignored concurrent modification")
	} else {
		var stale *staleWriteError
		if !errors.As(err, &stale) {
			t.Fatalf("remove conflict error = %v, want staleWriteError", err)
		}
	}

	// Symlinks have Blob == "" and carry identity in Link; stale remove/restore
	// against an older Link target must fail with staleWriteError.
	if err := rs.appendOp(context.Background(), journal.Op{Kind: journal.KindPut, Path: "link-race", Link: "target-a", Mode: 0o777}); err != nil {
		t.Fatal(err)
	}
	if err := peer.appendOp(context.Background(), journal.Op{Kind: journal.KindPut, Path: "link-race", Link: "target-b", Mode: 0o777}); err != nil {
		t.Fatal(err)
	}
	if err := rs.removeIfBase(context.Background(), "link-race", &commitBase{path: "link-race", link: "target-a"}, User{}, "remove stale symlink"); err == nil {
		t.Fatal("removeIfBase ignored concurrent symlink target change")
	} else {
		var stale *staleWriteError
		if !errors.As(err, &stale) {
			t.Fatalf("symlink remove conflict error = %v, want staleWriteError", err)
		}
	}
	if err := rs.commitIfBase(context.Background(), "link-race", qaHexSHA(raceOld), int64(len(raceOld)), User{}, "restore over stale symlink", &commitBase{path: "link-race", link: "target-a", blob: qaHexSHA(raceOld)}); err == nil {
		t.Fatal("commitIfBase ignored concurrent symlink target change")
	} else {
		var stale *staleWriteError
		if !errors.As(err, &stale) {
			t.Fatalf("symlink restore conflict error = %v, want staleWriteError", err)
		}
	}

	// Restore CAS must also reject transitions to/from an absent/deleted path:
	// 1. Existing path concurrently deleted before restore commits (mustExist).
	if err := rs.appendOp(context.Background(), journal.Op{Kind: journal.KindPut, Path: "vanish-race.md", Blob: qaHexSHA(raceCurrent), Size: int64(len(raceCurrent))}); err != nil {
		t.Fatal(err)
	}
	if err := peer.appendOp(context.Background(), journal.Op{Kind: journal.KindDelete, Path: "vanish-race.md"}); err != nil {
		t.Fatal(err)
	}
	if err := rs.commitIfBase(context.Background(), "vanish-race.md", qaHexSHA(raceOld), int64(len(raceOld)), User{}, "restore over concurrent delete", &commitBase{path: "vanish-race.md", sha: qaHexSHA(raceCurrent), mustExist: true, blob: qaHexSHA(raceOld)}); err == nil {
		t.Fatal("commitIfBase resurrected a file that was concurrently deleted")
	} else {
		var stale *staleWriteError
		if !errors.As(err, &stale) {
			t.Fatalf("concurrent-delete restore conflict error = %v, want staleWriteError", err)
		}
	}
	// 2. Absent path concurrently created before restore commits (mustBeAbsent).
	if err := peer.appendOp(context.Background(), journal.Op{Kind: journal.KindPut, Path: "created-race.md", Blob: externalSHA, Size: int64(len(external))}); err != nil {
		t.Fatal(err)
	}
	if err := rs.commitIfBase(context.Background(), "created-race.md", qaHexSHA(raceOld), int64(len(raceOld)), User{}, "restore over concurrent create", &commitBase{path: "created-race.md", mustBeAbsent: true, blob: qaHexSHA(raceOld)}); err == nil {
		t.Fatal("commitIfBase overwrote a file that was concurrently created on an absent path")
	} else {
		var stale *staleWriteError
		if !errors.As(err, &stale) {
			t.Fatalf("concurrent-create restore conflict error = %v, want staleWriteError", err)
		}
	}
}

func TestQA_C15_C16_C17_FilteredCacheLRUEvictionAndDataIndexFallback(t *testing.T) {
	rs := &RemoteSource{}
	sz := maxFilteredJournalBytes/2 - 1
	data1 := bytes.Repeat([]byte{'1'}, sz)
	data2 := bytes.Repeat([]byte{'2'}, sz)
	data3 := bytes.Repeat([]byte{'3'}, sz)
	mod := time.Unix(10, 0)
	o1 := remote.Object{Key: "journal/one.jsonl", Size: int64(len(data1)), Modified: mod}
	o2 := remote.Object{Key: "journal/two.jsonl", Size: int64(len(data2)), Modified: mod}
	o3 := remote.Object{Key: "journal/three.jsonl", Size: int64(len(data3)), Modified: mod}
	rs.putFilter(o1, "scope", data1)
	rs.putFilter(o2, "scope", data2)
	if _, ok := rs.cachedFilter(o1, "scope"); !ok {
		t.Fatal("first filtered journal disappeared before eviction")
	}
	rs.putFilter(o3, "scope", data3)
	if _, ok := rs.cachedFilter(o1, "scope"); !ok {
		t.Fatal("LRU access did not retain the oldest entry that was used")
	}
	if _, ok := rs.cachedFilter(o2, "scope"); ok {
		t.Fatal("filtered cache evicted a newer entry instead of the least recently used one")
	}
	if _, ok := rs.cachedFilter(o3, "scope"); !ok {
		t.Fatal("filtered cache wiped the newest entry while evicting")
	}
	if _, ok := rs.cachedFilter(remote.Object{Key: o1.Key, Size: o1.Size + 1, Modified: mod}, "scope"); ok {
		t.Fatal("filtered cache accepted stale size metadata")
	}
	if _, ok := rs.cachedFilter(remote.Object{Key: o1.Key, Size: o1.Size, Modified: mod.Add(time.Second)}, "scope"); ok {
		t.Fatal("filtered cache accepted stale modified metadata")
	}

	dir := t.TempDir()
	be, err := remote.Open(context.Background(), "file://"+dir)
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	cb := newCountBackend(be)
	journalKey := "journal/data-tail.jsonl"
	op1 := journal.Op{Seq: 1, Lamport: 1, Kind: journal.KindPut, Path: "one.md", Blob: qaHexSHA([]byte("one")), Size: 3}
	op2 := journal.Op{Seq: 2, Lamport: 2, Kind: journal.KindPut, Path: "two.md", Blob: qaHexSHA([]byte("two")), Size: 3}
	first, err := journal.Marshal([]journal.Op{op1})
	if err != nil {
		t.Fatal(err)
	}
	second, err := journal.Marshal([]journal.Op{op2})
	if err != nil {
		t.Fatal(err)
	}
	if err := cb.Put(context.Background(), journalKey, bytes.NewReader(first), int64(len(first))); err != nil {
		t.Fatal(err)
	}
	rs2 := &RemoteSource{Backend: cb}
	objects, err := rs2.listJournals(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := rs2.parsedJournals(context.Background(), objects)
	if err != nil || len(parsed) != 1 || len(parsed[0]) != 1 {
		t.Fatalf("initial parsed journal = %#v, err=%v", parsed, err)
	}
	beforeGets := cb.getsFor("data-tail")
	joined := append(append([]byte(nil), first...), second...)
	if err := cb.Put(context.Background(), journalKey, bytes.NewReader(joined), int64(len(joined))); err != nil {
		t.Fatal(err)
	}
	objects, err = rs2.listJournals(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err = rs2.parsedJournals(context.Background(), objects)
	if err != nil || len(parsed) != 1 || len(parsed[0]) != 2 {
		t.Fatalf("tail parsed journal = %#v, err=%v", parsed, err)
	}
	if got := cb.getsFor("data-tail"); got != beforeGets+1 {
		t.Fatalf("journal GETs after append = %d, want exactly one tail refresh after %d", got, beforeGets)
	}
}

func TestQA_C19_C20_QuotaJournalDeltaChunkDedupAndConcurrentDeviceBind(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	q := &recQuota{}
	srv.Quota = q
	h := srv.Handler()
	journalKey := "journal/qa-quota.jsonl"
	first := qaSizedJournal(t, 100)
	second := qaSizedJournal(t, 150)
	headers := http.Header{"X-Bdrive-Device": []string{"qa-quota"}}
	url := "/api/p/" + p.ID + "/store/object?key=" + journalKey
	if rec := qaRequest(t, h, http.MethodPut, url, first, headers); rec.Code != http.StatusOK {
		t.Fatalf("100-byte journal push: %d %s", rec.Code, rec.Body)
	}
	if rec := qaRequest(t, h, http.MethodPut, url, second, headers); rec.Code != http.StatusOK {
		t.Fatalf("150-byte journal rewrite: %d %s", rec.Code, rec.Body)
	}
	if len(q.usage) != 2 || q.usage[0].bytes != 100 || q.usage[1].bytes != 50 {
		t.Fatalf("journal quota usage = %+v, want 100 then 50", q.usage)
	}
	var total int64
	for _, usage := range q.usage {
		total += usage.bytes
	}
	if total != 150 {
		t.Fatalf("journal quota total = %d, want 150", total)
	}

	chunk := []byte("deduplicated chunk")
	chunkKey := "chunks/" + qaHexSHA(chunk)
	if rec := do(t, h, http.MethodPut, "/api/p/"+p.ID+"/store/object?key="+chunkKey, chunk); rec.Code != http.StatusOK {
		t.Fatalf("chunk push: %d %s", rec.Code, rec.Body)
	}
	usageAfterChunk := len(q.usage)
	if rec := do(t, h, http.MethodPut, "/api/p/"+p.ID+"/store/object?key="+chunkKey, chunk); rec.Code != http.StatusOK {
		t.Fatalf("chunk retry: %d %s", rec.Code, rec.Body)
	}
	if len(q.usage) != usageAfterChunk {
		t.Fatal("repeated chunk push was billed twice")
	}
	manifestKey := "manifests/" + shaOf("deduplicated whole")
	manifest := []byte(fmt.Sprintf(`{"v":1,"size":%d,"chunks":[{"h":%q,"n":%d}]}`, len(chunk), qaHexSHA(chunk), len(chunk)))
	if rec := do(t, h, http.MethodPut, "/api/p/"+p.ID+"/store/object?key="+manifestKey, manifest); rec.Code != http.StatusOK {
		t.Fatalf("manifest push: %d %s", rec.Code, rec.Body)
	}
	usageAfterManifest := len(q.usage)
	if rec := do(t, h, http.MethodPut, "/api/p/"+p.ID+"/store/object?key="+manifestKey, manifest); rec.Code != http.StatusOK {
		t.Fatalf("manifest retry: %d %s", rec.Code, rec.Body)
	}
	if len(q.usage) != usageAfterManifest {
		t.Fatal("repeated manifest push was billed twice")
	}

	registry, err := OpenDeviceRegistry(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan struct {
		user string
		err  error
	}, 2)
	var wg sync.WaitGroup
	for _, user := range []string{"alice@example.com", "bob@example.com"} {
		user := user
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := registry.Bind(user, DeviceInfo{ID: "qa-bind-race", Name: user}, func(string) bool { return true })
			results <- struct {
				user string
				err  error
			}{user, err}
		}()
	}
	wg.Wait()
	close(results)
	var winner string
	successes := 0
	failures := 0
	for result := range results {
		if result.err == nil {
			successes++
			winner = result.user
		} else {
			failures++
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("concurrent device binds: successes=%d failures=%d, want exactly one each", successes, failures)
	}
	owner, known := registry.OwnerOf("qa-bind-race")
	if !known || owner != winner {
		t.Fatalf("device owner = %q known=%v, want winner %q", owner, known, winner)
	}
}

func TestQA_JournalPutRejectsRewritingStoredOpIdentityAndReadOnlyBypass(t *testing.T) {
	h, srv, c, p := folderHub(t) // "locked/" is read-only for bob, "notes/" is writable
	const dev = "bobdev01"
	secRegisterDevice(t, h, p.ID, c["bob"], dev, dev, "darwin")
	push := func(ops []journal.Op) *httptest.ResponseRecorder {
		t.Helper()
		data, err := journal.Marshal(ops)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPut, "/api/p/"+p.ID+"/store/object?key=journal/"+dev+".jsonl", bytes.NewReader(data))
		req.AddCookie(c["bob"])
		req.Header.Set("X-Bdrive-Device", dev)
		req.Header.Set("X-Bdrive-Perms", "1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	sha1 := strings.Repeat("a", 64)
	sha2 := strings.Repeat("b", 64)
	op1 := journal.Op{
		Seq: 1, Lamport: 1, Time: time.Now().UTC(), Device: dev, User: "bob@x.io",
		Kind: journal.KindPut, Path: "notes/a.md", Blob: sha1, Size: 10, Mode: 0o644,
	}
	if rec := push([]journal.Op{op1}); rec.Code != http.StatusOK {
		t.Fatalf("initial push: %d %s", rec.Code, rec.Body)
	}
	// Rewriting Seq 1's Blob or Kind on a writable path must be rejected as an
	// append-only violation (409 Conflict).
	rewrittenBlob := op1
	rewrittenBlob.Blob = sha2
	if rec := push([]journal.Op{rewrittenBlob}); rec.Code != http.StatusConflict {
		t.Fatalf("rewriting stored Seq 1 Blob: got %d %s, want 409", rec.Code, rec.Body)
	}
	// After "notes/" becomes read-only for bob, rewriting Seq 1's Kind or Blob on
	// "notes/a.md" must not bypass opsWithinScope (must be refused with 403).
	if err := srv.Projects.SetFolder(p.ID, FolderRule{Prefix: "notes/", Default: PermRead}); err != nil {
		t.Fatal(err)
	}
	rewrittenKind := op1
	rewrittenKind.Kind = journal.KindDelete
	rewrittenKind.Blob = ""
	if rec := push([]journal.Op{rewrittenKind}); rec.Code != http.StatusForbidden {
		t.Fatalf("rewriting stored Seq 1 Kind after read-only transition: got %d %s, want 403", rec.Code, rec.Body)
	}
}

type onceMismatchBackend struct {
	remote.Backend
	cp       remote.ConditionalPutter
	mu       sync.Mutex
	mismatch map[string]bool
}

func (b *onceMismatchBackend) GetVersioned(ctx context.Context, key string) (io.ReadCloser, remote.Version, error) {
	return b.cp.GetVersioned(ctx, key)
}

func (b *onceMismatchBackend) PutIf(ctx context.Context, key string, r io.Reader, size int64, expect remote.Version) (remote.Version, error) {
	b.mu.Lock()
	fail := b.mismatch[key]
	if fail {
		delete(b.mismatch, key)
	}
	b.mu.Unlock()
	if fail {
		return remote.VersionAbsent, remote.ErrVersionMismatch
	}
	return b.cp.PutIf(ctx, key, r, size, expect)
}

func TestQA_StoreRoomHeadReconcilesOrderAndRetriesCAS(t *testing.T) {
	var wrapper *onceMismatchBackend
	srv, p, _ := newHub(t, true, func(be remote.Backend) remote.Backend {
		cp := be.(remote.ConditionalPutter)
		wrapper = &onceMismatchBackend{
			Backend:  be,
			cp:       cp,
			mismatch: map[string]bool{},
		}
		return wrapper
	})
	// Note: p.ID is known after newHub returns, so set the mismatch key now.
	wrapper.mu.Lock()
	wrapper.mismatch = map[string]bool{p.ID + "/" + headKey("notes.md"): true}
	wrapper.mu.Unlock()

	doc := crdt.New()
	txt := doc.GetText("body")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "v1\n", nil) })
	state1 := doc.EncodeStateAsUpdate()
	sha1 := qaHexSHA([]byte("v1\n"))

	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, txt.Len(), "v2\n", nil) })
	state2 := doc.EncodeStateAsUpdate()
	sha2 := qaHexSHA([]byte("v2\n"))

	ctx := context.Background()
	srv.storeRoomHistory(ctx, p.ID, "notes.md", sha1, state1)
	if got := srv.roomHead(ctx, p.ID, "notes.md"); got != sha1 {
		t.Fatalf("roomHead after state1 (with transient CAS mismatch) = %q, want %q", got, sha1)
	}

	// Inject another transient CAS mismatch when storing newer state2: it must retry and advance to sha2.
	wrapper.mu.Lock()
	wrapper.mismatch[p.ID+"/"+headKey("notes.md")] = true
	wrapper.mu.Unlock()
	srv.storeRoomHistory(ctx, p.ID, "notes.md", sha2, state2)
	if got := srv.roomHead(ctx, p.ID, "notes.md"); got != sha2 {
		t.Fatalf("roomHead after newer state2 = %q, want %q", got, sha2)
	}

	// A late/stale storeRoomHead for state1 must NOT overwrite the newer state2 head.
	srv.storeRoomHead(ctx, p.ID, "notes.md", sha1, state1)
	if got := srv.roomHead(ctx, p.ID, "notes.md"); got != sha2 {
		t.Fatalf("roomHead after stale state1 = %q, want newer %q preserved", got, sha2)
	}
}

type blockingGetBackend struct {
	remote.Backend
	target  string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingGetBackend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if key == b.target {
		b.once.Do(func() { close(b.entered) })
		select {
		case <-b.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return b.Backend.Get(ctx, key)
}

func TestQA_SharedJournalFetchAndReassembleIsolateCallerCancel(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	be, err := remote.Open(ctx, "file://"+dir)
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()

	// 1. Shared journal fetch (jflight) survives first caller cancellation.
	jKey := "journal/dev-cancel.jsonl"
	op := journal.Op{Seq: 1, Lamport: 1, Kind: journal.KindPut, Path: "doc.md", Blob: strings.Repeat("a", 64), Size: 4}
	jBytes, err := journal.Marshal([]journal.Op{op})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.Put(ctx, jKey, bytes.NewReader(jBytes), int64(len(jBytes))); err != nil {
		t.Fatal(err)
	}
	jb := &blockingGetBackend{
		Backend: be,
		target:  jKey,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	rsJ := &RemoteSource{Backend: jb}
	objs, err := rsJ.listJournals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ctx1, cancel1 := context.WithCancel(ctx)
	err1Ch := make(chan error, 1)
	go func() {
		_, err := rsJ.parsedJournals(ctx1, objs)
		err1Ch <- err
	}()
	<-jb.entered

	err2Ch := make(chan error, 1)
	go func() {
		_, err := rsJ.parsedJournals(ctx, objs)
		err2Ch <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel1()
	if err := <-err1Ch; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled caller 1 = %v, want context.Canceled", err)
	}
	close(jb.release)
	if err := <-err2Ch; err != nil {
		t.Fatalf("healthy caller 2 failed after caller 1 canceled: %v", err)
	}

	// 2. Shared chunked blob reassembly survives first caller cancellation.
	chunkData := []byte("shared-reassemble-chunk-payload")
	chunkSHA := qaHexSHA(chunkData)
	if err := be.Put(ctx, "chunks/"+chunkSHA, bytes.NewReader(chunkData), int64(len(chunkData))); err != nil {
		t.Fatal(err)
	}
	manifestJSON := fmt.Sprintf(`{"v":1,"size":%d,"chunks":[{"h":%q,"n":%d}]}`, len(chunkData), chunkSHA, len(chunkData))
	fileSHA := chunkSHA
	if err := be.Put(ctx, "manifests/"+fileSHA, strings.NewReader(manifestJSON), int64(len(manifestJSON))); err != nil {
		t.Fatal(err)
	}
	rb := &blockingGetBackend{
		Backend: be,
		target:  "chunks/" + chunkSHA,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	rsR := &RemoteSource{Backend: rb}
	rctx1, rcancel1 := context.WithCancel(ctx)
	rerr1Ch := make(chan error, 1)
	go func() {
		rc, err := rsR.reassemble(rctx1, fileSHA)
		if rc != nil {
			rc.Close()
		}
		rerr1Ch <- err
	}()
	<-rb.entered

	rerr2Ch := make(chan error, 1)
	go func() {
		rc, err := rsR.reassemble(ctx, fileSHA)
		if err != nil {
			rerr2Ch <- err
			return
		}
		defer rc.Close()
		got, rerr := io.ReadAll(rc)
		if rerr != nil {
			rerr2Ch <- rerr
			return
		}
		if !bytes.Equal(got, chunkData) {
			rerr2Ch <- fmt.Errorf("reassembled bytes = %q, want %q", got, chunkData)
			return
		}
		rerr2Ch <- nil
	}()
	time.Sleep(10 * time.Millisecond)
	rcancel1()
	if err := <-rerr1Ch; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled reassemble caller 1 = %v, want context.Canceled", err)
	}
	close(rb.release)
	if err := <-rerr2Ch; err != nil {
		t.Fatalf("healthy reassemble caller 2 failed after caller 1 canceled: %v", err)
	}
}
