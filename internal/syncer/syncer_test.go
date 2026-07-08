package syncer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/store"
)

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
