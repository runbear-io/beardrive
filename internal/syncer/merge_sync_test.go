package syncer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/store"
)

// The point of the merge, at the level that matters: two real devices, one
// file, edits in different places, no hand-merging afterwards.
func TestTwoDevicesEditingDifferentPartsMergeInsteadOfForking(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "alice", be)
	b := newDevice(t, "bob", be)

	write(t, a.Folder, "doc.md", "# Title\n\nintro\n\nbody\n\noutro\n")
	cycle(t, a)
	cycle(t, b) // bob now has the same file

	// Both edit, neither has synced: a genuine concurrent edit.
	write(t, a.Folder, "doc.md", "# TITLE FROM ALICE\n\nintro\n\nbody\n\noutro\n")
	write(t, b.Folder, "doc.md", "# Title\n\nintro\n\nbody\n\nOUTRO FROM BOB\n")

	cycle(t, a) // alice pushes first
	res := cycle(t, b)

	got := read(t, b.Folder, "doc.md")
	if !strings.Contains(got, "TITLE FROM ALICE") || !strings.Contains(got, "OUTRO FROM BOB") {
		t.Fatalf("merged file lost an edit:\n%s", got)
	}
	if res.Conflicts != 0 {
		t.Errorf("Conflicts = %d, want 0 for a clean merge", res.Conflicts)
	}
	if names := conflictCopiesIn(t, b.Folder); len(names) != 0 {
		t.Errorf("clean merge still produced conflict copies: %v", names)
	}

	// And it converges: alice pulls the merge rather than keeping her own.
	cycle(t, a)
	if a := read(t, a.Folder, "doc.md"); a != got {
		t.Fatalf("devices diverged:\n alice: %q\n bob:   %q", a, got)
	}
}

// Overlapping edits are NOT merged. A machine guessing between two rewrites of
// the same sentence is how people lose work quietly, so this keeps the
// conflict copy exactly as before.
func TestOverlappingEditsStillMakeAConflictCopy(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "alice", be)
	b := newDevice(t, "bob", be)

	write(t, a.Folder, "doc.md", "one\ntwo\nthree\n")
	cycle(t, a)
	cycle(t, b)

	write(t, a.Folder, "doc.md", "one\nALICE\nthree\n")
	write(t, b.Folder, "doc.md", "one\nBOB\nthree\n")

	cycle(t, a)
	res := cycle(t, b)

	if res.Conflicts == 0 {
		t.Fatal("two rewrites of the same line must not be merged")
	}
	if names := conflictCopiesIn(t, b.Folder); len(names) == 0 {
		t.Fatal("no conflict copy was preserved")
	}
}

// Binary content is never merged: line-merging bytes that are not lines
// produces a file that is neither side's, which for a binary format means a
// corrupt one.
func TestBinaryConflictsAreNotMerged(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "alice", be)
	b := newDevice(t, "bob", be)

	base := string([]byte{0x89, 'P', 'N', 'G', 0x00, 0x01, 0x02, '\n'})
	write(t, a.Folder, "img.bin", base)
	cycle(t, a)
	cycle(t, b)

	write(t, a.Folder, "img.bin", base+string([]byte{0xAA, 0x00}))
	write(t, b.Folder, "img.bin", base+string([]byte{0xBB, 0x00}))
	cycle(t, a)
	res := cycle(t, b)

	if res.Conflicts == 0 {
		t.Fatal("binary files must fall back to a conflict copy")
	}
}

func conflictCopiesIn(t *testing.T, folder string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(folder, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.Contains(info.Name(), ".bdrive-conflict-") {
			out = append(out, info.Name())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// An empty save that races a real edit never wins the path and never forks:
// the edit stays the file, and nothing is parked beside it. Bob rewrites the
// line the empty save removed, so the two cannot merge line by line.
func TestAnEmptyWriteNeverBeatsContent(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "alice", be)
	b := newDevice(t, "bob", be)

	write(t, a.Folder, "notes.md", "# Daily\n")
	cycle(t, a)
	cycle(t, b)

	write(t, b.Folder, "notes.md", "# Daily 2026-11-05\n\n- bob: shipped it\n")
	write(t, a.Folder, "notes.md", "")
	cycle(t, b)
	cycle(t, a)
	cycle(t, b)
	cycle(t, a)

	for _, d := range []struct {
		name, folder string
	}{{"alice", a.Folder}, {"bob", b.Folder}} {
		if got := read(t, d.folder, "notes.md"); !strings.Contains(got, "bob: shipped it") {
			t.Errorf("%s: notes.md = %q, want bob's edit", d.name, got)
		}
		if names := conflictCopiesIn(t, d.folder); len(names) != 0 {
			t.Errorf("%s: conflict copies %v for an empty write", d.name, names)
		}
	}
}

// The other order: the empty write lands first, the edit arrives later.
func TestContentReplacesAnEmptyWinner(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "alice", be)
	b := newDevice(t, "bob", be)

	write(t, a.Folder, "notes.md", "# Daily\n")
	cycle(t, a)
	cycle(t, b)

	write(t, a.Folder, "notes.md", "")
	write(t, b.Folder, "notes.md", "# Daily 2026-11-05\n\n- bob: shipped it\n")
	cycle(t, a)
	cycle(t, b)
	cycle(t, a)

	for _, d := range []struct {
		name, folder string
	}{{"alice", a.Folder}, {"bob", b.Folder}} {
		if got := read(t, d.folder, "notes.md"); !strings.Contains(got, "bob: shipped it") {
			t.Errorf("%s: notes.md = %q, want bob's edit", d.name, got)
		}
		if names := conflictCopiesIn(t, d.folder); len(names) != 0 {
			t.Errorf("%s: conflict copies %v for an empty write", d.name, names)
		}
	}
}

// Empty means the empty blob, not a Size a journal claims: a peer can write
// any Size beside any blob. Content declared as size zero is still content,
// whether it won the path or lost it.
func TestAClaimedZeroSizeIsNotAnEmptyFile(t *testing.T) {
	s := newDevice(t, "alice", sharedRemote(t))
	blob := func(text string) string {
		sum, _, err := s.Store.PutBlobReader(strings.NewReader(text))
		if err != nil {
			t.Fatal(err)
		}
		return sum
	}
	at := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name                       string
		mineLamport, theirsLamport int64
	}{
		{"winner claims zero", 3, 5},
		{"loser claims zero", 5, 3},
	} {
		mine := journal.Op{Seq: 1, Lamport: c.mineLamport, Time: at, Device: "alice", DeviceName: "alice",
			Kind: journal.KindPut, Path: "notes.md", Blob: blob("alice's notes\n"), Size: 14}
		theirs := journal.Op{Seq: 1, Lamport: c.theirsLamport, Time: at, Device: "bob", DeviceName: "bob",
			Kind: journal.KindPut, Path: "notes.md", Blob: blob("bob's notes\n"), Size: 0}
		all := []journal.Op{mine, theirs}
		st := &store.SyncState{}
		out, err := s.conflictCopiesWithState([]journal.Op{mine}, 0, []journal.Op{theirs}, st, all, journal.Replay(all))
		if err != nil {
			t.Fatal(err)
		}
		var copies, rewrites int
		for _, op := range out {
			switch {
			case strings.Contains(op.Path, ".bdrive-conflict-"):
				copies++
			case op.Path == "notes.md":
				rewrites++
			}
		}
		if copies != 1 || rewrites != 0 {
			t.Errorf("%s: %d conflict copies and %d rewrites of the path, want the loser preserved beside it", c.name, copies, rewrites)
		}
	}
}
