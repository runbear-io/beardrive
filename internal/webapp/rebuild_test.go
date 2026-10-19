package webapp

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

// gatedSource is a Source whose listing is set by the test and whose loads can
// be held open, so a test can put a write "during" a rebuild.
type gatedSource struct {
	mu    sync.Mutex
	files map[string]FileInfo
	gate  chan struct{} // when non-nil, Files blocks until it is closed
	loads int
}

func (g *gatedSource) set(paths ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.files = map[string]FileInfo{}
	for _, p := range paths {
		g.files[p] = FileInfo{Size: 1}
	}
}

func (g *gatedSource) hold() chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gate = make(chan struct{})
	return g.gate
}

func (g *gatedSource) Files(ctx context.Context) (map[string]FileInfo, error) {
	g.mu.Lock()
	gate := g.gate
	g.loads++
	out := make(map[string]FileInfo, len(g.files))
	for k, v := range g.files {
		out[k] = v
	}
	g.mu.Unlock()
	if gate != nil {
		<-gate
	}
	return out, nil
}

func (g *gatedSource) Open(context.Context, string, FileInfo) (io.ReadCloser, error) {
	return nil, fmt.Errorf("not used")
}

func has(t *testing.T, v *volume, path string) bool {
	t.Helper()
	snap, err := v.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, ok := snap.files[path]
	return ok
}

// A push is folded in the background: readers keep the previous snapshot
// until the rebuild lands, and the watcher is told only after it has.
func TestRefreshThenTellsWatchersOnlyOnceTheRebuildLanded(t *testing.T) {
	src := &gatedSource{}
	src.set("a.md")
	v := &volume{source: src, refresh: time.Hour}
	if !has(t, v, "a.md") {
		t.Fatal("seed")
	}

	src.set("a.md", "b.md")
	gate := src.hold()
	told := make(chan bool, 1)
	v.refreshThen(func() { told <- has(t, v, "b.md") })

	if has(t, v, "b.md") {
		t.Fatal("a reader waited for the rebuild instead of getting the previous snapshot")
	}
	select {
	case <-told:
		t.Fatal("watchers were told before the rebuild landed")
	case <-time.After(50 * time.Millisecond):
	}
	close(gate)
	if saw := <-told; !saw {
		t.Fatal("watchers were told, but fetching did not find the new file")
	}
}

// A write that arrives while a rebuild is running is not covered by it: its
// watcher is told after the NEXT rebuild, which starts after the write.
func TestAWriteDuringARebuildWaitsForTheNextOne(t *testing.T) {
	src := &gatedSource{}
	src.set("a.md")
	v := &volume{source: src, refresh: time.Hour}
	has(t, v, "a.md")

	gate := src.hold()
	first := make(chan struct{})
	v.refreshThen(func() { close(first) })
	for {
		src.mu.Lock()
		n := src.loads
		src.mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	src.set("a.md", "late.md") // lands after the running rebuild listed
	second := make(chan bool, 1)
	v.refreshThen(func() { second <- has(t, v, "late.md") })

	src.mu.Lock()
	src.gate = nil
	src.mu.Unlock()
	close(gate)
	<-first
	if saw := <-second; !saw {
		t.Fatal("a watcher was told about a write before a rebuild that includes it")
	}
}

// A slow background rebuild must not put its older listing back over a newer
// one a reader built in the meantime.
func TestAStaleRebuildDoesNotOverwriteANewerSnapshot(t *testing.T) {
	src := &gatedSource{}
	src.set("a.md")
	v := &volume{source: src, refresh: time.Hour}
	has(t, v, "a.md")

	gate := src.hold()
	done := make(chan struct{})
	v.refreshThen(func() { close(done) })
	for {
		src.mu.Lock()
		n := src.loads
		src.mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	src.mu.Lock()
	src.gate = nil
	src.mu.Unlock()
	src.set("a.md", "upload.md")
	v.invalidate() // an upload through the hub: rebuilt on the next read
	if !has(t, v, "upload.md") {
		t.Fatal("the synchronous rebuild missed the upload")
	}
	close(gate)
	<-done
	if !has(t, v, "upload.md") {
		t.Fatal("a background rebuild that started earlier put an older listing back")
	}
}

// current sees a write a background rebuild has not caught up to yet.
func TestCurrentDoesNotWaitForTheBackground(t *testing.T) {
	src := &gatedSource{}
	src.set("a.md")
	v := &volume{source: src, refresh: time.Hour}
	has(t, v, "a.md")

	src.set("a.md", "pushed.md")
	gate := src.hold()
	done := make(chan struct{})
	v.refreshThen(func() { close(done) })
	src.mu.Lock()
	src.gate = nil
	src.mu.Unlock()

	snap, err := v.current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snap.files["pushed.md"]; !ok {
		t.Fatal("current served the snapshot from before the push")
	}
	close(gate)
	<-done
}

// An invalidate that lands while a background rebuild is listing is not
// answered by that rebuild: it started before the write, so the next read
// still rebuilds.
func TestARebuildThatStartedBeforeAnInvalidateLeavesItStanding(t *testing.T) {
	src := &gatedSource{}
	src.set("a.md")
	v := &volume{source: src, refresh: time.Hour}
	has(t, v, "a.md")

	gate := src.hold()
	done := make(chan struct{})
	v.refreshThen(func() { close(done) })
	for {
		src.mu.Lock()
		n := src.loads
		src.mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	src.mu.Lock()
	src.gate = nil
	src.mu.Unlock()
	src.set("a.md", "upload.md")
	v.invalidate() // an upload through the hub, with no read before the rebuild lands
	close(gate)
	<-done
	if !has(t, v, "upload.md") {
		t.Fatal("a rebuild that listed before the upload satisfied its invalidate")
	}
}

type failingSource struct{ gatedSource }

func (f *failingSource) Files(ctx context.Context) (map[string]FileInfo, error) {
	f.mu.Lock()
	fail := f.gate != nil
	f.mu.Unlock()
	if fail {
		return nil, fmt.Errorf("storage unavailable")
	}
	return f.gatedSource.Files(ctx)
}

// A background rebuild that fails leaves the push to the next read instead of
// the previous snapshot standing until it expires, and does not tell watchers:
// they are told by the retry that lands it.
func TestAFailedBackgroundRebuildRetriesBeforeTellingWatchers(t *testing.T) {
	defer func(d time.Duration) { rebuildRetryMin = d }(rebuildRetryMin)
	rebuildRetryMin = time.Millisecond

	src := &failingSource{}
	src.set("a.md")
	v := &volume{source: src, refresh: time.Hour}
	has(t, v, "a.md")

	src.set("a.md", "pushed.md")
	src.hold() // makes the background load fail
	told := make(chan bool, 1)
	v.refreshThen(func() { told <- has(t, v, "pushed.md") })
	select {
	case <-told:
		t.Fatal("watchers were told after a rebuild that failed")
	case <-time.After(30 * time.Millisecond):
	}
	src.mu.Lock()
	src.gate = nil
	src.mu.Unlock()
	if !has(t, v, "pushed.md") {
		t.Fatal("the push stayed invisible after its background rebuild failed")
	}
	if saw := <-told; !saw {
		t.Fatal("watchers were told, but fetching did not find the push")
	}
}

// The conflict check behind If-Match writes reads the path's head. It must see
// a push the background has not folded in yet, or a save based on the
// previous version passes the check and overwrites the push.
func TestHeadSeesAPushTheBackgroundHasNotFoldedYet(t *testing.T) {
	src := &gatedSource{}
	src.set("a.md")
	v := &volume{source: src, refresh: time.Hour}
	has(t, v, "a.md")

	src.mu.Lock()
	src.files["a.md"] = FileInfo{Size: 1, Blob: "pushed"}
	src.mu.Unlock()
	gate := src.hold()
	done := make(chan struct{})
	v.refreshThen(func() { close(done) })
	src.mu.Lock()
	src.gate = nil
	src.mu.Unlock()

	s := &Server{}
	if head, _ := s.headBlob(context.Background(), v, "a.md"); head != "pushed" {
		t.Fatalf("head = %q: the conflict check read the snapshot from before the push", head)
	}
	close(gate)
	<-done
}

// A file a device has just pushed can be addressed at once (opened, shared,
// commented on) even while the background rebuild that lists it is running.
func TestAJustPushedPathResolvesBeforeTheBackgroundLands(t *testing.T) {
	src := &gatedSource{}
	src.set("a.md")
	v := &volume{source: src, refresh: time.Hour}
	has(t, v, "a.md")

	src.set("a.md", "new.md")
	gate := src.hold()
	done := make(chan struct{})
	v.refreshThen(func() { close(done) })
	src.mu.Lock()
	src.gate = nil
	src.mu.Unlock()

	snap, err := v.snapshotHaving(context.Background(), "new.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snap.files["new.md"]; !ok {
		t.Fatal("a just-pushed path was missing while the background caught up")
	}
	close(gate)
	<-done
}
