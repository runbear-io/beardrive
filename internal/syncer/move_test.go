package syncer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
)

// A renamed/moved folder must keep syncing seamlessly: state is keyed by the
// stable mount id, never by the path, so the move produces zero spurious ops
// and later edits sync normally.
func TestFolderMoveKeepsSyncing(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	a.MountID = "m-test0001"
	b := newDevice(t, "devb", be)

	write(t, a.Folder, "docs/spec.md", "v1")
	cycle(t, a)
	cycle(t, b)
	if read(t, b.Folder, "docs/spec.md") != "v1" {
		t.Fatal("initial sync failed")
	}

	// Rename/move the folder; the session keeps the same mount id, so the
	// state cache still matches — the move itself is not a change.
	moved := filepath.Join(t.TempDir(), "renamed-project")
	if err := os.Rename(a.Folder, moved); err != nil {
		t.Fatal(err)
	}
	a2 := &Session{Folder: moved, MountID: a.MountID, Store: a.Store, Device: a.Device, Backend: be}
	res := cycle(t, a2)
	if res.LocalOps != 0 || res.Materialized != 0 {
		t.Fatalf("move must be invisible to sync, got %+v", res)
	}

	// Edits at the new location keep syncing.
	time.Sleep(10 * time.Millisecond)
	write(t, moved, "docs/spec.md", "v2 after move")
	cycle(t, a2)
	cycle(t, b)
	if got := read(t, b.Folder, "docs/spec.md"); got != "v2 after move" {
		t.Fatalf("post-move edit did not sync: %q", got)
	}
}

// Ops written by a logged-in device carry the account.
func TestOpsCarryAccount(t *testing.T) {
	a := newDevice(t, "deva", nil)
	a.Account = config.Settings{Email: "alice@x.io", Name: "Alice"}
	write(t, a.Folder, "note.md", "hello")
	cycle(t, a)
	ops, err := a.Store.DeviceOps(a.Device.ID)
	if err != nil || len(ops) != 1 {
		t.Fatalf("ops = %v, %v", ops, err)
	}
	if ops[0].User != "alice@x.io" || ops[0].UserName != "Alice" {
		t.Fatalf("op identity = %+v, want the signed-in account", ops[0])
	}
	if ops[0].Author == "" {
		t.Fatal("git/OS fallback identity should still be present")
	}
}

// The registry self-heals: ResolveMount at the folder's new path updates the
// path the daemon and status use.
func TestRegistrySelfHeal(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	folder := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	proj, err := config.SaveProject(folder, config.Project{Volume: "demo", Remote: "file:///tmp/x"})
	if err != nil {
		t.Fatal(err)
	}
	if proj.ID == "" {
		t.Fatal("SaveProject must assign a mount id")
	}
	if _, _, err := config.ResolveMount(folder); err != nil {
		t.Fatal(err)
	}

	moved := filepath.Join(t.TempDir(), "proj-renamed")
	if err := os.Rename(folder, moved); err != nil {
		t.Fatal(err)
	}
	got, ok, err := config.ResolveMount(moved)
	if err != nil || !ok || got.ID != proj.ID {
		t.Fatalf("resolve after move = %+v %v %v", got, ok, err)
	}
	mounts, err := config.LoadMounts()
	if err != nil {
		t.Fatal(err)
	}
	if mounts[proj.ID].Path != moved {
		t.Fatalf("registry path = %q, want %q", mounts[proj.ID].Path, moved)
	}
}
