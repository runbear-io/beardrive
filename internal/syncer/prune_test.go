package syncer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// Multi-device behavior of `bdrive sync --prune`: removing from the hub what
// .bdriveignore excludes, without any device losing a byte on disk.

// hubState is what the devices converge to — the replayed journals, i.e. what
// the hub still holds.
func hubState(t *testing.T, s *Session) map[string]journal.FileState {
	t.Helper()
	all, err := s.Store.AllOps()
	if err != nil {
		t.Fatal(err)
	}
	return journal.Replay(all)
}

func prune(t *testing.T, s *Session) *Result {
	t.Helper()
	s.Prune = true
	defer func() { s.Prune = false }()
	return cycle(t, s)
}

func exists(t *testing.T, folder, rel string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(folder, filepath.FromSlash(rel)))
	return err == nil
}

// The headline behavior: A prunes an ignored path, the hub loses it, and B
// keeps its local copy while dropping it from tracking and pushing nothing
// back.
func TestPruneRemovesFromHubKeepsLocal(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	write(t, a.Folder, "notes.md", "keep me")
	write(t, a.Folder, ".omc/state.json", "{}")
	cycle(t, a)
	cycle(t, b)
	if !exists(t, b.Folder, ".omc/state.json") {
		t.Fatal("setup: the file should have synced to b")
	}

	write(t, a.Folder, IgnoreFile, ".omc/\n")
	res := prune(t, a)
	if res.Pruned != 1 {
		t.Fatalf("Pruned = %d, want 1", res.Pruned)
	}
	if _, ok := hubState(t, a)[".omc/state.json"]; ok {
		t.Fatal("pruned path should be gone from the replayed state")
	}
	if !exists(t, a.Folder, ".omc/state.json") {
		t.Fatal("prune must not touch the pruning device's own disk")
	}

	res = cycle(t, b)
	if !exists(t, b.Folder, ".omc/state.json") {
		t.Fatal("peer lost its local copy — this is the data loss prune exists to avoid")
	}
	if _, ok := hubState(t, b)[".omc/state.json"]; ok {
		t.Fatal("peer should see the path gone from the merged state")
	}
	if read(t, b.Folder, "notes.md") != "keep me" {
		t.Fatal("unrelated file disturbed")
	}
	// ...and nothing gets pushed back: the path is out of scope now.
	if res = cycle(t, b); res.LocalOps != 0 {
		t.Fatalf("peer re-journaled %d op(s) for a path it no longer tracks", res.LocalOps)
	}
	if _, ok := hubState(t, a)[".omc/state.json"]; ok {
		t.Fatal("path came back to the hub")
	}
}

// The motivating case: the path was dropped from the local cache cycles ago,
// when the ignore rule was first added, so nothing local remembers it. Prune
// reconciles against the replayed state, not the cache, so it still finds it.
func TestPruneFindsHistoricallyDroppedPaths(t *testing.T) {
	a := newDevice(t, "deva", sharedRemote(t))

	write(t, a.Folder, ".omc/a.txt", "one")
	write(t, a.Folder, ".omc/b.txt", "two")
	write(t, a.Folder, "keep.md", "kept")
	cycle(t, a)

	// The rule lands, and plain sync silently stops tracking the paths —
	// today's behavior, unchanged.
	write(t, a.Folder, IgnoreFile, ".omc/\n")
	res := cycle(t, a)
	if res.LocalOps != 1 { // the .bdriveignore put only, no deletes
		t.Fatalf("plain sync journaled %d ops, want 1 (the ignore file)", res.LocalOps)
	}
	cycle(t, a)
	cycle(t, a) // several cycles later, nothing local remembers .omc/

	res = prune(t, a)
	if res.Pruned != 2 {
		t.Fatalf("Pruned = %d, want 2 (both historically dropped paths)", res.Pruned)
	}
	state := hubState(t, a)
	if _, ok := state[".omc/a.txt"]; ok {
		t.Fatal(".omc/a.txt still on the hub")
	}
	if _, ok := state["keep.md"]; !ok {
		t.Fatal("prune removed an unrelated path")
	}
	if !exists(t, a.Folder, ".omc/a.txt") || !exists(t, a.Folder, ".omc/b.txt") {
		t.Fatal("prune deleted local files")
	}
}

// Prune reconciles against the shared rules only. A device's own legacy
// scope is a statement about its disk, not about what the team may hold, so
// pruning must never act on it.
func TestPruneIgnoresPerDeviceScope(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	write(t, a.Folder, "docs/guide.md", "shared")
	write(t, a.Folder, "src/main.go", "package main")
	write(t, a.Folder, ".omc/state.json", "{}")
	cycle(t, a)

	// B only wants docs/ locally.
	write(t, b.Folder, ".bdrive/config.json", `{"include": ["/docs/"]}`)
	cycle(t, b)
	if exists(t, b.Folder, "src/main.go") {
		t.Fatal("setup: src/ is outside b's scope")
	}

	res := prune(t, b)
	if res.Pruned != 0 {
		t.Fatalf("b pruned %d path(s); a narrow scope must never delete a teammate's files", res.Pruned)
	}
	if _, ok := hubState(t, b)["src/main.go"]; !ok {
		t.Fatal("out-of-scope path was removed from the hub")
	}

	// The shared rule is a different matter: once it exists, either device
	// may prune it, and b's scope still isn't consulted.
	write(t, b.Folder, IgnoreFile, ".omc/\n")
	res = prune(t, b)
	if res.Pruned != 1 {
		t.Fatalf("Pruned = %d, want 1 (the ignored path only)", res.Pruned)
	}
	state := hubState(t, b)
	if _, ok := state["src/main.go"]; !ok {
		t.Fatal("b's scope exclusion got pruned alongside the ignore rule")
	}
	cycle(t, a)
	if read(t, a.Folder, "src/main.go") != "package main" {
		t.Fatal("a lost a file that only b's scope excluded")
	}
}

// The race the report called unclosable: a peer receives the new rules and
// the deletes they justify in the SAME batch. The filter is reloaded from the
// pulled ignore file mid-cycle, so the files survive that cycle — not the
// next one.
func TestPruneRaceWithPulledIgnoreFile(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	write(t, a.Folder, ".omc/state.json", "{}")
	cycle(t, a)
	cycle(t, b)

	write(t, a.Folder, IgnoreFile, ".omc/\n")
	prune(t, a) // rules and deletes are pushed together

	cycle(t, b) // exactly one cycle
	if !exists(t, b.Folder, ".omc/state.json") {
		t.Fatal("peer unlinked the file in the cycle that delivered the rules")
	}
	if read(t, b.Folder, IgnoreFile) != ".omc/\n" {
		t.Fatal("peer should have the new rules after that cycle")
	}
}

func TestPruneIsIdempotent(t *testing.T) {
	a := newDevice(t, "deva", sharedRemote(t))

	write(t, a.Folder, ".omc/state.json", "{}")
	cycle(t, a)
	write(t, a.Folder, IgnoreFile, ".omc/\n")

	if res := prune(t, a); res.Pruned != 1 {
		t.Fatalf("first prune: Pruned = %d, want 1", res.Pruned)
	}
	if res := prune(t, a); res.Pruned != 0 {
		t.Fatalf("second prune: Pruned = %d, want 0", res.Pruned)
	}
}

// Accepted residual: a peer that edits the file in the window between the
// prune and its own pull wins by lamport order and the file returns to the
// hub. Nothing is silent about it — it shows in history — and a second prune
// removes it again.
func TestPeerEditResurrectsUntilPrunedAgain(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	write(t, a.Folder, "debug.log", "v1")
	cycle(t, a)
	cycle(t, b)

	// B works offline for a while, so its clock runs ahead of A's.
	b.Backend = nil
	for i := range 5 {
		write(t, b.Folder, "work/f"+string(rune('a'+i))+".txt", "busy")
		cycle(t, b)
	}
	time.Sleep(10 * time.Millisecond)
	write(t, b.Folder, "debug.log", "edited while a was pruning")
	cycle(t, b)

	write(t, a.Folder, IgnoreFile, "*.log\n")
	if res := prune(t, a); res.Pruned != 1 {
		t.Fatalf("Pruned = %d, want 1", res.Pruned)
	}

	b.Backend = be
	cycle(t, b) // b's later put beats a's delete
	if !exists(t, b.Folder, "debug.log") {
		t.Fatal("b lost its own edit")
	}
	cycle(t, a)
	if _, ok := hubState(t, a)["debug.log"]; !ok {
		t.Fatal("the peer's concurrent edit should have resurrected the path")
	}

	// A second prune settles it, and still nobody loses a file.
	if res := prune(t, a); res.Pruned != 1 {
		t.Fatalf("second prune: Pruned = %d, want 1", res.Pruned)
	}
	if _, ok := hubState(t, a)["debug.log"]; ok {
		t.Fatal("still on the hub after a second prune")
	}
	cycle(t, b)
	if !exists(t, b.Folder, "debug.log") || !exists(t, a.Folder, "debug.log") {
		t.Fatal("a prune deleted a local file")
	}
}
