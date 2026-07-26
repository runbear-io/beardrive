package syncer

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/store"
)

// TestPushProgress verifies the push phase reports upload progress: the total
// is the number of unique blobs, Done climbs to that total, and byte totals
// are populated. (Done isn't strictly ordered across the parallel workers, so
// we only assert it reaches the total.)
func TestPushProgress(t *testing.T) {
	a := newDevice(t, "deva", sharedRemote(t))
	const n = 25
	for i := 0; i < n; i++ {
		write(t, a.Folder, fmt.Sprintf("f%02d.txt", i), fmt.Sprintf("unique content for file %d — pad pad pad", i))
	}
	var mu sync.Mutex
	var total, maxDone int
	var toBytes int64
	a.OnProgress = func(p Progress) {
		mu.Lock()
		defer mu.Unlock()
		total = p.Total
		toBytes = p.ToBytes
		if p.Done > maxDone {
			maxDone = p.Done
		}
	}
	cycle(t, a)
	if total != n {
		t.Fatalf("progress Total = %d, want %d", total, n)
	}
	if maxDone != n {
		t.Fatalf("progress reached Done = %d, want %d", maxDone, n)
	}
	if toBytes == 0 {
		t.Fatal("progress ToBytes should be > 0")
	}
}

// newDevice simulates one device: its own folder, volume store, and identity,
// all syncing through a shared file:// remote.
func newDevice(t *testing.T, name string, backend remote.Backend) *Session {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "volume"))
	if err != nil {
		t.Fatal(err)
	}
	return &Session{
		Folder:  t.TempDir(),
		Store:   st,
		Device:  config.Device{ID: name, Name: name, Author: name + "@test"},
		Backend: backend,
	}
}

func sharedRemote(t *testing.T) remote.Backend {
	t.Helper()
	be, err := remote.Open(context.Background(), "file://"+t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return be
}

func write(t *testing.T, folder, rel, content string) {
	t.Helper()
	abs := filepath.Join(folder, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, folder, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(folder, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func cycle(t *testing.T, s *Session) *Result {
	t.Helper()
	res, err := s.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Offline {
		t.Fatalf("unexpected offline: %v", res.OfflineErr)
	}
	return res
}

func TestOfflineCycle(t *testing.T) {
	a := newDevice(t, "deva", nil)
	write(t, a.Folder, "notes/hello.md", "hi")
	res := cycle(t, a)
	if res.LocalOps != 1 {
		t.Fatalf("LocalOps = %d, want 1", res.LocalOps)
	}
	// idempotent: second cycle sees no changes
	res = cycle(t, a)
	if res.Activity() {
		t.Fatalf("second cycle should be quiet, got %+v", res)
	}
}

func TestTwoDeviceSync(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	// A creates files, B receives them
	write(t, a.Folder, "doc.txt", "v1")
	write(t, a.Folder, "sub/nested.txt", "deep")
	cycle(t, a)
	res := cycle(t, b)
	if res.PulledOps != 2 || res.Materialized != 2 {
		t.Fatalf("b pull: %+v", res)
	}
	if read(t, b.Folder, "doc.txt") != "v1" || read(t, b.Folder, "sub/nested.txt") != "deep" {
		t.Fatal("content mismatch after sync")
	}

	// B edits, A receives the update
	time.Sleep(10 * time.Millisecond) // ensure mtime moves
	write(t, b.Folder, "doc.txt", "v2 from b")
	cycle(t, b)
	cycle(t, a)
	if got := read(t, a.Folder, "doc.txt"); got != "v2 from b" {
		t.Fatalf("a got %q", got)
	}

	// B deletes, A's copy disappears
	os.Remove(filepath.Join(b.Folder, "sub", "nested.txt"))
	cycle(t, b)
	cycle(t, a)
	if _, err := os.Stat(filepath.Join(a.Folder, "sub", "nested.txt")); !os.IsNotExist(err) {
		t.Fatal("delete did not propagate")
	}
	// empty dir pruned
	if _, err := os.Stat(filepath.Join(a.Folder, "sub")); !os.IsNotExist(err) {
		t.Fatal("empty dir not pruned")
	}
}

func TestHistoryTracksDeviceAndAuthor(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	write(t, a.Folder, "f.txt", "from a")
	cycle(t, a)
	cycle(t, b)
	time.Sleep(10 * time.Millisecond)
	write(t, b.Folder, "f.txt", "from b")
	cycle(t, b)
	cycle(t, a)

	entries, err := LogEntries(a.Store, "f.txt", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 history entries, got %d: %+v", len(entries), entries)
	}
	// newest first
	if entries[0].Author != "devb@test" || entries[0].DeviceName != "devb" {
		t.Fatalf("newest entry should be devb's: %+v", entries[0])
	}
	if entries[1].Author != "deva@test" {
		t.Fatalf("oldest entry should be deva's: %+v", entries[1])
	}
}

func TestConcurrentEditConflictPreserved(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	// shared base
	write(t, a.Folder, "shared.txt", "base")
	cycle(t, a)
	cycle(t, b)

	// both edit before syncing
	time.Sleep(10 * time.Millisecond)
	write(t, a.Folder, "shared.txt", "edit from a")
	write(t, b.Folder, "shared.txt", "edit from b")
	cycle(t, a) // a pushes first
	cycle(t, b) // b scans its edit, pulls a's, loses or wins deterministically
	cycle(t, a) // a converges
	cycle(t, b)

	aContent := read(t, a.Folder, "shared.txt")
	bContent := read(t, b.Folder, "shared.txt")
	if aContent != bContent {
		t.Fatalf("devices diverged: %q vs %q", aContent, bContent)
	}

	// both versions must survive somewhere (winner at path, loser as conflict copy)
	all := map[string]bool{aContent: true}
	for _, folder := range []string{a.Folder, b.Folder} {
		entries, err := os.ReadDir(folder)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.Contains(e.Name(), ".bdrive-conflict-") {
				all[read(t, folder, e.Name())] = true
			}
		}
	}
	if !all["edit from a"] || !all["edit from b"] {
		t.Fatalf("a version was lost; surviving: %v", all)
	}
}

func TestMountExistingFolderImports(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	write(t, a.Folder, "pre-existing.txt", "I was here first")
	res := cycle(t, a)
	if res.LocalOps != 1 || !res.Pushed {
		t.Fatalf("import failed: %+v", res)
	}

	b := newDevice(t, "devb", be)
	cycle(t, b)
	if read(t, b.Folder, "pre-existing.txt") != "I was here first" {
		t.Fatal("existing file not imported/synced")
	}
}

func TestIgnoredFiles(t *testing.T) {
	a := newDevice(t, "deva", nil)
	write(t, a.Folder, ".DS_Store", "junk")
	write(t, a.Folder, ".git/config", "gitstuff")
	write(t, a.Folder, "real.txt", "data")
	res := cycle(t, a)
	if res.LocalOps != 1 {
		t.Fatalf("ignores leaked into journal: %+v", res)
	}
}

func TestOfflineThenReconnect(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)

	// work offline
	a.Backend = nil
	write(t, a.Folder, "offline.txt", "written offline")
	cycle(t, a)

	// reconnect: pending ops push
	a.Backend = be
	res := cycle(t, a)
	if !res.Pushed {
		t.Fatalf("reconnect should push pending ops: %+v", res)
	}

	b := newDevice(t, "devb", be)
	cycle(t, b)
	if read(t, b.Folder, "offline.txt") != "written offline" {
		t.Fatal("offline edit did not propagate after reconnect")
	}
}

func TestSameVolumeMountedAtTwoFolders(t *testing.T) {
	// One device mounts the same volume at two folders (e.g. ./shared in two
	// repos). They share the store (blobs+journals) but have separate mount
	// caches, and content propagates between them even with no remote.
	st, err := store.Open(filepath.Join(t.TempDir(), "volume"))
	if err != nil {
		t.Fatal(err)
	}
	dev := config.Device{ID: "dev1", Name: "dev1", Author: "dev1@test"}
	m1 := &Session{Folder: t.TempDir(), MountID: "mount1", Store: st, Device: dev}
	m2 := &Session{Folder: t.TempDir(), MountID: "mount2", Store: st, Device: dev}

	write(t, m1.Folder, "shared.md", "from folder one")
	cycle(t, m1)
	res := cycle(t, m2)
	if res.Materialized != 1 {
		t.Fatalf("folder two should materialize the file: %+v", res)
	}
	if read(t, m2.Folder, "shared.md") != "from folder one" {
		t.Fatal("content did not propagate between mounts")
	}

	// edit in folder two propagates back
	time.Sleep(10 * time.Millisecond)
	write(t, m2.Folder, "shared.md", "edited in folder two")
	cycle(t, m2)
	cycle(t, m1)
	if read(t, m1.Folder, "shared.md") != "edited in folder two" {
		t.Fatal("edit did not propagate back to folder one")
	}
}

func TestExecutableBitPreserved(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	abs := filepath.Join(a.Folder, "run.sh")
	os.WriteFile(abs, []byte("#!/bin/sh\necho hi\n"), 0o755)
	cycle(t, a)

	b := newDevice(t, "devb", be)
	cycle(t, b)
	fi, err := os.Stat(filepath.Join(b.Folder, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("exec bit lost: %v", fi.Mode())
	}
}

// TestNestedMountExcluded verifies that a subdirectory which is a BearDrive
// mount of its own (has .bdrive/config.json) is fenced off from the parent
// mount: the parent scanner never journals its files, dropping it emits no
// delete ops toward peers, and remote state is never materialized into it.
func TestNestedMountExcluded(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	// Both devices converge on a folder that includes team/.
	write(t, a.Folder, "readme.md", "root")
	write(t, a.Folder, "team/notes.md", "v1")
	cycle(t, a)
	cycle(t, b)
	if got := read(t, b.Folder, "team/notes.md"); got != "v1" {
		t.Fatalf("b team/notes.md = %q, want v1", got)
	}

	// team/ becomes a nested mount on A (its own project).
	write(t, a.Folder, "team/.bdrive/config.json", `{"mount_id":"m-nested"}`)
	write(t, a.Folder, "team/local.md", "only for the nested project")
	res := cycle(t, a)
	if res.LocalOps != 0 {
		t.Fatalf("a journaled %d ops for nested-mount content, want 0", res.LocalOps)
	}

	// B must keep its copy (no delete propagated) and never see new files.
	res = cycle(t, b)
	if got := read(t, b.Folder, "team/notes.md"); got != "v1" {
		t.Fatalf("b team/notes.md = %q after a's cycle, want v1 (no deletes)", got)
	}
	if _, err := os.Stat(filepath.Join(b.Folder, "team/local.md")); !os.IsNotExist(err) {
		t.Fatal("nested-mount file leaked to peer")
	}

	// B edits inside team/; A must not materialize over its nested mount.
	write(t, b.Folder, "team/notes.md", "v2")
	cycle(t, b)
	cycle(t, a)
	if got := read(t, a.Folder, "team/notes.md"); got != "v1" {
		t.Fatalf("a team/notes.md = %q, want v1 (nested mount not written)", got)
	}

	// Paths outside the nested mount keep syncing both ways.
	write(t, a.Folder, "readme.md", "root v2")
	cycle(t, a)
	cycle(t, b)
	if got := read(t, b.Folder, "readme.md"); got != "root v2" {
		t.Fatalf("b readme.md = %q, want root v2", got)
	}
}

// gated wraps a backend and refuses the operations the hub would refuse for a
// given permission level, with the same sentinel the http backend produces.
// The flags are read on every call so a test can revoke (or restore) access
// mid-run, which is exactly the case that used to look like a network fault.
type gated struct {
	remote.Backend
	read  *atomic.Bool // pulls allowed (List/Get)
	write *atomic.Bool // pushes allowed (Put)
}

func newGated(be remote.Backend) *gated {
	g := &gated{Backend: be, read: &atomic.Bool{}, write: &atomic.Bool{}}
	g.read.Store(true)
	g.write.Store(true)
	return g
}

func (g *gated) List(ctx context.Context, prefix string) ([]remote.Object, error) {
	if !g.read.Load() {
		return nil, fmt.Errorf("%w: server: 403 Forbidden", remote.ErrForbidden)
	}
	return g.Backend.List(ctx, prefix)
}

func (g *gated) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if !g.read.Load() {
		return nil, fmt.Errorf("%w: server: 403 Forbidden", remote.ErrForbidden)
	}
	return g.Backend.Get(ctx, key)
}

func (g *gated) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if !g.write.Load() {
		return fmt.Errorf("%w: server: 403 Forbidden", remote.ErrForbidden)
	}
	return g.Backend.Put(ctx, key, r, size)
}

// A read-only device keeps pulling its teammates' changes and journals its own
// edits locally, but nothing of its own ever reaches the remote — and the
// cycle says ReadOnly, never Offline, so the user is told rather than left
// watching a silent retry loop.
func TestReadOnlyDevicePullsOnly(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	gate := newGated(be)
	b := newDevice(t, "devb", gate)

	write(t, a.Folder, "shared.md", "from A")
	cycle(t, a)

	gate.write.Store(false) // B is downgraded to read
	write(t, b.Folder, "mine.md", "local only")
	res, err := b.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.ReadOnly || res.Offline {
		t.Fatalf("read-only push: %+v, want ReadOnly and not Offline", res)
	}
	if got := read(t, b.Folder, "shared.md"); got != "from A" {
		t.Fatalf("b shared.md = %q — a read-only device must still pull", got)
	}
	// B's own edit is journaled locally...
	ops, err := b.Store.DeviceOps(b.Device.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Path != "mine.md" {
		t.Fatalf("b's local journal = %+v, want one op for mine.md", ops)
	}
	// ...and never lands in the shared remote, however many cycles run.
	for i := 0; i < 3; i++ {
		if _, err := b.Cycle(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	c := newDevice(t, "devc", be)
	cycle(t, c)
	if _, err := os.Stat(filepath.Join(c.Folder, "mine.md")); !os.IsNotExist(err) {
		t.Fatal("a read-only device's edit reached the remote")
	}
	// The state is persisted so `bdrive status` can report it without a cycle.
	if st, err := b.Store.LoadSync(); err != nil || st.Access != store.AccessReadOnly {
		t.Fatalf("persisted access = %q (%v), want read-only", st.Access, err)
	}

	// Restoring write self-heals: the held-back op finally goes out.
	gate.write.Store(true)
	if res := cycle(t, b); !res.Pushed {
		t.Fatalf("re-granted device did not push: %+v", res)
	}
	cycle(t, c)
	if got := read(t, c.Folder, "mine.md"); got != "local only" {
		t.Fatalf("c mine.md = %q after the re-grant", got)
	}
	if st, _ := b.Store.LoadSync(); st.Access != store.AccessOK {
		t.Fatalf("persisted access = %q after re-grant, want cleared", st.Access)
	}
}

// A device whose access is revoked entirely pauses: the cycle reports
// NoAccess (not Offline), the working folder is left byte-for-byte alone —
// revoking access must never look like the hub deleting someone's files — and
// re-granting resumes normal sync with no manual step.
func TestNoAccessPausesSync(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	gate := newGated(be)
	c := newDevice(t, "devc", gate)

	write(t, a.Folder, "doc.md", "v1")
	cycle(t, a)
	cycle(t, c)
	if got := read(t, c.Folder, "doc.md"); got != "v1" {
		t.Fatalf("c doc.md = %q, want v1", got)
	}

	// A moves on while C's access is cut.
	write(t, a.Folder, "doc.md", "v2")
	write(t, a.Folder, "new.md", "after the cut")
	cycle(t, a)

	gate.read.Store(false)
	gate.write.Store(false)
	before := snapshotDir(t, c.Folder)
	write(t, c.Folder, "cs-own.md", "written while cut off")
	before["cs-own.md"] = "written while cut off"

	for i := 0; i < 3; i++ {
		res, err := c.Cycle(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !res.NoAccess || res.Offline {
			t.Fatalf("cycle %d: %+v, want NoAccess and not Offline", i, res)
		}
		if res.Materialized != 0 || res.Pushed {
			t.Fatalf("cycle %d touched the folder or pushed: %+v", i, res)
		}
	}
	if got := snapshotDir(t, c.Folder); !maps.Equal(got, before) {
		t.Fatalf("working folder changed while access was revoked:\n got %v\nwant %v", got, before)
	}
	if st, _ := c.Store.LoadSync(); st.Access != store.AccessNone {
		t.Fatalf("persisted access = %q, want no-access", st.Access)
	}

	// Re-granting needs no intervention: the next cycle converges both ways.
	gate.read.Store(true)
	gate.write.Store(true)
	cycle(t, c)
	if got := read(t, c.Folder, "doc.md"); got != "v2" {
		t.Fatalf("c doc.md = %q after the re-grant, want v2", got)
	}
	if got := read(t, c.Folder, "new.md"); got != "after the cut" {
		t.Fatalf("c new.md = %q after the re-grant", got)
	}
	cycle(t, a)
	if got := read(t, a.Folder, "cs-own.md"); got != "written while cut off" {
		t.Fatalf("a cs-own.md = %q — C's held-back edit should arrive", got)
	}
}

// snapshotDir reads every file under folder (excluding .bdrive) as path→content.
func snapshotDir(t *testing.T, folder string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(folder, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(folder, p)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, config.ProjectDir+"/") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
