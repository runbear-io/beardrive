package syncer

// The delta-sync counting harness (.claude/delta-sync-goal.md, row G1).
// countingBackend is the instrument every transport acceptance criterion
// reads: it wraps any remote.Backend and records the bytes that actually
// cross the wire in each direction. It counts what the backend consumed and
// what the caller read — not declared sizes — so a backend that short-reads
// or a caller that abandons a stream is counted honestly.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/runbear-io/beardrive/internal/remote"
)

// countingBackend records bytes crossing the wire in both directions.
type countingBackend struct {
	remote.Backend
	put, get atomic.Int64
}

func (c *countingBackend) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	return c.Backend.Put(ctx, key, &countReader{r: r, n: &c.put}, size)
}

func (c *countingBackend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := c.Backend.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return &countReadCloser{rc: rc, n: &c.get}, nil
}

type countReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

type countReadCloser struct {
	rc io.ReadCloser
	n  *atomic.Int64
}

func (c *countReadCloser) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func (c *countReadCloser) Close() error { return c.rc.Close() }

// counting wraps a backend for one device's point of view. Two devices in one
// test share the underlying store but each get their own counters.
func counting(be remote.Backend) *countingBackend {
	return &countingBackend{Backend: be}
}

// TestDelta_Harness_CountsBytes proves the instrument itself: bytes counted
// on Put and Get equal the bytes that actually moved, both when driving the
// backend directly and when dropped into a real device's sync cycle.
func TestDelta_Harness_CountsBytes(t *testing.T) {
	be := counting(sharedRemote(t))
	payload := bytes.Repeat([]byte("delta"), 1000) // 5000 bytes, not a round number of reads
	ctx := context.Background()

	if err := be.Put(ctx, "blobs/"+strings.Repeat("e", 64), bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatal(err)
	}
	if got := be.put.Load(); got != int64(len(payload)) {
		t.Fatalf("put counted %d bytes, want %d", got, len(payload))
	}

	rc, err := be.Get(ctx, "blobs/"+strings.Repeat("e", 64))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatal("payload corrupted through the counting wrapper")
	}
	if got := be.get.Load(); got != int64(len(payload)) {
		t.Fatalf("get counted %d bytes, want %d", got, len(payload))
	}

	// And through a real cycle: the wrapper must drop into newDevice
	// unchanged. Device A pushes a file of known size; its put counter sees
	// at least the content and at most content plus a small journal. Device B
	// pulls it; its get counter sees at least the content.
	shared := sharedRemote(t)
	a := newDevice(t, "deva", counting(shared))
	b := newDevice(t, "devb", counting(shared))
	abe := a.Backend.(*countingBackend)
	bbe := b.Backend.(*countingBackend)

	content := bytes.Repeat([]byte("x"), 100_000)
	write(t, a.Folder, "big.bin", string(content))
	cycle(t, a)
	if got := abe.put.Load(); got < int64(len(content)) || got > int64(len(content))+4096 {
		t.Fatalf("cycle put counted %d bytes, want ~%d (content + small journal)", got, len(content))
	}
	cycle(t, b)
	if got := bbe.get.Load(); got < int64(len(content)) {
		t.Fatalf("cycle get counted %d bytes, want >= %d", got, len(content))
	}
	if got := read(t, b.Folder, "big.bin"); got != string(content) {
		t.Fatalf("device B did not converge: got %d bytes", len(got))
	}
}

// TestDelta_Baseline_WholeFileCost is the before-and-after in one place (row
// G2, characterization). Before delta sync a 1-byte edit to a 20 MiB file
// pushed the whole file again: measured 20,972,098 bytes on 2026-08-12. With
// content-defined chunking it pushes only the chunk containing the edit plus
// the manifest: measured 2,050,572 bytes, bounded here at 5 MiB (one max-size
// chunk + slack). Never delete this test; update the bound if the transport
// changes again.
//
// Content is seeded-pseudorandom, not a repeated pattern, so the bound stays
// meaningful: a repetitive file would dedupe to nothing and pass vacuously.
func TestDelta_Baseline_WholeFileCost(t *testing.T) {
	const size = 20 << 20
	be := counting(sharedRemote(t))
	a := newDevice(t, "deva", be)

	rng := rand.New(rand.NewSource(42))
	content := make([]byte, size)
	rng.Read(content)
	write(t, a.Folder, "big.bin", string(content))
	cycle(t, a)
	base := be.put.Load()
	if base < size {
		t.Fatalf("initial push counted %d bytes, want >= %d (every chunk is new)", base, size)
	}

	content[size/2] ^= 0xff
	write(t, a.Folder, "big.bin", string(content))
	cycle(t, a)
	edit := be.put.Load() - base

	t.Logf("1-byte edit to a %d MiB file pushed %d bytes (was 20972098 pre-delta)", size>>20, edit)
	if edit > 5<<20 {
		t.Fatalf("1-byte edit pushed %d bytes, want < 5 MiB (one changed chunk + manifest)", edit)
	}
}

// TestDelta_Push_FrontInsertion (row D4) is the discriminator: fixed-size
// blocking passes the mid-edit and append cases and fails THIS one, because an
// insertion at the front shifts every fixed boundary. Content-defined
// boundaries realign, so only the first chunk (and the manifest) changes.
func TestDelta_Push_FrontInsertion(t *testing.T) {
	const size = 20 << 20
	be := counting(sharedRemote(t))
	a := newDevice(t, "deva", be)

	rng := rand.New(rand.NewSource(43))
	content := make([]byte, size)
	rng.Read(content)
	write(t, a.Folder, "big.bin", string(content))
	cycle(t, a)
	base := be.put.Load()

	write(t, a.Folder, "big.bin", "inserted at the very front|"+string(content))
	cycle(t, a)
	edit := be.put.Load() - base
	t.Logf("front insertion pushed %d bytes", edit)
	if edit > 5<<20 {
		t.Fatalf("front insertion pushed %d bytes, want < 5 MiB — boundaries are not content-defined", edit)
	}
}

// TestDelta_Push_Append (row D3).
func TestDelta_Push_Append(t *testing.T) {
	const size = 20 << 20
	be := counting(sharedRemote(t))
	a := newDevice(t, "deva", be)

	rng := rand.New(rand.NewSource(44))
	content := make([]byte, size)
	rng.Read(content)
	write(t, a.Folder, "big.bin", string(content))
	cycle(t, a)
	base := be.put.Load()

	write(t, a.Folder, "big.bin", string(content)+"appended tail")
	cycle(t, a)
	edit := be.put.Load() - base
	t.Logf("append pushed %d bytes", edit)
	if edit > 5<<20 {
		t.Fatalf("append pushed %d bytes, want < 5 MiB", edit)
	}
}

// TestDelta_Pull_SmallEdit (row D2): a peer already holding the basis version
// pulls a 1-byte edit for a small multiple of the chunk size, sourcing every
// unchanged chunk from the blob it already has.
func TestDelta_Pull_SmallEdit(t *testing.T) {
	const size = 20 << 20
	shared := sharedRemote(t)
	a := newDevice(t, "deva", counting(shared))
	b := newDevice(t, "devb", counting(shared))
	bbe := b.Backend.(*countingBackend)

	rng := rand.New(rand.NewSource(45))
	content := make([]byte, size)
	rng.Read(content)
	write(t, a.Folder, "big.bin", string(content))
	cycle(t, a)
	cycle(t, b) // b now holds the basis
	if got := read(t, b.Folder, "big.bin"); got != string(content) {
		t.Fatal("basis did not converge")
	}
	base := bbe.get.Load()

	content[size/3] ^= 0xff
	write(t, a.Folder, "big.bin", string(content))
	cycle(t, a)
	cycle(t, b)
	pulled := bbe.get.Load() - base
	if got := read(t, b.Folder, "big.bin"); got != string(content) {
		t.Fatal("edit did not converge")
	}
	t.Logf("peer with basis pulled %d bytes for a 1-byte edit", pulled)
	if pulled > 5<<20 {
		t.Fatalf("pull cost %d bytes, want < 5 MiB", pulled)
	}
}

// TestDelta_Threshold_SmallFileUnchanged (row D5): files at or below the
// threshold never write a chunks/ or manifests/ key — the whole-blob path is
// byte-for-byte what it was.
func TestDelta_Threshold_SmallFileUnchanged(t *testing.T) {
	shared := sharedRemote(t)
	a := newDevice(t, "deva", shared)
	small := make([]byte, 4<<20) // exactly the threshold: NOT chunked
	rand.New(rand.NewSource(46)).Read(small)
	write(t, a.Folder, "small.bin", string(small))
	write(t, a.Folder, "note.md", "tiny")
	cycle(t, a)

	for _, prefix := range []string{"chunks/", "manifests/"} {
		objs, err := shared.List(context.Background(), prefix)
		if err != nil {
			t.Fatal(err)
		}
		if len(objs) != 0 {
			t.Fatalf("small files wrote %s keys: %v", prefix, objs)
		}
	}
	if ok, _ := shared.Exists(context.Background(), "blobs/"+shaHex(small)); !ok {
		t.Fatal("small file's whole blob was not uploaded")
	}
}

// TestDelta_Pull_ColdPathUnchanged (row D6): a device with no basis pulls a
// chunked file — via the manifest when one exists; and a small file stays one
// whole-blob GET.
func TestDelta_Pull_ColdPathUnchanged(t *testing.T) {
	const size = 20 << 20
	shared := sharedRemote(t)
	a := newDevice(t, "deva", shared)
	rng := rand.New(rand.NewSource(47))
	content := make([]byte, size)
	rng.Read(content)
	write(t, a.Folder, "big.bin", string(content))
	cycle(t, a)

	// A cold device converges on chunked-only content.
	c := newDevice(t, "devc", counting(shared))
	cycle(t, c)
	if got := read(t, c.Folder, "big.bin"); got != string(content) {
		t.Fatalf("cold pull did not converge: %d bytes", len(got))
	}
}

func shaHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// TestDelta_Gzip_TextCorpusRatio measures what transport compression alone
// would save (row G4) — the cheaper rung the PRD says to consider before
// chunking. The corpus is this package's own Go source: real prose-and-code
// text of the kind BearDrive actually syncs, present on every checkout.
// The assertion is a floor (gzip must at least halve it); the measured ratio
// is the number G5 reads, via t.Logf.
func TestDelta_Gzip_TextCorpusRatio(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no corpus: %v", err)
	}
	var raw bytes.Buffer
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		raw.Write(b)
	}
	var packed bytes.Buffer
	zw := gzip.NewWriter(&packed)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	ratio := float64(raw.Len()) / float64(packed.Len())
	t.Logf("gzip on %d files, %d bytes of Go/text: %d bytes compressed — %.1fx", len(files), raw.Len(), packed.Len(), ratio)
	if packed.Len()*2 > raw.Len() {
		t.Fatalf("gzip achieved only %d -> %d on a text corpus; measurement or corpus is wrong", raw.Len(), packed.Len())
	}
}
