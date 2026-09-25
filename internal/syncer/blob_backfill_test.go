package syncer

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/runbear-io/beardrive/internal/remote"
)

// flakyBlobs fails every blob GET while down is set, the way a hub under a
// joining device's burst of downloads, a dropped connection, or a killed
// `bdrive init` loses some of them.
type flakyBlobs struct {
	remote.Backend
	down *atomic.Bool
	gets *atomic.Int64
}

func (f flakyBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if strings.HasPrefix(key, "blobs/") {
		f.gets.Add(1)
		if f.down.Load() {
			return nil, errors.New("connection reset by peer")
		}
	}
	return f.Backend.Get(ctx, key)
}

// A blob that could not be fetched in the cycle that pulled its op must still
// arrive later. The journal is on local disk by the end of that cycle, so the
// pull never yields the op again; the retry has to come from the replayed
// state, or the file never appears on this device.
func TestMissingBlobIsFetchedOnALaterCycle(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	write(t, a.Folder, "one.md", "first")
	write(t, a.Folder, "docs/two.md", "second")
	cycle(t, a)

	var down atomic.Bool
	var gets atomic.Int64
	down.Store(true)
	b := newDevice(t, "devb", flakyBlobs{Backend: be, down: &down, gets: &gets})
	cycle(t, b)
	if _, err := os.Stat(filepath.Join(b.Folder, "one.md")); err == nil {
		t.Fatal("one.md landed while every blob GET was failing")
	}

	down.Store(false)
	cycle(t, b)
	if got := read(t, b.Folder, "one.md"); got != "first" {
		t.Fatalf("one.md = %q, want %q", got, "first")
	}
	if got := read(t, b.Folder, "docs/two.md"); got != "second" {
		t.Fatalf("docs/two.md = %q, want %q", got, "second")
	}

	// Converged: nothing is missing, so a quiet cycle asks the hub for nothing.
	before := gets.Load()
	cycle(t, b)
	if n := gets.Load() - before; n != 0 {
		t.Fatalf("a converged cycle fetched %d blob(s), want 0", n)
	}
}

// Backfill follows the same rules materialize does: a path this device
// filters out is never written, so its content is never downloaded either.
func TestMissingBlobBackfillSkipsFilteredPaths(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	write(t, a.Folder, "keep.md", "kept")
	write(t, a.Folder, "skip/big.bin", "not wanted here")
	cycle(t, a)

	var down atomic.Bool
	var gets atomic.Int64
	down.Store(true)
	b := newDevice(t, "devb", flakyBlobs{Backend: be, down: &down, gets: &gets})
	write(t, b.Folder, IgnoreFile, "skip/\n")
	cycle(t, b)

	down.Store(false)
	cycle(t, b)
	if got := read(t, b.Folder, "keep.md"); got != "kept" {
		t.Fatalf("keep.md = %q, want %q", got, "kept")
	}
	if _, err := os.Stat(filepath.Join(b.Folder, "skip", "big.bin")); err == nil {
		t.Fatal("a filtered path was materialized")
	}
	ops, err := b.Store.AllOps()
	if err != nil {
		t.Fatal(err)
	}
	var skipBlob string
	for _, op := range ops {
		if op.Path == "skip/big.bin" {
			skipBlob = op.Blob
		}
	}
	if skipBlob == "" {
		t.Fatal("devb never pulled the op for skip/big.bin")
	}
	if b.Store.HasBlob(skipBlob) {
		t.Fatal("the blob of a filtered path was downloaded")
	}
}

// One cycle retries at most maxBackfill blobs, so a peer naming content it
// never uploaded costs a bounded number of requests per pass, and everything
// real still lands over a few cycles.
func TestMissingBlobBackfillIsBoundedPerCycle(t *testing.T) {
	prev := maxBackfill
	maxBackfill = 2
	t.Cleanup(func() { maxBackfill = prev })

	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	names := []string{"a.md", "b.md", "c.md", "d.md", "e.md"}
	for _, n := range names {
		write(t, a.Folder, n, "content of "+n)
	}
	cycle(t, a)

	var down atomic.Bool
	var gets atomic.Int64
	down.Store(true)
	b := newDevice(t, "devb", flakyBlobs{Backend: be, down: &down, gets: &gets})
	cycle(t, b)
	down.Store(false)

	present := func() int {
		n := 0
		for _, name := range names {
			if _, err := os.Stat(filepath.Join(b.Folder, name)); err == nil {
				n++
			}
		}
		return n
	}
	for i, want := range []struct {
		present     int
		backfilling bool
	}{{2, true}, {4, true}, {5, false}} {
		before := gets.Load()
		res := cycle(t, b)
		if n := gets.Load() - before; n > int64(maxBackfill) {
			t.Fatalf("cycle %d fetched %d blobs, want at most %d", i+2, n, maxBackfill)
		}
		if got := present(); got != want.present {
			t.Fatalf("after cycle %d: %d of %d files present, want %d", i+2, got, len(names), want.present)
		}
		if res.Backfilling != want.backfilling {
			t.Fatalf("cycle %d: Backfilling = %v, want %v", i+2, res.Backfilling, want.backfilling)
		}
	}
}

// Backfilling asks the daemon for an early remote pass, so it must stay off
// while nothing lands: content the hub cannot serve would otherwise hold a
// device at full cadence forever.
func TestMissingBlobBackfillThatLandsNothingDoesNotHurry(t *testing.T) {
	prev := maxBackfill
	maxBackfill = 1
	t.Cleanup(func() { maxBackfill = prev })

	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	write(t, a.Folder, "p.md", "p")
	write(t, a.Folder, "q.md", "q")
	cycle(t, a)

	var down atomic.Bool
	var gets atomic.Int64
	down.Store(true)
	b := newDevice(t, "devb", flakyBlobs{Backend: be, down: &down, gets: &gets})
	for i := range 3 {
		if res := cycle(t, b); res.Backfilling {
			t.Fatalf("cycle %d: Backfilling with every blob GET failing", i+1)
		}
	}
}

// The byte budget is what bounds a hub serving wrong bytes under large declared
// sizes: past it, the rest waits for a later cycle, though a cycle always takes
// at least one so a single large file still makes progress.
func TestMissingBlobBackfillStaysWithinItsByteBudget(t *testing.T) {
	prev := maxBackfillBytes
	maxBackfillBytes = pullBound(int64(len("content of x.md")))
	t.Cleanup(func() { maxBackfillBytes = prev })

	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	names := []string{"x.md", "y.md", "z.md"}
	for _, n := range names {
		write(t, a.Folder, n, "content of "+n[:1]+".md")
	}
	cycle(t, a)

	var down atomic.Bool
	var gets atomic.Int64
	down.Store(true)
	b := newDevice(t, "devb", flakyBlobs{Backend: be, down: &down, gets: &gets})
	cycle(t, b)
	down.Store(false)

	for i, want := range []int{1, 2, 3} {
		cycle(t, b)
		n := 0
		for _, name := range names {
			if _, err := os.Stat(filepath.Join(b.Folder, name)); err == nil {
				n++
			}
		}
		if n != want {
			t.Fatalf("after cycle %d: %d of %d files present, want %d", i+2, n, len(names), want)
		}
	}
}
