package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/remote"
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

// A third save landing between the swap and the swap back is kept on disk,
// never deleted with the staging file (bot review P1).
func TestLocalMCPKeepsASaveDuringSwapBack(t *testing.T) {
	m := testLocalMCP(t)
	ctx := context.Background()
	abs := filepath.Join(m.v.root, "notes.md")
	os.WriteFile(abs, []byte("v1\n"), 0o644)
	out, _ := m.read(ctx, lRead{Path: "notes.md"})
	sha := strings.TrimPrefix(strings.Fields(out)[1], "sha:")
	mcpBeforeRename = func() { os.WriteFile(abs, []byte("second\n"), 0o644) }
	mcpBeforeSwapBack = func() { os.WriteFile(abs, []byte("third\n"), 0o644) }
	defer func() { mcpBeforeRename, mcpBeforeSwapBack = nil, nil }()
	_, err := m.write(ctx, lWrite{Path: "notes.md", Content: "mine\n", SHA: sha})
	if err == nil {
		t.Fatal("write must be refused")
	}
	if b, _ := os.ReadFile(abs); string(b) != "second\n" {
		t.Fatalf("file = %q, want the second writer's version back", b)
	}
	matches, _ := filepath.Glob(abs + ".bdrive-mcp-displaced-*")
	if len(matches) != 1 {
		t.Fatalf("third save not kept: %v (%v)", matches, err)
	}
	if b, _ := os.ReadFile(matches[0]); string(b) != "third\n" {
		t.Fatalf("kept file = %q", b)
	}
}

// Without an atomic exchange an existing file is never overwritten.
func TestLocalMCPRefusesOverwriteWithoutSwap(t *testing.T) {
	m := testLocalMCP(t)
	abs := filepath.Join(m.v.root, "notes.md")
	os.WriteFile(abs, []byte("v1\n"), 0o644)
	swapFn = func(a, b string) error { return errNoSwap }
	defer func() { swapFn = swapFiles }()
	if _, err := m.write(context.Background(), lWrite{Path: "notes.md", Content: "x"}); err == nil || !strings.Contains(err.Error(), "atomically") {
		t.Fatalf("got %v", err)
	}
	if b, _ := os.ReadFile(abs); string(b) != "v1\n" {
		t.Fatalf("file changed: %q", b)
	}
	if _, err := m.write(context.Background(), lWrite{Path: "fresh.md", Content: "x"}); err != nil {
		t.Fatalf("creating a new file needs no swap: %v", err)
	}
}

// An editor holding the old file open writes through that descriptor after
// the swap succeeded: the write lands on the displaced file, which is kept
// rather than deleted (bot review P1).
func TestLocalMCPKeepsALateWriteThroughAnOpenDescriptor(t *testing.T) {
	m := testLocalMCP(t)
	ctx := context.Background()
	abs := filepath.Join(m.v.root, "notes.md")
	os.WriteFile(abs, []byte("v1\n"), 0o644)
	out, _ := m.read(ctx, lRead{Path: "notes.md"})
	sha := strings.TrimPrefix(strings.Fields(out)[1], "sha:")
	fd, err := os.OpenFile(abs, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	if _, err := m.write(ctx, lWrite{Path: "notes.md", Content: "mine\n", SHA: sha}); err != nil {
		t.Fatal(err)
	}
	// The write lands AFTER every check and after cleanup has run.
	if _, err := fd.WriteAt([]byte("late edit\n"), 0); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(abs); string(b) != "mine\n" {
		t.Fatalf("file = %q", b)
	}
	// The next tool call settles it: a changed displaced file becomes a
	// visible, syncing file next to the original and is reported.
	late := m.sweepDisplaced()
	if len(late) != 1 || !strings.HasPrefix(late[0], "notes.md.bdrive-mcp-late-") {
		t.Fatalf("late save not surfaced: %v", late)
	}
	if b, _ := os.ReadFile(filepath.Join(m.v.root, late[0])); !strings.HasPrefix(string(b), "late edit") {
		t.Fatalf("late write through an open descriptor was lost: %q", b)
	}
}

// Nothing displaced is ever deleted automatically, however old: an untouched
// version stays (a program may still write to it), a changed one is surfaced
// (bot review P1).
func TestLocalMCPDisplacedTTL(t *testing.T) {
	m := testLocalMCP(t)
	dir := m.displacedDir()
	os.MkdirAll(dir, 0o700)
	old := strconv.FormatInt(time.Now().Add(-48*time.Hour).UnixNano(), 10)
	old2 := strconv.FormatInt(time.Now().Add(-47*time.Hour).UnixNano(), 10)
	put := func(id, content, base string) {
		os.WriteFile(filepath.Join(dir, id), []byte(content), 0o600)
		meta, _ := json.Marshal(displacedMeta{Path: "a.md", Base: base})
		os.WriteFile(filepath.Join(dir, id+".json"), meta, 0o600)
	}
	untouched, _ := fileSHAOf("prev")
	put(old, "prev", untouched)
	put(old2, "changed later", untouched)
	late := m.sweepDisplaced()
	if _, err := os.Stat(filepath.Join(dir, old)); err != nil {
		t.Fatal("an untouched old version was deleted")
	}
	if len(late) != 1 {
		t.Fatalf("a changed expired version must be surfaced, not deleted: %v", late)
	}
	if b, _ := os.ReadFile(filepath.Join(m.v.root, late[0])); string(b) != "changed later" {
		t.Fatalf("surfaced = %q", b)
	}
}

func fileSHAOf(s string) (string, error) {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:]), nil
}

// A kept file gets a short name, so a long file name cannot make keeping it
// fail and let cleanup delete it (bot review P1).
func TestLocalMCPKeepsDisplacedFileWithALongName(t *testing.T) {
	m := testLocalMCP(t)
	ctx := context.Background()
	name := strings.Repeat("n", 240) + ".md"
	abs := filepath.Join(m.v.root, name)
	if err := os.WriteFile(abs, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := m.read(ctx, lRead{Path: name})
	sha := strings.TrimPrefix(strings.Fields(out)[1], "sha:")
	mcpBeforeRename = func() { os.WriteFile(abs, []byte("second\n"), 0o644) }
	mcpBeforeSwapBack = func() { os.WriteFile(abs, []byte("third\n"), 0o644) }
	defer func() { mcpBeforeRename, mcpBeforeSwapBack = nil, nil }()
	if _, err := m.write(ctx, lWrite{Path: name, Content: "mine\n", SHA: sha}); err == nil {
		t.Fatal("write must be refused")
	}
	found := false
	entries, _ := os.ReadDir(m.v.root)
	for _, e := range entries {
		if b, _ := os.ReadFile(filepath.Join(m.v.root, e.Name())); string(b) == "third\n" {
			found = true
		}
	}
	if !found {
		t.Fatal("the third save was lost")
	}
}

// When .bdrive/displaced cannot take the displaced version, it is kept next to
// the file (same directory, same filesystem) and the reply says where (bot
// review P1).
func TestLocalMCPDisplacedFallbackIsVisibleAndReported(t *testing.T) {
	m := testLocalMCP(t)
	ctx := context.Background()
	os.MkdirAll(filepath.Join(m.v.root, ".bdrive"), 0o755)
	os.WriteFile(m.displacedDir(), []byte("not a folder"), 0o644) // MkdirAll fails
	abs := filepath.Join(m.v.root, "notes.md")
	os.WriteFile(abs, []byte("v1\n"), 0o644)
	out, _ := m.read(ctx, lRead{Path: "notes.md"})
	sha := strings.TrimPrefix(strings.Fields(out)[1], "sha:")
	res, err := m.write(ctx, lWrite{Path: "notes.md", Content: "mine\n", SHA: sha})
	if err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(abs + ".bdrive-mcp-displaced-*")
	if len(matches) != 1 || !strings.Contains(res, filepath.Base(matches[0])) {
		t.Fatalf("fallback not kept/reported: %v\n%s", matches, res)
	}
}

// restore goes through the same conditional swap as write (bot review P1).
func TestLocalMCPRestoreRefusesARacingWrite(t *testing.T) {
	m := testLocalMCP(t)
	ctx := context.Background()
	abs := filepath.Join(m.v.root, "notes.md")
	os.WriteFile(abs, []byte("v1\n"), 0o644)
	if _, err := m.v.sess.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(abs, []byte("v2\n"), 0o644)
	if _, err := m.v.sess.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.v.refresh(); err != nil {
		t.Fatal(err)
	}
	v1, _ := fileSHAOf("v1\n")
	mcpBeforeRename = func() { os.WriteFile(abs, []byte("editor save\n"), 0o644) }
	defer func() { mcpBeforeRename = nil }()
	if _, err := m.restore(ctx, lRestore{Path: "notes.md", SHA: v1}); err == nil {
		t.Fatal("restore over a racing save must be refused")
	}
	if b, _ := os.ReadFile(abs); string(b) != "editor save\n" {
		t.Fatalf("racing save lost: %q", b)
	}
	mcpBeforeRename = nil
	if _, err := m.restore(ctx, lRestore{Path: "notes.md", SHA: v1}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(abs); string(b) != "v1\n" {
		t.Fatalf("restore = %q", b)
	}
}

// restore passes put's note about a displaced version through.
func TestLocalMCPRestoreReportsDisplacedFallback(t *testing.T) {
	m := testLocalMCP(t)
	ctx := context.Background()
	abs := filepath.Join(m.v.root, "notes.md")
	os.WriteFile(abs, []byte("v1\n"), 0o644)
	m.v.sess.Cycle(ctx)
	os.WriteFile(abs, []byte("v2\n"), 0o644)
	m.v.sess.Cycle(ctx)
	m.v.refresh()
	os.MkdirAll(filepath.Join(m.v.root, ".bdrive"), 0o755)
	os.WriteFile(m.displacedDir(), []byte("not a folder"), 0o644)
	v1, _ := fileSHAOf("v1\n")
	res, err := m.restore(ctx, lRestore{Path: "notes.md", SHA: v1})
	if err != nil || !strings.Contains(res, "kept as") {
		t.Fatalf("restore reply lost the note: %q %v", res, err)
	}
}

// A local-only file at a path a teammate also syncs: the local pass skips it
// (excluded here), so its journaled version must still be searched — and the
// local bytes never matched (bot review P2).
func TestLocalMCPGrepSearchesJournaledVersionOfLocalOnlyPath(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	ctx := context.Background()
	be, err := remote.Open(ctx, "file://"+t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dev := func(id string) *syncer.Session {
		st, _ := store.Open(filepath.Join(t.TempDir(), "vol"))
		return &syncer.Session{Folder: t.TempDir(), Store: st, Device: config.Device{ID: id}, Backend: be}
	}
	b := dev("devb")
	os.WriteFile(filepath.Join(b.Folder, ".bdriveignore"), []byte(".env\n"), 0o644)
	os.WriteFile(filepath.Join(b.Folder, ".env"), []byte("MY SECRET needle\n"), 0o644)
	if _, err := b.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	a := dev("deva")
	a.Cycle(ctx)
	os.WriteFile(filepath.Join(a.Folder, ".bdriveignore"), []byte("# widened\n"), 0o644)
	os.WriteFile(filepath.Join(a.Folder, ".env"), []byte("team needle\n"), 0o644)
	if _, err := a.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	v := &projectView{root: b.Folder, sess: b}
	if err := v.refresh(); err != nil {
		t.Fatal(err)
	}
	m := &localMCP{v: v}
	out, err := m.grep(ctx, lGrep{Pattern: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, ".env:1:team needle") || strings.Contains(out, "MY SECRET") {
		t.Fatalf("grep:\n%s", out)
	}
}

// restore refuses a version larger than the tool's read bound instead of
// loading it into memory (bot review P1).
func TestLocalMCPRestoreRefusesAHugeVersion(t *testing.T) {
	m := testLocalMCP(t)
	ctx := context.Background()
	abs := filepath.Join(m.v.root, "big.md")
	os.WriteFile(abs, []byte(strings.Repeat("x", 4096)), 0o644)
	m.v.sess.Cycle(ctx)
	os.WriteFile(abs, []byte("small now"), 0o644)
	m.v.sess.Cycle(ctx)
	m.v.refresh()
	old := localReadMax
	localReadMax = 1024
	defer func() { localReadMax = old }()
	v1, _ := fileSHAOf(strings.Repeat("x", 4096))
	if _, err := m.restore(ctx, lRestore{Path: "big.md", SHA: v1}); err == nil || !strings.Contains(err.Error(), "bdrive restore") {
		t.Fatalf("got %v", err)
	}
	if b, _ := os.ReadFile(abs); string(b) != "small now" {
		t.Fatalf("file changed: %q", b)
	}
}

// read pages without splitting the whole file: numbering, paging and the
// line count stay right on a file of many tiny lines (bot review P1).
func TestLocalMCPReadPagesInPlace(t *testing.T) {
	m := testLocalMCP(t)
	os.WriteFile(filepath.Join(m.v.root, "many.md"), []byte(strings.Repeat("a\n", 100000)+"last"), 0o644)
	out, err := m.read(context.Background(), lRead{Path: "many.md", Offset: 99999, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "(100001 lines") || !strings.Contains(out, " 99999\ta\n") || !strings.Contains(out, "100001\tlast\n") {
		t.Fatalf("read:\n%s", out)
	}
	out, _ = m.read(context.Background(), lRead{Path: "many.md", Limit: 2})
	if !strings.Contains(out, "     2\ta\n") || !strings.Contains(out, "[99999 more lines — read with offset:3]") {
		t.Fatalf("read head:\n%s", out)
	}
}

// A grep reply is bounded in bytes whatever limit is asked, and says it was cut.
func TestLocalMCPGrepOutputIsBounded(t *testing.T) {
	m := testLocalMCP(t)
	os.WriteFile(filepath.Join(m.v.root, "many.md"), []byte(strings.Repeat("needle "+strings.Repeat("z", 300)+"\n", 20000)), 0o644)
	out, err := m.grep(context.Background(), lGrep{Pattern: "needle", Limit: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > localReadOut+64<<10 || !strings.Contains(out, "results cut") {
		t.Fatalf("reply of %d bytes", len(out))
	}
}

// A write over the size bound is refused before anything touches the disk.
func TestLocalMCPRefusesAnOversizeWriteUpFront(t *testing.T) {
	m := testLocalMCP(t)
	old := localReadMax
	localReadMax = 10
	defer func() { localReadMax = old }()
	if _, err := m.write(context.Background(), lWrite{Path: "big.md", Content: strings.Repeat("x", 11)}); err == nil {
		t.Fatal("oversize write must be refused")
	}
	if _, err := os.Stat(filepath.Join(m.v.root, "big.md")); err == nil {
		t.Fatal("refused write still landed")
	}
}

// replace_all is sized before it is built (bot review P1).
func TestLocalMCPEditRefusesAnOversizeResultBeforeBuildingIt(t *testing.T) {
	m := testLocalMCP(t)
	abs := filepath.Join(m.v.root, "a.md")
	os.WriteFile(abs, []byte(strings.Repeat("a", 1000)), 0o644)
	old := localReadMax
	localReadMax = 5000
	defer func() { localReadMax = old }()
	_, err := m.edit(context.Background(), lEdit{Path: "a.md", OldString: "a", NewString: strings.Repeat("b", 100), ReplaceAll: true})
	if err == nil || !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("got %v", err)
	}
	if b, _ := os.ReadFile(abs); len(b) != 1000 {
		t.Fatal("file changed")
	}
}

// A raw read is bounded like a numbered one (bot review P1).
func TestLocalMCPRawReadIsBounded(t *testing.T) {
	m := testLocalMCP(t)
	os.WriteFile(filepath.Join(m.v.root, "big.md"), []byte(strings.Repeat("x", localReadOut+1)), 0o644)
	if _, err := m.read(context.Background(), lRead{Path: "big.md", Raw: true}); err == nil || !strings.Contains(err.Error(), "bdrive cat") {
		t.Fatalf("got %v", err)
	}
	os.WriteFile(filepath.Join(m.v.root, "ok.md"), []byte("small"), 0o644)
	if out, err := m.read(context.Background(), lRead{Path: "ok.md", Raw: true}); err != nil || out != "small" {
		t.Fatalf("small raw read: %q %v", out, err)
	}
}

// The swap, the check and any swap back happen under the volume lock, so no
// sync cycle can scan the moment our bytes sit at the path before a refusal
// (bot review P1).
func TestLocalMCPSwapHoldsTheVolumeLock(t *testing.T) {
	m := testLocalMCP(t)
	ctx := context.Background()
	abs := filepath.Join(m.v.root, "notes.md")
	os.WriteFile(abs, []byte("v1\n"), 0o644)
	out, _ := m.read(ctx, lRead{Path: "notes.md"})
	sha := strings.TrimPrefix(strings.Fields(out)[1], "sha:")
	held := false
	mcpBeforeRename = func() { os.WriteFile(abs, []byte("second\n"), 0o644) }
	mcpBeforeSwapBack = func() {
		unlock, err := m.v.sess.Store.TryLock()
		if err != nil || unlock == nil {
			held = true
			return
		}
		unlock()
	}
	defer func() { mcpBeforeRename, mcpBeforeSwapBack = nil, nil }()
	if _, err := m.write(ctx, lWrite{Path: "notes.md", Content: "mine\n", SHA: sha}); err == nil {
		t.Fatal("must be refused")
	}
	if !held {
		t.Fatal("the volume lock was not held between the swap and the swap back")
	}
}

// swapBackFails sets notes.md at v1 (journaled), reads its sha, and arranges
// for a second writer to save just before our swap and for the swap back to
// fail — the case where our refused bytes are left at the path.
func swapBackFails(t *testing.T) (*localMCP, string, string) {
	m := testLocalMCP(t)
	abs := filepath.Join(m.v.root, "notes.md")
	os.WriteFile(abs, []byte("v1\n"), 0o644)
	if _, err := m.v.sess.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.v.refresh()
	out, _ := m.read(context.Background(), lRead{Path: "notes.md"})
	sha := strings.TrimPrefix(strings.Fields(out)[1], "sha:")
	mcpBeforeRename = func() { os.WriteFile(abs, []byte("second\n"), 0o644) }
	calls := 0
	swapFn = func(a, b string) error {
		calls++
		if calls == 2 {
			return os.ErrPermission
		}
		return swapFiles(a, b)
	}
	t.Cleanup(func() { mcpBeforeRename, mcpBeforeSwapBack, swapFn = nil, nil, swapFiles })
	return m, abs, sha
}

func journaled(t *testing.T, m *localMCP, content string) bool {
	t.Helper()
	want, _ := fileSHAOf(content)
	ops, _ := m.v.sess.Store.DeviceOps("dev")
	for _, op := range ops {
		if op.Blob == want {
			return true
		}
	}
	return false
}

// When nothing can put the other version back, nothing more is written over
// the path (that could destroy a save made since), and the refused bytes are
// quarantined: no sync cycle journals them — not the next, not after a
// restart — until the file changes, which then syncs normally (bot review P1).
func TestLocalMCPFailedSwapBackQuarantinesTheRefusedWrite(t *testing.T) {
	m, abs, sha := swapBackFails(t)
	ctx := context.Background()
	_, err := m.write(ctx, lWrite{Path: "notes.md", Content: "mine\n", SHA: sha})
	if err == nil || !strings.Contains(err.Error(), "will NOT sync") {
		t.Fatalf("got %v", err)
	}
	matches, _ := filepath.Glob(abs + ".bdrive-mcp-displaced-*")
	if len(matches) != 1 {
		t.Fatalf("the other version was not kept: %v", matches)
	}
	if b, _ := os.ReadFile(matches[0]); string(b) != "second\n" {
		t.Fatalf("kept = %q", b)
	}
	m.v.sess.Cycle(ctx)
	restarted := &syncer.Session{Folder: m.v.sess.Folder, Store: m.v.sess.Store, Device: m.v.sess.Device}
	restarted.Cycle(ctx)
	if journaled(t, m, "mine\n") {
		t.Fatal("the refused write was journaled")
	}
	os.WriteFile(abs, []byte("merged by hand\n"), 0o644)
	restarted.Cycle(ctx)
	if !journaled(t, m, "merged by hand\n") {
		t.Fatal("a later edit at the quarantined path did not sync")
	}
}

// A save landing while the swap back fails is never written over: it stays
// at the path, and because it is not the refused bytes it syncs.
func TestLocalMCPConcurrentSaveDuringFailedSwapBackIsKeptAndSyncs(t *testing.T) {
	m, abs, sha := swapBackFails(t)
	mcpBeforeSwapBack = func() { os.WriteFile(abs, []byte("third\n"), 0o644) }
	if _, err := m.write(context.Background(), lWrite{Path: "notes.md", Content: "mine\n", SHA: sha}); err == nil {
		t.Fatal("must be refused")
	}
	if b, _ := os.ReadFile(abs); string(b) != "third\n" {
		t.Fatalf("concurrent save overwritten: %q", b)
	}
	m.v.sess.Cycle(context.Background())
	if !journaled(t, m, "third\n") || journaled(t, m, "mine\n") {
		t.Fatal("want the concurrent save journaled and the refused bytes not")
	}
}

// A successful write releases its quarantine: the bytes sync.
func TestLocalMCPSuccessfulWriteSyncs(t *testing.T) {
	m := testLocalMCP(t)
	ctx := context.Background()
	if _, err := m.write(ctx, lWrite{Path: "ok.md", Content: "fine\n"}); err != nil {
		t.Fatal(err)
	}
	m.v.sess.Cycle(ctx)
	if !journaled(t, m, "fine\n") {
		t.Fatal("a successful write must sync")
	}
}

// A later write to a path whose earlier refused bytes are quarantined adds
// its own entry and releases only its own: when it fails too (or dies before
// its swap), the earlier refused bytes stay held back (bot review P1).
func TestLocalMCPRetryKeepsAnEarlierQuarantine(t *testing.T) {
	m, abs, sha := swapBackFails(t)
	ctx := context.Background()
	if _, err := m.write(ctx, lWrite{Path: "notes.md", Content: "mine\n", SHA: sha}); err == nil {
		t.Fatal("first write must be refused")
	}
	// Second attempt fails at its own swap (not stuck): releases only itself.
	mcpBeforeRename = nil
	swapFn = func(a, b string) error { return os.ErrPermission }
	out, _ := m.read(ctx, lRead{Path: "notes.md"})
	cur := strings.TrimPrefix(strings.Fields(out)[1], "sha:")
	if _, err := m.write(ctx, lWrite{Path: "notes.md", Content: "again\n", SHA: cur}); err == nil {
		t.Fatal("second write must fail")
	}
	m.v.sess.Cycle(ctx)
	if journaled(t, m, "mine\n") {
		t.Fatal("the earlier refused write lost its quarantine and synced")
	}
	if b, _ := os.ReadFile(abs); string(b) != "mine\n" {
		t.Fatalf("file = %q", b)
	}
}
