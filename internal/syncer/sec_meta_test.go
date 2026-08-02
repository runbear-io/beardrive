package syncer

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// Round 5, scoreboard row 15 — the two op fields the board records as
// "argued, not tested".
//
// Round 3's argument was that journal.Less orders on Lamport first, so an
// attacker never needs Seq, and that Mtime is display-only. That covers
// ORDERING and nothing else. Mtime is also the number the log formatter
// prints and sorts by (DisplayTime/SortForDisplay), and it sits next to the
// size+mtime fingerprint that decides "never clobber dirty files". Seq is a
// signed int64 a peer chooses freely.
//
// These tests come at both from the receiving device: a hostile journal
// dropped into a shared file:// remote by hand, then explicit cycle() calls.
// Helpers prefixed secmeta; secjrnBlob/secjrnPush/secjrnOp and the package's
// newDevice/sharedRemote/write/read/cycle are reused.

// secmetaOp builds a peer put op for content already pushed by secjrnBlob.
func secmetaOp(seq int64, p, blob string, size int) journal.Op {
	op := secjrnOp(seq, p, blob, size)
	op.Mtime = time.Now().UTC()
	return op
}

// ---- Op.Mtime: the filesystem, the dirty check, and the audit order ----

// Mtime is a peer's claim about when a file was written. It must not become a
// fact about the victim's filesystem, and it must not disturb the size+mtime
// fingerprint that "materialize never clobbers dirty files" is decided with —
// the fingerprint is written AFTER the write (materializeFile), which is the
// half round 4 did not check.
func TestSec_SyncMeta_ExtremeMtimeNeverReachesTheFilesystemOrTheDirtyCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		when time.Time
	}{
		{"far future", time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)},
		{"max int64 nanoseconds", time.Unix(0, math.MaxInt64).UTC()},
		{"negative unix seconds", time.Unix(-62135596800, 0).UTC()},
		{"min int64 nanoseconds", time.Unix(0, math.MinInt64).UTC()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := sharedRemote(t)
			victim := newDevice(t, "victim", be)

			const content = "peer content"
			blob := secjrnBlob(t, be, content)
			op := secmetaOp(1, "notes/peer.md", blob, len(content))
			op.Mtime = tc.when
			secjrnPush(t, be, "attacker", []journal.Op{op})

			before := time.Now()
			cycle(t, victim)
			if got := read(t, victim.Folder, "notes/peer.md"); got != content {
				t.Fatalf("op did not materialize: %q", got)
			}
			abs := filepath.Join(victim.Folder, "notes", "peer.md")
			fi, err := os.Stat(abs)
			if err != nil {
				t.Fatal(err)
			}
			// The victim wrote this file now. A peer's claimed Mtime must not
			// be what the victim's own filesystem reports: every tool on that
			// machine (make, rsync, backup, the next scan) trusts it.
			if fi.ModTime().Before(before.Add(-time.Hour)) || fi.ModTime().After(time.Now().Add(time.Hour)) {
				t.Errorf("on-disk mtime is %v, not the time the victim wrote the file", fi.ModTime())
			}

			// The fingerprint written after the write has to agree with disk,
			// or the next cycle either re-materializes forever or treats an
			// untouched file as a fresh local edit and re-journals it.
			res := cycle(t, victim)
			if res.LocalOps != 0 {
				t.Errorf("a quiet second cycle journaled %d local op(s): the fingerprint disagrees with disk", res.LocalOps)
			}
			if got := read(t, victim.Folder, "notes/peer.md"); got != content {
				t.Errorf("second cycle changed the file: %q", got)
			}
		})
	}
}

// SortForDisplay orders the log by DisplayTime, which prefers Op.Mtime over
// Op.Time — and Op.Mtime is a peer's unverified claim. A single op stamped in
// the year 9999 therefore sits above every real entry in `bdrive log`, on
// every device, forever. `bdrive log` prints at most -n rows (50 by default),
// so a handful of such ops is all it takes to push the genuine history off the
// operator's screen — the same audit-trail capture the escape-sequence attack
// buys, with no escape sequence.
//
// The secure behavior: an op's displayed time may lag the moment it was
// journaled (a file can carry an old mtime) but it can never lead it. A
// claimed write time in the future of the op's own journal entry is not a
// timestamp, it is a sort key the attacker picked.
func TestSec_SyncMeta_FutureMtimeCannotOutrankRealHistory(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)

	// The victim's own real history.
	for _, rel := range []string{"a.md", "b.md", "c.md"} {
		write(t, victim.Folder, rel, "mine "+rel)
	}
	cycle(t, victim)

	const content = "peer content"
	blob := secjrnBlob(t, be, content)
	var ops []journal.Op
	for i := 1; i <= 3; i++ {
		op := secmetaOp(int64(i), "peer/"+string(rune('a'+i-1))+".md", blob, len(content))
		op.Time = time.Now().UTC().Add(-24 * time.Hour) // journaled yesterday
		op.Mtime = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
		ops = append(ops, op)
	}
	secjrnPush(t, be, "attacker", ops)
	cycle(t, victim)

	entries, err := LogEntries(victim.Store, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	SortForDisplay(entries)

	for _, op := range entries {
		if shown := DisplayTime(op); shown.After(op.Time.Add(time.Hour)) {
			t.Errorf("%s displays as %v, %v after the op was journaled (%v) — a peer chose the sort key",
				op.Path, shown, shown.Sub(op.Time), op.Time)
		}
	}
	// And concretely: the top of the log is not wholly the attacker's.
	top := entries
	if len(top) > 3 {
		top = top[:3]
	}
	all := true
	for _, op := range top {
		if !strings.HasPrefix(op.Path, "peer/") {
			all = false
		}
	}
	if all {
		t.Errorf("the newest 3 log rows are all the attacker's: %v %v %v",
			top[0].Path, top[1].Path, top[2].Path)
	}
}

// ---- Op.Seq: negative, duplicated, saturated ----

// Seq is a signed int64 off the wire. It is the last key of journal.Less and
// it is the per-device counter every device derives its own next Seq from.
// None of these shapes may drop an op, disorder replay, crash the cycle, or
// leak into the victim's own numbering.
func TestSec_SyncMeta_HostileSeqValuesStayInert(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)

	const content = "peer content"
	blob := secjrnBlob(t, be, content)

	// One journal, one device, every pathological Seq — including two ops
	// that share a Seq, which is what a device that rewound its counter
	// produces and what journal.Less has to break a tie on.
	seqs := []int64{-1, 0, math.MinInt64, math.MaxInt64, 7, 7}
	var ops []journal.Op
	for i, s := range seqs {
		op := secmetaOp(int64(i+1), "peer/f"+string(rune('a'+i))+".md", blob, len(content))
		op.Seq = s
		op.Lamport = int64(i + 1)
		ops = append(ops, op)
	}
	secjrnPush(t, be, "attacker", ops)

	if _, err := victim.Cycle(context.Background()); err != nil {
		t.Fatalf("hostile Seq values killed the cycle: %v", err)
	}
	for i := range seqs {
		rel := "peer/f" + string(rune('a'+i)) + ".md"
		if got := read(t, victim.Folder, rel); got != content {
			t.Errorf("%s = %q, want %q — an op was dropped by its Seq", rel, got, content)
		}
	}

	// The victim's own next ops must still be numbered from its own journal,
	// strictly increasing and positive: a peer's counter is not the victim's.
	write(t, victim.Folder, "mine.md", "mine")
	cycle(t, victim)
	write(t, victim.Folder, "mine2.md", "mine again")
	cycle(t, victim)
	mine, err := victim.Store.DeviceOps(victim.Device.ID)
	if err != nil {
		t.Fatal(err)
	}
	var prev int64
	for _, op := range mine {
		if op.Seq <= 0 || op.Seq <= prev {
			t.Fatalf("own op %q has Seq %d after previous %d — a peer's Seq reached this device's counter",
				op.Path, op.Seq, prev)
		}
		prev = op.Seq
	}
}

// Replay has to fold the same ops into the same state no matter what order
// the store happened to hand them back, and duplicate/extreme Seq values are
// exactly the shapes that make a comparator non-total. Same path, same
// lamport, different content: whoever wins must win every time.
func TestSec_SyncMeta_DuplicateSeqKeepsReplayDeterministic(t *testing.T) {
	base := secjrnOp(1, "shared.md", strings.Repeat("a", 64), 4)
	base.Time = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	var ops []journal.Op
	for i, blob := range []string{
		strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64),
	} {
		op := base
		op.Blob = blob
		op.Seq = 7 // identical
		op.Lamport = 5
		op.Note = "variant " + string(rune('a'+i))
		ops = append(ops, op)
	}

	want := journal.Replay(append([]journal.Op(nil), ops...))["shared.md"].Blob
	for _, perm := range [][]int{{0, 1, 2}, {2, 1, 0}, {1, 2, 0}, {0, 2, 1}, {2, 0, 1}, {1, 0, 2}} {
		shuffled := make([]journal.Op, len(perm))
		for i, j := range perm {
			shuffled[i] = ops[j]
		}
		if got := journal.Replay(shuffled)["shared.md"].Blob; got != want {
			t.Fatalf("permutation %v replays to %s, first order replayed to %s", perm, got[:4], want[:4])
		}
	}
}

// A cached fingerprint is the other half of the dirty check. Round 4 proved a
// peer's Op.Size/Op.Mtime cannot get INTO the cache; this asserts the values
// that come OUT of it after a peer op is materialized are the ones the victim
// measured, so a later real local edit is still detected as one.
func TestSec_SyncMeta_MaterializedFingerprintIsMeasuredNotClaimed(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)

	const content = "peer content"
	blob := secjrnBlob(t, be, content)
	op := secmetaOp(1, "notes/peer.md", blob, len(content))
	op.Mtime = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	op.Size = math.MaxInt64
	secjrnPush(t, be, "attacker", []journal.Op{op})
	cycle(t, victim)

	cache, err := victim.Store.LoadCache(victim.mountID())
	if err != nil {
		t.Fatal(err)
	}
	c, ok := cache["notes/peer.md"]
	if !ok {
		t.Fatal("no cache entry for the materialized path")
	}
	abs := filepath.Join(victim.Folder, "notes", "peer.md")
	fi, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	if c.Size != fi.Size() || c.MTimeNS != fi.ModTime().UnixNano() {
		t.Fatalf("cache %+v does not describe the file on disk (size %d, mtime %d)",
			c, fi.Size(), fi.ModTime().UnixNano())
	}

	// The check the fingerprint exists for still works: a real local edit
	// after the peer op must be seen and journaled.
	write(t, victim.Folder, "notes/peer.md", "the victim's own edit")
	res := cycle(t, victim)
	if res.LocalOps == 0 {
		t.Fatal("a real local edit was not detected: the dirty check is blind after a hostile op")
	}
	if got := read(t, victim.Folder, "notes/peer.md"); got != "the victim's own edit" {
		t.Fatalf("the victim's edit was clobbered: %q", got)
	}
}
