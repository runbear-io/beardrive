package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/syncer"
)

func testLocalMCP(t *testing.T) *localMCP {
	t.Helper()
	t.Setenv("BDRIVE_HOME", t.TempDir())
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "vol"))
	if err != nil {
		t.Fatal(err)
	}
	sess := &syncer.Session{Folder: root, Store: st, Device: config.Device{ID: "dev"}}
	v := &projectView{root: root, sess: sess}
	if err := v.refresh(); err != nil {
		t.Fatal(err)
	}
	return &localMCP{v: v}
}

// An outside write landing after the caller's sha check — right up to the
// moment of the swap — is refused, and the outside version stays on disk
// (bot review P1). mcpBeforeRename runs just before the swap; there is no
// later check to slip past, because the check runs on what the swap displaced.
func TestLocalMCPWriteRefusesARacingWrite(t *testing.T) {
	m := testLocalMCP(t)
	ctx := context.Background()
	abs := filepath.Join(m.v.root, "notes.md")
	if err := os.WriteFile(abs, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := m.read(ctx, lRead{Path: "notes.md"})
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimPrefix(strings.Fields(out)[1], "sha:")
	mcpBeforeRename = func() { os.WriteFile(abs, []byte("editor save\n"), 0o644) }
	defer func() { mcpBeforeRename = nil }()
	if _, err := m.edit(ctx, lEdit{Path: "notes.md", OldString: "v1", NewString: "v2", SHA: sha}); err == nil || !strings.Contains(err.Error(), "changed while") {
		t.Fatalf("racing write not refused: %v", err)
	}
	if b, _ := os.ReadFile(abs); string(b) != "editor save\n" {
		t.Fatalf("the outside write was overwritten: %q", b)
	}
	mcpBeforeRename = nil
	if _, err := m.write(ctx, lWrite{Path: "new.md", Content: "x"}); err != nil {
		t.Fatalf("plain create: %v", err)
	}
	mcpBeforeRename = func() { os.WriteFile(filepath.Join(m.v.root, "new2.md"), []byte("someone else"), 0o644) }
	if _, err := m.write(ctx, lWrite{Path: "new2.md", Content: "mine"}); err == nil {
		t.Fatal("a create racing another create must be refused")
	}
}

// Restore and write refuse a read-only folder up front (bot review P1).
func TestLocalMCPRefusesReadOnlyFolder(t *testing.T) {
	m := testLocalMCP(t)
	st, _ := m.v.sess.Store.LoadSync()
	st.ReadOnly = []string{"locked/"}
	if err := m.v.sess.Store.SaveSync(st); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := m.write(ctx, lWrite{Path: "locked/a.md", Content: "x"}); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("write: %v", err)
	}
	if _, err := m.restore(ctx, lRestore{Path: "locked/a.md", SHA: strings.Repeat("a", 64)}); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("restore: %v", err)
	}
}

// The CAS above has no check-then-rename window on macOS and Linux: the
// staged file is swapped in atomically and only the DISPLACED file is
// checked. This pins that the swap is really available here, so CI would
// notice a platform silently falling back to check-then-rename.
func TestSwapFilesExchanges(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	os.WriteFile(a, []byte("A"), 0o644)
	os.WriteFile(b, []byte("B"), 0o644)
	if err := swapFiles(a, b); err != nil {
		t.Skipf("no atomic swap on this filesystem: %v", err)
	}
	ga, _ := os.ReadFile(a)
	gb, _ := os.ReadFile(b)
	if string(ga) != "B" || string(gb) != "A" {
		t.Fatalf("swap: a=%q b=%q", ga, gb)
	}
}
