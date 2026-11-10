package webapp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

type memCheckpointBackend struct {
	objs map[string][]byte
	gets map[string]int
}

func newMemCheckpointBackend() *memCheckpointBackend {
	return &memCheckpointBackend{
		objs: make(map[string][]byte),
		gets: make(map[string]int),
	}
}

func (m *memCheckpointBackend) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.objs[key] = b
	return nil
}

func (m *memCheckpointBackend) Get(_ context.Context, key string) (io.ReadCloser, error) {
	b, ok := m.objs[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	m.gets[key]++
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *memCheckpointBackend) List(_ context.Context, prefix string) ([]remote.Object, error) {
	var out []remote.Object
	for k, b := range m.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, remote.Object{
				Key:      k,
				Size:     int64(len(b)),
				Modified: time.Unix(1700000000, 0).UTC(),
			})
		}
	}
	return out, nil
}

func (m *memCheckpointBackend) Exists(_ context.Context, key string) (bool, error) {
	_, ok := m.objs[key]
	return ok, nil
}

func (m *memCheckpointBackend) Close() error { return nil }

func TestFoldCheckpointRoundTripAndTombstoneProtection(t *testing.T) {
	shaA := strings.Repeat("a", 64)
	shaB := strings.Repeat("b", 64)
	t0 := time.Date(2026, 11, 10, 1, 0, 0, 0, time.UTC)

	ops := []journal.Op{
		{Seq: 1, Lamport: 10, Time: t0, Device: "dev1", Kind: journal.KindPut, Path: "live.md", Blob: shaA, Size: 12},
		{Seq: 2, Lamport: 20, Time: t0.Add(time.Minute), Device: "dev1", Kind: journal.KindPut, Path: "deleted.md", Blob: shaA, Size: 15},
		{Seq: 3, Lamport: 500, Time: t0.Add(2 * time.Minute), Device: "dev1", Kind: journal.KindDelete, Path: "deleted.md"},
	}
	files, tombs := foldFilesAndTombstones(ops)
	if _, ok := files["live.md"]; !ok {
		t.Fatal("expected live.md in folded files")
	}
	if _, ok := files["deleted.md"]; ok {
		t.Fatal("did not expect deleted.md in live files")
	}
	if tomb, ok := tombs["deleted.md"]; !ok || tomb.Lamport != 500 {
		t.Fatalf("expected tombstone at lamport 500, got %+v (ok=%v)", tomb, ok)
	}

	cp := foldCheckpoint{
		Journals: map[string]ckptJournalMeta{
			"journal/dev1.jsonl": {Size: 1024, ModTime: t0, PrefixSHA256: shaA, Ops: 3},
		},
		Files:      files,
		Tombstones: tombs,
		Moves:      moveIndex{},
	}
	raw, err := encodeFoldCheckpoint(cp)
	if err != nil {
		t.Fatalf("encodeFoldCheckpoint: %v", err)
	}
	decoded, err := decodeFoldCheckpoint(raw)
	if err != nil {
		t.Fatalf("decodeFoldCheckpoint: %v", err)
	}

	// Late-arriving put from an offline peer with lower lamport (300 < 500) must NOT resurrect deleted.md.
	stalePut := journal.Op{
		Seq: 1, Lamport: 300, Time: t0.Add(3 * time.Minute),
		Device: "dev2", Kind: journal.KindPut, Path: "deleted.md", Blob: shaB, Size: 99,
	}
	if applyFoldOp(decoded.Files, decoded.Tombstones, stalePut) {
		t.Fatal("stale put with lamport 300 resurrected path deleted at lamport 500")
	}
	if _, ok := decoded.Files["deleted.md"]; ok {
		t.Fatal("deleted.md present in files after stale put")
	}

	// Newer put with higher lamport (600 > 500) legitimately recreates deleted.md.
	newerPut := journal.Op{
		Seq: 2, Lamport: 600, Time: t0.Add(4 * time.Minute),
		Device: "dev2", Kind: journal.KindPut, Path: "deleted.md", Blob: shaB, Size: 99,
	}
	if !applyFoldOp(decoded.Files, decoded.Tombstones, newerPut) {
		t.Fatal("newer put with lamport 600 should recreate deleted.md")
	}
	if got := decoded.Files["deleted.md"].Blob; got != shaB {
		t.Fatalf("recreated deleted.md blob = %q, want %q", got, shaB)
	}
}

type fullQuota struct{}

func (fullQuota) CheckWrite(string, int64) error { return errors.New("storage quota exceeded") }
func (fullQuota) CheckSeat(string, int) error    { return nil }
func (fullQuota) RecordUsage(string, int64)      {}
func (fullQuota) CheckRead(string, int64) error  { return nil }
func (fullQuota) RecordEgress(string, int64)     {}

func TestHandleStoreSignAllowsExistingBlobWhenQuotaExceeded(t *testing.T) {
	be := newMemCheckpointBackend()
	sha := strings.Repeat("c", 64)
	_ = be.Put(context.Background(), "blobs/"+sha, strings.NewReader("hello"), 5)

	rs := &RemoteSource{Backend: be}
	v := &volume{source: rs}
	srv := &Server{
		Upload: UploadConfig{Enabled: true, TTL: 5 * time.Minute},
		Quota:  fullQuota{},
	}

	// 1. Existing blob should succeed with exists: true even when quota is full.
	body := `{"key":"blobs/` + sha + `","size":5}`
	req := httptest.NewRequest(http.MethodPost, "/api/p/p-1/store/sign", strings.NewReader(body))
	req.SetPathValue("project", "p-1")
	rec := httptest.NewRecorder()
	srv.handleStoreSign(v, rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("existing blob sign status = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"exists":true`) {
		t.Fatalf("existing blob sign body = %s, want exists:true", rec.Body.String())
	}

	// 2. Non-existent blob must still be rejected with 403 when quota is full.
	missingSHA := strings.Repeat("d", 64)
	body2 := `{"key":"blobs/` + missingSHA + `","size":5}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/p/p-1/store/sign", strings.NewReader(body2))
	req2.SetPathValue("project", "p-1")
	rec2 := httptest.NewRecorder()
	srv.handleStoreSign(v, rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("missing blob sign under full quota status = %d (%s), want 403", rec2.Code, rec2.Body.String())
	}
}

func TestFoldCheckpointRestoresWithGrownTailAndSurvivesHistoricalTombstone(t *testing.T) {
	dir := t.TempDir()
	be, err := remote.Open(context.Background(), "file://"+dir)
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()

	shaA := strings.Repeat("a", 64)
	shaB := strings.Repeat("b", 64)
	shaC := strings.Repeat("c", 64)
	shaD := strings.Repeat("d", 64)
	t0 := time.Date(2026, 11, 10, 1, 0, 0, 0, time.UTC)

	// Build a journal > minFoldCheckpointBytes (1 MiB) where log.md is put, deleted, and recreated.
	var ops []journal.Op
	for i := 1; i <= 6500; i++ {
		ops = append(ops, journal.Op{
			Seq: int64(i), Lamport: int64(i), Time: t0.Add(time.Duration(i) * time.Second),
			Device: "dev1", Kind: journal.KindPut,
			Path: "notes/pad-" + strings.Repeat("x", 80) + ".md",
			Blob: shaA, Size: 10,
		})
	}
	ops = append(ops,
		journal.Op{Seq: 6501, Lamport: 6501, Time: t0.Add(6501 * time.Second), Device: "dev1", Kind: journal.KindPut, Path: "log.md", Blob: shaA, Size: 10},
		journal.Op{Seq: 6502, Lamport: 6502, Time: t0.Add(6502 * time.Second), Device: "dev1", Kind: journal.KindDelete, Path: "log.md"},
		journal.Op{Seq: 6503, Lamport: 6503, Time: t0.Add(6503 * time.Second), Device: "dev1", Kind: journal.KindPut, Path: "log.md", Blob: shaB, Size: 20},
	)
	raw, err := journal.Marshal(ops)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(raw)) < minFoldCheckpointBytes {
		t.Fatalf("journal size %d < %d", len(raw), minFoldCheckpointBytes)
	}
	if err := be.Put(context.Background(), "journal/dev1.jsonl", bytes.NewReader(raw), int64(len(raw))); err != nil {
		t.Fatal(err)
	}

	rs1 := &RemoteSource{Backend: be}
	files1, _, err := rs1.FilesWithMoves(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := files1["log.md"].Blob; got != shaB {
		t.Fatalf("initial log.md blob = %q, want %q", got, shaB)
	}
	for range 100 {
		if ok, _ := be.Exists(context.Background(), hubFoldCheckpointKey); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ok, _ := be.Exists(context.Background(), hubFoldCheckpointKey); !ok {
		t.Fatal("expected fold checkpoint to be written")
	}

	// Append seq 6504 editing log.md AFTER the checkpoint was written.
	tail1, err := journal.Marshal([]journal.Op{
		{Seq: 6504, Lamport: 6504, Time: t0.Add(6504 * time.Second), Device: "dev1", Kind: journal.KindPut, Path: "log.md", Blob: shaC, Size: 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw1 := append(append([]byte(nil), raw...), tail1...)
	if err := be.Put(context.Background(), "journal/dev1.jsonl", bytes.NewReader(raw1), int64(len(raw1))); err != nil {
		t.Fatal(err)
	}

	// Cold start a fresh RemoteSource: restoreFoldCheckpoint must apply the grown tail without populating jcache.
	rs2 := &RemoteSource{Backend: be}
	files2, _, err := rs2.FilesWithMoves(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := files2["log.md"].Blob; got != shaC {
		t.Fatalf("cold restore with grown tail log.md blob = %q, want %q", got, shaC)
	}
	rs2.jmu.Lock()
	jcacheLen := len(rs2.jcache)
	rs2.jmu.Unlock()
	if jcacheLen != 0 {
		t.Fatalf("expected cold checkpoint+tail restore to leave jcache empty, got %d entries", jcacheLen)
	}

	// Append seq 6505 editing log.md: tryIncrementalFold must succeed even with empty jcache and historical tombstone on log.md.
	tail2, err := journal.Marshal([]journal.Op{
		{Seq: 6505, Lamport: 6505, Time: t0.Add(6505 * time.Second), Device: "dev1", Kind: journal.KindPut, Path: "log.md", Blob: shaD, Size: 40},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw2 := append(append([]byte(nil), raw1...), tail2...)
	if err := be.Put(context.Background(), "journal/dev1.jsonl", bytes.NewReader(raw2), int64(len(raw2))); err != nil {
		t.Fatal(err)
	}
	files3, _, err := rs2.FilesWithMoves(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := files3["log.md"].Blob; got != shaD {
		t.Fatalf("incremental fold after checkpoint restore log.md blob = %q, want %q", got, shaD)
	}
	rs2.jmu.Lock()
	jcacheLen2 := len(rs2.jcache)
	rs2.jmu.Unlock()
	if jcacheLen2 != 0 {
		t.Fatalf("expected incremental fold to leave jcache empty, got %d entries", jcacheLen2)
	}
}

func TestIncrementalHistIndexAppendsWithoutReReadingUnchangedJournals(t *testing.T) {
	rs, cb, f := countedProject(t)
	ctx := context.Background()
	x1, err := rs.historyIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n1 := len(x1.all)
	g1, g3 := cb.getsFor("dev1"), cb.getsFor("dev3")

	f.putAs("dev2", "bob@x.io", "Bob", "notes/other.md", "bob v2")
	x2, err := rs.historyIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(x2.all) != n1+1 {
		t.Fatalf("len(x2.all) = %d, want %d", len(x2.all), n1+1)
	}
	if cb.getsFor("dev1") != g1 || cb.getsFor("dev3") != g3 {
		t.Fatalf("incremental history index re-read unchanged journals: dev1=%d (was %d), dev3=%d (was %d)", cb.getsFor("dev1"), g1, cb.getsFor("dev3"), g3)
	}
	newestIdx := x2.byTime[0]
	if x2.all[newestIdx].Op.Path != "notes/other.md" || x2.kinds[newestIdx] != "edit" {
		t.Fatalf("newest entry = (%q, %q), want (notes/other.md, edit)", x2.all[newestIdx].Op.Path, x2.kinds[newestIdx])
	}
}
