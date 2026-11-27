package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/syncer"
)

// gc on a lazy mount: the on-demand cache is capped LRU-first, while content
// on disk, pinned files and young reads stay, and gc never mints a delete.
func TestPruneVolumeBlobs_LazyCacheLRU(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	ctx := context.Background()
	b := supervisorMount(t)
	remoteDir := b.remote
	be, err := remote.Open(ctx, "file://"+remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	aStore, err := store.Open(filepath.Join(t.TempDir(), "vol"))
	if err != nil {
		t.Fatal(err)
	}
	a := &syncer.Session{Folder: t.TempDir(), Store: aStore, Device: config.Device{ID: "deva"}, Backend: be}
	body := func(tag string) string { return tag + strings.Repeat("z", 4096) }
	_ = os.MkdirAll(filepath.Join(a.Folder, "pin"), 0o755)
	for _, f := range []string{"big1", "big2", "big3", "pin/big4"} {
		writeWorktreeFile(t, a.Folder, f, body(f))
	}
	writeWorktreeFile(t, a.Folder, "small", "s")
	if _, err := a.Cycle(ctx); err != nil {
		t.Fatal(err)
	}

	proj, _, _ := config.LoadProject(b.folder)
	proj.Lazy = config.Lazy{Enabled: true, Threshold: 1024, CacheMax: 9000, Pinned: []string{"pin"}}
	if _, err := config.SaveProject(b.folder, proj); err != nil {
		t.Fatal(err)
	}
	bStore, err := store.Open(b.volDir)
	if err != nil {
		t.Fatal(err)
	}
	bs := &syncer.Session{Folder: b.folder, MountID: b.mountID, Store: bStore, Device: config.Device{ID: "devb"}, Backend: be}
	if _, err := bs.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	ix, err := bs.LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	age := map[string]time.Duration{"big1": 3 * time.Hour, "big2": 2 * time.Hour, "big3": time.Hour}
	for f, d := range age {
		if err := bs.EnsureContent(ctx, f, ix.Files[f]); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-d)
		if err := os.Chtimes(bStore.BlobPath(ix.Files[f].Blob), old, old); err != nil {
			t.Fatal(err)
		}
	}
	// Pinned and small files are on disk: their blobs are old too, and kept.
	for _, f := range []string{"pin/big4", "small"} {
		old := time.Now().Add(-5 * time.Hour)
		_ = os.Chtimes(bStore.BlobPath(ix.Files[f].Blob), old, old)
	}

	if n, _, err := PruneVolumeBlobs(bStore, 10*time.Minute, true); err != nil || n != 1 {
		t.Fatalf("dry run counted %d (err %v), want 1", n, err)
	}
	if !bStore.HasBlob(ix.Files["big1"].Blob) {
		t.Fatal("dry run removed a blob")
	}
	if _, _, err := PruneVolumeBlobs(bStore, 10*time.Minute, false); err != nil {
		t.Fatal(err)
	}
	for f, want := range map[string]bool{"big1": false, "big2": true, "big3": true, "pin/big4": true, "small": true} {
		if got := bStore.HasBlob(ix.Files[f].Blob); got != want {
			t.Errorf("%s cached=%v, want %v", f, got, want)
		}
	}
	// The evicted file is still a file of the project: no delete, and a
	// read fetches it back.
	if _, err := bs.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	ops, _ := bStore.DeviceOps("devb")
	for _, op := range ops {
		if op.Kind == journal.KindDelete {
			t.Fatalf("gc led to a delete op: %+v", op)
		}
	}
	rc, src, err := bs.Open(ctx, ix, "big1")
	if err != nil || src != syncer.FromHub {
		t.Fatalf("re-read after eviction: src=%s err=%v", src, err)
	}
	rc.Close()
}

// An unpushed op's blob is never evicted, even when the only CURRENT path
// with those bytes is a lazy one: push must upload it before the journal,
// and nothing would fetch it back (review finding, BEA-349).
func TestPruneVolumeBlobs_LazyCacheKeepsUnpushedBlobs(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	ctx := context.Background()
	b := supervisorMount(t)
	be, err := remote.Open(ctx, "file://"+b.remote)
	if err != nil {
		t.Fatal(err)
	}
	aStore, err := store.Open(filepath.Join(t.TempDir(), "vol"))
	if err != nil {
		t.Fatal(err)
	}
	a := &syncer.Session{Folder: t.TempDir(), Store: aStore, Device: config.Device{ID: "deva"}, Backend: be}
	body := "big" + strings.Repeat("z", 4096)
	writeWorktreeFile(t, a.Folder, "big1", body)
	if _, err := a.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	proj, _, _ := config.LoadProject(b.folder)
	proj.Lazy = config.Lazy{Enabled: true, Threshold: 1024, CacheMax: 1}
	if _, err := config.SaveProject(b.folder, proj); err != nil {
		t.Fatal(err)
	}
	bStore, _ := store.Open(b.volDir)
	dev, err := config.LoadDevice() // the identity gc reads, as in production
	if err != nil {
		t.Fatal(err)
	}
	bs := &syncer.Session{Folder: b.folder, MountID: b.mountID, Store: bStore, Device: dev, Backend: be}
	if _, err := bs.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	ix, _ := bs.LoadIndex()
	if err := bs.EnsureContent(ctx, "big1", ix.Files["big1"]); err != nil {
		t.Fatal(err)
	}
	// Offline: write a copy with the same bytes, sync, delete it, sync.
	bs.Backend = nil
	writeWorktreeFile(t, b.folder, "copy", body)
	if _, err := bs.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(b.folder, "copy")); err != nil {
		t.Fatal(err)
	}
	if _, err := bs.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(bStore.BlobPath(ix.Files["big1"].Blob), old, old)
	if _, _, err := PruneVolumeBlobs(bStore, 10*time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if !bStore.HasBlob(ix.Files["big1"].Blob) {
		t.Fatal("gc evicted the blob of an unpushed op")
	}
	bs.Backend = be
	res, err := bs.Cycle(ctx)
	if err != nil || res.Offline || !res.Pushed {
		t.Fatalf("push after gc: res=%+v err=%v", res, err)
	}
}
