package syncer

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

// fallBehind puts a device in the state BEA-196 was reported from: its stored
// clock sits below the ops already in its OWN volume dir. The journals are
// left exactly as they are — only sync.json is rewound — which is the whole
// point: every peer journal is already fully downloaded, so `pull` returns
// nothing on every later cycle and the only place the clock could come back
// from is the journals on disk.
func fallBehind(t *testing.T, s *Session) {
	t.Helper()
	st, err := s.Store.LoadSync()
	if err != nil {
		t.Fatal(err)
	}
	st.Lamport = 1
	if err := s.Store.SaveSync(st); err != nil {
		t.Fatal(err)
	}
}

func highestLamport(t *testing.T, s *Session) int64 {
	t.Helper()
	ops, err := s.Store.AllOps()
	if err != nil {
		t.Fatal(err)
	}
	var hi int64
	for _, op := range ops {
		if op.Lamport > hi {
			hi = op.Lamport
		}
	}
	return hi
}

// A device whose stored clock has fallen below the project's must converge:
// an edit made on it survives its own sync and becomes the head for everyone.
//
// Before the fix this is the reported data loss exactly — the cycle reports
// `remote: pushed` for the path and, in the SAME run, replay resolves it to
// the peer's older op and materialize writes those bytes back over the file.
func TestBehindClockConverges(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	write(t, a.Folder, "report.md", "A's original")
	cycle(t, a)
	cycle(t, b) // B joins and pulls A's version
	if got := read(t, b.Folder, "report.md"); got != "A's original" {
		t.Fatalf("setup: B has %q", got)
	}
	// A keeps working, so the project's clock climbs well past B's.
	for _, s := range []string{"A v2", "A v3", "A v4"} {
		write(t, a.Folder, "report.md", s)
		cycle(t, a)
	}
	cycle(t, b)

	fallBehind(t, b)

	// B edits the file. This is the user's work.
	write(t, b.Folder, "report.md", "B's edit — the work that must not vanish")
	res := cycle(t, b)

	if got := read(t, b.Folder, "report.md"); got != "B's edit — the work that must not vanish" {
		t.Fatalf("B's own sync reverted B's edit: file now %q (LocalOps=%d PulledOps=%d Materialized=%d)",
			got, res.LocalOps, res.PulledOps, res.Materialized)
	}
	// And the edit has to actually win for everyone, not merely survive locally.
	cycle(t, a)
	if got := read(t, a.Folder, "report.md"); got != "B's edit — the work that must not vanish" {
		t.Fatalf("A converged on %q, want B's edit", got)
	}
}

// The clock has to come back from the journals this device already holds,
// because that is the only source left: every peer journal is fully
// downloaded, so pull returns nothing.
func TestBehindClockIsRederivedFromLocalJournals(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	write(t, a.Folder, "f.txt", "one")
	cycle(t, a)
	write(t, a.Folder, "f.txt", "two")
	cycle(t, a)
	cycle(t, b)

	hi := highestLamport(t, b)
	if hi == 0 {
		t.Fatal("setup: B holds no ops")
	}
	fallBehind(t, b)

	res := cycle(t, b) // nothing to pull, nothing local
	if res.PulledOps != 0 {
		t.Fatalf("setup is not exercising the fix: the cycle pulled %d ops", res.PulledOps)
	}
	st, err := b.Store.LoadSync()
	if err != nil {
		t.Fatal(err)
	}
	if st.Lamport < hi {
		t.Fatalf("clock still behind after a cycle: %d, want >= %d (the highest op in B's own journal dir)", st.Lamport, hi)
	}
}

// forbidNthJournal answers ErrForbidden on the Nth journal GET, so a pull dies
// partway through having already written the journals it did get.
type forbidNthJournal struct {
	remote.Backend
	gets *int
	n    int
}

func (f forbidNthJournal) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if strings.HasPrefix(key, "journal/") {
		*f.gets++
		if *f.gets >= f.n {
			return nil, remote.ErrForbidden
		}
	}
	return f.Backend.Get(ctx, key)
}

// A pull that dies on ErrForbidden has still WRITTEN the peer journals it got
// through, and the arm that handles it commits this device's local ops before
// returning. The absorb used to sit below that return, so those ops were
// journalled under a clock that was already behind the ops on disk — a second,
// independent route into the reported state.
func TestForbiddenPullStillAbsorbsTheClock(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	c := newDevice(t, "devc", be)
	write(t, a.Folder, "a.txt", "from A")
	cycle(t, a)
	write(t, c.Folder, "c.txt", "from C")
	cycle(t, c)

	gets := 0
	b := newDevice(t, "devb", forbidNthJournal{Backend: be, gets: &gets, n: 2})
	res, err := b.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.NoAccess {
		t.Fatalf("setup: the pull did not hit the forbidden GET (%d journal gets)", gets)
	}
	st, err := b.Store.LoadSync()
	if err != nil {
		t.Fatal(err)
	}
	hi := highestLamport(t, b)
	if hi == 0 {
		t.Fatal("setup: the aborted pull wrote no journal to disk")
	}
	if st.Lamport < hi {
		t.Fatalf("clock %d is below the ops the aborted pull already wrote to disk (%d)", st.Lamport, hi)
	}
}

// A local edit that loses replay must never vanish silently, even when the
// cycle pulled nothing — and after the clock fix there is exactly one way left
// to lose: a peer op above maxLamport, which absorbLamport refuses to absorb
// on purpose (it is the cap that stops a hostile peer installing a permanent
// write lock). The op still wins replay, so every local edit to that path is
// reverted, forever, and pull is empty on every cycle after the first.
//
// Before the fix conflictCopies bailed on `len(pulled) == 0`, so the revert
// came with no copy and no message: the reported symptom, reachable by anyone
// who can write one journal line.
func TestLosingEditIsPreservedWithoutAPull(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)
	write(t, a.Folder, "doc.md", "A's version")
	cycle(t, a)
	cycle(t, b)

	// Append an unabsorbable op straight into B's LOCAL copy of A's journal,
	// so replay already knows it and the pull fetches nothing.
	peer := b.Store.JournalPath("deva")
	ops, err := journal.ReadFile(peer)
	if err != nil {
		t.Fatal(err)
	}
	last := ops[len(ops)-1]
	blob, size, err := b.Store.PutBlobBytes([]byte("unabsorbable"))
	if err != nil {
		t.Fatal(err)
	}
	win := last
	win.Seq, win.Lamport = last.Seq+1, maxLamport+1
	win.Blob, win.Size = blob, size
	if err := journal.Append(peer, []journal.Op{win}); err != nil {
		t.Fatal(err)
	}

	write(t, b.Folder, "doc.md", "B's losing edit")
	res := cycle(t, b)

	if res.PulledOps != 0 {
		t.Fatalf("setup is not exercising the no-pull path: pulled %d ops", res.PulledOps)
	}
	if got := read(t, b.Folder, "doc.md"); got == "B's losing edit" {
		t.Fatal("setup: the edit did not lose, so there is nothing to preserve")
	}
	if res.Conflicts == 0 {
		t.Fatal("B's edit lost replay and was reverted with no conflict copy")
	}
	if !conflictCopyHolds(t, b.Folder, "B's losing edit") {
		t.Fatal("no conflict copy holds B's edit")
	}

	// And it must not re-emit one on every later cycle: an op that stays
	// unpushed is re-examined forever, so without a dedupe this litters.
	before := countConflictCopies(t, b.Folder)
	res2 := cycle(t, b)
	if res2.Conflicts != 0 || countConflictCopies(t, b.Folder) != before {
		t.Fatalf("a second cycle minted another copy (Conflicts=%d, files %d -> %d)",
			res2.Conflicts, before, countConflictCopies(t, b.Folder))
	}
}

func conflictCopyHolds(t *testing.T, folder, want string) bool {
	t.Helper()
	ents, _ := os.ReadDir(folder)
	for _, e := range ents {
		if !strings.Contains(e.Name(), ".bdrive-conflict-") {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(folder, e.Name())); err == nil && string(b) == want {
			return true
		}
	}
	return false
}

func countConflictCopies(t *testing.T, folder string) int {
	t.Helper()
	n := 0
	ents, _ := os.ReadDir(folder)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".bdrive-conflict-") {
			n++
		}
	}
	return n
}
