package webapp

// Delta-sync hub rows (.claude/delta-sync-goal.md). The store's key space
// grows two content-addressed classes: chunks/<sha256> (one content-defined
// chunk) and manifests/<sha256> (the chunk list for the whole file with that
// sha). Both inherit blobs/' properties: immutable, never deleted.

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/remote"
)

// placeChunked hand-builds a chunked-only file in a project's file:// store:
// its chunks and manifest exist, the whole blob does not. Returns the file's
// sha (the blob/manifest key).
func placeChunked(t *testing.T, dir string, parts ...string) string {
	t.Helper()
	whole := strings.Join(parts, "")
	sha := shaOf(whole)
	if err := os.MkdirAll(filepath.Join(dir, "chunks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "manifests"), 0o755); err != nil {
		t.Fatal(err)
	}
	var entries []string
	for _, p := range parts {
		if err := os.WriteFile(filepath.Join(dir, "chunks", shaOf(p)), []byte(p), 0o644); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, fmt.Sprintf(`{"h":%q,"n":%d}`, shaOf(p), len(p)))
	}
	man := fmt.Sprintf(`{"v":1,"size":%d,"chunks":[%s]}`, len(whole), strings.Join(entries, ","))
	if err := os.WriteFile(filepath.Join(dir, "manifests", sha), []byte(man), 0o644); err != nil {
		t.Fatal(err)
	}
	return sha
}

// TestDelta_Hub_BackfillOnce (row H1): a blob that exists only as chunks +
// manifest is served by reassembly, verified, and backfilled to blobs/<sha>
// so the second read hits the whole blob directly.
func TestDelta_Hub_BackfillOnce(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	dir := filepath.Join(root, p.ID)
	sha := placeChunked(t, dir, "first chunk of the file|", "second chunk of the file")
	whole := "first chunk of the file|second chunk of the file"
	h := srv.Handler()
	url := "/api/p/" + p.ID + "/store/object?key=blobs/" + sha

	blobPath := filepath.Join(dir, "blobs", sha)
	if _, err := os.Stat(blobPath); err == nil {
		t.Fatal("whole blob exists before the test ran")
	}
	rec := do(t, h, "GET", url, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != whole {
		t.Fatalf("reassembled read: %d %q, want 200 %q", rec.Code, rec.Body, whole)
	}
	if _, err := os.Stat(blobPath); err != nil {
		t.Fatalf("whole blob not backfilled after reassembly: %v", err)
	}
	// Second read is served from the backfilled blob (and still correct).
	if rec := do(t, h, "GET", url, nil); rec.Code != http.StatusOK || rec.Body.String() != whole {
		t.Fatalf("second read: %d %q", rec.Code, rec.Body)
	}
}

// TestDelta_Manifest_SelfVerifying (row C4): a manifest whose chunks do not
// reassemble to its key is refused, and nothing is backfilled.
func TestDelta_Manifest_SelfVerifying(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	dir := filepath.Join(root, p.ID)
	sha := placeChunked(t, dir, "honest part|", "also honest")
	// Corrupt one chunk in place: the manifest still names it, the bytes are
	// wrong, so reassembly must not hash to the key.
	badChunk := filepath.Join(dir, "chunks", shaOf("honest part|"))
	if err := os.WriteFile(badChunk, []byte("hostile bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	rec := do(t, h, "GET", "/api/p/"+p.ID+"/store/object?key=blobs/"+sha, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("corrupt reassembly served: %d %q, want 404", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(dir, "blobs", sha)); err == nil {
		t.Fatal("corrupt reassembly was backfilled")
	}
}

// TestSec_Chunks_ReassemblyBoundsHostileManifest: a manifest is member-written
// and cannot be hash-checked at ingest, so its declared chunk list is attacker
// input. Listing one real chunk many times must not make the hub spool the
// amplified total (temp-dir/RAM exhaustion per read) — the declared sum is
// refused past maxReassembleBytes before any chunk is fetched. And a stored
// chunk object LONGER than its manifest entry must not sail past the cap: the
// copy is bounded by the declared size and a length mismatch fails the read.
func TestSec_Chunks_ReassemblyBoundsHostileManifest(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	dir := filepath.Join(root, p.ID)
	h := srv.Handler()

	// One real 1 MiB chunk, listed enough times to declare ~300 GiB.
	chunk := strings.Repeat("A", 1<<20)
	sha := placeChunked(t, dir, chunk) // sha of the 1-chunk file; manifest will be replaced
	entry := fmt.Sprintf(`{"h":%q,"n":%d}`, shaOf(chunk), len(chunk))
	entries := make([]string, 300<<10)
	for i := range entries {
		entries[i] = entry
	}
	man := fmt.Sprintf(`{"v":1,"size":%d,"chunks":[%s]}`, int64(len(chunk))*int64(len(entries)), strings.Join(entries, ","))
	if err := os.WriteFile(filepath.Join(dir, "manifests", sha), []byte(man), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := do(t, h, "GET", "/api/p/"+p.ID+"/store/object?key=blobs/"+sha, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("amplified manifest was served: %d (%d bytes)", rec.Code, rec.Body.Len())
	}
	if _, err := os.Stat(filepath.Join(dir, "blobs", sha)); err == nil {
		t.Fatal("amplified manifest was backfilled")
	}

	// A stored chunk longer than its declared size: the manifest says 10
	// bytes, the object holds a megabyte. The copy is bounded and refused.
	longSha := placeChunked(t, dir, "0123456789")
	if err := os.WriteFile(filepath.Join(dir, "chunks", shaOf("0123456789")), []byte(strings.Repeat("B", 1<<20)), 0o644); err != nil {
		t.Fatal(err)
	}
	rec = do(t, h, "GET", "/api/p/"+p.ID+"/store/object?key=blobs/"+longSha, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("oversized chunk object was served: %d", rec.Code)
	}
}

// TestSec_Chunks_ManifestMustNameUploadedChunks (CTO H6): the manifest key
// is the one member-writable object that is not content-addressed, and a
// member who can READ a file can publish its true chunk hashes without
// uploading a byte — poisoning the empty slot under a whole-pushed blob so a
// later honest push skips chunks that do not exist. The ingest door is where
// the invariant is enforced: a manifest is accepted only when the store
// holds every chunk it names. The honest client writes chunks first, so this
// always passes for it.
func TestSec_Chunks_ManifestMustNameUploadedChunks(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/store/"

	// Naming a chunk the store does not hold: refused, nothing stored.
	key := "manifests/" + shaOf("victim file")
	vapor := []byte(`{"v":1,"size":9,"chunks":[{"h":"` + shaOf("not uploaded") + `","n":9}]}`)
	if rec := do(t, h, "PUT", base+"object?key="+key, vapor); rec.Code != http.StatusBadRequest {
		t.Fatalf("manifest naming an absent chunk: %d %s, want 400", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", base+"object?key="+key, nil); rec.Code == http.StatusOK {
		t.Fatal("refused manifest was stored anyway")
	}

	// Upload the chunk, and the same manifest is accepted.
	chunk := []byte("not uploaded")
	if rec := do(t, h, "PUT", base+"object?key=chunks/"+shaOf(string(chunk)), chunk); rec.Code != http.StatusOK {
		t.Fatalf("chunk put: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "PUT", base+"object?key="+key, vapor); rec.Code != http.StatusOK {
		t.Fatalf("manifest with its chunks present: %d %s, want 200", rec.Code, rec.Body)
	}
}

// TestSec_Chunks_ManifestWriteOnce: a manifest is the one member-writable
// object that is neither content-addressed nor hash-checkable at ingest, so
// the hub stores it write-once. Re-putting identical bytes stays a no-op
// (the retry after an interrupted push must work); a DIFFERENT body for an
// existing key is refused — overwriting was the only way a member could
// re-point an existing file's chunk list after the fact.
func TestSec_Chunks_ManifestWriteOnce(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/store/"
	key := "manifests/" + shaOf("some whole file")

	// The chunks a manifest names must exist first (the ingest invariant),
	// so upload them before the manifests that reference them.
	for _, c := range []string{"hello", "evil!"} {
		if rec := do(t, h, "PUT", base+"object?key=chunks/"+shaOf(c), []byte(c)); rec.Code != http.StatusOK {
			t.Fatalf("chunk put: %d %s", rec.Code, rec.Body)
		}
	}
	man := []byte(`{"v":1,"size":5,"chunks":[{"h":"` + shaOf("hello") + `","n":5}]}`)
	if rec := do(t, h, "PUT", base+"object?key="+key, man); rec.Code != http.StatusOK {
		t.Fatalf("first manifest put: %d %s", rec.Code, rec.Body)
	}
	// Identical retry: accepted as a no-op.
	if rec := do(t, h, "PUT", base+"object?key="+key, man); rec.Code != http.StatusOK {
		t.Fatalf("identical manifest retry: %d %s, want 200", rec.Code, rec.Body)
	}
	// A different body for the same key: refused, and the original survives.
	other := []byte(`{"v":1,"size":5,"chunks":[{"h":"` + shaOf("evil!") + `","n":5}]}`)
	if rec := do(t, h, "PUT", base+"object?key="+key, other); rec.Code != http.StatusConflict {
		t.Fatalf("manifest overwrite: %d %s, want 409", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", base+"object?key="+key, nil); rec.Body.String() != string(man) {
		t.Fatalf("stored manifest changed after refused overwrite: %q", rec.Body)
	}
}

// TestDelta_Hub_ChunkPresignRefusesExisting (row H2): chunks presign like
// blobs — content-addressed and immutable — and BOTH presign doors' rule
// applies: a key that already exists is never signed again (the sealing
// invariant in RemoteSource.verify rests on it).
func TestDelta_Hub_ChunkPresignRefusesExisting(t *testing.T) {
	var sb *signingBackend
	srv, p, root := newHub(t, true, func(be remote.Backend) remote.Backend {
		sb = &signingBackend{Backend: be}
		return sb
	})
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/store/"

	// A fresh chunk key signs direct.
	freshKey := "chunks/" + shaOf("fresh chunk")
	rec := do(t, h, "POST", base+"sign", map[string]any{"key": freshKey, "size": 11})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"direct"`) || !strings.Contains(rec.Body.String(), `"url"`) {
		t.Fatalf("sign fresh chunk: %d %s, want direct with url", rec.Code, rec.Body)
	}
	if len(sb.signed) != 1 {
		t.Fatalf("signed = %v, want exactly the fresh chunk", sb.signed)
	}

	// An existing chunk key is answered exists:true and never signed.
	existing := "existing chunk content"
	if err := os.MkdirAll(filepath.Join(root, p.ID, "chunks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, p.ID, "chunks", shaOf(existing)), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	rec = do(t, h, "POST", base+"sign", map[string]any{"key": "chunks/" + shaOf(existing), "size": int64(len(existing))})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"exists":true`) {
		t.Fatalf("sign existing chunk: %d %s, want exists:true", rec.Code, rec.Body)
	}
	if len(sb.signed) != 1 {
		t.Fatalf("existing chunk was signed: %v", sb.signed)
	}
}

// TestDelta_Hub_ManifestNeverPresigned (row H3): manifests are mutable-shaped
// trust (their key is not their content's hash), so like journals they always
// flow through the server.
func TestDelta_Hub_ManifestNeverPresigned(t *testing.T) {
	var sb *signingBackend
	srv, p, _ := newHub(t, true, func(be remote.Backend) remote.Backend {
		sb = &signingBackend{Backend: be}
		return sb
	})
	h := srv.Handler()
	rec := do(t, h, "POST", "/api/p/"+p.ID+"/store/sign",
		map[string]any{"key": "manifests/" + shaOf("some file"), "size": 128})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"server"`) {
		t.Fatalf("sign manifest: %d %s, want server mode", rec.Code, rec.Body)
	}
	if len(sb.signed) != 0 {
		t.Fatalf("manifest was presigned: %v", sb.signed)
	}
}

// TestDelta_Hub_KeySpace (row H4): validStoreKey, the list prefix allowlist,
// and the put hash check accept and constrain both new key classes.
func TestDelta_Hub_KeySpace(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/store/"

	// Malformed spellings are refused exactly like malformed blob keys.
	bad := []string{
		"chunks/short", "chunks/../../etc/passwd",
		"chunks/" + strings.Repeat("G", 64),
		"manifests/short", "manifests/" + strings.Repeat("Z", 64),
		"manifests/../device.json",
	}
	for _, key := range bad {
		if rec := do(t, h, "GET", base+"object?key="+key, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("get %q: %d, want 400", key, rec.Code)
		}
		if rec := do(t, h, "PUT", base+"object?key="+key, []byte("x")); rec.Code != http.StatusBadRequest {
			t.Errorf("put %q: %d, want 400", key, rec.Code)
		}
	}

	// A chunk is content-addressed: content that does not hash to its key is
	// refused, content that does is stored and served back.
	chunk := []byte("chunk content for the key-space test")
	goodKey := "chunks/" + shaOf(string(chunk))
	if rec := do(t, h, "PUT", base+"object?key="+goodKey, []byte("not that content")); rec.Code != http.StatusBadRequest {
		t.Fatalf("put chunk with wrong content: %d, want 400", rec.Code)
	}
	if rec := do(t, h, "PUT", base+"object?key="+goodKey, chunk); rec.Code != http.StatusOK {
		t.Fatalf("put chunk: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", base+"object?key="+goodKey, nil); rec.Code != http.StatusOK || rec.Body.String() != string(chunk) {
		t.Fatalf("get chunk: %d %q", rec.Code, rec.Body)
	}

	// A manifest's key is the FILE's sha, not the manifest body's own hash —
	// there is no ingest-time hash relation to enforce (readers verify by
	// reassembly) — but every chunk it names must already be in the store.
	// The chunk uploaded above satisfies that; a manifest naming vapor is
	// TestSec_Chunks_ManifestMustNameUploadedChunks' subject.
	manifest := []byte(`{"v":1,"size":36,"chunks":[{"h":"` + shaOf(string(chunk)) + `","n":36}]}`)
	manKey := "manifests/" + shaOf("whole file content, not the manifest body")
	if rec := do(t, h, "PUT", base+"object?key="+manKey, manifest); rec.Code != http.StatusOK {
		t.Fatalf("put manifest: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", base+"object?key="+manKey, nil); rec.Code != http.StatusOK || rec.Body.String() != string(manifest) {
		t.Fatalf("get manifest: %d %q", rec.Code, rec.Body)
	}

	// Both prefixes list.
	for _, prefix := range []string{"chunks/", "manifests/"} {
		if rec := do(t, h, "GET", base+"list?prefix="+prefix, nil); rec.Code != http.StatusOK {
			t.Errorf("list %q: %d %s", prefix, rec.Code, rec.Body)
		}
	}

	// exists works for both.
	if rec := do(t, h, "GET", base+"exists?key="+goodKey, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "true") {
		t.Errorf("exists chunk: %d %s", rec.Code, rec.Body)
	}
}
