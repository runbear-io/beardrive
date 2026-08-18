package syncer

// Row C3: assembled content is verified against Op.Blob before it enters the
// blob store. A manifest is remote content a peer (or hub) wrote; every chunk
// in it can be individually honest while the assembly is not the file the op
// names. The wrong bytes must never land under the op's sha and the path must
// never materialize from them.

import (
	"bytes"
	"context"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

func TestDelta_Pull_RejectsMismatch(t *testing.T) {
	shared := sharedRemote(t)
	a := newDevice(t, "deva", shared)
	b := newDevice(t, "devb", shared)

	rng := rand.New(rand.NewSource(53))
	content := make([]byte, 8<<20)
	rng.Read(content)
	write(t, a.Folder, "big.bin", string(content))
	cycle(t, a)
	sha := shaHex(content)

	// Hostile rewrite: the manifest now lists the first chunk twice. Each
	// chunk is individually valid (it hashes to its key), but the assembly is
	// not the file the op names.
	spans, err := a.chunkSpans(sha)
	if err != nil || len(spans) < 2 {
		t.Fatalf("spans = %d, %v", len(spans), err)
	}
	man := manifest{V: 1, Size: spans[0].n * 2, Chunks: []chunkRef{
		{H: spans[0].sha, N: spans[0].n},
		{H: spans[0].sha, N: spans[0].n},
	}}
	mb, _ := json.Marshal(man)
	if err := shared.Put(context.Background(), "manifests/"+sha, bytes.NewReader(mb), int64(len(mb))); err != nil {
		t.Fatal(err)
	}

	res, err := b.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The contradiction is surfaced (errBlobContent → Offline), the wrong
	// bytes never land under the op's sha, and the path never materializes.
	if !res.Offline {
		t.Fatal("a manifest assembling to the wrong content must surface as Offline")
	}
	if res.OfflineErr == nil {
		t.Fatal("no error recorded for the mismatch")
	}
	if b.Store.HasBlob(sha) {
		t.Fatal("wrong bytes were filed under the op's sha")
	}
	if _, err := os.Stat(filepath.Join(b.Folder, "big.bin")); err == nil {
		t.Fatal("path materialized from a mismatching assembly")
	}
}

// TestDelta_Ceiling_LargeFileMaterializes pins maxPullBytes at its raised
// value: a 40 MiB file — over the old 32 MiB ceiling that silently kept large
// files off every receiving device — must land on a peer. If this fails after
// a bound change, the ceiling regressed below what the product now promises.
func TestDelta_Ceiling_LargeFileMaterializes(t *testing.T) {
	shared := sharedRemote(t)
	a := newDevice(t, "deva", shared)
	b := newDevice(t, "devb", shared)

	content := make([]byte, 40<<20)
	rand.New(rand.NewSource(54)).Read(content)
	write(t, a.Folder, "video.bin", string(content))
	cycle(t, a)
	cycle(t, b)
	if got := read(t, b.Folder, "video.bin"); got != string(content) {
		t.Fatalf("40 MiB file did not materialize on the peer (%d bytes)", len(got))
	}
}
