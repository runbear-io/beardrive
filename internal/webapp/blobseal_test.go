package webapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/remote"
)

// RemoteSource.verify re-hashes a stored blob on every read on a presigning
// hub, because a presigned URL is replayable for its whole TTL
// (TestSec_Blob_AVerifiedBlobIsRecheckedWhenTheStoredObjectChanges). That cost
// every S3/GCS hub 2x egress and a full-object hash before the reader's first
// byte, forever — even though the URL stops being replayable at mint+TTL, and
// neither presign door will mint a new one for a key that exists.
//
// These tests pin both halves of the boundary: the check stops once the object
// is provably immutable, and NOT before.

// sealBackend is a storage backend that can presign (so verify runs at all)
// and counts the reads the hub makes through it.
type sealBackend struct {
	remote.Backend
	mu   sync.Mutex
	gets int
}

func (b *sealBackend) SignPut(context.Context, string, int64, time.Duration) (*remote.SignedPut, error) {
	return &remote.SignedPut{URL: "https://storage.invalid/x"}, nil
}

func (b *sealBackend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	b.mu.Lock()
	b.gets++
	b.mu.Unlock()
	return b.Backend.Get(ctx, key)
}

func (b *sealBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.gets
}

// sealSource builds a RemoteSource over a counting, signing file:// backend,
// and hands back the directory behind it so a test can age what it stores.
func sealSource(t testing.TB, ttl time.Duration) (*RemoteSource, *sealBackend, string) {
	t.Helper()
	dir := t.TempDir()
	be, err := remote.Open(context.Background(), "file://"+dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { be.Close() })
	c := &sealBackend{Backend: be}
	return &RemoteSource{Backend: c, PresignTTL: ttl}, c, dir
}

// sealAge backdates everything the store holds, so the hub reads the object as
// d old. Since the skew margin is an absolute allowance rather than a multiple
// of the TTL, a short TTL no longer stands in for an old object — the object
// has to actually be old. Walks rather than guessing the key layout.
func sealAge(t testing.TB, dir string, d time.Duration) {
	t.Helper()
	when := time.Now().Add(-d)
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		return os.Chtimes(p, when, when)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// sealPut stores content under its own address and returns the sha.
func sealPut(t testing.TB, be remote.Backend, content string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	sha := hex.EncodeToString(sum[:])
	if err := be.Put(context.Background(), "blobs/"+sha, strings.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	return sha
}

func sealRead(t testing.TB, src *RemoteSource, sha string) string {
	t.Helper()
	rc, err := src.OpenBlob(context.Background(), sha)
	if err != nil {
		t.Fatalf("open blob: %v", err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Past the presign TTL nobody but the hub can write the object any more, so
// the verification is cached and a read costs exactly one storage read — the
// point of the whole change.
func TestBlobVerificationStopsOnceTheObjectCannotChange(t *testing.T) {
	src, be, dir := sealSource(t, time.Minute)
	sha := sealPut(t, be, "reviewed content")
	sealAge(t, dir, time.Hour+10*time.Minute) // past the TTL and the skew margin

	if got := sealRead(t, src, sha); got != "reviewed content" {
		t.Fatalf("first read returned %q", got)
	}
	first := be.count()
	if first < 2 {
		t.Fatalf("the first read did %d storage reads, want at least 2 (verify + stream)", first)
	}

	if got := sealRead(t, src, sha); got != "reviewed content" {
		t.Fatalf("second read returned %q", got)
	}
	if n := be.count() - first; n != 1 {
		t.Errorf("a read of a sealed blob cost %d storage reads, want 1 (the stream): "+
			"the verification is not being cached once the object is provably immutable", n)
	}
}

// While a presigned URL for the key could still be live, the object is still
// writable by whoever holds it, so every read must re-hash. This is the
// boundary that keeps round 14's finding fixed; the attack itself is asserted
// by TestSec_Blob_AVerifiedBlobIsRecheckedWhenTheStoredObjectChanges.
func TestBlobVerificationRepeatsWhileAPresignedURLCouldBeLive(t *testing.T) {
	for _, c := range []struct {
		name string
		ttl  time.Duration
		age  time.Duration
	}{
		// Inside the TTL: a minted URL is plainly still live.
		{"younger than the TTL", time.Hour, time.Minute},
		// Past the TTL but inside the skew margin. The object's age comes from
		// the STORE's clock and the comparison runs on the HUB's, so "past the
		// TTL" by a few minutes is not proof that the URL is dead — a hub
		// running ahead of storage would seal a blob still open to a replay.
		{"past the TTL, inside the skew margin", time.Minute, 30 * time.Minute},
	} {
		t.Run(c.name, func(t *testing.T) {
			src, be, dir := sealSource(t, c.ttl)
			sha := sealPut(t, be, "fresh content")
			sealAge(t, dir, c.age)

			sealRead(t, src, sha)
			first := be.count()
			sealRead(t, src, sha)
			if n := be.count() - first; n < 2 {
				t.Errorf("a read of a blob that a live presigned URL could still overwrite cost %d storage reads, "+
					"want at least 2 (re-verify + stream): the verification was cached too early", n)
			}
		})
	}
}

// BenchmarkBlobRead is the before/after for the read cost, in one run: the
// "unsealed" arm IS the old behavior (re-hash on every read), the "sealed" arm
// is the new one. It also reports the storage reads and bytes read per blob
// read, which is what an S3/GCS bill and a reader's time-to-first-byte are
// actually made of — a local file:// stand-in understates the latency and the
// egress but not the operation count.
func BenchmarkBlobRead(b *testing.B) {
	content := strings.Repeat("x", 4<<20) // 4 MiB, a middling document
	for _, c := range []struct {
		name string
		ttl  time.Duration
		age  time.Duration
	}{
		{"unsealed(before)", time.Hour, 0},            // a live URL could still overwrite it
		{"sealed(after)", time.Minute, 2 * time.Hour}, // past the TTL and the margin
	} {
		b.Run(c.name, func(b *testing.B) {
			src, be, dir := sealSource(b, c.ttl)
			sha := sealPut(b, be, content)
			sealAge(b, dir, c.age)
			rc, err := src.OpenBlob(context.Background(), sha) // warm the seal
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, rc)
			rc.Close()
			start := be.count()
			b.SetBytes(int64(len(content)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rc, err := src.OpenBlob(context.Background(), sha)
				if err != nil {
					b.Fatal(err)
				}
				io.Copy(io.Discard, rc)
				rc.Close()
			}
			b.StopTimer()
			b.ReportMetric(float64(be.count()-start)/float64(b.N), "storage-reads/op")
		})
	}
}

// A blob whose stored bytes do not hash to its key is refused every time, and
// never sealed — a failed verification must not become a cached "checked".
func TestBlobThatFailsVerificationIsNeverSealed(t *testing.T) {
	src, be, dir := sealSource(t, time.Minute)
	sha := sealPut(t, be, "honest")
	// Overwrite with something else under the same key, the way a replayed
	// presigned PUT does.
	if err := be.Put(context.Background(), "blobs/"+sha, strings.NewReader("hostile"), 7); err != nil {
		t.Fatal(err)
	}
	// Old enough to seal, so this proves the hash is checked BEFORE the cache
	// is populated rather than the age doing the refusing.
	sealAge(t, dir, time.Hour+10*time.Minute)
	for i := 0; i < 3; i++ {
		if _, err := src.OpenBlob(context.Background(), sha); err == nil {
			t.Fatalf("read %d served content that does not hash to its key", i)
		}
	}
}
