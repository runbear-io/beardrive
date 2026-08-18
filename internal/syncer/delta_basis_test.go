package syncer

// Regressions from the CTO review: the delta basis is only trustworthy when
// its chunks are actually on the remote, and a bad manifest must never deny a
// file whose correct whole blob exists. The assertion device in these tests
// is always a FRESH third party — a peer that already holds the basis can
// source missing chunks locally and mask exactly these bugs.

import (
	"bytes"
	"context"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/runbear-io/beardrive/internal/remote"
)

// TestDelta_Basis_GrownAcrossThresholdUploadsSharedChunks: v1 is below the
// chunking threshold and goes up as a whole blob; v2 grows past it with v1 as
// the local basis. The basis's chunks were NEVER uploaded, so pushChunked
// must not skip them — a fresh device (and the hub, and every read surface)
// has nothing else to assemble from. Before the fix the manifest named
// chunks that did not exist and the file silently never arrived.
func TestDelta_Basis_GrownAcrossThresholdUploadsSharedChunks(t *testing.T) {
	shared := sharedRemote(t)
	a := newDevice(t, "deva", shared)

	rng := rand.New(rand.NewSource(80))
	v1 := make([]byte, 3<<20) // below threshold: whole-blob push, no chunks
	rng.Read(v1)
	write(t, a.Folder, "grow.bin", string(v1))
	cycle(t, a)

	v2 := append(append([]byte{}, v1...), make([]byte, 3<<20)...) // 6 MiB: chunked, basis = v1
	rng.Read(v2[len(v1):])
	write(t, a.Folder, "grow.bin", string(v2))
	cycle(t, a)

	// The fresh device is the assertion.
	b := newDevice(t, "devb", shared)
	cycle(t, b)
	if got := read(t, b.Folder, "grow.bin"); got != string(v2) {
		t.Fatalf("fresh device did not receive the grown file (%d bytes) — basis chunks were skipped without being uploaded", len(got))
	}
}

// TestDelta_Basis_WholeBlobHistoryUploadsAllChunks is the upgrade shape: the
// device's own journal holds a large put whose content went up as a WHOLE
// blob (what a pre-delta binary did), and the first post-upgrade edit chunks
// with that op as basis. The pre-delta remote state is constructed honestly:
// after v1's (current-code, chunked) push, the whole blob is written and the
// chunks/ and manifests/ trees are GENUINELY REMOVED from the file:// remote
// — a directory, so os.RemoveAll is truthful absence, not an empty object
// that Exists still reports true for (the mistake that made an earlier
// version of this test pass with or without the fix).
func TestDelta_Basis_WholeBlobHistoryUploadsAllChunks(t *testing.T) {
	remoteDir := t.TempDir()
	shared, err := remote.Open(context.Background(), "file://"+remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()

	a := newDevice(t, "deva", shared)
	rng := rand.New(rand.NewSource(81))
	v1 := make([]byte, 10<<20)
	rng.Read(v1)
	write(t, a.Folder, "old.bin", string(v1))
	if _, err := a.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Rewrite the remote into the pre-delta shape: whole blob present,
	// chunk/manifest trees gone.
	sha := shaHex(v1)
	f, err := a.Store.OpenBlob(sha)
	if err != nil {
		t.Fatal(err)
	}
	if err := shared.Put(context.Background(), "blobs/"+sha, f, int64(len(v1))); err != nil {
		t.Fatal(err)
	}
	f.Close()
	for _, d := range []string{"chunks", "manifests"} {
		if err := os.RemoveAll(filepath.Join(remoteDir, d)); err != nil {
			t.Fatal(err)
		}
	}
	if ok, _ := shared.Exists(context.Background(), "manifests/"+sha); ok {
		t.Fatal("test setup failed: the basis manifest still exists")
	}

	// The upgraded device edits. Basis = v1 from its own journal; v1 has no
	// stored manifest, so no chunk may be skipped.
	v2 := append([]byte{}, v1...)
	v2[5<<20] ^= 0xff
	write(t, a.Folder, "old.bin", string(v2))
	cycle(t, a)

	b := newDevice(t, "devb", shared)
	cycle(t, b)
	if got := read(t, b.Folder, "old.bin"); got != string(v2) {
		t.Fatalf("fresh device did not receive the post-upgrade edit (%d bytes)", len(got))
	}
}

// TestDelta_Basis_SquattedManifestCannotSkipChunks (CTO H3): the first write
// of a manifest key is unverifiable, so a member can plant one under a
// whole-pushed blob's public sha BEFORE that file ever grows. The planted
// manifest's chunk list is what the pusher trusts — so it must be the STORED
// list that feeds the skip set, never a local re-chunk of the basis: a
// squatted empty manifest then skips nothing, and the file still reaches a
// fresh device.
func TestDelta_Basis_SquattedManifestCannotSkipChunks(t *testing.T) {
	shared := sharedRemote(t)
	a := newDevice(t, "deva", shared)
	rng := rand.New(rand.NewSource(83))
	v1 := make([]byte, 3<<20) // below threshold: whole-blob push, no manifest
	rng.Read(v1)
	write(t, a.Folder, "grow.bin", string(v1))
	cycle(t, a)

	// Mallory squats the basis's manifest slot: empty chunk list, accepted
	// because the key was never written.
	if err := shared.Put(context.Background(), "manifests/"+shaHex(v1),
		bytes.NewReader([]byte(`{"v":1,"size":0,"chunks":[]}`)), 0); err != nil {
		t.Fatal(err)
	}

	// The file grows past the threshold; v1 is the basis.
	v2 := append(append([]byte{}, v1...), make([]byte, 3<<20)...)
	rng.Read(v2[len(v1):])
	write(t, a.Folder, "grow.bin", string(v2))
	cycle(t, a)

	b := newDevice(t, "devb", shared)
	cycle(t, b)
	if got := read(t, b.Folder, "grow.bin"); got != string(v2) {
		t.Fatalf("a squatted basis manifest made push skip chunks it never uploaded (%d bytes arrived)", len(got))
	}
}

// TestDelta_Basis_TruthfulSquatCannotSkipChunks (CTO H6, the third recurrence
// of the same root cause): a member who can READ the file can publish its
// true chunk hashes without uploading a byte — so a manifest that is
// hash-accurate is still not proof of upload. The pusher's only valid skip
// proof is asking the remote per chunk; with it, the squat costs the
// attacker nothing and gains them nothing.
func TestDelta_Basis_TruthfulSquatCannotSkipChunks(t *testing.T) {
	shared := sharedRemote(t)
	a := newDevice(t, "deva", shared)
	rng := rand.New(rand.NewSource(85))
	v1 := make([]byte, 3<<20) // below threshold: whole-blob push, empty manifest slot
	rng.Read(v1)
	write(t, a.Folder, "grow.bin", string(v1))
	cycle(t, a)

	// Mallory chunks the bytes she can read and publishes the REAL hashes —
	// uploading no chunks at all.
	spans, err := a.chunkSpans(shaHex(v1))
	if err != nil {
		t.Fatal(err)
	}
	man := manifest{V: 1}
	for _, sp := range spans {
		man.Size += sp.n
		man.Chunks = append(man.Chunks, chunkRef{H: sp.sha, N: sp.n})
	}
	mb, _ := json.Marshal(man)
	if err := shared.Put(context.Background(), "manifests/"+shaHex(v1), bytes.NewReader(mb), int64(len(mb))); err != nil {
		t.Fatal(err)
	}

	v2 := append(append([]byte{}, v1...), make([]byte, 3<<20)...)
	rng.Read(v2[len(v1):])
	write(t, a.Folder, "grow.bin", string(v2))
	cycle(t, a)

	b := newDevice(t, "devb", shared)
	cycle(t, b)
	if got := read(t, b.Folder, "grow.bin"); got != string(v2) {
		t.Fatalf("a truthful squatted manifest made push skip chunks that were never uploaded (%d bytes arrived)", len(got))
	}
}

// TestDelta_Pull_BadManifestFallsBackToWholeBlob (CTO H1): a well-formed but
// wrong manifest must not deny a file whose correct whole blob exists — any
// fetchChunked failure falls through to the independently-verified blob path.
func TestDelta_Pull_BadManifestFallsBackToWholeBlob(t *testing.T) {
	shared := sharedRemote(t)
	a := newDevice(t, "deva", shared)
	rng := rand.New(rand.NewSource(82))
	content := make([]byte, 8<<20)
	rng.Read(content)
	write(t, a.Folder, "big.bin", string(content))
	cycle(t, a)
	sha := shaHex(content)

	// Ensure the correct whole blob exists remotely (the hub's backfill
	// produces exactly this state), then corrupt the manifest.
	f, err := a.Store.OpenBlob(sha)
	if err != nil {
		t.Fatal(err)
	}
	if err := shared.Put(context.Background(), "blobs/"+sha, f, int64(len(content))); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := shared.Put(context.Background(), "manifests/"+sha,
		bytes.NewReader([]byte(`{"v":1,"size":1,"chunks":[{"h":"`+shaOfString("nope")+`","n":1}]}`)), 0); err != nil {
		t.Fatal(err)
	}

	b := newDevice(t, "devb", shared)
	cycle(t, b)
	if got := read(t, b.Folder, "big.bin"); got != string(content) {
		t.Fatalf("a bad manifest denied a file whose correct whole blob exists (%d bytes)", len(got))
	}
}

func shaOfString(s string) string { return shaHex([]byte(s)) }

// TestDelta_Push_ManifestRefusalFallsBackToWholeBlob (CTO H4): the manifest
// key is write-once on the hub, so a squatter (or a client with different
// chunker parameters) can make every manifest PUT for a given blob fail
// forever. That refusal must cost one whole-blob upload for that file — not
// the device's entire push leg, which is what a returned error here caused:
// the journal never went up, the same job was rebuilt every cycle, and
// nothing the device ever authored pushed again.
func TestDelta_Push_ManifestRefusalFallsBackToWholeBlob(t *testing.T) {
	shared := sharedRemote(t)
	fb := &failingBackend{Backend: shared, failPrefix: "manifests/"} // refused forever
	a := newDevice(t, "deva", fb)

	rng := rand.New(rand.NewSource(84))
	content := make([]byte, 8<<20)
	rng.Read(content)
	write(t, a.Folder, "big.bin", string(content))
	write(t, a.Folder, "note.md", "the rest of the push leg must survive")
	res, err := a.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Offline {
		t.Fatalf("a refused manifest wedged the push: %v", res.OfflineErr)
	}

	b := newDevice(t, "devb", shared)
	cycle(t, b)
	if got := read(t, b.Folder, "big.bin"); got != string(content) {
		t.Fatalf("whole-blob fallback did not deliver the file (%d bytes)", len(got))
	}
	if got := read(t, b.Folder, "note.md"); got != "the rest of the push leg must survive" {
		t.Fatal("the small file behind the refused manifest never pushed")
	}
}
