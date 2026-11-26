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

// An outside write landing between the sha check and the rename is refused,
// not overwritten (bot review P1).
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
