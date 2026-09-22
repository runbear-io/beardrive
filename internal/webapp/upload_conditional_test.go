package webapp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/runbear-io/beardrive/internal/remote"
)

/* A journal append is a read-modify-write, and Put is last-writer-wins.

   Inside one process a mutex is enough, and that is what the hub has always
   used. Across two it is not: both read the same journal, both append their
   own ops, and whichever writes second silently erases the other's. No error
   anywhere — both writes succeeded. That is the lost update the "each device
   writes only its own journal" invariant prevents BETWEEN devices,
   reappearing inside one device id the moment the hub runs more than once
   (docs/hub-load-prd.md Stage 10).

   The fix is a precondition evaluated by the STORE, because that is the only
   party both processes share. These tests drive the retry logic that sits on
   top of it — the part that is ours to get wrong. The GCS adapter underneath
   is a few lines handing a generation to the storage service. */

// condBackend is a store with compare-and-swap, and a hook for injecting the
// interleaving that loses an update.
type condBackend struct {
	mu      sync.Mutex
	data    map[string][]byte
	version map[string]int
	// beforePut runs while the caller holds a version it read, standing in
	// for another process writing in that window.
	beforePut func(key string)
	puts      int
	conflicts int
}

func newCondBackend() *condBackend {
	return &condBackend{data: map[string][]byte{}, version: map[string]int{}}
}

func (b *condBackend) GetVersioned(_ context.Context, key string) (io.ReadCloser, remote.Version, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	body, ok := b.data[key]
	if !ok {
		return nil, remote.VersionAbsent, errNotExist
	}
	return io.NopCloser(bytes.NewReader(body)), remote.Version(strconv.Itoa(b.version[key])), nil
}

func (b *condBackend) PutIf(_ context.Context, key string, r io.Reader, _ int64, expect remote.Version) (remote.Version, error) {
	if b.beforePut != nil {
		hook := b.beforePut
		b.beforePut = nil // once: the interleaving happens, then the retry wins
		hook(key)
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return remote.VersionAbsent, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	cur := remote.VersionAbsent
	if _, ok := b.data[key]; ok {
		cur = remote.Version(strconv.Itoa(b.version[key]))
	}
	if cur != expect {
		b.conflicts++
		return remote.VersionAbsent, remote.ErrVersionMismatch
	}
	b.data[key] = body
	b.version[key]++
	b.puts++
	return remote.Version(strconv.Itoa(b.version[key])), nil
}

// write is another process appending, bypassing our retry loop entirely.
func (b *condBackend) write(key string, body []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data[key] = body
	b.version[key]++
}

func (b *condBackend) get(key string) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data[key]
}

// Wraps os.ErrNotExist because that is what a real backend's "no such object"
// unwraps to, and remote.IsNotExist is what tells "absent" from "broken".
// A fake with its own private sentinel would pass a test the real thing fails.
var errNotExist = fmt.Errorf("no such object: %w", os.ErrNotExist)

// The interface the fallback path needs, so one fake serves both.
func (b *condBackend) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	body, _ := io.ReadAll(r)
	b.write(key, body)
	return nil
}
func (b *condBackend) Get(_ context.Context, key string) (io.ReadCloser, error) {
	body := b.get(key)
	if body == nil {
		return nil, errNotExist
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}
func (b *condBackend) Exists(_ context.Context, key string) (bool, error) {
	return b.get(key) != nil, nil
}
func (b *condBackend) List(context.Context, string) ([]remote.Object, error) { return nil, nil }
func (b *condBackend) Close() error                                          { return nil }

func TestConditionalAppendRetriesInsteadOfLosingAnUpdate(t *testing.T) {
	be := newCondBackend()
	const key = "journal/dev-a.jsonl"
	be.write(key, []byte("first\n"))

	// Another process appends in the window between our read and our write —
	// the interleaving that silently erased an op under last-writer-wins.
	be.beforePut = func(k string) {
		be.write(k, []byte("first\nfrom-the-other-process\n"))
	}

	r := &RemoteSource{}
	if err := r.appendConditional(context.Background(), be, key, []byte("ours\n")); err != nil {
		t.Fatalf("append: %v", err)
	}

	got := string(be.get(key))
	if !strings.Contains(got, "from-the-other-process") {
		t.Fatalf("the other process's append was erased — this is the lost update:\n%s", got)
	}
	if !strings.Contains(got, "ours") {
		t.Fatalf("our own append is missing:\n%s", got)
	}
	if be.conflicts == 0 {
		t.Fatal("no precondition ever failed, so the retry path was never exercised " +
			"and this test proves nothing")
	}
	// Exactly once, not twice: a retry re-appends onto what is there now.
	if n := strings.Count(got, "ours"); n != 1 {
		t.Fatalf("the retry duplicated our append %d times:\n%s", n, got)
	}
}

// A journal that does not exist yet is the other race: two processes both
// creating it. VersionAbsent means "create, and fail if somebody beat me".
func TestConditionalAppendCreatesWithoutClobbering(t *testing.T) {
	be := newCondBackend()
	const key = "journal/dev-new.jsonl"

	be.beforePut = func(k string) { be.write(k, []byte("someone-else-created-it\n")) }

	r := &RemoteSource{}
	if err := r.appendConditional(context.Background(), be, key, []byte("ours\n")); err != nil {
		t.Fatalf("append: %v", err)
	}
	got := string(be.get(key))
	if !strings.Contains(got, "someone-else-created-it") {
		t.Fatalf("creating the journal erased a journal another process had just created:\n%s", got)
	}
	if !strings.Contains(got, "ours") {
		t.Fatalf("our append is missing:\n%s", got)
	}
}

// Losing forever must fail the commit rather than spin: a store that refuses
// every precondition is misbehaving, not busy, and the caller retrying is
// better than the hub looping inside a request.
func TestConditionalAppendGivesUpRatherThanSpinning(t *testing.T) {
	be := &alwaysConflict{}
	r := &RemoteSource{}
	err := r.appendConditional(context.Background(), be, "journal/x.jsonl", []byte("ours\n"))
	if err == nil {
		t.Fatal("an append that never won reported success")
	}
	if !errors.Is(err, remote.ErrVersionMismatch) {
		t.Fatalf("the error lost its cause: %v", err)
	}
	if be.tries < 2 {
		t.Fatalf("gave up after %d attempt(s); a single contended commit should retry", be.tries)
	}
	if be.tries > 10 {
		t.Fatalf("retried %d times — that is a spin, not a retry", be.tries)
	}
}

type alwaysConflict struct{ tries int }

func (a *alwaysConflict) GetVersioned(context.Context, string) (io.ReadCloser, remote.Version, error) {
	return io.NopCloser(strings.NewReader("")), remote.Version("1"), nil
}
func (a *alwaysConflict) PutIf(context.Context, string, io.Reader, int64, remote.Version) (remote.Version, error) {
	a.tries++
	return remote.VersionAbsent, remote.ErrVersionMismatch
}

// And the contrast: a backend WITHOUT the capability keeps the old path
// exactly. That is every self-hosted hub on file:// or S3, and it is correct
// there because such a hub runs as one process.
func TestABackendWithoutCompareAndSwapKeepsTheOldPath(t *testing.T) {
	var be plainBackend
	r := &RemoteSource{Backend: &be}
	if err := r.appendLastWriterWins(context.Background(), "journal/x.jsonl", []byte("ours\n")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if !strings.Contains(string(be.body), "ours") {
		t.Fatal("the fallback path did not write the append")
	}
	if _, ok := any(&be).(remote.ConditionalPutter); ok {
		t.Fatal("this fake is supposed to lack the capability")
	}
}

type plainBackend struct{ body []byte }

func (p *plainBackend) Put(_ context.Context, _ string, r io.Reader, _ int64) error {
	p.body, _ = io.ReadAll(r)
	return nil
}
func (p *plainBackend) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(p.body)), nil
}
func (p *plainBackend) Exists(context.Context, string) (bool, error)          { return p.body != nil, nil }
func (p *plainBackend) List(context.Context, string) ([]remote.Object, error) { return nil, nil }
func (p *plainBackend) Close() error                                          { return nil }
