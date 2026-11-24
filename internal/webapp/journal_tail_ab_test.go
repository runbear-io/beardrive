package webapp_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/webapp"
)

type abMemObj struct {
	data []byte
	ver  int64
	mod  time.Time
}

type abByteReader struct {
	r     io.Reader
	onInc func(n int64)
}

func (b *abByteReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 {
		b.onInc(int64(n))
	}
	return n, err
}

func (b *abByteReader) Close() error { return nil }

// abCountBackend implements only interfaces present at commit ecd3318:
// Backend, ConditionalPutter, RangeGetter, and VersionedRangeGetter.
type abCountBackend struct {
	mu            sync.Mutex
	objs          map[string]abMemObj
	nextVer       int64
	clock         time.Time
	fullGetBytes  int64
	rangeGetBytes int64
	fullGets      int
	rangeGets     int
}

func newABCountBackend() *abCountBackend {
	return &abCountBackend{
		objs:  make(map[string]abMemObj),
		clock: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC),
	}
}

func (b *abCountBackend) resetStats() {
	b.mu.Lock()
	b.fullGetBytes = 0
	b.rangeGetBytes = 0
	b.fullGets = 0
	b.rangeGets = 0
	b.mu.Unlock()
}

func (b *abCountBackend) stats() (fullGets int, fullBytes int64, rangeGets int, rangeBytes int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fullGets, b.fullGetBytes, b.rangeGets, b.rangeGetBytes
}

func (b *abCountBackend) isJournalKey(key string) bool {
	return strings.Contains(key, "journal/") && strings.HasSuffix(key, ".jsonl")
}

func (b *abCountBackend) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextVer++
	b.clock = b.clock.Add(time.Second)
	b.objs[key] = abMemObj{data: data, ver: b.nextVer, mod: b.clock}
	return nil
}

func (b *abCountBackend) Get(_ context.Context, key string) (io.ReadCloser, error) {
	b.mu.Lock()
	obj, ok := b.objs[key]
	if ok && b.isJournalKey(key) {
		b.fullGets++
	}
	b.mu.Unlock()
	if !ok {
		return nil, os.ErrNotExist
	}
	cp := append([]byte(nil), obj.data...)
	return &abByteReader{
		r: bytes.NewReader(cp),
		onInc: func(n int64) {
			if b.isJournalKey(key) {
				b.mu.Lock()
				b.fullGetBytes += n
				b.mu.Unlock()
			}
		},
	}, nil
}

func (b *abCountBackend) List(_ context.Context, prefix string) ([]remote.Object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []remote.Object
	for k, obj := range b.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, remote.Object{Key: k, Size: int64(len(obj.data)), Modified: obj.mod})
		}
	}
	return out, nil
}

func (b *abCountBackend) Exists(_ context.Context, key string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.objs[key]
	return ok, nil
}

func (b *abCountBackend) Close() error { return nil }

func (b *abCountBackend) GetVersioned(_ context.Context, key string) (io.ReadCloser, remote.Version, error) {
	b.mu.Lock()
	obj, ok := b.objs[key]
	b.mu.Unlock()
	if !ok {
		return nil, remote.VersionAbsent, os.ErrNotExist
	}
	cp := append([]byte(nil), obj.data...)
	return io.NopCloser(bytes.NewReader(cp)), remote.Version(strconv.FormatInt(obj.ver, 10)), nil
}

func (b *abCountBackend) PutIf(_ context.Context, key string, r io.Reader, _ int64, expect remote.Version) (remote.Version, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return remote.VersionAbsent, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	obj, exists := b.objs[key]
	cur := remote.VersionAbsent
	if exists {
		cur = remote.Version(strconv.FormatInt(obj.ver, 10))
	}
	if cur != expect {
		return remote.VersionAbsent, remote.ErrVersionMismatch
	}
	b.nextVer++
	b.clock = b.clock.Add(time.Second)
	b.objs[key] = abMemObj{data: data, ver: b.nextVer, mod: b.clock}
	return remote.Version(strconv.FormatInt(b.nextVer, 10)), nil
}

func (b *abCountBackend) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	rc, _, err := b.GetVersionedRange(ctx, key, offset, length)
	return rc, err
}

func (b *abCountBackend) GetVersionedRange(_ context.Context, key string, offset, length int64) (io.ReadCloser, remote.Version, error) {
	b.mu.Lock()
	obj, ok := b.objs[key]
	if ok && b.isJournalKey(key) {
		b.rangeGets++
	}
	b.mu.Unlock()
	if !ok {
		return nil, remote.VersionAbsent, os.ErrNotExist
	}
	size := int64(len(obj.data))
	if offset < 0 || offset > size {
		return nil, remote.VersionAbsent, fmt.Errorf("range offset %d out of bounds for size %d", offset, size)
	}
	end := size
	if length >= 0 && offset+length < end {
		end = offset + length
	}
	cp := append([]byte(nil), obj.data[offset:end]...)
	ver := remote.Version(strconv.FormatInt(obj.ver, 10))
	return &abByteReader{
		r: bytes.NewReader(cp),
		onInc: func(n int64) {
			if b.isJournalKey(key) {
				b.mu.Lock()
				b.rangeGetBytes += n
				b.mu.Unlock()
			}
		},
	}, ver, nil
}

var (
	_ remote.Backend              = (*abCountBackend)(nil)
	_ remote.ConditionalPutter    = (*abCountBackend)(nil)
	_ remote.RangeGetter          = (*abCountBackend)(nil)
	_ remote.VersionedRangeGetter = (*abCountBackend)(nil)
)

func abSHA256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func abDo(t *testing.T, h http.Handler, method, target string, body []byte, hdr http.Header) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func abSeedLargeJournal(t *testing.T, be *abCountBackend, projID, devID string, n int) ([]byte, string) {
	t.Helper()
	content := []byte("seed file body")
	blob := abSHA256(content)
	if err := be.Put(context.Background(), projID+"/blobs/"+blob, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	baseTime := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	ops := make([]journal.Op, 0, n)
	for i := 1; i <= n; i++ {
		ops = append(ops, journal.Op{
			Seq:     int64(i),
			Lamport: int64(i),
			Time:    baseTime.Add(time.Duration(i) * time.Second),
			Device:  devID,
			User:    "alice@example.com",
			Kind:    journal.KindPut,
			Path:    fmt.Sprintf("notes/seed-%05d.md", i),
			Blob:    blob,
			Size:    int64(len(content)),
			Mode:    0o644,
		})
	}
	raw, err := journal.Marshal(ops)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 2<<20 {
		t.Fatalf("seeded journal size = %d, want >= 2 MiB", len(raw))
	}
	if err := be.Put(context.Background(), projID+"/journal/"+devID+".jsonl", bytes.NewReader(raw), int64(len(raw))); err != nil {
		t.Fatal(err)
	}
	return raw, blob
}

func setupABHub(t *testing.T, devID string) (*abCountBackend, http.Handler, string, []byte, string) {
	t.Helper()
	be := newABCountBackend()
	db, err := webapp.OpenProjectDB(filepath.Join(t.TempDir(), "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := db.GetOrCreate("ab-proj", "")
	if err != nil {
		t.Fatal(err)
	}
	raw, blob := abSeedLargeJournal(t, be, p.ID, devID, 10000)
	srv := &webapp.Server{
		Root:     be,
		Projects: db,
		Device:   webapp.Identity{ID: "web-hub", Name: "Web Hub", Author: "hub@example.com"},
		Upload:   webapp.UploadConfig{Enabled: true},
	}
	h := srv.Handler()
	apiBase := "/api/p/" + p.ID + "/"
	for _, path := range []string{"tree", "history?n=5", "history/emptied"} {
		if rec := abDo(t, h, http.MethodGet, apiBase+path, nil, nil); rec.Code != http.StatusOK {
			t.Fatalf("warmup %s: %d %s", path, rec.Code, rec.Body)
		}
	}
	return be, h, apiBase, raw, blob
}

func TestJournalTailAppendReadsOnlyTailBytes_AB(t *testing.T) {
	t.Run("HealthyControl_UnchangedAndMismatchFallback", func(t *testing.T) {
		be, h, apiBase, rawDev1, _ := setupABHub(t, "dev1")

		be.resetStats()
		for _, path := range []string{"tree", "history?n=5", "history/emptied"} {
			if rec := abDo(t, h, http.MethodGet, apiBase+path, nil, nil); rec.Code != http.StatusOK {
				t.Fatalf("control unchanged %s: %d %s", path, rec.Code, rec.Body)
			}
		}
		if fg, fb, rg, rb := be.stats(); fb != 0 || rb != 0 || fg != 0 || rg != 0 {
			t.Fatalf("unchanged reads transferred bytes: fullGets=%d (%d B), rangeGets=%d (%d B)", fg, fb, rg, rb)
		}

		mismatchURL := fmt.Sprintf("%sstore/object?key=journal/dev1.jsonl&offset=100&prefix_sha256=%s", apiBase, strings.Repeat("f", 64))
		rec := abDo(t, h, http.MethodGet, mismatchURL, nil, nil)
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), rawDev1) {
			t.Fatalf("mismatch fallback: code=%d len=%d want=%d", rec.Code, rec.Body.Len(), len(rawDev1))
		}
		if got := rec.Header().Get("X-Bdrive-Tail-Offset"); got != "" {
			t.Fatalf("mismatch fallback returned unexpected X-Bdrive-Tail-Offset=%q", got)
		}
	})

	t.Run("CaseA_ConsecutiveStoreAppendNewFiles", func(t *testing.T) {
		be, h, apiBase, rawDev1, blob := setupABHub(t, "dev1")

		t0 := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
		line1, err := journal.Marshal([]journal.Op{{
			Seq: 10001, Lamport: 10001, Time: t0, Device: "dev1", User: "alice@example.com",
			Kind: journal.KindPut, Path: "notes/brand-new-1.md", Blob: blob, Size: 14, Mode: 0o644,
		}})
		if err != nil {
			t.Fatal(err)
		}
		line2, err := journal.Marshal([]journal.Op{{
			Seq: 10002, Lamport: 10002, Time: t0.Add(time.Second), Device: "dev1", User: "alice@example.com",
			Kind: journal.KindPut, Path: "notes/brand-new-2.md", Blob: blob, Size: 14, Mode: 0o644,
		}})
		if err != nil {
			t.Fatal(err)
		}

		s0 := int64(len(rawDev1))
		h0 := abSHA256(rawDev1)
		if r1 := abDo(t, h, http.MethodPost, fmt.Sprintf("%sstore/append?key=journal/dev1.jsonl&prev_size=%d&prev_sha256=%s", apiBase, s0, h0), line1, http.Header{"X-Bdrive-Device": {"dev1"}}); r1.Code != http.StatusOK {
			t.Fatalf("append 1: %d %s", r1.Code, r1.Body)
		}
		joined1 := append(append([]byte(nil), rawDev1...), line1...)
		s1 := int64(len(joined1))
		h1 := abSHA256(joined1)
		if r2 := abDo(t, h, http.MethodPost, fmt.Sprintf("%sstore/append?key=journal/dev1.jsonl&prev_size=%d&prev_sha256=%s", apiBase, s1, h1), line2, http.Header{"X-Bdrive-Device": {"dev1"}}); r2.Code != http.StatusOK {
			t.Fatalf("append 2: %d %s", r2.Code, r2.Body)
		}

		be.resetStats()
		treeRec := abDo(t, h, http.MethodGet, apiBase+"tree", nil, nil)
		if treeRec.Code != http.StatusOK || !strings.Contains(treeRec.Body.String(), "brand-new-2.md") {
			t.Fatalf("tree after appends: %d (contains brand-new-2=%v)", treeRec.Code, strings.Contains(treeRec.Body.String(), "brand-new-2.md"))
		}
		histRec := abDo(t, h, http.MethodGet, apiBase+"history?n=5", nil, nil)
		var histOut struct {
			Entries []webapp.HistoryEntry `json:"entries"`
		}
		if histRec.Code != http.StatusOK || json.Unmarshal(histRec.Body.Bytes(), &histOut) != nil || len(histOut.Entries) == 0 || histOut.Entries[0].Path != "notes/brand-new-2.md" {
			t.Fatalf("history after appends: %d %s", histRec.Code, histRec.Body)
		}
		empRec := abDo(t, h, http.MethodGet, apiBase+"history/emptied", nil, nil)
		if empRec.Code != http.StatusOK {
			t.Fatalf("history/emptied after appends: %d %s", empRec.Code, empRec.Body)
		}
		tailURL := fmt.Sprintf("%sstore/object?key=journal/dev1.jsonl&offset=%d&prefix_sha256=%s", apiBase, s0, h0)
		tailRec := abDo(t, h, http.MethodGet, tailURL, nil, nil)
		wantTail := append(append([]byte(nil), line1...), line2...)
		if tailRec.Code != http.StatusOK || tailRec.Header().Get("X-Bdrive-Tail-Offset") != strconv.FormatInt(s0, 10) || !bytes.Equal(tailRec.Body.Bytes(), wantTail) {
			t.Fatalf("tail get after 2 appends: code=%d off=%q bodyLen=%d want=%d", tailRec.Code, tailRec.Header().Get("X-Bdrive-Tail-Offset"), tailRec.Body.Len(), len(wantTail))
		}

		fg, fb, _, rb := be.stats()
		if fg != 0 || fb != 0 {
			t.Fatalf("Case A re-downloaded whole journal: fullGets=%d fullGetBytes=%d (rangeBytes=%d)", fg, fb, rb)
		}
		if rb > 8192 {
			t.Fatalf("Case A downloaded too many range bytes: %d > 8192", rb)
		}
	})

	t.Run("CaseB_BrowserUploadContentNewFile", func(t *testing.T) {
		be, h, apiBase, _, _ := setupABHub(t, "web-hub")

		be.resetStats()
		upRec := abDo(t, h, http.MethodPut, apiBase+"upload/content?path=notes/browser-new.md", []byte("browser content"), nil)
		if upRec.Code != http.StatusOK {
			t.Fatalf("upload/content: %d %s", upRec.Code, upRec.Body)
		}
		for _, path := range []string{"tree", "history?n=5", "history/emptied"} {
			if r := abDo(t, h, http.MethodGet, apiBase+path, nil, nil); r.Code != http.StatusOK {
				t.Fatalf("after upload %s: %d %s", path, r.Code, r.Body)
			}
		}
		fg, fb, _, rb := be.stats()
		if fg != 0 || fb != 0 {
			t.Fatalf("Case B re-downloaded whole journal after upload/content: fullGets=%d fullGetBytes=%d (rangeBytes=%d)", fg, fb, rb)
		}
		if rb > 8192 {
			t.Fatalf("Case B downloaded too many range bytes: %d > 8192", rb)
		}
	})
}
