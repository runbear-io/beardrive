package syncer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/store"
)

var errConnForceClosed = errors.New("http2: client connection force closed via ClientConn.Close")

// pushFaultRemote fails blob uploads the way a large import sees them fail:
// after failAfter successful uploads every further one fails (a connection
// that stays down), or with transient set, each blob's first attempt fails (an
// expired presigned URL, a force-closed connection). Every journal write is
// checked against the blob-before-journal invariant.
type pushFaultRemote struct {
	remote.Backend
	mu        sync.Mutex
	failAfter int // 0: never
	transient bool
	blobPuts  int
	failed    map[string]bool
	violation error
	onFail    func() // called on each persistent blob failure
}

func (f *pushFaultRemote) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	switch {
	case strings.HasPrefix(key, "blobs/"):
		f.mu.Lock()
		if f.transient && !f.failed[key] {
			if f.failed == nil {
				f.failed = map[string]bool{}
			}
			f.failed[key] = true
			f.mu.Unlock()
			return errConnForceClosed
		}
		if f.failAfter > 0 && f.blobPuts >= f.failAfter {
			onFail := f.onFail
			f.mu.Unlock()
			if onFail != nil {
				onFail()
			}
			return errConnForceClosed
		}
		f.blobPuts++
		f.mu.Unlock()
	case strings.HasPrefix(key, "journal/"):
		data, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		f.checkOps(ctx, data)
		r, size = bytes.NewReader(data), int64(len(data))
	}
	return f.Backend.Put(ctx, key, r, size)
}

func (f *pushFaultRemote) AppendJournal(ctx context.Context, req remote.JournalAppendRequest) error {
	ap, ok := f.Backend.(remote.JournalAppender)
	if !ok {
		return remote.ErrJournalAppendUnsupported
	}
	f.checkOps(ctx, req.Data)
	return ap.AppendJournal(ctx, req)
}

func (f *pushFaultRemote) checkOps(ctx context.Context, data []byte) {
	ops, _ := journal.Parse(data)
	for _, op := range ops {
		if op.Kind != journal.KindPut || op.Blob == "" {
			continue
		}
		if ok, err := f.Backend.Exists(ctx, "blobs/"+op.Blob); err != nil || !ok {
			f.mu.Lock()
			if f.violation == nil {
				f.violation = fmt.Errorf("journal names %s (blob %s) before its blob is on the remote", op.Path, op.Blob)
			}
			f.mu.Unlock()
		}
	}
}

func (f *pushFaultRemote) set(failAfter int, transient bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failAfter, f.transient, f.blobPuts = failAfter, transient, 0
}

func smallPushBatches(t *testing.T, blobs int) {
	t.Helper()
	prevBlobs, prevDelay := pushBatchBlobs, pushRetryDelay
	pushBatchBlobs, pushRetryDelay = blobs, time.Millisecond
	t.Cleanup(func() { pushBatchBlobs, pushRetryDelay = prevBlobs, prevDelay })
}

func countSynced(t *testing.T, folder string, n int) int {
	t.Helper()
	got := 0
	for i := range n {
		rel := fmt.Sprintf("import/f%02d.md", i)
		b, err := os.ReadFile(filepath.Join(folder, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		if want := fmt.Sprintf("content %d", i); string(b) != want {
			t.Fatalf("%s = %q, want %q", rel, b, want)
		}
		got++
	}
	return got
}

// A push that fails partway through a large import keeps the batches it
// committed: their ops are on the hub (peers see those files), and the next
// cycle uploads only what is left.
func testPartialPushCommitsBatches(t *testing.T, writer, peer remote.Backend) {
	smallPushBatches(t, 3)
	const files = 10
	fault := &pushFaultRemote{Backend: writer, failAfter: 5}
	a := newDevice(t, "deva", fault)
	var savedMidCycle atomic.Int64
	fault.onFail = func() {
		if st, err := a.Store.LoadSync(); err == nil {
			savedMidCycle.Store(st.PushedOps)
		}
	}
	for i := range files {
		write(t, a.Folder, fmt.Sprintf("import/f%02d.md", i), fmt.Sprintf("content %d", i))
	}
	res, err := a.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Offline {
		t.Fatal("cycle succeeded while blob uploads were failing")
	}
	if got := savedMidCycle.Load(); got != 3 {
		t.Fatalf("pushed-op cursor on disk while the push was still running = %d, want 3 (the committed batch)", got)
	}

	b := newDevice(t, "devb", peer)
	cycle(t, b)
	if got := countSynced(t, b.Folder, files); got != 3 {
		t.Fatalf("peer has %d of the files after the failed push, want the first batch of 3", got)
	}

	fault.set(0, false)
	cycle(t, a)
	if fault.blobPuts != files-3 {
		t.Fatalf("retry uploaded %d blobs, want %d (committed batches are not re-sent)", fault.blobPuts, files-3)
	}
	cycle(t, b)
	if got := countSynced(t, b.Folder, files); got != files {
		t.Fatalf("peer has %d of %d files after the retry", got, files)
	}
	if fault.violation != nil {
		t.Fatal(fault.violation)
	}
}

func TestPartialPushCommitsBatches(t *testing.T) {
	be := sharedRemote(t)
	testPartialPushCommitsBatches(t, be, be)
}

// The same through a hub, where every batch after the first is a journal
// append.
func TestPartialPushCommitsBatchesThroughHub(t *testing.T) {
	storage := sharedRemote(t)
	ts, p := newHub(t, storage, true)
	viaServer, err := remote.Open(context.Background(), ts.URL+"/p/"+p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer viaServer.Close()
	testPartialPushCommitsBatches(t, viaServer, remote.Prefixed(storage, p.ID))
}

// An upload that fails once (expired presigned URL, connection force-closed
// under it) is retried within the cycle instead of failing the push.
func TestPushRetriesTransientUploadFailure(t *testing.T) {
	smallPushBatches(t, 4)
	be := sharedRemote(t)
	fault := &pushFaultRemote{Backend: be, transient: true}
	a := newDevice(t, "deva", fault)
	const files = 10
	for i := range files {
		write(t, a.Folder, fmt.Sprintf("import/f%02d.md", i), fmt.Sprintf("content %d", i))
	}
	cycle(t, a)

	b := newDevice(t, "devb", be)
	cycle(t, b)
	if got := countSynced(t, b.Folder, files); got != files {
		t.Fatalf("peer has %d of %d files", got, files)
	}
	if fault.violation != nil {
		t.Fatal(fault.violation)
	}
}

// A run that published batches and exited before recording them leaves the
// hub holding more of this device's journal than the cursor says. The next
// run's shorter publish is refused as dropping ops; it must adopt the hub's
// length and finish instead of retrying the same refused batch forever.
func TestPushResumesAfterExitWithUnrecordedBatches(t *testing.T) {
	smallPushBatches(t, 2)
	storage := sharedRemote(t)
	ts, p := newHub(t, storage, true)
	viaServer, err := remote.Open(context.Background(), ts.URL+"/p/"+p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer viaServer.Close()

	const files = 10
	fault := &pushFaultRemote{Backend: viaServer, failAfter: 7}
	a := newDevice(t, "deva", fault)
	for i := range files {
		write(t, a.Folder, fmt.Sprintf("import/f%02d.md", i), fmt.Sprintf("content %d", i))
	}
	if res, err := a.Cycle(context.Background()); err != nil || !res.Offline {
		t.Fatalf("first cycle: res=%+v err=%v, want offline", res, err)
	}
	st, err := a.Store.LoadSync()
	if err != nil {
		t.Fatal(err)
	}
	if st.PushedOps != 6 {
		t.Fatalf("PushedOps = %d after three committed batches, want 6", st.PushedOps)
	}
	// The process exited after the first batch was recorded; the next two
	// reached the hub but their cursor did not.
	st.PushedOps = 2
	if err := a.Store.SaveSync(st); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(a.Store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	fault.set(0, false)
	a2 := &Session{Folder: a.Folder, Store: reopened, Device: a.Device, Backend: fault}
	cycle(t, a2)
	cycle(t, a2)

	b := newDevice(t, "devb", remote.Prefixed(storage, p.ID))
	cycle(t, b)
	if got := countSynced(t, b.Folder, files); got != files {
		t.Fatalf("peer has %d of %d files", got, files)
	}
	if fault.violation != nil {
		t.Fatal(fault.violation)
	}
}
