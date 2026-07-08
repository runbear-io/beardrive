package syncer

import (
	"os"
	"path/filepath"
	"testing"
)

// Multi-device behavior of .bdriveignore and the .bdrive include list.

func TestIgnoredFilesDoNotSync(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	write(t, a.Folder, IgnoreFile, "*.secret\n")
	write(t, a.Folder, "notes.md", "hello")
	write(t, a.Folder, "key.secret", "hunter2")
	cycle(t, a)
	cycle(t, b)

	if got := read(t, b.Folder, "notes.md"); got != "hello" {
		t.Fatalf("notes.md = %q", got)
	}
	if got := read(t, b.Folder, IgnoreFile); got != "*.secret\n" {
		t.Fatalf(".bdriveignore should sync like a normal file, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(b.Folder, "key.secret")); !os.IsNotExist(err) {
		t.Fatal("ignored file must not reach other devices")
	}
}

func TestNewlyIgnoredFileIsNotDeletedRemotely(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	write(t, a.Folder, "debug.log", "lines")
	cycle(t, a)
	cycle(t, b)
	if read(t, b.Folder, "debug.log") != "lines" {
		t.Fatal("setup: file should have synced")
	}

	// A opts out afterwards; the file must stop syncing without a delete
	// op, so it stays on disk on every device.
	write(t, a.Folder, IgnoreFile, "*.log\n")
	res := cycle(t, a)
	if res.LocalOps != 1 { // only the .bdriveignore put, no delete for debug.log
		t.Fatalf("LocalOps = %d, want 1 (the .bdriveignore itself)", res.LocalOps)
	}
	cycle(t, b)
	cycle(t, b) // second cycle: filter from the pulled .bdriveignore is active
	if read(t, a.Folder, "debug.log") != "lines" || read(t, b.Folder, "debug.log") != "lines" {
		t.Fatal("newly ignored file must remain on disk everywhere")
	}
}

func TestIncludeListLimitsSync(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	write(t, a.Folder, ".bdrive/config.json", `{"include": ["docs/"]}`)
	write(t, a.Folder, "docs/guide.md", "included")
	write(t, a.Folder, "src/main.go", "excluded")
	cycle(t, a)
	cycle(t, b)

	if got := read(t, b.Folder, "docs/guide.md"); got != "included" {
		t.Fatalf("docs/guide.md = %q", got)
	}
	for _, absent := range []string{"src/main.go", ".bdrive/config.json"} {
		if _, err := os.Stat(filepath.Join(b.Folder, absent)); !os.IsNotExist(err) {
			t.Fatalf("%s must not sync", absent)
		}
	}
}
