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
