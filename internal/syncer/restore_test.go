package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

func sha(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func journalBytes(t *testing.T, s *Session, device string) []byte {
	t.Helper()
	b, err := os.ReadFile(s.Store.JournalPath(device))
	if err != nil {
		t.Fatalf("read journal %s: %v", device, err)
	}
	return b
}

// The one that matters: a restore is an ordinary edit, so both devices
// converge on the restored content and NO journal is ever rewritten — the
// restoring device's own log grows by one op, everyone else's is untouched
// down to the byte.
func TestRestoreConvergesAcrossDevices(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	write(t, a.Folder, "f.txt", "v1")
	cycle(t, a)
	cycle(t, b)
	time.Sleep(10 * time.Millisecond) // ensure mtime moves
	write(t, a.Folder, "f.txt", "v2 is longer")
	cycle(t, a)
	cycle(t, b)
	if read(t, b.Folder, "f.txt") != "v2 is longer" {
		t.Fatal("b did not receive v2")
	}

	beforeA := journalBytes(t, b, "deva")
	mineBefore, err := b.Store.DeviceOps("devb")
	if err != nil {
		t.Fatal(err)
	}

	b.Note = "restore f.txt@" + sha("v1")[:8]
	if err := b.Restore(context.Background(), "f.txt", sha("v1")); err != nil {
		t.Fatal(err)
	}
	cycle(t, b)
	cycle(t, a)

	if got := read(t, b.Folder, "f.txt"); got != "v1" {
		t.Fatalf("b after restore = %q, want v1", got)
	}
	if got := read(t, a.Folder, "f.txt"); got != "v1" {
		t.Fatalf("a did not converge: %q", got)
	}
	if string(journalBytes(t, b, "deva")) != string(beforeA) {
		t.Fatal("restore rewrote another device's journal")
	}
	mineAfter, err := b.Store.DeviceOps("devb")
	if err != nil {
		t.Fatal(err)
	}
	if len(mineAfter) != len(mineBefore)+1 {
		t.Fatalf("own journal ops %d → %d, want exactly one appended", len(mineBefore), len(mineAfter))
	}
	last := mineAfter[len(mineAfter)-1]
	if last.Kind != journal.KindPut || last.Path != "f.txt" || last.Blob != sha("v1") {
		t.Fatalf("restore op = %+v", last)
	}
	if last.Note != b.Note {
		t.Fatalf("restore op note = %q, want %q", last.Note, b.Note)
	}
}

// Restoring while offline still converges once the device is back: the
// restore is a local edit, so it is journaled now and pushed later.
func TestRestoreOfflineConverges(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	write(t, a.Folder, "f.txt", "v1")
	cycle(t, a)
	cycle(t, b)
	time.Sleep(10 * time.Millisecond)
	write(t, a.Folder, "f.txt", "v2")
	cycle(t, a)
	cycle(t, b)

	b.Backend = nil // hub unreachable
	if err := b.Restore(context.Background(), "f.txt", sha("v1")); err != nil {
		t.Fatal(err)
	}
	cycle(t, b)
	if read(t, b.Folder, "f.txt") != "v1" {
		t.Fatal("offline restore did not land")
	}

	b.Backend = be
	cycle(t, b)
	cycle(t, a)
	if got := read(t, a.Folder, "f.txt"); got != "v1" {
		t.Fatalf("a after b came back = %q, want v1", got)
	}
}

// A deleted file comes back: restore is content-addressed, and the blob
// outlives the delete op.
func TestRestoreAfterDelete(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	write(t, a.Folder, "f.txt", "keepme")
	cycle(t, a)
	cycle(t, b)
	os.Remove(filepath.Join(a.Folder, "f.txt"))
	cycle(t, a)
	cycle(t, b)
	if _, err := os.Stat(filepath.Join(b.Folder, "f.txt")); !os.IsNotExist(err) {
		t.Fatal("delete did not reach b")
	}

	if err := b.Restore(context.Background(), "f.txt", sha("keepme")); err != nil {
		t.Fatal(err)
	}
	cycle(t, b)
	cycle(t, a)
	if got := read(t, a.Folder, "f.txt"); got != "keepme" {
		t.Fatalf("a after restore of a deleted file = %q", got)
	}
}

// A path the project doesn't sync must fail loudly: the scan would drop the
// write and the user would be told "restored" with nothing happening.
func TestRestoreRefusesIgnoredPath(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	write(t, a.Folder, "secret.env", "shh")
	cycle(t, a)
	write(t, a.Folder, IgnoreFile, "*.env\n")
	cycle(t, a)

	if err := a.Restore(context.Background(), "secret.env", sha("shh")); err == nil {
		t.Fatal("restoring an ignored path should refuse")
	}
}

// The content may live only on the hub — a device that never held that
// version fetches it, and verifies what it got.
func TestRestoreFetchesMissingBlob(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	write(t, a.Folder, "f.txt", "v1")
	cycle(t, a)
	time.Sleep(10 * time.Millisecond)
	write(t, a.Folder, "f.txt", "v2")
	cycle(t, a)
	cycle(t, b)

	// Simulate a device that never held that version (joined late, or pruned
	// its store): the bytes exist only on the hub.
	os.Remove(b.Store.BlobPath(sha("v1")))
	if b.Store.HasBlob(sha("v1")) {
		t.Fatal("v1 should be gone from b's store")
	}
	if err := b.Restore(context.Background(), "f.txt", sha("v1")); err != nil {
		t.Fatal(err)
	}
	if read(t, b.Folder, "f.txt") != "v1" {
		t.Fatal("blob was not fetched from the hub")
	}
}
