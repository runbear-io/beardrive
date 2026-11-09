package syncer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/store"
)

func qaBlob(t *testing.T, s *store.Store, text string) string {
	t.Helper()
	h, _, err := s.PutBlobReader(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func qaSession(t *testing.T, name string, be remote.Backend) *Session {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "volume"))
	if err != nil {
		t.Fatal(err)
	}
	return &Session{Folder: t.TempDir(), Store: s, Device: config.Device{ID: name, Name: name, Author: name + "@test"}, Backend: be}
}

func TestQA_C01_ScanDetectsConcurrentFileChange(t *testing.T) {
	be := sharedRemote(t)
	a := qaSession(t, "qa-a", be)
	write(t, a.Folder, "doc.md", "one")
	cycle(t, a)
	write(t, a.Folder, "doc.md", "two")
	res := cycle(t, a)
	if res.LocalOps == 0 {
		t.Fatal("scan did not detect changed file")
	}
	if got := read(t, a.Folder, "doc.md"); got != "two" {
		t.Fatalf("content = %q", got)
	}
}

func TestQA_C04_CoWBlobModeAndWorktreeWritable(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "volume"))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(src, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	sha, _, _, _, _, err := s.PutBlobFileVerified(src)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.BlobPath(sha))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o444 {
		t.Fatalf("blob mode = %o, want 0444", fi.Mode().Perm())
	}
	tmp, fi, err := s.MaterializeBlobToTemp(sha, t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp)
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("materialized mode = %o, want 0644", fi.Mode().Perm())
	}
	if err := os.WriteFile(tmp, []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	mat := &Session{Folder: work, Store: s, Device: config.Device{ID: "qa-cow"}}
	wrote, err := mat.materializeFile("work.md", journal.FileState{Blob: sha, Size: int64(len("content")), Mode: 0o644}, map[string]store.CachedFile{})
	if err != nil || !wrote {
		t.Fatalf("materializeFile wrote=%v err=%v", wrote, err)
	}
	workInfo, err := os.Stat(filepath.Join(work, "work.md"))
	if err != nil {
		t.Fatal(err)
	}
	if workInfo.Mode().Perm() != 0o644 {
		t.Fatalf("worktree mode = %o, want 0644", workInfo.Mode().Perm())
	}
	if err := os.RemoveAll(s.Dir()); err != nil {
		t.Fatal(err)
	}
}

func TestQA_C03_MaterializeDeletesDanglingSymlink(t *testing.T) {
	be := sharedRemote(t)
	a, b := qaSession(t, "qa-a", be), qaSession(t, "qa-b", be)
	if err := os.Symlink("missing-target", filepath.Join(a.Folder, "link")); err != nil {
		t.Fatal(err)
	}
	cycle(t, a)
	cycle(t, b)
	if _, err := os.Lstat(filepath.Join(b.Folder, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(a.Folder, "link")); err != nil {
		t.Fatal(err)
	}
	cycle(t, a)
	cycle(t, b)
	if _, err := os.Lstat(filepath.Join(b.Folder, "link")); !os.IsNotExist(err) {
		t.Fatalf("dangling link remains: %v", err)
	}
	cycle(t, b)
	if _, err := os.Lstat(filepath.Join(b.Folder, "link")); !os.IsNotExist(err) {
		t.Fatalf("link resurrected: %v", err)
	}
}

func qaOp(path, dev, kind, blob, base string, lamport int64) journal.Op {
	return journal.Op{Seq: lamport, Lamport: lamport, Time: time.Unix(lamport, 0).UTC(), Device: dev, Kind: kind, Path: path, Blob: blob, Base: base, Size: int64(len(blob)), Mode: 0o644}
}

func TestQA_C05_C07_C08_BaseMergeThreePeersAndJSONValidation(t *testing.T) {
	be := sharedRemote(t)
	a, b, c := qaSession(t, "qa-a", be), qaSession(t, "qa-b", be), qaSession(t, "qa-c", be)
	write(t, a.Folder, "lines.txt", "a\nb\nc\n")
	cycle(t, a)
	cycle(t, b)
	cycle(t, c)
	write(t, a.Folder, "lines.txt", "A\nb\nc\n")
	cycle(t, a)
	ops, err := a.Store.DeviceOps(a.Device.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) < 2 || ops[len(ops)-1].Base == "" {
		t.Fatalf("edit Base not recorded: %+v", ops)
	}
	write(t, b.Folder, "lines.txt", "a\nB\nc\n")
	write(t, c.Folder, "lines.txt", "a\nb\nC\n")
	cycle(t, b)
	cycle(t, c)
	cycle(t, a)
	cycle(t, b)
	cycle(t, c)
	for _, s := range []*Session{a, b, c} {
		if got := read(t, s.Folder, "lines.txt"); got != "A\nB\nC\n" {
			t.Fatalf("merged lines on %s = %q", s.Device.ID, got)
		}
	}

	base := qaBlob(t, a.Store, "{\n  \"a\": 1,\n  \"b\": 2,\n  \"c\": 3\n}\n")
	ja := qaBlob(t, a.Store, "{\n  \"a\": 10,\n  \"b\": 2,\n  \"c\": 3\n}\n")
	jb := qaBlob(t, a.Store, "{\n  \"a\": 1,\n  \"b\": 2,\n  \"c\": 30\n}\n")
	mine := qaOp("x.json", "a", journal.KindPut, ja, base, 2)
	theirs := qaOp("x.json", "b", journal.KindPut, jb, base, 3)
	bad := qaBlob(t, a.Store, "{\n  \"a\": 1,\n  \"b\": 2,\n  \"c\": 30,\n  \"unterminated\":\n}\n")
	badOp := qaOp("x.json", "b", journal.KindPut, bad, base, 4)
	if _, ok := a.tryMerge("x.json", mine, badOp, []journal.Op{qaOp("x.json", "z", journal.KindPut, base, "", 1), mine, badOp}); ok {
		t.Fatal("invalid JSON merge accepted")
	}
	copies, err := a.conflictCopiesWithState([]journal.Op{mine}, 2, []journal.Op{badOp}, &store.SyncState{}, []journal.Op{qaOp("x.json", "z", journal.KindPut, base, "", 1), mine, badOp}, map[string]journal.FileState{"x.json": {Blob: bad}})
	if err != nil || len(copies) != 1 || !strings.Contains(copies[0].Path, ".bdrive-conflict-") {
		t.Fatalf("invalid JSON conflict copy = %+v, err=%v", copies, err)
	}
	if _, ok := a.tryMerge("x.json", mine, theirs, []journal.Op{qaOp("x.json", "z", journal.KindPut, base, "", 1), mine, theirs}); !ok {
		t.Fatal("valid JSON merge rejected")
	}

	jlBase := qaBlob(t, a.Store, "{\"base\":1}\n")
	jlA := qaBlob(t, a.Store, "{\"base\":1}\n{\"a\":1}\n")
	jlB := qaBlob(t, a.Store, "{\"base\":1}\n{\"b\":1}\n")
	jla := qaOp("x.jsonl", "a", journal.KindPut, jlA, jlBase, 2)
	jlb := qaOp("x.jsonl", "b", journal.KindPut, jlB, jlBase, 3)
	m, ok := a.tryMerge("x.jsonl", jla, jlb, []journal.Op{qaOp("x.jsonl", "z", journal.KindPut, jlBase, "", 1), jla, jlb})
	if !ok {
		t.Fatal("JSONL append merge rejected")
	}
	out, err := os.ReadFile(a.Store.BlobPath(m.blob))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != "{\"base\":1}\n{\"a\":1}\n{\"b\":1}\n" {
		t.Fatalf("JSONL output = %q", got)
	}
	if _, ok := mergeJSONLAppends([]byte("1"), []byte("12"), []byte("13"), jla, jlb); ok {
		t.Fatal("mergeJSONLAppends accepted in-line mutation on unterminated base")
	}
	if mergedUnterm, ok := mergeJSONLAppends([]byte("{\"base\":1}"), []byte("{\"base\":1}\n{\"a\":1}\n"), []byte("{\"base\":1}\n{\"b\":1}\n"), jla, jlb); !ok || string(mergedUnterm) != "{\"base\":1}\n{\"a\":1}\n{\"b\":1}\n" {
		t.Fatalf("mergeJSONLAppends newline-separated append on unterminated base = (%v, %q)", ok, string(mergedUnterm))
	}

	// Peer edits from an older ancestor V0 while mine edits from a newer V1:
	// commonAncestor must select V0, not mine.Base (V1), so V0->V1 additions
	// are preserved instead of treated as deleted by the peer.
	v0Blob := qaBlob(t, a.Store, "line1\nline2\nline3\n")
	v1Blob := qaBlob(t, a.Store, "line1\nline2\nline3\nadded-in-v1\n")
	mineV2Blob := qaBlob(t, a.Store, "line1\nline2\nline3\nadded-in-v1\nadded-in-v2\n")
	theirFromV0Blob := qaBlob(t, a.Store, "LINE1-THEIRS\nline2\nline3\n")
	v0Op := qaOp("anc.txt", "z", journal.KindPut, v0Blob, "", 1)
	v1Op := qaOp("anc.txt", "z", journal.KindPut, v1Blob, v0Blob, 3)
	theirFromV0 := qaOp("anc.txt", "b", journal.KindPut, theirFromV0Blob, v0Blob, 2)
	mineFromV1 := qaOp("anc.txt", "a", journal.KindPut, mineV2Blob, v1Blob, 4)
	mergedAnc, ok := a.tryMerge("anc.txt", mineFromV1, theirFromV0, []journal.Op{v0Op, theirFromV0, v1Op, mineFromV1})
	if !ok {
		t.Fatal("merge across older peer base V0 and newer mine.Base V1 failed")
	}
	mergedAncBytes, err := os.ReadFile(a.Store.BlobPath(mergedAnc.blob))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(mergedAncBytes); got != "LINE1-THEIRS\nline2\nline3\nadded-in-v1\nadded-in-v2\n" {
		t.Fatalf("merged content = %q, want V0-based merge preserving added-in-v1 and added-in-v2", got)
	}
}

func TestQA_C06_LazyBlobPullSkipsHistoricalOverwrittenBlobs(t *testing.T) {
	base := sharedRemote(t)
	count := &qaCountingBackend{Backend: base, gets: map[string]int{}}
	a := qaSession(t, "qa-a", count)
	for i := range 10 {
		write(t, a.Folder, "doc.md", string(rune('a'+i)))
		cycle(t, a)
	}
	write(t, a.Folder, "gone.md", "never-needed")
	cycle(t, a)
	if err := os.Remove(filepath.Join(a.Folder, "gone.md")); err != nil {
		t.Fatal(err)
	}
	cycle(t, a)
	fold, err := a.Store.ReplayFold()
	if err != nil {
		t.Fatal(err)
	}
	live := fold.Target["doc.md"].Blob
	count.reset()
	b := qaSession(t, "qa-b", count)
	cycle(t, b)
	for key, n := range count.snapshot() {
		if strings.HasPrefix(key, "blobs/") && n > 0 && key != "blobs/"+live {
			t.Fatalf("historical blob fetched %s (%d)", key, n)
		}
	}
	if got := read(t, b.Folder, "doc.md"); got != "j" {
		t.Fatalf("live content = %q", got)
	}
	if _, err := os.Stat(filepath.Join(b.Folder, "gone.md")); !os.IsNotExist(err) {
		t.Fatalf("deleted file materialized: %v", err)
	}

	// An unpushed local edit retains its loser blob; pulling a peer edit must
	// fetch the peer winner and the cached base needed by the three-way merge.
	collisionCount := &qaCountingBackend{Backend: base, gets: map[string]int{}}
	c := qaSession(t, "qa-c", collisionCount)
	cycle(t, c)
	if err := os.Remove(c.Store.BlobPath(live)); err != nil {
		t.Fatal(err)
	}
	write(t, c.Folder, "doc.md", "local edit")
	c.Backend = nil
	if _, err := c.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	d := qaSession(t, "qa-d", collisionCount)
	cycle(t, d)
	write(t, d.Folder, "doc.md", "peer edit")
	cycle(t, d)
	peerOps, err := d.Store.DeviceOps(d.Device.ID)
	if err != nil || len(peerOps) == 0 {
		t.Fatalf("peer ops = %+v, err=%v", peerOps, err)
	}
	peerBlob := peerOps[len(peerOps)-1].Blob
	collisionCount.reset()
	c.Backend = collisionCount
	cycle(t, c)
	counts := collisionCount.snapshot()
	if counts["blobs/"+peerBlob] == 0 || counts["blobs/"+live] == 0 {
		t.Fatalf("collision fetches = %v, peer=%s base=%s", counts, peerBlob, live)
	}
	if !c.Store.HasBlob(live) {
		t.Fatal("base blob was not backfilled for collision")
	}
	localOps, err := c.Store.DeviceOps(c.Device.ID)
	if err != nil || len(localOps) == 0 || !c.Store.HasBlob(localOps[len(localOps)-1].Blob) {
		t.Fatalf("local loser blob missing: %+v, err=%v", localOps, err)
	}
}

type qaCountingBackend struct {
	remote.Backend
	mu   sync.Mutex
	gets map[string]int
}

func (b *qaCountingBackend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	b.mu.Lock()
	b.gets[key]++
	b.mu.Unlock()
	return b.Backend.Get(ctx, key)
}
func (b *qaCountingBackend) reset() { b.mu.Lock(); defer b.mu.Unlock(); b.gets = map[string]int{} }
func (b *qaCountingBackend) snapshot() map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]int{}
	for k, v := range b.gets {
		out[k] = v
	}
	return out
}

func TestQA_C09_C10_SpoolFlockAndMultiWorktreePruneLocalBlobs(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "volume"))
	if err != nil {
		t.Fatal(err)
	}
	const n = 100
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			if err := s.LogRead("r/"+string(rune(i))+".md", "s"); err != nil {
				t.Error(err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			if err := s.LogInboundForMount("mount2", "p/"+string(rune(i))+".md", i%2 == 0); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	var readDrain sync.WaitGroup
	readResults := make(chan []store.ReadEvent, 4)
	for range 4 {
		readDrain.Add(1)
		go func() {
			defer readDrain.Done()
			got, err := s.PendingReads()
			if err != nil {
				t.Error(err)
				return
			}
			readResults <- got
		}()
	}
	readDrain.Wait()
	close(readResults)
	for got := range readResults {
		if len(got) != n {
			t.Fatalf("concurrent reads drain = %d, want %d", len(got), n)
		}
	}
	if err := s.ClearPendingReads(); err != nil {
		t.Fatal(err)
	}
	var inboundDrain sync.WaitGroup
	inboundResults := make(chan []store.InboundEvent, 4)
	for range 4 {
		inboundDrain.Add(1)
		go func() {
			defer inboundDrain.Done()
			got, err := s.DrainInboundForMount("mount2")
			if err != nil {
				t.Error(err)
				return
			}
			inboundResults <- got
		}()
	}
	inboundDrain.Wait()
	close(inboundResults)
	var inboundTotal int
	for got := range inboundResults {
		inboundTotal += len(got)
	}
	if inboundTotal != n {
		t.Fatalf("concurrent inbound drain = %d, want %d", inboundTotal, n)
	}
	live := qaBlob(t, s, "live")
	old := qaBlob(t, s, "base-blob")
	orphan := qaBlob(t, s, "truly-orphan-blob")
	op := journal.Op{Seq: 1, Lamport: 1, Time: time.Now(), Device: "qa", Kind: journal.KindPut, Path: "live", Blob: live, Base: old, Size: 4}
	if err := s.AppendOps("qa", []journal.Op{op}); err != nil {
		t.Fatal(err)
	}
	cache := map[string]store.CachedFile{"other": {Blob: live}}
	if err := s.SaveCache("mount2", cache); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(s.BlobPath(old), past, past)
	_ = os.Chtimes(s.BlobPath(orphan), past, past)
	removed, _, err := s.PruneLocalBlobs(nil, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 || s.HasBlob(orphan) || !s.HasBlob(live) || !s.HasBlob(old) {
		t.Fatalf("PruneLocalBlobs removed=%d, hasOrphan=%v, hasLive=%v, hasBase=%v",
			removed, s.HasBlob(orphan), s.HasBlob(live), s.HasBlob(old))
	}
}

func TestQA_V2JournalAppendAndTailPullWithFallback(t *testing.T) {
	be := newQAJournalBackend()
	a, b := qaSession(t, "qa-a", be), qaSession(t, "qa-b", be)
	write(t, a.Folder, "one", "1")
	cycle(t, a)
	cycle(t, b)
	write(t, a.Folder, "two", "2")
	cycle(t, a)
	if be.appendCalls == 0 {
		t.Fatal("append path not used")
	}
	cycle(t, b)
	if be.tailCalls == 0 {
		t.Fatal("tail path not used")
	}
	beforeFull := be.putJournalCalls
	be.unsupported = true
	write(t, a.Folder, "three", "3")
	cycle(t, a)
	if be.putJournalCalls <= beforeFull {
		t.Fatal("full journal fallback not used")
	}
}

type qaJournalBackend struct {
	mu                                      sync.Mutex
	objects                                 map[string][]byte
	appendCalls, tailCalls, putJournalCalls int
	unsupported                             bool
}

func newQAJournalBackend() *qaJournalBackend { return &qaJournalBackend{objects: map[string][]byte{}} }
func (b *qaJournalBackend) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[key] = append([]byte(nil), data...)
	if strings.HasPrefix(key, "journal/") {
		b.putJournalCalls++
	}
	return nil
}
func (b *qaJournalBackend) Get(_ context.Context, key string) (io.ReadCloser, error) {
	b.mu.Lock()
	d, ok := b.objects[key]
	b.mu.Unlock()
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(d)), nil
}
func (b *qaJournalBackend) List(_ context.Context, prefix string) ([]remote.Object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []remote.Object
	for k, v := range b.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, remote.Object{Key: k, Size: int64(len(v))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}
func (b *qaJournalBackend) Exists(_ context.Context, key string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.objects[key]
	return ok, nil
}
func (b *qaJournalBackend) Close() error { return nil }
func (b *qaJournalBackend) AppendJournal(_ context.Context, req remote.JournalAppendRequest) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.appendCalls++
	if b.unsupported {
		return remote.ErrJournalAppendUnsupported
	}
	key := "journal/" + req.Device + ".jsonl"
	b.objects[key] = append(b.objects[key], req.Data...)
	return nil
}
func (b *qaJournalBackend) GetJournalTail(_ context.Context, key string, offset int64, _ string) (io.ReadCloser, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tailCalls++
	d := b.objects[key]
	if offset > int64(len(d)) {
		return nil, false, errors.New("offset")
	}
	return io.NopCloser(bytes.NewReader(d[offset:])), true, nil
}

func TestQA_RedefinesAppliedDetectsBaseMutation(t *testing.T) {
	ts := time.Date(2026, 11, 9, 15, 0, 0, 0, time.UTC)
	baseA := strings.Repeat("a", 64)
	baseB := strings.Repeat("b", 64)
	blob := strings.Repeat("c", 64)
	applied := []journal.Op{{
		Device: "peer-dev", Seq: 1, Lamport: 10, Time: ts,
		Kind: journal.KindPut, Path: "notes/doc.md", Blob: blob, Base: baseA, Size: 42, Mode: 0o644,
	}}
	same := []journal.Op{applied[0]}
	if redefinesApplied(same, applied) {
		t.Fatal("identical op was flagged as redefinition")
	}
	mutatedBase := []journal.Op{applied[0]}
	mutatedBase[0].Base = baseB
	if !redefinesApplied(mutatedBase, applied) {
		t.Fatal("redefinesApplied failed to detect Base mutation on applied (Device, Seq)")
	}

	// End-to-end pull check: a same-size journal rewrite on remote must not be
	// skipped by a size-only gate before redefinesApplied.
	remoteStore := sharedRemote(t)
	victim := newDevice(t, "victim-dev", remoteStore)
	contentA := "same-size-body-A"
	contentB := "same-size-body-B"
	shaA := secfx4Blob(t, remoteStore, contentA)
	shaB := secfx4Blob(t, remoteStore, contentB)
	lineA := secfx7Op(t, "peer-same", 1, journal.KindPut, "a.txt", shaA, len(contentA)) + "\n"
	lineB := secfx7Op(t, "peer-same", 1, journal.KindPut, "b.txt", shaB, len(contentB)) + "\n"
	if len(lineA) != len(lineB) {
		t.Fatalf("line lengths differ: %d vs %d", len(lineA), len(lineB))
	}
	secfx4Journal(t, remoteStore, "peer-same", lineA)
	cycle(t, victim)
	if got := read(t, victim.Folder, "a.txt"); got != contentA {
		t.Fatalf("victim did not apply initial peer op: %q", got)
	}
	secfx4Journal(t, remoteStore, "peer-same", lineB)
	cycle(t, victim)
	if _, err := os.Stat(filepath.Join(victim.Folder, "b.txt")); !os.IsNotExist(err) {
		t.Fatalf("same-size redefined journal op was accepted: err=%v", err)
	}
	storedBefore, err := os.ReadFile(victim.Store.JournalPath("peer-same"))
	if err != nil {
		t.Fatal(err)
	}
	if string(storedBefore) != lineA {
		t.Fatalf("local peer journal was overwritten by same-size rewrite: %q", string(storedBefore))
	}

	// A peer withdrawing an applied op under a read-only path must not cause a
	// read-only / folder-restricted client to re-assert the op into its own journal.
	roLine1 := secfx7Op(t, "peer-ro", 1, journal.KindPut, "ro/doc.txt", shaA, len(contentA)) + "\n"
	roLine2 := secfx7Op(t, "peer-ro", 2, journal.KindPut, "other.txt", shaB, len(contentB)) + "\n"
	secfx4Journal(t, remoteStore, "peer-ro", roLine1+roLine2)
	cycle(t, victim)
	stRO, err := victim.Store.LoadSync()
	if err != nil {
		t.Fatal(err)
	}
	stRO.ReadOnly = []string{"ro/"}
	if err := victim.Store.SaveSync(stRO); err != nil {
		t.Fatal(err)
	}
	roLine3 := secfx7Op(t, "peer-ro", 3, journal.KindPut, "other2.txt", shaB, len(contentB)) + "\n"
	secfx4Journal(t, remoteStore, "peer-ro", "{corrupt-line\n"+roLine2+roLine3)
	cycle(t, victim)
	ownOps, err := victim.Store.DeviceOps(victim.Device.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ownOps {
		if strings.HasPrefix(op.Path, "ro/") {
			t.Fatalf("victim re-asserted read-only path into own journal: %+v", op)
		}
	}
}
