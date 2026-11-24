package syncer

import (
	"context"
	"os"
	"testing"
	"time"
)

// bdrive gc: the dry run deletes nothing, the prune keeps the current tree,
// unpushed content and young blobs, and a removed version restores by
// fetching it back from the hub.
func TestGCKeepRulesAndRestoreFromHub(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	for _, v := range []string{"v1", "v2 longer", "v3 longest"} {
		write(t, a.Folder, "f.txt", v)
		cycle(t, a)
		time.Sleep(10 * time.Millisecond)
	}
	write(t, a.Folder, "g.txt", "g1")
	cycle(t, a)
	// An offline edit: committed locally, never pushed.
	a.Backend = nil
	write(t, a.Folder, "g.txt", "g2 offline")
	if _, err := a.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.Backend = be

	old := time.Now().Add(-time.Hour)
	for _, c := range []string{"v1", "v3 longest", "g1", "g2 offline"} {
		if err := os.Chtimes(a.Store.BlobPath(sha(c)), old, old); err != nil {
			t.Fatal(err)
		}
	}
	// v2's blob stays young: pinned by minAge.

	n, freed, err := a.Store.PlanPruneLocalBlobsForDevice("deva", nil, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || freed != int64(len("v1")) || !a.Store.HasBlob(sha("v1")) {
		t.Fatalf("dry run = %d blobs / %d bytes, v1 present=%v; want 1 / 2 / true", n, freed, a.Store.HasBlob(sha("v1")))
	}

	if n, _, err = a.Store.PruneLocalBlobsForDevice("deva", nil, time.Minute); err != nil || n != 1 {
		t.Fatalf("prune removed %d (err %v), want 1", n, err)
	}
	for c, want := range map[string]bool{
		"v1":         false, // old, pushed, not current
		"v2 longer":  true,  // young
		"v3 longest": true,  // current
		"g1":         true,  // history of a path with an unpushed edit
		"g2 offline": true,  // unpushed
	} {
		if got := a.Store.HasBlob(sha(c)); got != want {
			t.Errorf("blob %q present=%v, want %v", c, got, want)
		}
	}

	if err := a.Restore(context.Background(), "f.txt", sha("v1")); err != nil {
		t.Fatal(err)
	}
	cycle(t, a)
	if got := read(t, a.Folder, "f.txt"); got != "v1" {
		t.Fatalf("after restore f.txt = %q, want v1", got)
	}
}
