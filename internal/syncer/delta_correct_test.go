package syncer

// Correctness rows of the delta-sync goal (.claude/delta-sync-goal.md):
// chunked transport must change nothing about what devices converge to.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

// failingBackend fails Puts of keys matching a prefix until allowed, so tests
// can kill a push between its ordered stages.
type failingBackend struct {
	remote.Backend
	failPrefix string
	allowed    bool
}

func (f *failingBackend) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if !f.allowed && strings.HasPrefix(key, f.failPrefix) {
		return io.ErrUnexpectedEOF
	}
	return f.Backend.Put(ctx, key, r, size)
}

// TestDelta_Order_ChunksBeforeManifest (row C5): a push that dies during
// chunk upload leaves no broken op — neither manifest nor journal was
// written, so a peer sees nothing, and the next cycle heals. (A failure at
// the MANIFEST stage is no longer an interruption at all: the hub's
// write-once refusal there falls back to a whole-blob push — see
// TestDelta_Push_ManifestRefusalFallsBackToWholeBlob — so the chunk stage is
// where a mid-push death is modeled.)
func TestDelta_Order_ChunksBeforeManifest(t *testing.T) {
	shared := sharedRemote(t)
	fb := &failingBackend{Backend: shared, failPrefix: "chunks/"}
	a := newDevice(t, "deva", fb)
	b := newDevice(t, "devb", shared)

	rng := rand.New(rand.NewSource(48))
	content := make([]byte, 8<<20)
	rng.Read(content)
	write(t, a.Folder, "big.bin", string(content))
	res, err := a.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Offline {
		t.Fatal("a push that could not finish must degrade to Offline, not succeed")
	}
	// Peer sees nothing: the journal (last in the order) was never pushed.
	cycle(t, b)
	if _, err := os.Stat(filepath.Join(b.Folder, "big.bin")); err == nil {
		t.Fatal("peer materialized a file whose push never completed")
	}
	// Next cycle heals.
	fb.allowed = true
	cycle(t, a)
	cycle(t, b)
	if got := read(t, b.Folder, "big.bin"); got != string(content) {
		t.Fatal("did not converge after the healing cycle")
	}
}

// TestDelta_Order_ManifestBeforeJournal (row C6): killed between manifest and
// journal — same story one stage later.
func TestDelta_Order_ManifestBeforeJournal(t *testing.T) {
	shared := sharedRemote(t)
	fb := &failingBackend{Backend: shared, failPrefix: "journal/"}
	a := newDevice(t, "deva", fb)
	b := newDevice(t, "devb", shared)

	rng := rand.New(rand.NewSource(49))
	content := make([]byte, 8<<20)
	rng.Read(content)
	write(t, a.Folder, "big.bin", string(content))
	res, err := a.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Offline {
		t.Fatal("journal put failed; cycle must be Offline")
	}
	cycle(t, b)
	if _, err := os.Stat(filepath.Join(b.Folder, "big.bin")); err == nil {
		t.Fatal("peer materialized a file whose journal was never pushed")
	}
	fb.allowed = true
	cycle(t, a)
	cycle(t, b)
	if got := read(t, b.Folder, "big.bin"); got != string(content) {
		t.Fatal("did not converge after the healing cycle")
	}
}

// TestDelta_Converge_ThreeDeviceConflict (row C2): concurrent offline edits of
// a large chunked file resolve exactly like small files — one winner at the
// path, the loser preserved as a conflict copy, all three devices converged.
func TestDelta_Converge_ThreeDeviceConflict(t *testing.T) {
	shared := sharedRemote(t)
	a := newDevice(t, "deva", shared)
	b := newDevice(t, "devb", shared)
	c := newDevice(t, "devc", shared)

	rng := rand.New(rand.NewSource(50))
	content := make([]byte, 8<<20)
	rng.Read(content)
	write(t, a.Folder, "big.bin", string(content))
	cycle(t, a)
	cycle(t, b)
	cycle(t, c)

	// Two devices edit different regions while "offline" (before syncing).
	ca := append([]byte{}, content...)
	ca[100] ^= 0xff
	cb := append([]byte{}, content...)
	cb[len(cb)-100] ^= 0xff
	write(t, a.Folder, "big.bin", string(ca))
	write(t, b.Folder, "big.bin", string(cb))

	cycle(t, a)
	cycle(t, b)
	cycle(t, a)
	cycle(t, b)
	cycle(t, c)

	va := read(t, a.Folder, "big.bin")
	vb := read(t, b.Folder, "big.bin")
	vc := read(t, c.Folder, "big.bin")
	if va != vb || vb != vc {
		t.Fatal("devices did not converge on the same winner")
	}
	if va != string(ca) && va != string(cb) {
		t.Fatal("winner is neither edit")
	}
	loser := string(ca)
	if va == string(ca) {
		loser = string(cb)
	}
	found := false
	for _, folder := range []string{a.Folder, b.Folder, c.Folder} {
		entries, _ := os.ReadDir(folder)
		for _, e := range entries {
			if strings.Contains(e.Name(), ".bdrive-conflict-") &&
				read(t, folder, e.Name()) == loser {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("losing edit was not preserved as a conflict copy")
	}
}

// TestDelta_Pull_MissingChunkDoesNotStall (row C7): one op whose chunk is
// unfetchable must not stop complete ops behind it from landing — the posture
// the blob path already has. A corrupt chunk (bytes that do not hash to the
// key) is refused exactly like a missing one.
func TestDelta_Pull_MissingChunkDoesNotStall(t *testing.T) {
	shared := sharedRemote(t)
	a := newDevice(t, "deva", shared)
	b := newDevice(t, "devb", shared)

	rng := rand.New(rand.NewSource(51))
	content := make([]byte, 8<<20)
	rng.Read(content)
	write(t, a.Folder, "big.bin", string(content))
	write(t, a.Folder, "after.md", "small file behind the big one")
	cycle(t, a)

	objs, err := shared.List(context.Background(), "chunks/")
	if err != nil || len(objs) == 0 {
		t.Fatalf("no chunks on remote: %v", err)
	}
	badChunk := objs[0].Key
	if err := shared.Put(context.Background(), badChunk, bytes.NewReader([]byte("wrong")), 5); err != nil {
		t.Fatal(err)
	}

	cycle(t, b) // must not stall: after.md lands even though big.bin cannot
	if got := read(t, b.Folder, "after.md"); got != "small file behind the big one" {
		t.Fatal("an unfetchable chunk stalled the op behind it")
	}
	if _, err := os.Stat(filepath.Join(b.Folder, "big.bin")); err == nil {
		t.Fatal("big.bin materialized despite its chunk being corrupt")
	}

	// Heal the chunk from the writer's own blob. Content is refetched when
	// the path's journal grows (the same trigger the whole-blob path has), so
	// the writer edits the file once more and the peer converges on that.
	chunkSha := strings.TrimPrefix(badChunk, "chunks/")
	spans, err := a.chunkSpans(shaHex(content))
	if err != nil {
		t.Fatal(err)
	}
	for _, sp := range spans {
		if sp.sha != chunkSha {
			continue
		}
		f, err := a.Store.OpenBlob(shaHex(content))
		if err != nil {
			t.Fatal(err)
		}
		err = shared.Put(context.Background(), badChunk, io.NewSectionReader(f, sp.off, sp.n), sp.n)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	content[0] ^= 0xff
	write(t, a.Folder, "big.bin", string(content))
	cycle(t, a)
	cycle(t, b)
	if got := read(t, b.Folder, "big.bin"); got != string(content) {
		t.Fatal("did not converge after the chunk was restored and the path re-journaled")
	}
}

// TestDelta_Journal_ByteIdentical (row C1, in-process half): a chunked file's
// journal op carries exactly the same JSON fields as a small file's — no new
// keys, nothing removed. The cross-binary half is E2/E3 (merge-base binary).
func TestDelta_Journal_ByteIdentical(t *testing.T) {
	shared := sharedRemote(t)
	a := newDevice(t, "deva", shared)
	rng := rand.New(rand.NewSource(52))
	content := make([]byte, 8<<20)
	rng.Read(content)
	write(t, a.Folder, "big.bin", string(content))
	write(t, a.Folder, "small.md", "tiny")
	cycle(t, a)

	ops, err := a.Store.DeviceOps("deva")
	if err != nil || len(ops) != 2 {
		t.Fatalf("ops = %d, %v", len(ops), err)
	}
	keysOf := func(op journal.Op) map[string]bool {
		raw, err := json.Marshal(op)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		ks := map[string]bool{}
		for k := range m {
			ks[k] = true
		}
		return ks
	}
	var big, small journal.Op
	for _, op := range ops {
		if op.Path == "big.bin" {
			big = op
		} else {
			small = op
		}
	}
	bk, sk := keysOf(big), keysOf(small)
	for k := range bk {
		if !sk[k] {
			t.Errorf("chunked op carries a field small ops do not: %q", k)
		}
	}
	for k := range sk {
		if !bk[k] {
			t.Errorf("chunked op is missing field %q", k)
		}
	}
}
