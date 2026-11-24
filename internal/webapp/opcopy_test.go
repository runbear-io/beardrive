package webapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

// ---- QA-02a: zero-copy histIndex + incremental/fallback parity vs cold hub ----

func TestZeroCopyHistoryIncrementalVsColdParity(t *testing.T) {
	srv, p, root := newHub(t, false, nil)
	projDir := filepath.Join(root, p.ID)
	f := newFakeRemoteAt(t, projDir)

	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	f.putAt("dev1", "old.md", "v1", t0)
	f.putAt("dev1", "old.md", "v2", t0.Add(time.Minute))
	f.move("dev1", "old.md", "moved.md", "v2", t0.Add(2*time.Minute))
	f.putAt("dev2", "b.md", "b1", t0.Add(3*time.Minute))
	f.putAt("dev3", "c.md", "c1", t0.Add(4*time.Minute))

	cb := newCountBackend(remote.Prefixed(srv.Root, p.ID+"/"))
	_, vol, err := srv.projectVolume(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	rs := &RemoteSource{Backend: cb}
	vol.source = rs
	h := srv.Handler()

	assertParity := func(step string) {
		t.Helper()
		coldSrv := &Server{Root: srv.Root, Projects: srv.Projects}
		coldH := coldSrv.Handler()
		urls := []string{
			"/api/p/" + p.ID + "/history?n=3",
			"/api/p/" + p.ID + "/history?path=moved.md&n=10",
			"/api/p/" + p.ID + "/history/summary?days=7&tz=UTC&all=1",
		}
		for _, u := range urls {
			got := do(t, h, "GET", u, nil)
			want := do(t, coldH, "GET", u, nil)
			if got.Code != 200 || want.Code != 200 {
				t.Fatalf("%s %s: status got=%d want=%d", step, u, got.Code, want.Code)
			}
			if got.Body.String() != want.Body.String() {
				t.Fatalf("%s %s mismatch:\ngot:  %s\nwant: %s", step, u, got.Body.String(), want.Body.String())
			}
		}
	}

	// 1. Cold build parity.
	assertParity("cold")
	dev1Gets, dev3Gets := cb.getsFor("dev1"), cb.getsFor("dev3")

	// 2. Two monotonic incremental tail appends on dev2 only.
	f.putAt("dev2", "moved.md", "v3", t0.Add(5*time.Minute))
	assertParity("inc1")
	f.putAt("dev2", "b.md", "b2", t0.Add(6*time.Minute))
	assertParity("inc2")
	if cb.getsFor("dev1") != dev1Gets || cb.getsFor("dev3") != dev3Gets {
		t.Fatalf("incremental appends re-read untouched journals: dev1 %d->%d, dev3 %d->%d",
			dev1Gets, cb.getsFor("dev1"), dev3Gets, cb.getsFor("dev3"))
	}

	// 3. Populate jcache via FilesWithMoves, then append another monotonic op and read history.
	if _, _, err := rs.FilesWithMoves(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.putAt("dev2", "moved.md", "v4", t0.Add(7*time.Minute))
	assertParity("inc_after_jcache_refresh")

	// 4. Out-of-order Lamport append forces fallback to full rebuild.
	f.append("dev1", journal.Op{
		Kind: journal.KindPut, Path: "ooo.md", Blob: strings.Repeat("a", 64),
		Size: 1, Mode: 0o644, Time: t0.Add(30 * time.Second),
	})
	// Force lamport lower than previous max by writing directly with lamport=1.
	b1 := journal.Op{
		Seq: 99, Lamport: 1, Time: t0.Add(15 * time.Second), Device: "dev1",
		Kind: journal.KindPut, Path: "ooo-early.md", Blob: strings.Repeat("a", 64), Size: 1, Mode: 0o644,
	}
	if err := journal.Append(filepath.Join(projDir, "journal", "dev1.jsonl"), []journal.Op{b1}); err != nil {
		t.Fatal(err)
	}
	assertParity("out_of_order_fallback")

	// 5. Vanished journal fallback.
	if err := os.Remove(filepath.Join(projDir, "journal", "dev3.jsonl")); err != nil {
		t.Fatal(err)
	}
	assertParity("vanished_journal_fallback")
}

// ---- QA-02b: histIndex re-anchors shared jcache slices across pushes ----

func TestHistIndexReanchorsSharedJournalSlices(t *testing.T) {
	ctx := t.Context()
	rs, _, f := countedProject(t)
	if _, _, err := rs.FilesWithMoves(ctx); err != nil {
		t.Fatal(err)
	}
	x0, err := rs.historyIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}

	rs.jmu.Lock()
	dev1Cached := rs.jcache["journal/dev1.jsonl"].ops
	dev2Cached := rs.jcache["journal/dev2.jsonl"].ops
	rs.jmu.Unlock()

	findSource := func(x *histIndex, key string) *sourcedJournal {
		for j := range x.sources {
			if x.sources[j].Key == key {
				return &x.sources[j]
			}
		}
		return nil
	}

	s0Dev1 := findSource(x0, "journal/dev1.jsonl")
	s0Dev2 := findSource(x0, "journal/dev2.jsonl")
	if s0Dev1 == nil || s0Dev2 == nil {
		t.Fatalf("missing keyed sources in x0: %+v", x0.sources)
	}
	if unsafe.SliceData(s0Dev1.Ops) != unsafe.SliceData(dev1Cached) {
		t.Fatal("x0 dev1 slice does not share jcache backing array")
	}
	origDev2Len := len(s0Dev2.Ops)
	snapOrder := append([]opRef(nil), x0.order...)
	snapKinds := append([]uint8(nil), x0.kinds...)

	// Push o1 to dev2 and force parsedJournals to refresh jcache FIRST (updatedMeta re-anchor path).
	f.putAs("dev2", "bob@x.io", "Bob", "notes/other.md", "bob v2")
	keep, err := rs.listJournals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rs.parsedJournals(ctx, keep); err != nil {
		t.Fatal(err)
	}
	rs.jmu.Lock()
	dev2AtO1 := rs.jcache["journal/dev2.jsonl"].ops
	rs.jmu.Unlock()
	if unsafe.SliceData(dev2AtO1) == unsafe.SliceData(dev2Cached) {
		t.Fatal("expected fetchJournal at o1 to allocate new grown jcache slice")
	}
	x1, err := rs.historyIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s1Dev2 := findSource(x1, "journal/dev2.jsonl")
	if s1Dev2 == nil || unsafe.SliceData(s1Dev2.Ops) != unsafe.SliceData(dev2AtO1) {
		t.Fatal("x1 did not re-anchor onto grown jcache slice via updatedMeta")
	}
	if len(s1Dev2.Ops) != origDev2Len || cap(s1Dev2.Ops) != origDev2Len {
		t.Fatalf("re-anchored slice len/cap = %d/%d, want %d/%d", len(s1Dev2.Ops), cap(s1Dev2.Ops), origDev2Len, origDev2Len)
	}
	if !reflect.DeepEqual(x0.order, snapOrder) || !reflect.DeepEqual(x0.kinds, snapKinds) {
		t.Fatal("incremental update mutated prev histIndex slices")
	}

	s1Tail := &x1.sources[len(x1.sources)-1]
	if s1Tail.Key != "journal/dev2.jsonl" || unsafe.SliceData(s1Tail.Ops) != unsafe.SliceData(dev2AtO1[origDev2Len:]) {
		t.Fatal("x1 tail source did not record key/offset onto dev2AtO1")
	}

	// Now push o2 to dev2 and build history BEFORE parsedJournals refreshes jcache (prev.journals re-anchor path).
	f.putAs("dev2", "bob@x.io", "Bob", "notes/other.md", "bob v3")
	x2, err := rs.historyIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s2Dev2 := findSource(x2, "journal/dev2.jsonl")
	if s2Dev2 == nil || unsafe.SliceData(s2Dev2.Ops) != unsafe.SliceData(dev2AtO1) {
		t.Fatal("x2 did not re-anchor onto dev2AtO1 jcache slice via prev.journals")
	}
	s2PriorTail := &x2.sources[len(x1.sources)-1]
	if s2PriorTail.Key != "journal/dev2.jsonl" || unsafe.SliceData(s2PriorTail.Ops) != unsafe.SliceData(dev2AtO1[origDev2Len:]) {
		t.Fatal("x2 did not re-anchor prior tail source onto dev2AtO1")
	}

	// And verify dev1 remains pinned onto the original dev1 jcache slice.
	s2Dev1 := findSource(x2, "journal/dev1.jsonl")
	if s2Dev1 == nil || unsafe.SliceData(s2Dev1.Ops) != unsafe.SliceData(dev1Cached) {
		t.Fatal("x2 lost dev1 backing array sharing")
	}
}

// ---- QA-03: cacheBudget couples historyCache and journalCache (CI -race via Concurrent) ----

func TestConcurrentCacheBudgetCouplesHistAndJournalEviction(t *testing.T) {
	ctx := t.Context()

	t.Run("zero_byte_history_does_not_block_journal_eviction", func(t *testing.T) {
		dirA := t.TempDir()
		beA, err := remote.Open(ctx, "file://"+dirA)
		if err != nil {
			t.Fatal(err)
		}
		a := &RemoteSource{Backend: beA}
		// Empty project: builds 0-byte historyCache entry in hubCaches.
		if _, err := a.historyIndex(ctx); err != nil {
			t.Fatal(err)
		}
		// Populate A's journals so A now holds >0 journalCache bytes while old 0-byte hist entry exists.
		fA := newFakeRemoteAt(t, dirA)
		fA.putAs("dev1", "alice@x.io", "Alice", "a.md", "hello")
		if _, _, err := a.FilesWithMoves(ctx); err != nil {
			t.Fatal(err)
		}
		aBytes := hubCaches.heldJournals(a)

		b, _, _ := countedProject(t)
		withCacheBudget(t, aBytes) // too small to keep A alongside B
		if _, _, err := b.FilesWithMoves(ctx); err != nil {
			t.Fatal(err)
		}

		a.jmu.Lock()
		aJ := len(a.jcache)
		a.jmu.Unlock()
		if aJ != 0 || hubCaches.held(a) != 0 {
			t.Fatalf("0-byte historyCache blocked eviction of A: jcache=%d held=%d", aJ, hubCaches.held(a))
		}
		histCache.mu.Lock()
		_, hasHist := histCache.entries[a]
		histCache.mu.Unlock()
		if hasHist {
			t.Fatal("0-byte historyCache was not cascaded with journalCache eviction")
		}
	})

	t.Run("three_project_eviction_drops_history_before_journal", func(t *testing.T) {
		a, _, _ := countedProject(t)
		c, _, _ := countedProject(t)
		b, _, _ := countedProject(t)

		if _, _, err := a.FilesWithMoves(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := a.historyIndex(ctx); err != nil {
			t.Fatal(err)
		}
		aJ := hubCaches.heldJournals(a)
		aH := hubCaches.held(a) - aJ
		if aH <= 0 {
			t.Fatalf("expected positive histIndex charge, got %d", aH)
		}
		// Small C steals the single "protected" journalCache slot.
		if _, _, err := c.FilesWithMoves(ctx); err != nil {
			t.Fatal(err)
		}
		cJ := hubCaches.heldJournals(c)

		// Budget fits A's jcache + C's jcache + B's jcache, forcing A's historyCache out first.
		withCacheBudget(t, aJ+cJ+cJ+10)
		if _, _, err := b.FilesWithMoves(ctx); err != nil {
			t.Fatal(err)
		}

		a.jmu.Lock()
		aJKept := len(a.jcache)
		a.jmu.Unlock()
		histCache.mu.Lock()
		_, aHKept := histCache.entries[a]
		histCache.mu.Unlock()
		if aJKept == 0 && aHKept {
			t.Fatal("evicted A's journalCache while A's historyCache still pinned its slices")
		}
		if aHKept {
			t.Fatal("expected A's historyCache to be evicted before/without A's journalCache")
		}
	})

	t.Run("history_hit_and_incremental_put_refresh_journal_recency", func(t *testing.T) {
		a, _, fA := countedProject(t)
		b, _, _ := countedProject(t)
		c, _, _ := countedProject(t)

		if _, _, err := a.FilesWithMoves(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := a.historyIndex(ctx); err != nil {
			t.Fatal(err)
		}
		if _, _, err := b.FilesWithMoves(ctx); err != nil {
			t.Fatal(err)
		}
		// Refresh A via history hit AND incremental history put; B is now LRU.
		if _, err := a.historyIndex(ctx); err != nil {
			t.Fatal(err)
		}
		fA.putAs("dev2", "bob@x.io", "Bob", "notes/other.md", "updated")
		if _, err := a.historyIndex(ctx); err != nil {
			t.Fatal(err)
		}

		withCacheBudget(t, hubCaches.held(a)+hubCaches.heldJournals(b)+10)
		if _, _, err := c.FilesWithMoves(ctx); err != nil {
			t.Fatal(err)
		}

		a.jmu.Lock()
		aKept := len(a.jcache)
		a.jmu.Unlock()
		b.jmu.Lock()
		bKept := len(b.jcache)
		b.jmu.Unlock()
		if aKept == 0 || bKept != 0 {
			t.Fatalf("expected active history project A kept (%d) and idle B dropped (%d)", aKept, bKept)
		}
	})

	t.Run("concurrent_eviction_drop_and_reads_race_free", func(t *testing.T) {
		a, _, fA := countedProject(t)
		b, _, _ := countedProject(t)
		withCacheBudget(t, 128)
		var wg sync.WaitGroup
		for g := range 6 {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				src := a
				if id%2 == 1 {
					src = b
				}
				for i := range 10 {
					_, _, _ = src.FilesWithMoves(ctx)
					_, _ = src.historyIndex(ctx)
					if id == 0 && i == 5 {
						fA.putAs("dev1", "alice@x.io", "Alice", "notes/plan.md", "conc")
					}
					src.jmu.Lock()
					jgen := src.jgen
					src.jmu.Unlock()
					src.dropJournals(jgen)
				}
			}(g)
		}
		wg.Wait()
	})

	t.Run("evicted_journal_during_build_skips_orphaned_hist_cache", func(t *testing.T) {
		a, _, _ := countedProject(t)
		b, _, _ := countedProject(t)

		keep, err := a.listJournals(ctx)
		if err != nil {
			t.Fatal(err)
		}
		sources, err := a.sourcedJournalsOf(ctx, keep)
		if err != nil {
			t.Fatal(err)
		}
		a.jmu.Lock()
		oldJGen := a.jgen
		a.jmu.Unlock()
		x := buildHistIndex(sources)

		// Another project evicts A's journalCache while A is still building histIndex.
		withCacheBudget(t, hubCaches.heldJournals(a))
		if _, _, err := b.FilesWithMoves(ctx); err != nil {
			t.Fatal(err)
		}

		histCache.putIfJournal(a, journalFingerprint(keep), x, oldJGen, true)
		if histCache.peek(a) != nil {
			t.Fatal("putIfJournal cached orphaned histIndex after journalCache was evicted")
		}
		if hubCaches.held(a) != 0 {
			t.Fatalf("expected 0 budget held for A after journal eviction, got %d", hubCaches.held(a))
		}
	})
}

// ---- QA-04: appendOpsIf multi-path extra, existingOps, keepAfter, landedAfter (CI -race via Upload) ----

type listSwitchBackend struct {
	*condBackend
	mu     sync.Mutex
	onList func(call int) []remote.Object
	calls  int
}

func (b *listSwitchBackend) List(ctx context.Context, prefix string) ([]remote.Object, error) {
	b.mu.Lock()
	b.calls++
	n := b.calls
	fn := b.onList
	b.mu.Unlock()
	if prefix == "journal/" && fn != nil {
		return fn(n), nil
	}
	return b.condBackend.List(ctx, prefix)
}

func TestUploadAppendOpsIfMultiPathExistingAndLandedAfter(t *testing.T) {
	ctx := t.Context()

	t.Run("multipath_extra_and_existing_tail_in_holds_and_stamp", func(t *testing.T) {
		be := newCondBackend()
		const hubKey = "journal/hub-a.jsonl"
		const peerKey = "journal/peer-1.jsonl"
		sha1 := strings.Repeat("1", 64)
		sha2 := strings.Repeat("2", 64)
		sha3 := strings.Repeat("3", 64)

		initialPeer, _ := journal.Marshal([]journal.Op{
			{Seq: 1, Lamport: 1, Time: time.Unix(100, 0).UTC(), Device: "peer-1", Kind: journal.KindPut, Path: "p1.md", Blob: sha1, Size: 1},
			{Seq: 2, Lamport: 2, Time: time.Unix(101, 0).UTC(), Device: "peer-1", Kind: journal.KindPut, Path: "p2.md", Blob: sha2, Size: 1},
		})
		be.write(peerKey, initialPeer)

		// Another hub instance appended to hub-a.jsonl (replacing p2.md with sha3 at Seq 10, Lamport 20),
		// but List still lags and does not list hub-a.jsonl yet — only GetVersioned returns it!
		unseenHub, _ := journal.Marshal([]journal.Op{
			{Seq: 10, Lamport: 20, Time: time.Unix(102, 0).UTC(), Device: "hub-a", Kind: journal.KindPut, Path: "p2.md", Blob: sha3, Size: 1},
		})
		be.write(hubKey, unseenHub)

		lb := &listSwitchBackend{
			condBackend: be,
			onList: func(int) []remote.Object {
				return []remote.Object{{Key: peerKey, Size: int64(len(initialPeer)), Modified: time.Unix(101, 0).UTC()}}
			},
		}
		rs := &RemoteSource{Backend: lb, Device: Identity{ID: "hub-a", Name: "hub", Author: "hub"}}

		// Conflict on p2.md inside extra must fail with staleWriteError naming sha3 (from existingOps),
		// and repeated p1.md in bases must stay deterministic.
		cb := &commitBase{
			path: "p1.md", sha: sha1, blob: sha2,
			extra: []commitBase{
				{path: "p2.md", sha: sha2, blob: sha1},
				{path: "p1.md", sha: sha1, blob: sha2},
			},
		}
		err := rs.appendOpsIf(ctx, []journal.Op{{Kind: journal.KindPut, Path: "p1.md", Blob: sha2, Size: 1}}, cb)
		var stale *staleWriteError
		if !errors.As(err, &stale) || stale.head != sha3 {
			t.Fatalf("expected staleWriteError head=%s, got err=%v", sha3, err)
		}

		// Non-conflicting multi-path write succeeds and stamp bumps Seq > 10 and Lamport > 20 from existingOps.
		okBase := &commitBase{
			path: "p1.md", sha: sha1, blob: sha2,
			extra: []commitBase{{path: "p2.md", sha: sha3, blob: sha1}},
		}
		ops := []journal.Op{{Kind: journal.KindPut, Path: "p1.md", Blob: sha2, Size: 1}}
		if err := rs.appendOpsIf(ctx, ops, okBase); err != nil {
			t.Fatalf("non-conflicting appendOpsIf: %v", err)
		}
		parsed, _ := journal.Parse(be.get(hubKey))
		last := parsed[len(parsed)-1]
		if last.Seq != 11 || last.Lamport != 21 {
			t.Fatalf("stamped seq=%d lamport=%d, want 11 and 21 (past unseen existingOps)", last.Seq, last.Lamport)
		}
	})

	t.Run("keepAfter_fingerprint_change_stale_vs_unrelated", func(t *testing.T) {
		sha1 := strings.Repeat("1", 64)
		sha2 := strings.Repeat("2", 64)
		sha3 := strings.Repeat("3", 64)
		peerV1, _ := journal.Marshal([]journal.Op{
			{Seq: 1, Lamport: 1, Time: time.Unix(100, 0).UTC(), Device: "peer-1", Kind: journal.KindPut, Path: "doc.md", Blob: sha1, Size: 1},
		})
		peerStale, _ := journal.Marshal([]journal.Op{
			{Seq: 1, Lamport: 1, Time: time.Unix(100, 0).UTC(), Device: "peer-1", Kind: journal.KindPut, Path: "doc.md", Blob: sha1, Size: 1},
			{Seq: 2, Lamport: 2, Time: time.Unix(101, 0).UTC(), Device: "peer-1", Kind: journal.KindPut, Path: "doc.md", Blob: sha3, Size: 1},
		})
		peerUnrelated, _ := journal.Marshal([]journal.Op{
			{Seq: 1, Lamport: 1, Time: time.Unix(100, 0).UTC(), Device: "peer-1", Kind: journal.KindPut, Path: "doc.md", Blob: sha1, Size: 1},
			{Seq: 2, Lamport: 2, Time: time.Unix(101, 0).UTC(), Device: "peer-1", Kind: journal.KindPut, Path: "other.md", Blob: sha3, Size: 1},
		})

		// Case A: keepAfter reveals conflicting write on doc.md -> immediate staleWriteError (no PutIf).
		beA := newCondBackend()
		beA.write("journal/peer-1.jsonl", peerV1)
		lbA := &listSwitchBackend{
			condBackend: beA,
			onList: func(call int) []remote.Object {
				if call >= 2 {
					beA.write("journal/peer-1.jsonl", peerStale)
					return []remote.Object{{Key: "journal/peer-1.jsonl", Size: int64(len(peerStale)), Modified: time.Unix(101, 0).UTC()}}
				}
				return []remote.Object{{Key: "journal/peer-1.jsonl", Size: int64(len(peerV1)), Modified: time.Unix(100, 0).UTC()}}
			},
		}
		rsA := &RemoteSource{Backend: lbA, Device: Identity{ID: "hub-a"}}
		err := rsA.appendOpsIf(ctx, []journal.Op{{Kind: journal.KindPut, Path: "doc.md", Blob: sha2, Size: 1}}, &commitBase{path: "doc.md", sha: sha1, blob: sha2})
		var stale *staleWriteError
		if !errors.As(err, &stale) || stale.head != sha3 {
			t.Fatalf("keepAfter stale: err=%v want head=%s", err, sha3)
		}
		if beA.puts != 0 {
			t.Fatalf("keepAfter stale wrote %d Puts, want 0", beA.puts)
		}

		// Case B: keepAfter reveals unrelated path -> ErrVersionMismatch (because appendConditionalExisting checks ErrVersionMismatch? Wait: verify error surfaces).
		// Note: appendConditionalExisting returns ErrVersionMismatch from next(existing) directly (pre-existing behavior);
		// keepAfter only validates base.holdsJournals before PutIf.
		beB := newCondBackend()
		beB.write("journal/peer-1.jsonl", peerV1)
		lbB := &listSwitchBackend{
			condBackend: beB,
			onList: func(call int) []remote.Object {
				if call == 2 {
					beB.write("journal/peer-1.jsonl", peerUnrelated)
					return []remote.Object{{Key: "journal/peer-1.jsonl", Size: int64(len(peerUnrelated)), Modified: time.Unix(101, 0).UTC()}}
				}
				return []remote.Object{{Key: "journal/peer-1.jsonl", Size: int64(len(peerV1)), Modified: time.Unix(100, 0).UTC()}}
			},
		}
		rsB := &RemoteSource{Backend: lbB, Device: Identity{ID: "hub-a"}}
		errB := rsB.appendOpsIf(ctx, []journal.Op{{Kind: journal.KindPut, Path: "doc.md", Blob: sha2, Size: 1}}, &commitBase{path: "doc.md", sha: sha1, blob: sha2})
		if !errors.Is(errB, remote.ErrVersionMismatch) {
			t.Fatalf("keepAfter unrelated expected ErrVersionMismatch, got %v", errB)
		}
	})

	t.Run("landedAfter_does_not_build_histCache", func(t *testing.T) {
		srv, p, _ := newHub(t, true, nil)
		h := srv.Handler()
		u := "/api/p/" + p.ID + "/upload/content?path=retry.md"
		baseText := "# List\n\n- item 1\n"
		aliceText := "# List\n\n- item 1\n- alice\n"
		bobText := "# List\n\n- item 1\n- bob\n"

		v0 := putSHA(t, h, u, baseText, "")
		putSHA(t, h, u, aliceText, v0)
		putContent(t, h, u, bobText, v0)
		rec := putContent(t, h, u, aliceText, v0)
		if rec.Code/100 != 2 {
			t.Fatalf("alice retry: %d %s", rec.Code, rec.Body.String())
		}
		_, v, err := srv.projectVolume(p.ID)
		if err != nil {
			t.Fatal(err)
		}
		rs := v.source.(*RemoteSource)
		if histCache.peek(rs) != nil {
			t.Fatal("landedAfter populated histCache unnecessarily")
		}
	})
}

// ---- QA-05: handleHistoryEmptied fast path + cross-journal delete/move ordering ----

func TestHistoryEmptiedFastPathAndCrossJournalOrder(t *testing.T) {
	t.Run("fast_path_zero_journal_gets_when_no_empty_files", func(t *testing.T) {
		srv, p, root := newHub(t, false, nil)
		f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
		f.put("dev1", "a.md", "non-empty")
		f.put("dev2", "b.md", "also non-empty")

		cb := newCountBackend(remote.Prefixed(srv.Root, p.ID+"/"))
		_, vol, err := srv.projectVolume(p.ID)
		if err != nil {
			t.Fatal(err)
		}
		vol.source = &RemoteSource{Backend: cb}
		h := srv.Handler()

		// Prime tree fold.
		if rec := do(t, h, "GET", "/api/p/"+p.ID+"/tree", nil); rec.Code != 200 {
			t.Fatalf("tree: %d", rec.Code)
		}
		getsBefore := cb.journalGets()

		rec := do(t, h, "GET", "/api/p/"+p.ID+"/history/emptied", nil)
		if rec.Code != 200 {
			t.Fatalf("emptied: %d %s", rec.Code, rec.Body.String())
		}
		if cb.journalGets() != getsBefore {
			t.Fatalf("emptied fetched %d journals when no 0-byte candidates exist", cb.journalGets()-getsBefore)
		}
	})

	t.Run("cross_journal_delete_after_move_when_delete_journal_sorts_first", func(t *testing.T) {
		srv, p, root := newHub(t, false, nil)
		projDir := filepath.Join(root, p.ID)
		f := newFakeRemoteAt(t, projDir)

		content := "had real content"
		sum := sha256.Sum256([]byte(content))
		shaContent := hex.EncodeToString(sum[:])
		emptySum := sha256.Sum256(nil)
		shaEmpty := hex.EncodeToString(emptySum[:])
		for b, data := range map[string][]byte{shaContent: []byte(content), shaEmpty: nil} {
			if err := os.WriteFile(filepath.Join(projDir, "blobs", b), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}

		t0 := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
		// dev-z sorts lexicographically AFTER dev-a, yet created & moved the file earlier in Lamport order!
		if err := journal.Append(filepath.Join(projDir, "journal", "dev-z.jsonl"), []journal.Op{
			{Seq: 1, Lamport: 1, Time: t0, Device: "dev-z", Kind: journal.KindPut, Path: "src.md", Blob: shaContent, Size: int64(len(content)), Mode: 0o644},
			{Seq: 2, Lamport: 2, Time: t0.Add(2 * time.Second), Device: "dev-z", Kind: journal.KindPut, Path: "dst.md", Blob: shaContent, Size: int64(len(content)), Mode: 0o644},
			{Seq: 3, Lamport: 3, Time: t0.Add(3 * time.Second), Device: "dev-z", Kind: journal.KindDelete, Path: "src.md"},
		}); err != nil {
			t.Fatal(err)
		}
		// dev-a sorts FIRST in List order, yet deleted dst.md and recreated it empty AFTER the move!
		if err := journal.Append(filepath.Join(projDir, "journal", "dev-a.jsonl"), []journal.Op{
			{Seq: 1, Lamport: 4, Time: t0.Add(4 * time.Second), Device: "dev-a", Kind: journal.KindDelete, Path: "dst.md"},
			{Seq: 2, Lamport: 5, Time: t0.Add(5 * time.Second), Device: "dev-a", Kind: journal.KindPut, Path: "dst.md", Blob: shaEmpty, Size: 0, Mode: 0o644},
		}); err != nil {
			t.Fatal(err)
		}
		_ = f

		rec := do(t, srv.Handler(), "GET", "/api/p/"+p.ID+"/history/emptied", nil)
		var out struct{ Paths []string }
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("emptied: %d %s", rec.Code, rec.Body.String())
		}
		if len(out.Paths) != 0 {
			t.Fatalf("cross-journal recreated empty file reported as emptied: %v", out.Paths)
		}
	})
}

// ---- QA-06: handleRestore strict Less tie, hidden 404-before-409, symlink/invalid blob ----

func TestRestoreZeroCopyTiesHidden404Before409AndInvalidBlob(t *testing.T) {
	t.Run("hidden_404_before_409_and_tie_order", func(t *testing.T) {
		h, _, cookies, p, root := permHubAt(t)
		projDir := filepath.Join(root, p.ID)
		f := newFakeRemoteAt(t, projDir)

		t0 := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
		const secretText = "secret moved version"
		sum := sha256.Sum256([]byte(secretText))
		shaSec := hex.EncodeToString(sum[:])
		if err := os.WriteFile(filepath.Join(projDir, "blobs", shaSec), []byte(secretText), 0o644); err != nil {
			t.Fatal(err)
		}

		// Move a file from a_secret/ to public/curr.md with the same content shaSec:
		// put(a_secret/curr.md, shaSec) at t0+1m precedes move->put(public/curr.md, shaSec) at t0+2m,
		// and lies strictly inside a_secret/curr.md's [zero, t0+2m) chain segment.
		f.putAt("dev1", "a_secret/curr.md", secretText, t0.Add(time.Minute))
		f.move("dev1", "a_secret/curr.md", "public/curr.md", secretText, t0.Add(2*time.Minute))

		const oldSec = "older secret"
		sumOld := sha256.Sum256([]byte(oldSec))
		shaOld := hex.EncodeToString(sumOld[:])
		if err := os.WriteFile(filepath.Join(projDir, "blobs", shaOld), []byte(oldSec), 0o644); err != nil {
			t.Fatal(err)
		}
		f.putAt("dev1", "a_secret/mv.md", oldSec, t0.Add(3*time.Minute))
		f.putAt("dev1", "a_secret/mv.md", secretText, t0.Add(4*time.Minute))
		f.move("dev1", "a_secret/mv.md", "public/mv.md", secretText, t0.Add(5*time.Minute))

		if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/folders", map[string]any{"prefix": "a_secret", "default": PermNone}, cookies["alice"]); rec.Code != 200 {
			t.Fatalf("set rule: %d %s", rec.Code, rec.Body.String())
		}

		// Bob tries to restore public/curr.md to its OWN current shaSec:
		// visibility on earliest found.Path (a_secret/curr.md) must return 404 before 409!
		rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/restore", map[string]string{
			"path": "public/curr.md",
			"sha":  shaSec,
		}, cookies["bob"])
		if rec.Code != http.StatusNotFound {
			t.Fatalf("bob restore current hidden-origin sha expected 404 before 409, got %d: %s", rec.Code, rec.Body.String())
		}

		// Hidden non-current ancestor version also returns 404.
		rec2 := doAs(t, h, "POST", "/api/p/"+p.ID+"/restore", map[string]string{
			"path": "public/mv.md",
			"sha":  shaOld,
		}, cookies["bob"])
		if rec2.Code != http.StatusNotFound {
			t.Fatalf("bob restore hidden ancestor = %d, want 404", rec2.Code)
		}

		// Cross-journal strict journal.Less winner: dev-b has Device="dev-a" < dev-a's Device="dev-z".
		const tieText = "tie content"
		sumTie := sha256.Sum256([]byte(tieText))
		shaTie := hex.EncodeToString(sumTie[:])
		if err := os.WriteFile(filepath.Join(projDir, "blobs", shaTie), []byte(tieText), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := journal.Append(filepath.Join(projDir, "journal", "dev-a.jsonl"), []journal.Op{
			{Seq: 1, Lamport: 1, Time: t0, Device: "dev-z", Kind: journal.KindPut, Path: "public/tie.md", Blob: shaTie, Size: 99, Mode: 0o644},
		}); err != nil {
			t.Fatal(err)
		}
		if err := journal.Append(filepath.Join(projDir, "journal", "dev-b.jsonl"), []journal.Op{
			{Seq: 1, Lamport: 1, Time: t0, Device: "dev-a", Kind: journal.KindPut, Path: "public/tie.md", Blob: shaTie, Size: 77, Mode: 0o644},
		}); err != nil {
			t.Fatal(err)
		}
		f.putAt("dev1", "public/tie.md", "latest tie", t0.Add(6*time.Minute))

		recTie := doAs(t, h, "POST", "/api/p/"+p.ID+"/restore", map[string]string{
			"path": "public/tie.md",
			"sha":  shaTie,
		}, cookies["alice"])
		var tieResp struct {
			Size int64 `json:"size"`
		}
		if recTie.Code != http.StatusOK || json.Unmarshal(recTie.Body.Bytes(), &tieResp) != nil {
			t.Fatalf("tie restore: %d %s", recTie.Code, recTie.Body.String())
		}
		if tieResp.Size != 77 {
			t.Fatalf("strict journal.Less picked Size=%d, want 77 (dev-b/Device=dev-a)", tieResp.Size)
		}

		// Equal journal.Less stability: identical comparison key across dev-a and dev-b keeps dev-a (key order).
		const eqText = "equal key content"
		sumEq := sha256.Sum256([]byte(eqText))
		shaEq := hex.EncodeToString(sumEq[:])
		if err := os.WriteFile(filepath.Join(projDir, "blobs", shaEq), []byte(eqText), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := journal.Append(filepath.Join(projDir, "journal", "dev-a.jsonl"), []journal.Op{
			{Seq: 2, Lamport: 2, Time: t0.Add(time.Second), Device: "dev-a", Kind: journal.KindPut, Path: "public/eq.md", Blob: shaEq, Size: 11, Mode: 0o644},
		}); err != nil {
			t.Fatal(err)
		}
		if err := journal.Append(filepath.Join(projDir, "journal", "dev-b.jsonl"), []journal.Op{
			{Seq: 2, Lamport: 2, Time: t0.Add(time.Second), Device: "dev-a", Kind: journal.KindPut, Path: "public/eq.md", Blob: shaEq, Size: 22, Mode: 0o644},
		}); err != nil {
			t.Fatal(err)
		}
		f.putAt("dev1", "public/eq.md", "latest eq", t0.Add(7*time.Minute))

		recEq := doAs(t, h, "POST", "/api/p/"+p.ID+"/restore", map[string]string{
			"path": "public/eq.md",
			"sha":  shaEq,
		}, cookies["alice"])
		var eqResp struct {
			Size int64 `json:"size"`
		}
		if recEq.Code != http.StatusOK || json.Unmarshal(recEq.Body.Bytes(), &eqResp) != nil {
			t.Fatalf("eq restore: %d %s", recEq.Code, recEq.Body.String())
		}
		if eqResp.Size != 11 {
			t.Fatalf("equal journal.Less stability picked Size=%d, want 11 (first journal dev-a)", eqResp.Size)
		}
	})

	t.Run("symlink_and_invalid_blob_trailing_ops", func(t *testing.T) {
		srv, p, root := newHub(t, true, nil)
		f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
		h := srv.Handler()
		f.put("dev1", "bad.md", "good content")
		sum := sha256.Sum256([]byte("good content"))
		goodSHA := hex.EncodeToString(sum[:])
		f.append("dev1", journal.Op{Kind: journal.KindPut, Path: "bad.md", Blob: "../../not-a-sha", Size: 0})

		// Trailing invalid-blob put is ignored by foldFiles and commitBase.holdsJournals, so goodSHA is still current -> 409.
		rec := do(t, h, "POST", "/api/p/"+p.ID+"/restore", map[string]string{"path": "bad.md", "sha": goodSHA})
		if rec.Code != http.StatusConflict {
			t.Fatalf("restore with trailing invalid blob = %d (%s), want 409", rec.Code, rec.Body.String())
		}

		// Trailing symlink over goodSHA -> files[p] has Link != "", so restore succeeds (200) and restores regular file.
		f.put("dev1", "sym.md", "good content")
		f.append("dev1", journal.Op{Kind: journal.KindPut, Path: "sym.md", Link: "target.md", Mode: 0o777})
		recSym := do(t, h, "POST", "/api/p/"+p.ID+"/restore", map[string]string{"path": "sym.md", "sha": goodSHA})
		if recSym.Code != http.StatusOK {
			t.Fatalf("restore over symlink = %d: %s", recSym.Code, recSym.Body.String())
		}
		if got := do(t, h, "GET", "/api/p/"+p.ID+"/file?path=sym.md", nil); got.Code != 200 || got.Body.String() != "good content" {
			t.Fatalf("after restore over symlink: %d %q", got.Code, got.Body.String())
		}
	})
}

// ---- QA-07: undo-run deleted file (mustBeAbsent) + unknown-kind/interleaved tie order ----

func TestUndoRunDeletedFileMustBeAbsentAndTieOrder(t *testing.T) {
	t.Run("http_undo_deleted_file_resurrects_and_conflicts_on_concurrent_recreate", func(t *testing.T) {
		srv, p, root := newHub(t, true, nil)
		f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
		h := srv.Handler()

		f.put("dev1", "victim.md", "original bytes")
		f.append("dev1", journal.Op{
			Kind: journal.KindDelete, Path: "victim.md",
			Session: "run-del", Note: "claude-code session run-del",
		})

		rec := do(t, h, "POST", "/api/p/"+p.ID+"/undo-run", map[string]any{
			"device": "dev1", "session": "run-del", "preview": false,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("undo deleted file failed (%d): %s", rec.Code, rec.Body.String())
		}
		fileRec := do(t, h, "GET", "/api/p/"+p.ID+"/file?path=victim.md", nil)
		if fileRec.Code != 200 || fileRec.Body.String() != "original bytes" {
			t.Fatalf("resurrected file = %d %q", fileRec.Code, fileRec.Body.String())
		}
	})

	t.Run("undo_deleted_file_mustBeAbsent_unit_conflict", func(t *testing.T) {
		shaOld := strings.Repeat("a", 64)
		shaRace := strings.Repeat("b", 64)
		t0 := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
		journals := []sourcedJournal{
			{
				Key: "journal/dev1.jsonl", From: "dev1",
				Ops: []journal.Op{
					{Seq: 1, Lamport: 1, Time: t0, Device: "dev1", Kind: journal.KindPut, Path: "victim.md", Blob: shaOld, Size: 3},
					{Seq: 2, Lamport: 2, Time: t0.Add(time.Second), Device: "dev1", Kind: journal.KindDelete, Path: "victim.md", Session: "s1"},
				},
			},
		}
		plan, files := planUndoJournals(journals, undoSel{Device: "dev1", Session: "s1"})
		if _, ok := files["victim.md"]; ok {
			t.Fatal("deleted path must be absent from planUndoJournals current map")
		}
		if len(plan.Ops) != 1 || plan.Ops[0].Kind != journal.KindPut {
			t.Fatalf("unexpected plan: %+v", plan)
		}
		// Build commitBase exactly as handleUndoRun does.
		cb := &commitBase{path: "victim.md", mustBeAbsent: true, blob: shaOld}
		// Still absent -> holds.
		if err := cb.holdsJournals(journals, nil); err != nil {
			t.Fatalf("expected absent path to hold: %v", err)
		}
		// Another peer recreated victim.md with shaRace -> mustBeAbsent rejects!
		journals[0].Ops = append(journals[0].Ops, journal.Op{
			Seq: 3, Lamport: 3, Time: t0.Add(2 * time.Second), Device: "dev1",
			Kind: journal.KindPut, Path: "victim.md", Blob: shaRace, Size: 5,
		})
		var stale *staleWriteError
		if err := cb.holdsJournals(journals, nil); !errors.As(err, &stale) || stale.head != shaRace {
			t.Fatalf("recreated file did not trigger staleWriteError(%s): %v", shaRace, err)
		}
	})

	t.Run("planUndo_wrapper_unknown_kind_and_interleaved_ties", func(t *testing.T) {
		t0 := time.Date(2026, 11, 4, 1, 0, 0, 0, time.UTC)
		tiedBase := journal.Op{Seq: 1, Lamport: 1, Time: t0, Device: "shared", Kind: journal.KindPut, Path: "tie.md", Blob: "v1", Size: 2}
		secondTie := tiedBase
		secondTie.UserName = "later-tie"
		sourced := []sourcedOp{
			{From: "dev1", Op: tiedBase},
			{From: "dev2", Op: secondTie}, // interleaved dev2 between two dev1 ops
			{From: "dev1", Op: journal.Op{Seq: 2, Lamport: 2, Time: t0.Add(time.Minute), Device: "dev1", Kind: journal.KindPut, Path: "tie.md", Blob: "v2", Size: 2, Session: "r1"}},
			// Unknown kind op after the run must be ignored by current state calculation.
			{From: "dev1", Op: journal.Op{Seq: 3, Lamport: 3, Time: t0.Add(2 * time.Minute), Device: "dev1", Kind: "noop", Path: "tie.md"}},
		}
		plan := planUndo(sourced, undoSel{Device: "dev1", Session: "r1"})
		if len(plan.Ops) != 1 || plan.Ops[0].Blob != "v1" {
			t.Fatalf("plan = %+v, want restore of v1", plan)
		}
	})
}

// ---- QA-08: visibleSHASet bounded cache, cancel isolation, fail-closed (CI -race via Store) ----

type errBackend struct {
	remote.Backend
	failList atomic.Bool
}

func (e *errBackend) List(ctx context.Context, prefix string) ([]remote.Object, error) {
	if e.failList.Load() {
		return nil, errors.New("injected storage outage")
	}
	return e.Backend.List(ctx, prefix)
}

func TestStoreVisibleSHACacheBoundedCancelAndFailClosed(t *testing.T) {
	t.Run("bounded_scopes_pushes_and_no_empty_blob", func(t *testing.T) {
		rs, _, f := countedProject(t)
		ctx := t.Context()
		// Add a delete op whose Blob is "" to confirm "" is never cached.
		f.del("dev1", "notes/plan.md")

		for i := range maxVisibleSHAScopes + 10 {
			pf := pathFilter{
				project: Project{ID: "p", Folders: []FolderRule{{Prefix: "secret/", Default: PermNone}}},
				email:   fmt.Sprintf("u%d@x.io", i),
				base:    PermRead,
				tag:     fmt.Sprintf("scope-%03d", i),
				on:      true,
			}
			shas, err := pf.visibleSHASet(ctx, rs)
			if err != nil {
				t.Fatal(err)
			}
			if _, hasEmpty := shas[""]; hasEmpty {
				t.Fatal("visibleSHASet inserted empty blob key from delete/symlink op")
			}
		}
		rs.vshaMu.Lock()
		nScopes := len(rs.vshaCache)
		nEntries := rs.vshaEntries
		rs.vshaMu.Unlock()
		if nScopes > maxVisibleSHAScopes {
			t.Fatalf("vshaCache leaked %d scopes > %d", nScopes, maxVisibleSHAScopes)
		}
		if nEntries > maxVisibleSHAEntries {
			t.Fatalf("vshaEntries = %d > %d", nEntries, maxVisibleSHAEntries)
		}

		// Advancing fp replaces previous fp entries cleanly.
		f.putAs("dev1", "alice@x.io", "Alice", "new.md", "fresh")
		pf := pathFilter{
			project: Project{ID: "p", Folders: []FolderRule{{Prefix: "secret/", Default: PermNone}}},
			email:   "u0@x.io", base: PermRead, tag: "scope-new", on: true,
		}
		if _, err := pf.visibleSHASet(ctx, rs); err != nil {
			t.Fatal(err)
		}
		rs.vshaMu.Lock()
		nAfter := len(rs.vshaCache)
		rs.vshaMu.Unlock()
		if nAfter != 1 {
			t.Fatalf("after fp bump vshaCache kept %d stale entries, want 1", nAfter)
		}
	})

	t.Run("singleflight_caller_cancel_does_not_fail_second_caller", func(t *testing.T) {
		rs, gb := gatedProject(t)
		pf := pathFilter{
			project: Project{ID: "p", Folders: []FolderRule{{Prefix: "secret/", Default: PermNone}}},
			email:   "bob@x.io", base: PermRead, tag: "scope-bob", on: true,
		}
		ctx1, cancel1 := context.WithCancel(t.Context())
		err1Ch := make(chan error, 1)
		go func() {
			_, err := pf.visibleSHASet(ctx1, rs)
			err1Ch <- err
		}()
		res2Ch := make(chan struct {
			n   int
			err error
		}, 1)
		go func() {
			shas, err := pf.visibleSHASet(t.Context(), rs)
			res2Ch <- struct {
				n   int
				err error
			}{len(shas), err}
		}()

		// Wait until gatedProject's Get is reached, then cancel Caller 1 before releasing.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			gb.mu.Lock()
			lists := gb.lists
			gb.mu.Unlock()
			if lists >= 2 {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		cancel1()
		if err := <-err1Ch; !errors.Is(err, context.Canceled) {
			t.Fatalf("caller 1 err = %v, want context.Canceled", err)
		}
		close(gb.release)
		res2 := <-res2Ch
		if res2.err != nil || res2.n == 0 {
			t.Fatalf("caller 2 failed because caller 1 canceled: n=%d err=%v", res2.n, res2.err)
		}
	})

	t.Run("storage_error_fails_closed_and_recovers", func(t *testing.T) {
		h, srv, cookies, p, sha := hiddenHub(t)
		// Seed a readable public file whose SHA we can resolve once healthy.
		pubRec := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, cookies["bob"])
		if pubRec.Code != 200 {
			t.Fatalf("tree: %d", pubRec.Code)
		}
		_, v, err := srv.projectVolume(p.ID)
		if err != nil {
			t.Fatal(err)
		}
		rs := v.source.(*RemoteSource)
		eb := &errBackend{Backend: rs.Backend}
		rs.Backend = eb

		pubSHA := shaOfSeeded(t, h, p.ID, cookies["alice"], "notes/open.md")
		_ = sha

		eb.failList.Store(true)
		if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/blob?sha="+pubSHA, nil, cookies["bob"]); rec.Code != http.StatusNotFound {
			t.Fatalf("storage outage did not fail closed on /blob: %d", rec.Code)
		}
		eb.failList.Store(false)
		if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/blob?sha="+pubSHA, nil, cookies["bob"]); rec.Code != http.StatusOK {
			t.Fatalf("recovery after outage failed (negative cached?): %d %s", rec.Code, rec.Body.String())
		}
	})
}

// ---- QA-09: dataapply.go tombstone fallback picks greatest KindDelete ----

func TestDataApplyZeroCopyTombstoneFallback(t *testing.T) {
	h, srv, cookies, p, root := permHubAt(t)
	x, err := OpenDataIndex("sqlite", filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	srv.Data = x
	d := &dataHub{t: t, h: h, srv: srv, cookies: cookies, p: p}
	d.enable()

	projDir := filepath.Join(root, p.ID)
	newFakeRemoteAt(t, projDir)
	body := `{"status":"live"}`
	sum := sha256.Sum256([]byte(body))
	blob := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(projDir, "blobs", blob), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	if err := journal.Append(filepath.Join(projDir, "journal", "dev1.jsonl"), []journal.Op{
		{Seq: 1, Lamport: 1, Time: t0, Device: "dev1", User: "alice@x.io", Kind: journal.KindPut, Path: "items/a.json", Blob: blob, Size: int64(len(body)), Mode: 0o644},
	}); err != nil {
		t.Fatal(err)
	}
	d.srv.dataWrote(t.Context(), d.p.ID)
	if got := d.query("alice", map[string]any{"pattern": "items/*.json", "count": true}).Count; got != 1 {
		t.Fatalf("initial indexed rows = %d, want 1", got)
	}

	// Delete items/a.json from two journals (dev2 wins at Lamport 20 > dev1 Lamport 10).
	if err := journal.Append(filepath.Join(projDir, "journal", "dev1.jsonl"), []journal.Op{
		{Seq: 2, Lamport: 10, Time: t0.Add(time.Minute), Device: "dev1", User: "alice@x.io", Kind: journal.KindDelete, Path: "items/a.json"},
	}); err != nil {
		t.Fatal(err)
	}
	winDel := journal.Op{
		Seq: 1, Lamport: 20, Time: t0.Add(2 * time.Minute), Device: "dev2", User: "bob@x.io",
		Kind: journal.KindDelete, Path: "items/a.json",
	}
	if err := journal.Append(filepath.Join(projDir, "journal", "dev2.jsonl"), []journal.Op{winDel}); err != nil {
		t.Fatal(err)
	}

	_, v, err := d.srv.projectVolume(d.p.ID)
	if err != nil {
		t.Fatal(err)
	}
	rs := v.source.(*RemoteSource)
	// Refresh snapshot so snap.files lacks items/a.json, then clear foldTombstones so
	// dataPass exercises the loadSourcedJournals fallback path.
	v.invalidate()
	if _, err := v.snapshot(t.Context()); err != nil {
		t.Fatal(err)
	}
	rs.foldMu.Lock()
	rs.foldTombstones = nil
	rs.foldMu.Unlock()

	st := d.srv.dataProjectState(d.p.ID)
	st.mu.Lock()
	err = d.srv.dataPass(t.Context(), d.p.ID, st)
	st.mu.Unlock()
	if err != nil {
		t.Fatalf("dataPass: %v", err)
	}
	if got := d.query("alice", map[string]any{"pattern": "items/*.json", "count": true}).Count; got != 0 {
		t.Fatalf("after delete query count = %d, want 0", got)
	}
	var deleted bool
	var win []byte
	var changedBy string
	if err := x.w.QueryRow(`SELECT deleted, win, changed_by FROM data_items WHERE project = ? AND path = ?`, d.p.ID, "items/a.json").Scan(&deleted, &win, &changedBy); err != nil {
		t.Fatalf("scan tombstone row: %v", err)
	}
	if !deleted || !bytes.Equal(win, winKey(winDel)) || changedBy != "bob@x.io" {
		t.Fatalf("tombstone row = (deleted=%v, changedBy=%q, winMatch=%v), want (true, bob@x.io, true)",
			deleted, changedBy, bytes.Equal(win, winKey(winDel)))
	}
}

// ---- QA-11: concurrent mixed reads/writes/evictions race + slice immutability (CI -race via Concurrent) ----

func TestConcurrentZeroCopyReadWriteEvictionImmutability(t *testing.T) {
	h, srv, cookies, p, root := permHubAt(t)
	projDir := filepath.Join(root, p.ID)
	f := newFakeRemoteAt(t, projDir)

	t0 := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	// Small (< 2 MiB) journals exercise the parseChunk cap>len clamp path too.
	for i := range 20 {
		f.putAt("dev1", fmt.Sprintf("notes/f-%02d.md", i), fmt.Sprintf("body-%d", i), t0.Add(time.Duration(i)*time.Second))
	}
	f.putAt("dev1", "notes/empty.md", "had bytes", t0.Add(21*time.Second))
	f.putAt("dev1", "notes/empty.md", "", t0.Add(22*time.Second))
	f.putAt("dev1", "secret/s.md", "hidden", t0.Add(23*time.Second))
	f.putAt("dev2", "notes/undo.md", "v0", t0.Add(24*time.Second))
	f.append("dev2", journal.Op{
		Kind: journal.KindPut, Path: "notes/undo.md",
		Blob: sha256HexBytes([]byte("v1")), Size: 2, Mode: 0o644,
		Time: t0.Add(25 * time.Second), Session: "sess-1",
	})
	_ = os.WriteFile(filepath.Join(projDir, "blobs", sha256HexBytes([]byte("v1"))), []byte("v1"), 0o644)

	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/folders", map[string]any{"prefix": "secret", "default": PermNone}, cookies["alice"]); rec.Code != 200 {
		t.Fatalf("set rule: %d", rec.Code)
	}

	_, v, err := srv.projectVolume(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	rs := v.source.(*RemoteSource)

	// Warm once and verify jcache slices are structurally clamped (cap == len).
	if _, _, err := rs.FilesWithMoves(t.Context()); err != nil {
		t.Fatal(err)
	}
	rs.jmu.Lock()
	for k, c := range rs.jcache {
		if cap(c.ops) != len(c.ops) {
			rs.jmu.Unlock()
			t.Fatalf("jcache[%s] has cap=%d != len=%d; borrowed slice append could mutate cache", k, cap(c.ops), len(c.ops))
		}
	}
	dev1Orig := rs.jcache["journal/dev1.jsonl"].ops
	dev1Snap := append([]journal.Op(nil), dev1Orig...)
	rs.jmu.Unlock()

	pubSHA := sha256HexBytes([]byte("body-1"))
	var wg sync.WaitGroup
	// Readers hitting every refactored route concurrently.
	for g := range 6 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range 12 {
				switch (id + i) % 5 {
				case 0:
					_ = doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, cookies["bob"])
				case 1:
					_ = doAs(t, h, "GET", "/api/p/"+p.ID+"/history?n=10", nil, cookies["bob"])
				case 2:
					_ = doAs(t, h, "GET", "/api/p/"+p.ID+"/history/summary?days=7", nil, cookies["bob"])
				case 3:
					_ = doAs(t, h, "GET", "/api/p/"+p.ID+"/history/emptied", nil, cookies["bob"])
				case 4:
					_ = doAs(t, h, "GET", "/api/p/"+p.ID+"/blob?sha="+pubSHA, nil, cookies["bob"])
				}
			}
		}(g)
	}
	// Concurrent writers (upload + restore + undo preview) and cache drops.
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 6 {
			_ = doAs(t, h, "PUT", fmt.Sprintf("/api/p/%s/upload/content?path=notes/w-%d.md", p.ID, i),
				[]byte(fmt.Sprintf("w-%d", i)), cookies["alice"])
			_ = doAs(t, h, "POST", "/api/p/"+p.ID+"/restore",
				map[string]string{"path": "notes/f-01.md", "sha": pubSHA}, cookies["alice"])
			_ = doAs(t, h, "POST", "/api/p/"+p.ID+"/undo-run",
				map[string]any{"device": "dev2", "session": "sess-1", "preview": true}, cookies["alice"])
		}
	}()
	go func() {
		defer wg.Done()
		for range 8 {
			rs.jmu.Lock()
			jgen := rs.jgen
			rs.jmu.Unlock()
			rs.dropJournals(jgen)
			runtimeYield()
		}
	}()
	wg.Wait()

	// Verify both the originally loaned backing slice and refreshed jcache entry kept dev1 intact.
	if !reflect.DeepEqual(dev1Orig, dev1Snap) {
		t.Fatal("originally cached dev1 slice backing array was mutated during concurrent zero-copy operations")
	}
	if _, err := rs.loadSourcedJournals(t.Context()); err != nil {
		t.Fatal(err)
	}
	rs.jmu.Lock()
	gotDev1 := rs.jcache["journal/dev1.jsonl"].ops
	rs.jmu.Unlock()
	if len(gotDev1) < len(dev1Snap) || !reflect.DeepEqual(gotDev1[:len(dev1Snap)], dev1Snap) {
		t.Fatalf("cached dev1 journal slice mismatch after refresh: len=%d want>=%d", len(gotDev1), len(dev1Snap))
	}
}

func sha256HexBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
