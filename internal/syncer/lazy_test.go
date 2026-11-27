package syncer

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
)

const lazyTestThreshold = 1024

// setLazy writes the device's .bdrive/config.json with a lazy policy.
func setLazy(t *testing.T, s *Session, on bool, pinned ...string) {
	t.Helper()
	p, _, err := config.LoadProject(s.Folder)
	if err != nil {
		t.Fatal(err)
	}
	p.Lazy = config.Lazy{Enabled: on, Threshold: lazyTestThreshold, Pinned: pinned}
	if _, err := config.SaveProject(s.Folder, p); err != nil {
		t.Fatal(err)
	}
}

func big(tag string) string { return tag + strings.Repeat("x", 4*lazyTestThreshold) + "\n" }

func ownDeletes(t *testing.T, s *Session) []string {
	t.Helper()
	ops, err := s.Store.DeviceOps(s.Device.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, op := range ops {
		if op.Kind == journal.KindDelete {
			out = append(out, op.Path)
		}
	}
	return out
}

func onDisk(s *Session, rel string) bool {
	_, err := os.Lstat(filepath.Join(s.Folder, filepath.FromSlash(rel)))
	return err == nil
}

// lazyPair is A (syncs everything) and B (lazy, "pinned" pinned) after B's
// first cycle on a project holding one small, one big and one pinned big file.
func lazyPair(t *testing.T) (a, b *Session) {
	t.Helper()
	be := sharedRemote(t)
	a = newDevice(t, "deva", be)
	write(t, a.Folder, "small.md", "small")
	write(t, a.Folder, "docs/big.html", big("v1"))
	write(t, a.Folder, "pinned/big.json", big("pin"))
	cycle(t, a)
	b = newDevice(t, "devb", be)
	setLazy(t, b, true, "pinned")
	cycle(t, b)
	return a, b
}

func TestLazy_BigFileStaysOnHub(t *testing.T) {
	_, b := lazyPair(t)
	if read(t, b.Folder, "small.md") != "small" {
		t.Fatal("small file not materialized")
	}
	if read(t, b.Folder, "pinned/big.json") != big("pin") {
		t.Fatal("pinned big file not materialized")
	}
	if onDisk(b, "docs/big.html") {
		t.Fatal("lazy file was written to disk")
	}
	if b.Store.HasBlob(sha(big("v1"))) {
		t.Fatal("lazy file's content was downloaded on sync")
	}
	ix, err := b.LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ix.Files["docs/big.html"]; !ok || ix.Materialized("docs/big.html") {
		t.Fatal("index must know the lazy file and report it not materialized")
	}
	if n, _ := ix.LazyCount(); n != 1 {
		t.Fatalf("LazyCount = %d, want 1", n)
	}
}

// The core safety rule: a lazy file absent from disk is never a local delete,
// through a full scan, a watcher hint naming it (remove and rename), the
// parent folder removed, and a fresh Session on the same store (restart).
func TestLazy_AbsentIsNeverADelete(t *testing.T) {
	a, b := lazyPair(t)
	ctx := context.Background()
	cycle(t, b) // full scan
	for _, hint := range []*ScanHint{
		{Events: []ScanEvent{{Path: "docs/big.html", Kind: ScanRemove}}},
		{Events: []ScanEvent{{Path: "docs/big.html", Kind: ScanRename}}},
		{Events: []ScanEvent{{Path: "docs", Kind: ScanRemove, Directory: true}}},
		{Full: true},
	} {
		if _, err := b.CycleWithScanHint(ctx, hint); err != nil {
			t.Fatal(err)
		}
	}
	// Restart: a new process, same folder and store.
	b2 := &Session{Folder: b.Folder, Store: b.Store, Device: b.Device, Backend: b.Backend}
	cycle(t, b2)
	if d := ownDeletes(t, b2); len(d) != 0 {
		t.Fatalf("lazy device journaled deletes: %v", d)
	}
	cycle(t, a)
	if read(t, a.Folder, "docs/big.html") != big("v1") {
		t.Fatal("peer lost the lazy file")
	}
}

func TestLazy_ReadThroughCacheAndPeerUpdate(t *testing.T) {
	a, b := lazyPair(t)
	ctx := context.Background()
	ix, _ := b.LoadIndex()
	rc, src, err := b.Open(ctx, ix, "docs/big.html")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != big("v1") || src != FromHub {
		t.Fatalf("first read: src=%s ok=%v", src, string(got) == big("v1"))
	}
	// Cache hit: no backend needed at all.
	be := b.Backend
	b.Backend = nil
	rc, src, err = b.Open(ctx, ix, "docs/big.html")
	if err != nil || src != FromCache {
		t.Fatalf("second read: src=%s err=%v", src, err)
	}
	rc.Close()
	b.Backend = be
	// Reading never puts the file on disk, even with its blob cached.
	cycle(t, b)
	if onDisk(b, "docs/big.html") {
		t.Fatal("a read materialized the lazy file")
	}
	// A peer update moves the fold; the old cached copy is not served.
	write(t, a.Folder, "docs/big.html", big("v2"))
	cycle(t, a)
	cycle(t, b)
	if onDisk(b, "docs/big.html") || b.Store.HasBlob(sha(big("v2"))) {
		t.Fatal("peer update of a lazy file was downloaded")
	}
	ix, _ = b.LoadIndex()
	rc, src, err = b.Open(ctx, ix, "docs/big.html")
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(rc)
	rc.Close()
	if string(got) != big("v2") || src != FromHub {
		t.Fatalf("after update: src=%s, v2=%v", src, string(got) == big("v2"))
	}
	// A peer delete of a lazy file touches nothing on this disk.
	if err := os.Remove(filepath.Join(a.Folder, "docs/big.html")); err != nil {
		t.Fatal(err)
	}
	cycle(t, a)
	cycle(t, b)
	ix, _ = b.LoadIndex()
	if _, ok := ix.Files["docs/big.html"]; ok {
		t.Fatal("deleted lazy file still in index")
	}
	if d := ownDeletes(t, b); len(d) != 0 {
		t.Fatalf("lazy device journaled deletes: %v", d)
	}
}

// A write to a lazy path fetches the hub version first, so a concurrent edit
// on another device merges instead of forking into a conflict copy.
func TestLazy_WriteFetchesBaseAndMerges(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	lines := make([]string, 0, 400)
	for i := 0; i < 400; i++ {
		lines = append(lines, "line "+strings.Repeat("y", 10))
	}
	base := strings.Join(lines, "\n") + "\n"
	write(t, a.Folder, "notes.md", base)
	cycle(t, a)
	b := newDevice(t, "devb", be)
	setLazy(t, b, true)
	cycle(t, b)
	if onDisk(b, "notes.md") {
		t.Fatal("precondition: notes.md should be lazy")
	}
	ctx := context.Background()
	ix, _ := b.LoadIndex()
	rc, _, err := b.Open(ctx, ix, "notes.md") // fetches the base into the store
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := io.ReadAll(rc)
	rc.Close()
	write(t, b.Folder, "notes.md", "B top\n"+string(cur))
	write(t, a.Folder, "notes.md", base+"A bottom\n")
	cycle(t, a)
	cycle(t, b)
	cycle(t, a)
	want := "B top\n" + base + "A bottom\n"
	if got := read(t, a.Folder, "notes.md"); got != want {
		t.Fatalf("A did not get the merge (len %d vs %d)", len(got), len(want))
	}
	if got := read(t, b.Folder, "notes.md"); got != want {
		t.Fatal("B did not get the merge")
	}
	// B wrote it, so it is an ordinary local file now.
	if ix, _ := b.LoadIndex(); !ix.Materialized("notes.md") {
		t.Fatal("a written lazy path should be tracked")
	}
}

func TestLazy_TurnOffAndPinDownload(t *testing.T) {
	_, b := lazyPair(t)
	setLazy(t, b, true, "pinned", "docs")
	cycle(t, b)
	if read(t, b.Folder, "docs/big.html") != big("v1") {
		t.Fatal("pinning a folder did not download it")
	}
	setLazy(t, b, false)
	cycle(t, b)
	if d := ownDeletes(t, b); len(d) != 0 {
		t.Fatalf("journaled deletes: %v", d)
	}
}

// A conflict copy is always written to disk, even when it is large: it holds
// a losing edit, and lazy mode must not make that edit vanish from view.
func TestLazy_ConflictCopyIsMaterialized(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	bin := func(tag string) string { return tag + "\x00" + strings.Repeat("b", 4*lazyTestThreshold) }
	write(t, a.Folder, "data.bin", bin("v0"))
	cycle(t, a)
	b := newDevice(t, "devb", be)
	setLazy(t, b, true)
	cycle(t, b)
	write(t, b.Folder, "data.bin", bin("from-b")) // B's own edit: tracked now
	write(t, a.Folder, "data.bin", bin("from-a"))
	cycle(t, a)
	cycle(t, b)
	entries, _ := os.ReadDir(b.Folder)
	found := false
	for _, e := range entries {
		if strings.Contains(e.Name(), conflictMarker) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no conflict copy on the lazy device's disk: %v", entries)
	}
}

// A stale untracked file under a read-only folder, at a lazy path, is put
// back from the hub (its bytes kept aside), not left serving stale content.
func TestLazy_ReadOnlyStaleFileIsReverted(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	write(t, a.Folder, "locked/big.md", big("hub"))
	cycle(t, a)
	b := newDevice(t, "devb", scopedRemote{Backend: be, readOnly: []string{"locked/"}, tag: "t1"})
	setLazy(t, b, true)
	write(t, b.Folder, "locked/big.md", "stale local copy")
	cycle(t, b)
	cycle(t, b)
	if got := read(t, b.Folder, "locked/big.md"); got != big("hub") {
		t.Fatalf("read-only stale file not reverted: %q", got[:min(20, len(got))])
	}
	if got := read(t, b.Folder, "locked/big (local, not synced).md"); got != "stale local copy" {
		t.Fatalf("local bytes not kept aside: %q", got)
	}
	if d := ownDeletes(t, b); len(d) != 0 {
		t.Fatalf("deletes: %v", d)
	}
}

// Open serves only what the project syncs: never an excluded local file or a
// file inside a nested project (bot review P1).
func TestLazy_OpenRefusesExcludedAndNestedFiles(t *testing.T) {
	a := newDevice(t, "deva", nil)
	setLazy(t, a, true)
	write(t, a.Folder, IgnoreFile, ".env\n")
	write(t, a.Folder, ".env", "SECRET=1")
	write(t, a.Folder, "notes.md", "ok")
	write(t, a.Folder, "inner/notes.md", "other project")
	if _, err := config.SaveProject(filepath.Join(a.Folder, "inner"), config.Project{}); err != nil {
		t.Fatal(err)
	}
	write(t, a.Folder, "fresh.md", "not journaled yet")
	cycle(t, a)
	write(t, a.Folder, "fresh2.md", "not journaled yet")
	ix, err := a.LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, rel := range []string{".env", "inner/notes.md"} {
		if rc, _, err := a.Open(ctx, ix, rel); err == nil {
			rc.Close()
			t.Errorf("%s was readable", rel)
		}
	}
	for _, rel := range []string{"notes.md", "fresh2.md"} {
		rc, _, err := a.Open(ctx, ix, rel)
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		rc.Close()
	}
}

// A local file this device's accepted rules exclude is never served from
// disk, even when a teammate syncs a file at the same path; and nothing
// inside a nested project is writable from the parent (bot review P1).
func TestLazy_LocalOnlyFileAndNestedProjectAreProtected(t *testing.T) {
	be := sharedRemote(t)
	b := newDevice(t, "devb", be)
	write(t, b.Folder, IgnoreFile, ".env\n")
	write(t, b.Folder, ".env", "MY SECRET")
	cycle(t, b) // b accepts rules that exclude .env
	a := newDevice(t, "deva", be)
	cycle(t, a)
	write(t, a.Folder, IgnoreFile, "# widened\n")
	write(t, a.Folder, ".env", "team value")
	cycle(t, a)
	cycle(t, b)
	ix, err := b.LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	rc, src, err := b.Open(context.Background(), ix, ".env")
	if err == nil {
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) == "MY SECRET" || src == FromDisk {
			t.Fatalf("served the local excluded .env from %s", src)
		}
	}
	if _, err := config.SaveProject(filepath.Join(b.Folder, "inner"), config.Project{}); err != nil {
		t.Fatal(err)
	}
	if err := b.Writable("inner/new.md"); err == nil {
		t.Fatal("a nested project's path must not be writable from the parent")
	}
	if err := b.Writable("notes/new.md"); err != nil {
		t.Fatalf("ordinary path: %v", err)
	}
}

// Quarantined local content is never journaled — full scan, watcher hint,
// restart — so a peer never receives it; any other content at the path
// syncs and ends the quarantine.
func TestQuarantinedContentNeverSyncs(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	write(t, a.Folder, "notes.md", "v1")
	cycle(t, a)
	b := newDevice(t, "devb", be)
	cycle(t, b)
	if err := b.Quarantine("notes.md", sha("refused"), false); err != nil {
		t.Fatal(err)
	}
	write(t, b.Folder, "notes.md", "refused")
	cycle(t, b)
	if _, err := b.CycleWithScanHint(context.Background(), &ScanHint{Events: []ScanEvent{{Path: "notes.md", Kind: ScanWrite}}}); err != nil {
		t.Fatal(err)
	}
	b2 := &Session{Folder: b.Folder, Store: b.Store, Device: b.Device, Backend: b.Backend}
	cycle(t, b2)
	cycle(t, a)
	if got := read(t, a.Folder, "notes.md"); got != "v1" {
		t.Fatalf("peer received quarantined content: %q", got)
	}
	write(t, b.Folder, "notes.md", "real edit")
	cycle(t, b2)
	cycle(t, a)
	if got := read(t, a.Folder, "notes.md"); got != "real edit" {
		t.Fatalf("a later real edit did not sync: %q", got)
	}
	q, _ := b.Store.LoadQuarantine(b.mountID())
	if len(q) != 0 {
		t.Fatalf("quarantine not ended by a real edit: %v", q)
	}
}
