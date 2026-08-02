package syncer

// Round 8, the sabotage round: every guard on scoreboard row 15 was reverted
// one at a time and the whole TestSec suite re-run. These are the guards that
// SURVIVED their reversion — the suite stayed green with the fix removed, so
// the row was closed on tests that do not actually hold it up.
//
// Each test here is constructed so only the guard under test can produce the
// refusal: the fixture reaches the guard directly (a seeded state cache, a
// direct call to the namer, the clock helpers themselves) rather than through
// a path some other guard already closes.
//
// Helper prefix: secaud3.

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/store"
)

// ---- the lamport ceiling: two guards that mask each other ----

// TestSec_Lamport_APeerReadingCannotBeAbsorbedPastTheCeiling
//
// absorbLamport's `peer <= maxLamport` clause and tickLamport's MaxInt64 stop
// are two separate guards, and the existing end-to-end test
// (TestSec_SyncJournal_ExtremeLamportCannotFreezeADevice) goes green with
// EITHER one removed, because the surviving one covers for it: with no ceiling
// on absorb, tick's stop keeps the clock off the wrap; with no stop on tick,
// the ceiling keeps the clock from ever reaching MaxInt64. Only reverting both
// reproduces the freeze, so neither guard is individually held up by anything.
//
// The secure behavior is the one the constant documents: a value that is not a
// clock reading is not absorbed.
func TestSec_Lamport_APeerReadingCannotBeAbsorbedPastTheCeiling(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cur, peer int64
		want      int64
	}{
		{"ordinary advance", 5, 9, 9},
		{"the ceiling itself is a legal reading", 5, maxLamport, maxLamport},
		{"one past the ceiling is not", 5, maxLamport + 1, 5},
		{"MaxInt64 is not", 5, math.MaxInt64, 5},
		{"a negative reading never moves the clock back", 5, -1, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := absorbLamport(tc.cur, tc.peer); got != tc.want {
				t.Errorf("absorbLamport(%d, %d) = %d, want %d — a peer's lamport value moved this device's clock somewhere it cannot come back from",
					tc.cur, tc.peer, got, tc.want)
			}
		})
	}
}

// TestSec_Lamport_TheLocalTickNeverWrapsNegative
//
// The other half of the pair. A device that legitimately absorbed the ceiling
// must still be able to write an op that sorts after it, and the increment must
// never wrap: a negative clock sorts every op this device writes from then on
// before everything it has already seen — a silent, permanent write lock.
func TestSec_Lamport_TheLocalTickNeverWrapsNegative(t *testing.T) {
	for _, cur := range []int64{0, 1, maxLamport, maxLamport + 1, math.MaxInt64 - 1, math.MaxInt64} {
		got := tickLamport(cur)
		if got < cur {
			t.Errorf("tickLamport(%d) = %d — the local clock went BACKWARDS; every op this device writes now sorts before everything it has already seen", cur, got)
		}
		if got <= 0 {
			t.Errorf("tickLamport(%d) = %d — the local clock is not positive", cur, got)
		}
	}
}

// ---- materialize's delete loop: three guards, none of them held up ----

// secaud3Delete drives materialize's DELETE pass and nothing else: an empty
// target (so every cache key is a candidate for removal) and a cache holding
// exactly the given keys, each fingerprinted against the file on disk so the
// loop's dirty check passes and os.Remove is actually reached.
//
// It calls materialize directly rather than going through Cycle on purpose.
// scan runs first in a real cycle and its OWN delete pass applies the same
// unsafeRel/neverSync rule to the same cache — so a cycle-level fixture can
// never tell which of the two guards refused, and the scan-side copy masks the
// materialize-side one entirely (this is why the whole-cycle version of these
// tests stayed green with the materialize guards deleted). Here only the loop
// under test can produce the refusal.
func secaud3Delete(t *testing.T, s *Session, keys map[string]string) {
	t.Helper()
	cache := map[string]store.CachedFile{}
	for rel, onDisk := range keys {
		fi, err := os.Stat(onDisk)
		if err != nil {
			t.Fatalf("fixture: %v", err)
		}
		cache[rel] = store.CachedFile{
			Blob: strings.Repeat("0", 64), Size: fi.Size(),
			Mode: 0o644, MTimeNS: fi.ModTime().UnixNano(),
		}
	}
	filter, err := loadFilter(s.Folder, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.materialize(map[string]journal.FileState{}, cache, filter); err != nil {
		t.Fatalf("materialize: %v", err)
	}
}

// TestSec_Cache_ADeleteFromTheStateCacheCannotUnlinkAReservedPath
//
// The delete pass in materialize mints an os.Remove from every cache key the
// replayed target no longer holds. `.git/hooks/pre-commit` is a clean relative
// path — it passes store.cleanRel, so LoadCache hands it straight back — and
// without the loop's own neverSync clause it is unlinked from the working
// folder. Deleting a teammate's git hooks needs no traversal and no symlink.
func TestSec_Cache_ADeleteFromTheStateCacheCannotUnlinkAReservedPath(t *testing.T) {
	victim := newDevice(t, "victim", nil)
	victim.MountID = "m-audit3a"

	hook := filepath.Join(victim.Folder, ".git", "hooks", "pre-commit")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho real hook\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	secaud3Delete(t, victim, map[string]string{".git/hooks/pre-commit": hook})

	if _, err := os.Stat(hook); err != nil {
		t.Fatalf("materialize deleted a reserved path named only by the state cache: %v — .git/ is never BearDrive's to remove", err)
	}
}

// TestSec_Cache_ADeleteFromTheStateCacheCannotUnlinkOutsideTheMount
//
// The same loop, the on-disk half of the boundary. "docs/secret.md" is a clean
// relative path by every lexical test; when `docs` is a symlink it names a file
// outside the mount, and os.Remove follows it. store.UnderRoot is the only
// thing between the loop and that file — cleanRel is lexical and cannot see it.
func TestSec_Cache_ADeleteFromTheStateCacheCannotUnlinkOutsideTheMount(t *testing.T) {
	victim := newDevice(t, "victim", nil)
	victim.MountID = "m-audit3b"

	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(secret, []byte("not this project's file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(victim.Folder, "docs")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	secaud3Delete(t, victim, map[string]string{"docs/secret.md": secret})

	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("materialize deleted a file OUTSIDE the mount: %v — a symlinked directory made a clean relative cache key name someone else's file", err)
	}
}

// TestSec_Cache_ADeleteForANewlyIgnoredPathLeavesTheFileOnDisk
//
// The third guard in the same loop, and the one that is data loss rather than
// escape: a path that left sync scope (an added .bdriveignore rule, or a
// teammate's --prune) is absent from the replayed target exactly like a deleted
// one. Without the filter.Skip clause the loop unlinks every device's local
// copy — the data loss the feature exists to avoid.
func TestSec_Cache_ADeleteForANewlyIgnoredPathLeavesTheFileOnDisk(t *testing.T) {
	victim := newDevice(t, "victim", nil)
	victim.MountID = "m-audit3c"

	write(t, victim.Folder, "build/artifact.bin", "expensive local output")
	write(t, victim.Folder, IgnoreFile, "build/\n")
	kept := filepath.Join(victim.Folder, "build", "artifact.bin")
	secaud3Delete(t, victim, map[string]string{"build/artifact.bin": kept})

	if got, err := os.ReadFile(kept); err != nil || string(got) != "expensive local output" {
		t.Fatalf("ignoring a path deleted the local file (err %v) — leaving sync scope is not a delete", err)
	}
}

// ---- the conflict-copy namer: length was tested, shape was not ----

// TestSec_ConflictName_APeerDeviceNameCannotSteerTheCopyOutOfItsDirectory
//
// Op.DeviceName is arbitrary JSON off a peer's journal and lands inside a
// filename the VICTIM then journals, signs and pushes under its own device id.
// The existing test only ever passes 300 'A's, so it measures the length clip
// and never the sanitizer: a name carrying '/' or ".." reshapes the path.
func TestSec_ConflictName_APeerDeviceNameCannotSteerTheCopyOutOfItsDirectory(t *testing.T) {
	when := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	for _, hostile := range []string{
		"../../../etc",
		"a/b/c",
		"..",
		".git",
		"x\x00y",
		"\n../evil",
	} {
		got := conflictName("notes/shared.md", hostile, when)
		if dir := filepath.ToSlash(filepath.Dir(got)); dir != "notes" {
			t.Errorf("conflictName(..., %q) = %q — the copy left its own directory (dir %q)", hostile, got, dir)
		}
		if !journal.SafePath(got) {
			t.Errorf("conflictName(..., %q) = %q — the victim would journal, sign and push a path it refuses from everyone else", hostile, got)
		}
		if strings.Contains(strings.TrimPrefix(got, "notes/"), "/") {
			t.Errorf("conflictName(..., %q) = %q — a separator from the peer's device name survived into the name", hostile, got)
		}
	}
}

// TestSec_ConflictName_ALongPathBaseIsStillAWritableName
//
// The other half of the bound. The existing test attacks DeviceName, which is
// clipped to 32 — so the clip on the path's own BASE is never reached. A peer
// controls Op.Path too, and NAME_MAX is 255 everywhere BearDrive runs: an
// unwritable conflict name is worse than an ugly one, because the op is already
// in the victim's own journal by the time the write is attempted and replays
// (and fails) on every cycle from then on.
func TestSec_ConflictName_ALongPathBaseIsStillAWritableName(t *testing.T) {
	when := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	long := strings.Repeat("n", 400) + ".md"
	got := conflictName("notes/"+long, "peer", when)
	base := filepath.Base(filepath.FromSlash(got))
	if len(base) > 255 {
		t.Errorf("conflict copy base name is %d bytes — longer than NAME_MAX, so the op the victim just signed can never be written on any later cycle either", len(base))
	}
}

// ---- neverSync's reserved-FILE clause ----

// TestSec_SyncPeer_AReservedFileNameFromAPeerNeverLandsOnDisk
//
// neverSync has three clauses; the suite holds up two of them. The reserved
// NAME clause (config.ReservedName) survives its own removal green. A peer's
// journal names the files the scanner is defined never to produce — the
// ".bdrive-tmp-" prefix is the atomic-write temp name the scanner is documented
// to ignore, so a planted one is a file that materializes and which no later
// scan will ever journal, notice or clean up.
func TestSec_SyncPeer_AReservedFileNameFromAPeerNeverLandsOnDisk(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)

	const content = "planted by a peer"
	blob := secjrnBlob(t, be, content)
	reserved := []string{".bdrive-tmp-evil", "notes/.bdrive-tmp-x", ".DS_Store", "notes/.DS_Store"}
	var ops []journal.Op
	for i, p := range reserved {
		ops = append(ops, secjrnOp(int64(i+1), p, blob, len(content)))
	}
	// A control op, so a cycle that simply did nothing cannot pass this test.
	ops = append(ops, secjrnOp(int64(len(ops)+1), "notes/ok.md", blob, len(content)))
	secjrnPush(t, be, "attacker", ops)

	if _, err := secpeerCycle(t, victim); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if _, err := os.Stat(filepath.Join(victim.Folder, "notes", "ok.md")); err != nil {
		t.Fatalf("control: the ordinary op did not materialize: %v", err)
	}
	for _, p := range reserved {
		if _, err := os.Stat(filepath.Join(victim.Folder, filepath.FromSlash(p))); err == nil {
			t.Errorf("a peer's op materialized %q — a name BearDrive is defined never to sync, so no later scan will journal, notice or clean it up", p)
		}
	}
}
