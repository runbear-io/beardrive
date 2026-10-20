package webapp

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/remote"
)

// gateBackend holds every journal Get until release is closed, so a test can
// line up concurrent callers behind one cold load.
type gateBackend struct {
	*countBackend
	release chan struct{}
}

func (b *gateBackend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	<-b.release
	return b.countBackend.Get(ctx, key)
}

func gatedProject(t *testing.T) (*RemoteSource, *gateBackend) {
	t.Helper()
	dir := t.TempDir()
	f := newFakeRemoteAt(t, dir)
	f.putAs("dev1", "alice@x.io", "Alice", "notes/plan.md", "v1")
	f.putAs("dev2", "bob@x.io", "Bob", "notes/other.md", "bob")
	f.putAs("dev3", "carol@x.io", "Carol", "readme.md", "top")
	be, err := remote.Open(context.Background(), "file://"+dir)
	if err != nil {
		t.Fatal(err)
	}
	gb := &gateBackend{countBackend: newCountBackend(be), release: make(chan struct{})}
	return &RemoteSource{Backend: gb}, gb
}

// Requests that arrive while the history index for one journal state is being
// built wait for that build instead of each building their own copy: on a
// large project every copy is the whole project's ops, and a team opening the
// same page at once multiplied the hub's memory by the number of people.
func TestConcurrentHistoryRequestsShareOneBuild(t *testing.T) {
	rs, gb := gatedProject(t)
	const n = 5
	got := make([]*histIndex, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			x, err := rs.historyIndex(context.Background())
			if err != nil {
				t.Error(err)
			}
			got[i] = x
		}()
	}
	waitForGets(gb)
	close(gb.release)
	wg.Wait()
	for i := 1; i < n; i++ {
		if got[i] != got[0] {
			t.Fatal("concurrent history requests built separate indexes")
		}
	}
	if g := gb.journalGets(); g != 3 {
		t.Fatalf("%d journal fetches for 3 journals: concurrent builds each fetched", g)
	}
}

// The same holds for the fold behind the tree: one cold fold, however many
// requests are waiting on it.
func TestConcurrentFoldsShareOneLoad(t *testing.T) {
	rs, gb := gatedProject(t)
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := rs.FilesWithMoves(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	waitForGets(gb)
	close(gb.release)
	wg.Wait()
	if g := gb.journalGets(); g != 3 {
		t.Fatalf("%d journal fetches for 3 journals: concurrent folds each fetched", g)
	}
}

// waitForGets gives the concurrent callers time to reach the gate.
func waitForGets(gb *gateBackend) {
	for range 50 {
		gb.mu.Lock()
		n := gb.lists
		gb.mu.Unlock()
		if n >= 5 {
			return
		}
		runtimeYield()
	}
}

func runtimeYield() { <-time.After(2 * time.Millisecond) }

func countedProject(t *testing.T) (*RemoteSource, *countBackend, *fakeRemote) {
	t.Helper()
	dir := t.TempDir()
	f := newFakeRemoteAt(t, dir)
	f.putAs("dev1", "alice@x.io", "Alice", "notes/plan.md", "v1")
	f.putAs("dev2", "bob@x.io", "Bob", "notes/other.md", "bob")
	f.putAs("dev3", "carol@x.io", "Carol", "readme.md", "top")
	be, err := remote.Open(context.Background(), "file://"+dir)
	if err != nil {
		t.Fatal(err)
	}
	cb := newCountBackend(be)
	return &RemoteSource{Backend: cb}, cb, f
}

func withCacheBudget(t *testing.T, bytes int64) {
	t.Helper()
	old := hubCaches.limitBytes()
	hubCaches.setLimit(bytes)
	t.Cleanup(func() { hubCaches.setLimit(old) })
}

// A project bigger than the cache budget keeps its parsed journals: its next
// fold needs every one of them in memory anyway, so dropping them saves
// nothing and turns every push into a re-download of the whole project.
func TestAProjectLargerThanTheBudgetStillReparsesOnlyThePush(t *testing.T) {
	withCacheBudget(t, 1)
	rs, cb, f := countedProject(t)
	ctx := context.Background()
	if _, _, err := rs.FilesWithMoves(ctx); err != nil {
		t.Fatal(err)
	}
	b1, b3 := cb.getsFor("dev1"), cb.getsFor("dev3")
	f.putAs("dev2", "bob@x.io", "Bob", "notes/other.md", "bob again")
	if _, _, err := rs.FilesWithMoves(ctx); err != nil {
		t.Fatal(err)
	}
	if cb.getsFor("dev1") != b1 || cb.getsFor("dev3") != b3 {
		t.Fatal("a push re-fetched journals that did not change")
	}
}

// The budget is shared across projects: going over it drops the project used
// least recently, not the one being folded.
func TestTheBudgetDropsTheLeastRecentlyUsedProject(t *testing.T) {
	a, _, _ := countedProject(t)
	b, _, _ := countedProject(t)
	ctx := context.Background()
	a.FilesWithMoves(ctx)
	a.jmu.Lock()
	one := a.jbytes
	a.jmu.Unlock()
	withCacheBudget(t, one+one/2) // room for one project
	b.FilesWithMoves(ctx)

	a.jmu.Lock()
	aKept := len(a.jcache)
	a.jmu.Unlock()
	b.jmu.Lock()
	bKept := len(b.jcache)
	b.jmu.Unlock()
	if aKept != 0 || bKept == 0 {
		t.Fatalf("a kept %d, b kept %d: want the older project dropped and the newer kept", aKept, bKept)
	}
}

// Likewise a history index larger than the budget is still cached: building
// it per request costs the same memory, plus the sort, every time.
func TestAHistoryIndexLargerThanTheBudgetIsStillReused(t *testing.T) {
	withCacheBudget(t, 1)
	rs, _, _ := countedProject(t)
	ctx := context.Background()
	x1, err := rs.historyIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	x2, _ := rs.historyIndex(ctx)
	if x1 != x2 {
		t.Fatal("a history index over the budget was rebuilt on the next request")
	}
}

// History indexes are charged to the same budget as parsed journals: one
// ceiling for the hub, not one per kind of cache.
func TestHistoryIndexesShareTheJournalBudget(t *testing.T) {
	a, _, _ := countedProject(t)
	b, _, _ := countedProject(t)
	ctx := context.Background()
	a.FilesWithMoves(ctx)
	if _, err := a.historyIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if got := hubCaches.held(a); got <= hubCaches.heldJournals(a) {
		t.Fatalf("a's history index is not charged: holds %d, journals alone %d", got, hubCaches.heldJournals(a))
	}
	withCacheBudget(t, hubCaches.held(a)+1) // room for one project's caches
	b.FilesWithMoves(ctx)
	b.historyIndex(ctx)

	histCache.mu.Lock()
	_, aIndexKept := histCache.entries[a]
	_, bIndexKept := histCache.entries[b]
	histCache.mu.Unlock()
	if aIndexKept || !bIndexKept {
		t.Fatalf("a's index kept=%v, b's kept=%v: want the older project's index dropped for the newer", aIndexKept, bIndexKept)
	}
}
