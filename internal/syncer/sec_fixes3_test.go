package syncer

// Round 4 — the target is round 3's own fixes on the receiving device
// (scoreboard row 15): unsafeRel, config.ReservedDir/EqualFold, safeMode, and
// the absorbLamport/tickLamport ceiling.
//
// Every test asserts the SECURE behavior, so it goes green the moment the hole
// is closed and stays as a permanent regression test. Helpers are prefixed
// secfx3 per the harness rules; no existing file is touched.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

// secfx3Blob and secfx3Op mirror the round-3 helpers in sec_journal_test.go
// without touching that file: content in the shared remote, and a put op with
// every field a real device sets.
func secfx3Blob(t *testing.T, be remote.Backend, content string) string {
	t.Helper()
	sum := sha256hex(content)
	if err := be.Put(context.Background(), "blobs/"+sum, strings.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	return sum
}

func secfx3Push(t *testing.T, be remote.Backend, dev string, ops []journal.Op) {
	t.Helper()
	data, err := journal.Marshal(ops)
	if err != nil {
		t.Fatal(err)
	}
	if err := be.Put(context.Background(), "journal/"+dev+".jsonl", strings.NewReader(string(data)), int64(len(data))); err != nil {
		t.Fatal(err)
	}
}

func secfx3Op(seq int64, p, blob string, size int) journal.Op {
	return journal.Op{
		Seq: seq, Lamport: seq, Time: time.Now().UTC(),
		Device: "attacker", DeviceName: "attacker", Author: "attacker@test",
		Kind: journal.KindPut, Path: p, Blob: blob, Size: int64(size), Mode: 0o644,
	}
}

// ---------------------------------------------------------------------------
// unsafeRel — the mount boundary is a STRING check, and the filesystem is not.
// ---------------------------------------------------------------------------

// unsafeRel refuses anything that is not already a clean relative slash path.
// That closes "../x", but the mount root is not a string: a directory inside
// the mount that is a symlink pointing somewhere else makes "docs/pwned.txt" a
// perfectly clean relative path that lands outside the mount anyway.
//
// The asymmetry is the whole bug, and it is visible in this package's own
// code: walkFolder refuses to descend into a symlinked directory
// (!d.Type().IsRegular() → vSkipFile), so scan never uploads through one —
// but materialize resolves the same path with filepath.Join + MkdirAll +
// os.CreateTemp, all of which follow it. A peer therefore writes any file the
// victim's user can write, through a symlink the victim created for entirely
// ordinary reasons ("ln -s ~/Documents/shared notes").
//
// This is the same class round 3 closed on the hub's single-volume uploader
// (TestSec_Path_DirUploadCannotEscapeThroughSymlink → underRoot); the syncer's
// materialize never got the same treatment.
func TestSec_SyncJournal_PeerCannotMaterializeThroughASymlinkedDirectory(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)

	// A directory the victim already has, outside the mount, reached through a
	// symlink inside it. This is the victim's own pre-existing layout — the
	// attacker only supplies a journal line.
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(victim.Folder, "docs")); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}

	const content = "written through a symlink by a peer journal"
	blob := secfx3Blob(t, be, content)
	ops := []journal.Op{
		secfx3Op(1, "docs/pwned.txt", blob, len(content)),
		secfx3Op(2, "docs/nested/deeper/pwned.txt", blob, len(content)),
		secfx3Op(3, "notes/ok.md", blob, len(content)), // control
	}
	secfx3Push(t, be, "attacker", ops)

	cycle(t, victim)

	// Control: the pull and materialize really ran, so the rest measures the
	// guard rather than a dead fixture.
	if got := read(t, victim.Folder, "notes/ok.md"); got != content {
		t.Fatalf("control op did not materialize: %q", got)
	}

	for _, rel := range []string{"pwned.txt", "nested/deeper/pwned.txt"} {
		abs := filepath.Join(outside, rel)
		if _, err := os.Stat(abs); err == nil {
			t.Errorf("a peer journal wrote %s — outside the mount root %s, through the symlink at docs/", abs, victim.Folder)
		}
	}
	// Belt and braces: nothing may have been created on the far side at all,
	// not even the parent chain (round 3's own lesson from underRoot).
	if ents, err := os.ReadDir(outside); err == nil && len(ents) != 0 {
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("materialize created %v outside the mount root through the symlinked directory", names)
	}
}

// ---------------------------------------------------------------------------
// unsafeRel — a path that is clean, relative, and still unwritable.
// ---------------------------------------------------------------------------

// unsafeRel is the only thing standing between Op.Path and the filesystem, and
// it judges the path's SHAPE. A NUL byte is a perfectly clean relative slash
// path by every test unsafeRel makes — and os.Rename refuses it with EINVAL.
//
// materializeFile turns that into `fmt.Errorf("write %s: %w", ...)`, materialize
// propagates it, and Cycle returns it. The op lives in the pulled journal
// forever, so it is replayed into `target` on every subsequent cycle and every
// cycle from then on fails at the same line: the device never pushes again, and
// nothing it does locally ever reaches the team. One line of JSON, pushed to a
// project's own journal key, permanently and silently kills sync on every
// device that reads it.
//
// This is the sync-layer twin of round 3's
// TestSec_Reads_OneUnstorableBucketCannotWedgeTheLedger, and the same rule
// applies here — from this package's own stated invariant: "unreadable/vanished
// files during scan are skipped and retried next cycle... never break sync".
func TestSec_SyncJournal_UnwritablePathCannotWedgeTheCycle(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)

	const content = "payload"
	blob := secfx3Blob(t, be, content)
	poison := []string{
		"notes/bad\x00name.md",              // NUL: EINVAL from every syscall
		"notes/" + strings.Repeat("a", 400), // ENAMETOOLONG on every unix
	}
	ops := make([]journal.Op, 0, len(poison)+1)
	for i, p := range poison {
		ops = append(ops, secfx3Op(int64(i+1), p, blob, len(content)))
	}
	ops = append(ops, secfx3Op(int64(len(poison)+1), "notes/ok.md", blob, len(content)))
	secfx3Push(t, be, "attacker", ops)

	// Cycle must not fail. (cycle() t.Fatals on error, so call it directly and
	// report the error as the finding rather than as a harness crash.)
	res, err := victim.Cycle(context.Background())
	if err != nil {
		t.Fatalf("a peer op with an unwritable Path failed the whole cycle: %v\n"+
			"the op is in the pulled journal permanently, so every later cycle fails here too — sync is dead on this device", err)
	}
	if res.Offline {
		t.Fatalf("cycle went offline: %v", res.OfflineErr)
	}
	if got := read(t, victim.Folder, "notes/ok.md"); got != content {
		t.Fatalf("control op did not materialize: %q", got)
	}

	// And the device must still be able to do its job afterwards: journal a
	// local edit and push it. This is what the wedge actually costs.
	write(t, victim.Folder, "notes/mine.md", "victim edit")
	res2, err := victim.Cycle(context.Background())
	if err != nil {
		t.Fatalf("the next cycle also failed — the poisoned op is permanent: %v", err)
	}
	if res2.LocalOps == 0 {
		t.Errorf("the victim's own edit was never journaled after the poisoned op")
	}
	if res2.Offline {
		t.Errorf("the victim can no longer reach the remote: %v", res2.OfflineErr)
	}
}

// ---------------------------------------------------------------------------
// absorbLamport / tickLamport — the ceiling is inclusive, and the tick stops.
// ---------------------------------------------------------------------------

// Round 3 capped what a peer's Lamport can do to this device's clock:
// absorbLamport takes `peer <= maxLamport` and tickLamport refuses to advance
// at or above it. maxLamport is 1<<62 — and BOTH comparisons are inclusive, so
// an op carrying exactly 1<<62 is absorbed, and the clock is then frozen at the
// ceiling forever.
//
// A frozen clock is the same weapon with one bit knocked off math.MaxInt64.
// Every op the victim writes from then on carries Lamport == 1<<62, so
// journal.Less falls through to Time — and the attacker's op, dated in the
// future, outranks every edit the victim will ever make. The victim's own
// change to its own file is journaled and then reverted on its own disk, with
// no conflict copy and no error: exactly the failure
// TestSec_SyncJournal_ExtremeLamportCannotFreezeADevice describes, reachable
// with a value the clamp accepts.
//
// The secure behavior is the invariant the clock exists for: an op this device
// writes after pulling must sort AFTER everything it pulled.
func TestSec_SyncJournal_CeilingLamportCannotFreezeADevice(t *testing.T) {
	const maxLamportLocal = int64(1) << 62 // mirrors syncer.maxLamport
	for _, tc := range []struct {
		name  string
		value int64
	}{
		{"at the ceiling", maxLamportLocal},
		{"one below the ceiling", maxLamportLocal - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := sharedRemote(t)
			victim := newDevice(t, "victim", be)

			theirs := secfx3Blob(t, be, "attacker version")
			bomb := secfx3Op(1, "shared.md", theirs, len("attacker version"))
			bomb.Lamport = tc.value
			// Dated far in the future: with the clock frozen, Less falls
			// through to Time, and this is what wins there.
			bomb.Time = time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)
			secfx3Push(t, be, "attacker", []journal.Op{bomb})

			cycle(t, victim)
			if got := read(t, victim.Folder, "shared.md"); got != "attacker version" {
				t.Fatalf("control: peer op did not materialize: %q", got)
			}

			// The victim edits the shared file, exactly as a user would.
			write(t, victim.Folder, "shared.md", "victim version")
			res := cycle(t, victim)
			if res.LocalOps == 0 {
				t.Fatal("control: the local edit was not journaled at all")
			}
			if got := read(t, victim.Folder, "shared.md"); got != "victim version" {
				t.Errorf("the victim's own later edit was reverted on its own disk: %q\n"+
					"a peer Lamport of %d is accepted by absorbLamport and then pins tickLamport at the ceiling forever",
					got, tc.value)
			}
		})
	}
}

// The narrower statement of the same defect, so a fix that only special-cases
// one path still has to satisfy the clock invariant: after pulling anything,
// this device's next own op must carry a Lamport strictly greater than every
// op it has seen. Without that, "last writer wins" stops meaning anything for
// every file this device will ever touch — not just the one the attacker named.
func TestSec_SyncJournal_LocalClockStillAdvancesAfterAHostileLamport(t *testing.T) {
	const maxLamportLocal = int64(1) << 62
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)

	blob := secfx3Blob(t, be, "peer content")
	bomb := secfx3Op(1, "theirs.md", blob, len("peer content"))
	bomb.Lamport = maxLamportLocal
	secfx3Push(t, be, "attacker", []journal.Op{bomb})
	cycle(t, victim)

	write(t, victim.Folder, "a.md", "one")
	cycle(t, victim)
	write(t, victim.Folder, "b.md", "two")
	cycle(t, victim)

	ops, err := victim.Store.AllOps()
	if err != nil {
		t.Fatal(err)
	}
	var a, b journal.Op
	for _, op := range ops {
		switch {
		case op.Device == "victim" && op.Path == "a.md":
			a = op
		case op.Device == "victim" && op.Path == "b.md":
			b = op
		}
	}
	if a.Path == "" || b.Path == "" {
		t.Fatalf("control: the victim's own ops are missing (%d ops total)", len(ops))
	}
	if !(b.Lamport > a.Lamport) {
		t.Errorf("this device's clock stopped advancing: a.md=%d b.md=%d — two independent local edits share a Lamport, "+
			"so their order is decided by wall-clock time a peer also controls", a.Lamport, b.Lamport)
	}
	if !(a.Lamport > maxLamportLocal) {
		t.Errorf("a local op written after pulling Lamport=%d carries %d — it does not sort after what this device already saw",
			maxLamportLocal, a.Lamport)
	}
}

// ---------------------------------------------------------------------------
// safeMode — the mask itself, and the cache/disk agreement round 3 claims.
// ---------------------------------------------------------------------------

// safeMode is `m & 0o777 &^ 0o022`, applied in materializeFile BEFORE the cache
// compare so that a mode the peer named and the mode on disk cannot disagree
// forever. Assert the round trip: a second cycle with nothing new must write
// nothing, and the cache's Mode must equal what is actually on disk. (This one
// is expected to pass — it pins the fix so a later refactor that moves the mask
// into writeFile makes materialize rewrite every file on every cycle.)
func TestSec_SyncJournal_SafeModeCacheAgreesWithDisk(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)

	const content = "#!/bin/sh\n"
	blob := secfx3Blob(t, be, content)
	ops := []journal.Op{}
	modes := map[string]uint32{
		"m777.sh": 0o777,
		"m666.md": 0o666,
		"m600.md": 0o600,
		"m000.md": 0,
	}
	i := int64(0)
	for rel, m := range modes {
		i++
		op := secfx3Op(i, rel, blob, len(content))
		op.Mode = m
		ops = append(ops, op)
	}
	secfx3Push(t, be, "attacker", ops)

	first := cycle(t, victim)
	if first.Materialized == 0 {
		t.Fatalf("control: nothing materialized (%+v)", first)
	}
	// Nothing changed anywhere: a second cycle must be a no-op. If safeMode
	// were applied after the cache compare, every file would be rewritten
	// forever — a permanent mtime churn that also re-journals on every device.
	second := cycle(t, victim)
	if second.Materialized != 0 {
		t.Errorf("a second cycle rewrote %d files with nothing changed — the cache disagrees with what safeMode put on disk",
			second.Materialized)
	}
	if second.LocalOps != 0 {
		t.Errorf("a second cycle journaled %d local ops with nothing changed", second.LocalOps)
	}
	for rel := range modes {
		fi, err := os.Stat(filepath.Join(victim.Folder, rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if fi.Mode().Perm()&0o022 != 0 {
			t.Errorf("%s materialized group/other-writable (%v)", rel, fi.Mode().Perm())
		}
	}
}

// ---------------------------------------------------------------------------
// ReservedDir + EqualFold — the shapes a case-folding guard still lets past.
// ---------------------------------------------------------------------------

// config.ReservedDir now compares with strings.EqualFold, which is simple
// Unicode case folding. The guard's whole purpose is to stop a peer planting a
// file the OS will resolve into the real .git or .bdrive, so it has to refuse
// every spelling the FILESYSTEM will fold onto one of those names, not every
// spelling Go's EqualFold folds.
//
// Two families it does not cover:
//   - a trailing dot or space (".git." / ".git ") — NTFS strips both when
//     opening a path, so ".git./hooks/pre-commit" IS .git/hooks/pre-commit on
//     Windows and on an SMB share.
//   - the Kelvin sign U+212A and the dotless capital I U+0130/U+0131, which
//     several case-insensitive filesystems fold onto "k" and "i". EqualFold
//     does fold U+212A onto 'k' — asserted here so a future switch to
//     strings.ToLower (which does NOT) is caught.
//
// Also pinned: an NFD-decomposed spelling. ".bdrive" and ".git" are pure ASCII
// so they have no decomposed form, which is the reason this is safe today —
// this test states that as an assertion so adding a non-ASCII reserved name
// later cannot silently open the hole.
func TestSec_SyncJournal_ReservedDirGuardCoversFilesystemFoldings(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)

	const content = "#!/bin/sh\necho pwned\n"
	blob := secfx3Blob(t, be, content)

	variants := []string{
		".git./hooks/pre-commit",
		".git /hooks/pre-commit",
		".bdrive./config.json",
		".bdrive /config.json",
		".GIT/hooks/pre-commit", // round 3's shape — control, must stay refused
		".Bdrive/config.json",
	}
	ops := make([]journal.Op, 0, len(variants)+1)
	for i, p := range variants {
		ops = append(ops, secfx3Op(int64(i+1), p, blob, len(content)))
	}
	ops = append(ops, secfx3Op(int64(len(variants)+1), "ok.md", blob, len(content)))
	secfx3Push(t, be, "attacker", ops)

	cycle(t, victim)
	if got := read(t, victim.Folder, "ok.md"); got != content {
		t.Fatalf("control op did not materialize: %q", got)
	}

	var landed []string
	for _, p := range variants {
		if _, err := os.Lstat(filepath.Join(victim.Folder, filepath.FromSlash(p))); err == nil {
			landed = append(landed, p)
		}
	}
	if len(landed) != 0 {
		t.Errorf("a peer journal materialized under a reserved-directory spelling the filesystem folds away: %v\n"+
			"Windows/SMB strip a trailing dot or space, so \".git./hooks/pre-commit\" IS .git/hooks/pre-commit there", landed)
	}
}

// ---------------------------------------------------------------------------
// unsafeRel — the degenerate shapes, asserted refused rather than assumed.
// ---------------------------------------------------------------------------

// unsafeRel's rule is "already a clean relative slash path". Several shapes
// satisfy some of its clauses and not others, and one — "." — satisfies ALL of
// them (path.Clean(".") == ".", not absolute, no "../" prefix) and resolves to
// the mount root itself. It survives today only because materializeFile
// happens to hash the directory and fail; that is luck, not a guard, so pin
// the outcome.
//
// The Windows-separator shapes are here for the record: on unix
// filepath.FromSlash is the identity, so `..\..\x` is one ordinary filename
// inside the mount and nothing escapes. What this asserts is only that nothing
// escapes; see the report's suspicions for the GOOS=windows story.
func TestSec_SyncJournal_DegenerateRelativePathsMaterializeNothing(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)
	outside := filepath.Dir(victim.Folder)
	before, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}

	const content = "degenerate"
	blob := secfx3Blob(t, be, content)
	shapes := []string{
		"", ".", "..", "docs/", "./x.md", "x/./y.md", "a//b.md", "/etc/x.md", "docs/..",
		`..\..\etc\passwd`, `C:\Windows\System32\drivers\etc\hosts`,
	}
	ops := make([]journal.Op, 0, len(shapes)+1)
	for i, p := range shapes {
		ops = append(ops, secfx3Op(int64(i+1), p, blob, len(content)))
	}
	ops = append(ops, secfx3Op(int64(len(shapes)+1), "ok.md", blob, len(content)))
	secfx3Push(t, be, "attacker", ops)

	if _, err := victim.Cycle(context.Background()); err != nil {
		t.Fatalf("a degenerate Path failed the whole cycle: %v", err)
	}
	if got := read(t, victim.Folder, "ok.md"); got != content {
		t.Fatalf("control op did not materialize: %q", got)
	}

	// The mount root must still be a directory, not a file the op replaced.
	if fi, err := os.Stat(victim.Folder); err != nil || !fi.IsDir() {
		t.Fatalf("the mount root itself was written: %v %v", fi, err)
	}
	// Nothing new above the mount.
	after, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("a degenerate op created something above the mount root: before=%d after=%d entries", len(before), len(after))
	}
	// And nothing outside from the absolute shape.
	if _, err := os.Stat("/etc/x.md"); err == nil {
		t.Error("an absolute Op.Path was written at /etc/x.md")
	}
}
