package webapp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

type tailQAMemObj struct {
	data []byte
	ver  int64
	mod  time.Time
	crc  uint32
	meta map[string]string
}

type tailQAByteReader struct {
	r     io.Reader
	onInc func(n int64)
}

func (b *tailQAByteReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 && b.onInc != nil {
		b.onInc(int64(n))
	}
	return n, err
}

func (b *tailQAByteReader) Close() error { return nil }

type tailTestMetaBackend struct {
	mu            sync.Mutex
	objs          map[string]tailQAMemObj
	nextVer       int64
	clock         time.Time
	immutable     bool
	fullGets      int
	fullGetBytes  int64
	rangeGets     int
	rangeGetBytes int64
}

func newTailTestMetaBackend(immutable bool) *tailTestMetaBackend {
	return &tailTestMetaBackend{
		objs:      make(map[string]tailQAMemObj),
		clock:     time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC),
		immutable: immutable,
	}
}

func copyQAMeta(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[strings.ToLower(k)] = v
	}
	return out
}

func (b *tailTestMetaBackend) resetStats() {
	b.mu.Lock()
	b.fullGets = 0
	b.fullGetBytes = 0
	b.rangeGets = 0
	b.rangeGetBytes = 0
	b.mu.Unlock()
}

func (b *tailTestMetaBackend) stats() (fullGets int, fullBytes int64, rangeGets int, rangeBytes int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fullGets, b.fullGetBytes, b.rangeGets, b.rangeGetBytes
}

func (b *tailTestMetaBackend) isJournalKey(key string) bool {
	return strings.Contains(key, "journal/") && strings.HasSuffix(key, ".jsonl")
}

func (b *tailTestMetaBackend) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextVer++
	b.clock = b.clock.Add(time.Second)
	b.objs[key] = tailQAMemObj{
		data: data,
		ver:  b.nextVer,
		mod:  b.clock,
		crc:  crc32cSum(data),
	}
	return nil
}

func (b *tailTestMetaBackend) Get(_ context.Context, key string) (io.ReadCloser, error) {
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
	return &tailQAByteReader{
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

func (b *tailTestMetaBackend) List(_ context.Context, prefix string) ([]remote.Object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []remote.Object
	for k, obj := range b.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, remote.Object{
				Key:              k,
				Size:             int64(len(obj.data)),
				Modified:         obj.mod,
				Version:          remote.Version(strconv.FormatInt(obj.ver, 10)),
				ImmutableVersion: b.immutable,
				CRC32C:           obj.crc,
			})
		}
	}
	return out, nil
}

func (b *tailTestMetaBackend) Exists(_ context.Context, key string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.objs[key]
	return ok, nil
}

func (b *tailTestMetaBackend) Close() error { return nil }

func (b *tailTestMetaBackend) metaOfLocked(obj tailQAMemObj) remote.ObjectMeta {
	return remote.ObjectMeta{
		Version:  remote.Version(strconv.FormatInt(obj.ver, 10)),
		Size:     int64(len(obj.data)),
		CRC32C:   obj.crc,
		Metadata: copyQAMeta(obj.meta),
	}
}

func (b *tailTestMetaBackend) GetVersioned(ctx context.Context, key string) (io.ReadCloser, remote.Version, error) {
	rc, meta, err := b.GetVersionedWithMeta(ctx, key)
	return rc, meta.Version, err
}

func (b *tailTestMetaBackend) GetVersionedWithMeta(_ context.Context, key string) (io.ReadCloser, remote.ObjectMeta, error) {
	b.mu.Lock()
	obj, ok := b.objs[key]
	if ok && b.isJournalKey(key) {
		b.fullGets++
	}
	var meta remote.ObjectMeta
	if ok {
		meta = b.metaOfLocked(obj)
	}
	b.mu.Unlock()
	if !ok {
		return nil, remote.ObjectMeta{}, os.ErrNotExist
	}
	cp := append([]byte(nil), obj.data...)
	return &tailQAByteReader{
		r: bytes.NewReader(cp),
		onInc: func(n int64) {
			if b.isJournalKey(key) {
				b.mu.Lock()
				b.fullGetBytes += n
				b.mu.Unlock()
			}
		},
	}, meta, nil
}

func (b *tailTestMetaBackend) PutIf(ctx context.Context, key string, r io.Reader, size int64, expect remote.Version) (remote.Version, error) {
	meta, err := b.PutIfWithMeta(ctx, key, r, size, expect, nil)
	return meta.Version, err
}

func (b *tailTestMetaBackend) PutIfWithMeta(_ context.Context, key string, r io.Reader, size int64, expect remote.Version, meta map[string]string) (remote.ObjectMeta, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return remote.ObjectMeta{}, err
	}
	if size >= 0 && int64(len(data)) != size {
		return remote.ObjectMeta{}, fmt.Errorf("size mismatch: got %d want %d", len(data), size)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	obj, exists := b.objs[key]
	cur := remote.VersionAbsent
	if exists {
		cur = remote.Version(strconv.FormatInt(obj.ver, 10))
	}
	if cur != expect {
		return remote.ObjectMeta{}, remote.ErrVersionMismatch
	}
	b.nextVer++
	b.clock = b.clock.Add(time.Second)
	nextObj := tailQAMemObj{
		data: data,
		ver:  b.nextVer,
		mod:  b.clock,
		crc:  crc32cSum(data),
		meta: copyQAMeta(meta),
	}
	b.objs[key] = nextObj
	return b.metaOfLocked(nextObj), nil
}

func (b *tailTestMetaBackend) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	rc, _, err := b.GetVersionedRange(ctx, key, offset, length)
	return rc, err
}

func (b *tailTestMetaBackend) GetVersionedRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, remote.Version, error) {
	rc, meta, err := b.GetVersionedRangeWithMeta(ctx, key, offset, length)
	return rc, meta.Version, err
}

func (b *tailTestMetaBackend) GetVersionedRangeWithMeta(_ context.Context, key string, offset, length int64) (io.ReadCloser, remote.ObjectMeta, error) {
	if offset < 0 {
		return nil, remote.ObjectMeta{}, fmt.Errorf("invalid offset %d", offset)
	}
	b.mu.Lock()
	obj, ok := b.objs[key]
	if ok && b.isJournalKey(key) {
		b.rangeGets++
	}
	var meta remote.ObjectMeta
	if ok {
		meta = b.metaOfLocked(obj)
	}
	b.mu.Unlock()
	if !ok {
		return nil, remote.ObjectMeta{}, os.ErrNotExist
	}
	sz := int64(len(obj.data))
	if offset > sz {
		return nil, remote.ObjectMeta{}, fmt.Errorf("range offset %d > size %d", offset, sz)
	}
	if offset == sz || length == 0 {
		return io.NopCloser(bytes.NewReader(nil)), meta, nil
	}
	end := sz
	if length >= 0 && offset+length < end {
		end = offset + length
	}
	cp := append([]byte(nil), obj.data[offset:end]...)
	return &tailQAByteReader{
		r: bytes.NewReader(cp),
		onInc: func(n int64) {
			if b.isJournalKey(key) {
				b.mu.Lock()
				b.rangeGetBytes += n
				b.mu.Unlock()
			}
		},
	}, meta, nil
}

var (
	_ remote.Backend              = (*tailTestMetaBackend)(nil)
	_ remote.ConditionalPutter    = (*tailTestMetaBackend)(nil)
	_ remote.RangeGetter          = (*tailTestMetaBackend)(nil)
	_ remote.VersionedRangeGetter = (*tailTestMetaBackend)(nil)
	_ remote.VersionedMetaBackend = (*tailTestMetaBackend)(nil)
)

// vrgOnlyTestBackend exposes VersionedRangeGetter + ConditionalPutter without
// VersionedMetaBackend so local_cas gating in RemoteSource is directly exercised.
type vrgOnlyTestBackend struct {
	inner *tailTestMetaBackend
}

func (b *vrgOnlyTestBackend) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	return b.inner.Put(ctx, key, r, size)
}
func (b *vrgOnlyTestBackend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return b.inner.Get(ctx, key)
}
func (b *vrgOnlyTestBackend) List(ctx context.Context, prefix string) ([]remote.Object, error) {
	objs, err := b.inner.List(ctx, prefix)
	for i := range objs {
		objs[i].Version = ""
		objs[i].ImmutableVersion = false
		objs[i].CRC32C = 0
	}
	return objs, err
}
func (b *vrgOnlyTestBackend) Exists(ctx context.Context, key string) (bool, error) {
	return b.inner.Exists(ctx, key)
}
func (b *vrgOnlyTestBackend) Close() error { return nil }
func (b *vrgOnlyTestBackend) GetVersioned(ctx context.Context, key string) (io.ReadCloser, remote.Version, error) {
	return b.inner.GetVersioned(ctx, key)
}
func (b *vrgOnlyTestBackend) PutIf(ctx context.Context, key string, r io.Reader, size int64, expect remote.Version) (remote.Version, error) {
	return b.inner.PutIf(ctx, key, r, size, expect)
}
func (b *vrgOnlyTestBackend) GetVersionedRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, remote.Version, error) {
	return b.inner.GetVersionedRange(ctx, key, offset, length)
}

func seedLargeMetaJournal(t *testing.T, be remote.Backend, projID, devID string, n int) ([]byte, string) {
	t.Helper()
	content := []byte("seed file body")
	blob := qaHexSHA(content)
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
		t.Fatalf("seeded size %d < 2 MiB", len(raw))
	}
	if err := be.Put(context.Background(), projID+"/journal/"+devID+".jsonl", bytes.NewReader(raw), int64(len(raw))); err != nil {
		t.Fatal(err)
	}
	return raw, blob
}

func tailQARequest(t *testing.T, h http.Handler, method, target string, body []byte, hdr http.Header) *httptest.ResponseRecorder {
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

func TestQA07_MetaChainTailMultiInstanceAndLongHistory(t *testing.T) {
	t.Run("MultiInstance20AppendsPreservesAnchorAndZeroFullGet", func(t *testing.T) {
		be := newTailTestMetaBackend(true)
		dbPath := filepath.Join(t.TempDir(), "projects.json")
		dbA, err := OpenProjectDB(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		p, _, err := dbA.GetOrCreate("qa07", "")
		if err != nil {
			t.Fatal(err)
		}
		raw, blob := seedLargeMetaJournal(t, be, p.ID, "dev1", 10000)

		srvA := &Server{
			Root: be, Projects: dbA,
			Device: Identity{ID: "web-a", Name: "Web A", Author: "hub@example.com"},
			Upload: UploadConfig{Enabled: true},
		}
		dbB, err := OpenProjectDB(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		srvB := &Server{
			Root: be, Projects: dbB,
			Device: Identity{ID: "web-b", Name: "Web B", Author: "hub@example.com"},
			Upload: UploadConfig{Enabled: true},
		}
		hA, hB := srvA.Handler(), srvB.Handler()
		api := "/api/p/" + p.ID + "/"

		// Warm both Hub A and Hub B at initial 2 MiB state.
		for _, h := range []http.Handler{hA, hB} {
			for _, u := range []string{"tree", "history?n=5", "history/emptied"} {
				if rec := tailQARequest(t, h, http.MethodGet, api+u, nil, nil); rec.Code != http.StatusOK {
					t.Fatalf("warmup %s: %d %s", u, rec.Code, rec.Body)
				}
			}
		}

		// Hub A performs 20 consecutive /store/append writes (> maxMetaChainPrefixes=14).
		cur := append([]byte(nil), raw...)
		ckptOffset := int64(len(cur))
		ckptSHA := qaHexSHA(cur)
		t0 := time.Date(2026, 11, 20, 2, 0, 0, 0, time.UTC)
		var expectedTail []byte
		for step := 1; step <= 20; step++ {
			line, err := journal.Marshal([]journal.Op{{
				Seq:     int64(10000 + step),
				Lamport: int64(10000 + step),
				Time:    t0.Add(time.Duration(step) * time.Second),
				Device:  "dev1",
				User:    "alice@example.com",
				Kind:    journal.KindPut,
				Path:    fmt.Sprintf("notes/tail-%02d.md", step),
				Blob:    blob,
				Size:    14,
				Mode:    0o644,
			}})
			if err != nil {
				t.Fatal(err)
			}
			u := fmt.Sprintf("%sstore/append?key=journal/dev1.jsonl&prev_size=%d&prev_sha256=%s", api, len(cur), qaHexSHA(cur))
			if rec := tailQARequest(t, hA, http.MethodPost, u, line, http.Header{"X-Bdrive-Device": {"dev1"}}); rec.Code != http.StatusOK {
				t.Fatalf("append %d: %d %s", step, rec.Code, rec.Body)
			}
			cur = append(cur, line...)
			expectedTail = append(expectedTail, line...)

			// Interleaved reader call on Hub A must NOT wipe storedMax/hasAppend in jverCache!
			if step == 1 || step == 20 {
				tailGet := fmt.Sprintf("%sstore/object?key=journal/dev1.jsonl&offset=%d&prefix_sha256=%s", api, ckptOffset, ckptSHA)
				rec := tailQARequest(t, hA, http.MethodGet, tailGet, nil, nil)
				if rec.Code != http.StatusOK || rec.Header().Get("X-Bdrive-Tail-Offset") != strconv.FormatInt(ckptOffset, 10) {
					t.Fatalf("Hub A interleaved tail GET step %d: code=%d off=%q", step, rec.Code, rec.Header().Get("X-Bdrive-Tail-Offset"))
				}
			}
		}

		// Hub B has NEVER seen those 20 writes in jverCache.
		// Everything must still resolve using atomic bdrive-append-chain metadata + range GETs!
		be.resetStats()
		if rec := tailQARequest(t, hB, http.MethodGet, api+"tree", nil, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "tail-20.md") {
			t.Fatalf("Hub B tree: %d contains=%v", rec.Code, strings.Contains(rec.Body.String(), "tail-20.md"))
		}
		if rec := tailQARequest(t, hB, http.MethodGet, api+"history?n=5", nil, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "notes/tail-20.md") {
			t.Fatalf("Hub B history: %d %s", rec.Code, rec.Body)
		}
		// Hub B serves GET /store/object from checkpoint-era anchor offset BEFORE all 20 writes!
		tailGet := fmt.Sprintf("%sstore/object?key=journal/dev1.jsonl&offset=%d&prefix_sha256=%s", api, ckptOffset, ckptSHA)
		rec := tailQARequest(t, hB, http.MethodGet, tailGet, nil, nil)
		if rec.Code != http.StatusOK || rec.Header().Get("X-Bdrive-Tail-Offset") != strconv.FormatInt(ckptOffset, 10) || !bytes.Equal(rec.Body.Bytes(), expectedTail) {
			t.Fatalf("Hub B anchor tail GET: code=%d off=%q len=%d want=%d", rec.Code, rec.Header().Get("X-Bdrive-Tail-Offset"), rec.Body.Len(), len(expectedTail))
		}
		fg, fb, rg, rb := be.stats()
		if fg != 0 || fb != 0 {
			t.Fatalf("Hub B did full GETs after 20 remote appends: fullGets=%d (%d B), rangeGets=%d (%d B)", fg, fb, rg, rb)
		}
		if rg != 3 || rb != int64(len(expectedTail))*3+1 {
			t.Fatalf("unexpected Hub B range stats: rangeGets=%d rangeBytes=%d (want 3 gets totaling %d B)", rg, rb, int64(len(expectedTail))*3+1)
		}

		// Verify Hub B's jbytes invariant equals exact sum of c.bytes (tryIncrementalFold
		// served tree without needing parsedJournals, so jcache is still at warmup raw).
		_, vB, err := srvB.projectVolume(p.ID)
		if err != nil {
			t.Fatal(err)
		}
		rsB := vB.source.(*RemoteSource)
		rsB.jmu.Lock()
		var sum int64
		for _, c := range rsB.jcache {
			sum += c.bytes
		}
		jbytes := rsB.jbytes
		rsB.jmu.Unlock()
		if jbytes != sum || sum != int64(len(raw)) {
			t.Fatalf("Hub B pre-parsedJournals jbytes=%d sum=%d want=%d", jbytes, sum, len(raw))
		}

		// Calling parsedJournals directly on Hub B must also update jcache via range GETs only.
		be.resetStats()
		objsB, err := rsB.listJournals(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rsB.parsedJournals(context.Background(), objsB); err != nil {
			t.Fatal(err)
		}
		if fgB, fbB, rgB, _ := be.stats(); fgB != 0 || fbB != 0 || rgB != 1 {
			t.Fatalf("Hub B parsedJournals after incremental appends: full=%d(%d) range=%d", fgB, fbB, rgB)
		}
		rsB.jmu.Lock()
		sum = 0
		for _, c := range rsB.jcache {
			sum += c.bytes
		}
		jbytes = rsB.jbytes
		rsB.jmu.Unlock()
		if jbytes != sum || sum != int64(len(cur)) {
			t.Fatalf("Hub B post-parsedJournals jbytes=%d sum=%d want=%d", jbytes, sum, len(cur))
		}

		// Verify Hub A's jverCache still kept hasAppend & storedMax after interleaved lookupVerifiedPrefixForGet.
		_, vA, err := srvA.projectVolume(p.ID)
		if err != nil {
			t.Fatal(err)
		}
		rsA := vA.source.(*RemoteSource)
		be.mu.Lock()
		latestObj := be.objs[p.ID+"/journal/dev1.jsonl"]
		be.mu.Unlock()
		metaEntry, has := rsA.loadJournalAppendMeta("journal/dev1.jsonl", remote.Version(strconv.FormatInt(latestObj.ver, 10)))
		if !has || metaEntry.storedMax != 10020 {
			t.Fatalf("Hub A lost append metadata after reader lookup: has=%v storedMax=%d", has, metaEntry.storedMax)
		}
	})

	t.Run("TamperedMutableVersionRejected409AndFailClosed", func(t *testing.T) {
		be := newTailTestMetaBackend(false)
		db, err := OpenProjectDB(filepath.Join(t.TempDir(), "projects.json"))
		if err != nil {
			t.Fatal(err)
		}
		p, _, err := db.GetOrCreate("qa07-mutable", "")
		if err != nil {
			t.Fatal(err)
		}
		raw, blob := seedLargeMetaJournal(t, be, p.ID, "dev1", 10000)
		srv := &Server{
			Root: be, Projects: db,
			Device: Identity{ID: "web-hub", Name: "Web Hub", Author: "hub@example.com"},
			Upload: UploadConfig{Enabled: true},
		}
		h := srv.Handler()
		api := "/api/p/" + p.ID + "/"

		line1, _ := journal.Marshal([]journal.Op{{
			Seq: 10001, Lamport: 10001, Time: time.Date(2026, 11, 20, 2, 0, 0, 0, time.UTC),
			Device: "dev1", User: "alice@example.com", Kind: journal.KindPut, Path: "notes/a.md", Blob: blob, Size: 14, Mode: 0o644,
		}})
		u1 := fmt.Sprintf("%sstore/append?key=journal/dev1.jsonl&prev_size=%d&prev_sha256=%s", api, len(raw), qaHexSHA(raw))
		if rec := tailQARequest(t, h, http.MethodPost, u1, line1, http.Header{"X-Bdrive-Device": {"dev1"}}); rec.Code != http.StatusOK {
			t.Fatalf("append 1: %d %s", rec.Code, rec.Body)
		}

		// Tamper stored byte 0 in place WITHOUT bumping version (mutable version simulation).
		fullKey := p.ID + "/journal/dev1.jsonl"
		be.mu.Lock()
		obj := be.objs[fullKey]
		joined := append([]byte(nil), obj.data...)
		obj.data[0] = 'X'
		be.objs[fullKey] = obj
		be.mu.Unlock()

		line2, _ := journal.Marshal([]journal.Op{{
			Seq: 10002, Lamport: 10002, Time: time.Date(2026, 11, 20, 2, 0, 1, 0, time.UTC),
			Device: "dev1", User: "alice@example.com", Kind: journal.KindPut, Path: "notes/b.md", Blob: blob, Size: 14, Mode: 0o644,
		}})
		u2 := fmt.Sprintf("%sstore/append?key=journal/dev1.jsonl&prev_size=%d&prev_sha256=%s", api, len(joined), qaHexSHA(joined))
		rec := tailQARequest(t, h, http.MethodPost, u2, line2, http.Header{"X-Bdrive-Device": {"dev1"}})
		if rec.Code != http.StatusConflict {
			t.Fatalf("tampered mutable prefix must be 409 Conflict, got %d %s", rec.Code, rec.Body)
		}

		// readVerifiedJournalTail must also fail closed on version mismatch or midstate SHA mismatch.
		_, v, _ := srv.projectVolume(p.ID)
		rs := v.source.(*RemoteSource)
		wrongVerObj := remote.Object{Key: "journal/dev1.jsonl", Size: int64(len(obj.data)), Version: "wrong-ver"}
		if _, ok := rs.readVerifiedJournalTail(context.Background(), wrongVerObj, int64(len(raw)), qaHexSHA(raw), nil, nlTrue); ok {
			t.Fatal("readVerifiedJournalTail accepted mismatched Version")
		}
	})
}

func TestQA08_MissingNewlineBoundaryNormalizationAndFailClosed(t *testing.T) {
	ctx := context.Background()
	be := newTailTestMetaBackend(true)
	rs := &RemoteSource{Backend: be}
	cp := be

	// 1. Existing journal WITHOUT trailing newline -> appendConditionalExisting must insert '\n'.
	firstNoNL := []byte(`{"seq":1,"lamport":1,"time":"2026-11-20T00:00:00Z","device":"hub","kind":"put","path":"a.md","blob":"` + strings.Repeat("a", 64) + `","size":1}`)
	if err := be.Put(ctx, "journal/hub.jsonl", bytes.NewReader(firstNoNL), int64(len(firstNoNL))); err != nil {
		t.Fatal(err)
	}
	err := rs.appendConditionalExisting(ctx, cp, "journal/hub.jsonl", func(_ []byte) ([]byte, error) {
		return []byte(`{"seq":2,"lamport":2,"time":"2026-11-20T00:00:01Z","device":"hub","kind":"put","path":"b.md","blob":"` + strings.Repeat("b", 64) + `","size":1}` + "\n"), nil
	})
	if err != nil {
		t.Fatalf("appendConditionalExisting: %v", err)
	}
	be.mu.Lock()
	stored := be.objs["journal/hub.jsonl"]
	be.mu.Unlock()
	ops, err := journal.Parse(stored.data)
	if err != nil || len(ops) != 2 {
		t.Fatalf("expected 2 valid ops after missing-NL normalization, got len=%d err=%v raw=%q", len(ops), err, stored.data)
	}
	// Since prevLineBoundary was false at len(firstNoNL), lookupVerifiedTail with requireLineBoundary=true MUST refuse.
	meta := remote.ObjectMeta{
		Version:  remote.Version(strconv.FormatInt(stored.ver, 10)),
		Size:     int64(len(stored.data)),
		CRC32C:   stored.crc,
		Metadata: copyQAMeta(stored.meta),
	}
	if _, _, ok := rs.lookupVerifiedTail(ctxKey("journal/hub.jsonl"), meta, int64(len(firstNoNL)), qaHexSHA(firstNoNL), true); ok {
		t.Fatal("lookupVerifiedTail accepted offset with prevLineBoundary=false when requireLineBoundary=true")
	}

	// Cold RemoteSource calling readVerifiedJournalTail with nlUnknown also proves offset-1 byte is NOT '\n' and fails closed.
	coldRS := &RemoteSource{Backend: be}
	listObj := remote.Object{
		Key:              "journal/hub.jsonl",
		Size:             int64(len(stored.data)),
		Modified:         stored.mod,
		Version:          meta.Version,
		ImmutableVersion: true,
		CRC32C:           stored.crc,
	}
	if _, ok := coldRS.readVerifiedJournalTail(ctx, listObj, int64(len(firstNoNL)), qaHexSHA(firstNoNL), nil, nlUnknown); ok {
		t.Fatal("readVerifiedJournalTail succeeded across missing newline at prevSize-1")
	}

	// 2. appendLastWriterWins must also insert missing newline.
	lwwBe := newTailTestMetaBackend(false)
	lwwRS := &RemoteSource{Backend: lwwBe}
	if err := lwwBe.Put(ctx, "journal/lww.jsonl", bytes.NewReader(firstNoNL), int64(len(firstNoNL))); err != nil {
		t.Fatal(err)
	}
	line2 := []byte(`{"seq":2,"lamport":2,"time":"2026-11-20T00:00:01Z","device":"hub","kind":"put","path":"c.md","blob":"` + strings.Repeat("c", 64) + `","size":1}` + "\n")
	if err := lwwRS.appendLastWriterWins(ctx, "journal/lww.jsonl", line2); err != nil {
		t.Fatalf("appendLastWriterWins: %v", err)
	}
	lwwBe.mu.Lock()
	lwwRaw := lwwBe.objs["journal/lww.jsonl"].data
	lwwBe.mu.Unlock()
	if lwwOps, err := journal.Parse(lwwRaw); err != nil || len(lwwOps) != 2 {
		t.Fatalf("appendLastWriterWins failed to normalize missing newline: len=%d err=%v", len(lwwOps), err)
	}
}

func ctxKey(k string) string { return k }

func TestQA09_JournalCacheSliceClippingBudgetAndRace(t *testing.T) {
	ctx := context.Background()
	be := newTailTestMetaBackend(true)
	rs := &RemoteSource{Backend: be}
	blob := strings.Repeat("a", 64)

	var raw []byte
	t0 := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)
	for i := 1; i <= 50; i++ {
		l, _ := journal.Marshal([]journal.Op{{
			Seq: int64(i), Lamport: int64(i), Time: t0.Add(time.Duration(i) * time.Second),
			Device: "dev1", Kind: journal.KindPut, Path: fmt.Sprintf("f-%03d.md", i), Blob: blob, Size: 5,
		}})
		raw = append(raw, l...)
	}
	if _, err := putIfWithMeta(ctx, be, be, "journal/dev1.jsonl", bytes.NewReader(raw), int64(len(raw)), remote.VersionAbsent, nil); err != nil {
		t.Fatal(err)
	}

	objs, err := rs.listJournals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := rs.parsedJournals(ctx, objs)
	if err != nil || len(parsed) != 1 || len(parsed[0]) != 50 {
		t.Fatalf("initial parsedJournals: len=%d err=%v", len(parsed), err)
	}
	if cap(parsed[0]) != len(parsed[0]) {
		t.Fatalf("parsedJournals returned unclipped slice: len=%d cap=%d", len(parsed[0]), cap(parsed[0]))
	}

	// Append repeatedly with metadata chain so reuseCur grows internal spare capacity.
	prevMeta, _ := func() (remote.ObjectMeta, error) {
		_, m, err := be.GetVersionedWithMeta(ctx, "journal/dev1.jsonl")
		return m, err
	}()
	prevSize := int64(len(raw))
	prevSHA := qaHexSHA(raw)

	var concurrentErr error
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				curObjs, err := rs.listJournals(ctx)
				if err != nil {
					concurrentErr = err
					return
				}
				res, err := rs.parsedJournals(ctx, curObjs)
				if err != nil {
					concurrentErr = err
					return
				}
				for _, ops := range res {
					if cap(ops) != len(ops) {
						concurrentErr = fmt.Errorf("concurrent reader saw unclipped slice len=%d cap=%d", len(ops), cap(ops))
						return
					}
					// Appending to returned slice must allocate a new array instead of corrupting jcache.
					_ = append(ops, journal.Op{Seq: 99999, Path: "evil.md"})
				}
			}
		}
	}()

	for step := 1; step <= 8; step++ {
		tail, _ := journal.Marshal([]journal.Op{{
			Seq: int64(50 + step), Lamport: int64(50 + step), Time: t0.Add(time.Duration(50+step) * time.Second),
			Device: "dev1", Kind: journal.KindPut, Path: fmt.Sprintf("f-%03d.md", 50+step), Blob: blob, Size: 5,
		}})
		raw = append(raw, tail...)
		fullSHA := qaHexSHA(raw)
		fullCRC := crc32cSum(raw)
		prefixes, putMeta := rs.prepareJournalWriteMeta("journal/dev1.jsonl", prevMeta, prevSize, prevSHA, true, int64(len(raw)), fullSHA, true, fullCRC, true)
		newMeta, err := putIfWithMeta(ctx, be, be, "journal/dev1.jsonl", bytes.NewReader(raw), int64(len(raw)), prevMeta.Version, putMeta)
		if err != nil {
			close(stop)
			wg.Wait()
			t.Fatal(err)
		}
		rs.storeJournalWriteMeta("journal/dev1.jsonl", newMeta.Version, int64(len(raw)), fullSHA, fullCRC, int64(50+step), true, true, prevSize, prevSHA, true, prefixes)
		prevMeta = newMeta
		prevSize = int64(len(raw))
		prevSHA = fullSHA

		curObjs, err := rs.listJournals(ctx)
		if err != nil {
			close(stop)
			wg.Wait()
			t.Fatal(err)
		}
		p, err := rs.parsedJournals(ctx, curObjs)
		if err != nil {
			close(stop)
			wg.Wait()
			t.Fatal(err)
		}
		if len(p[0]) != 50+step || cap(p[0]) != len(p[0]) {
			close(stop)
			wg.Wait()
			t.Fatalf("step %d: len=%d cap=%d", step, len(p[0]), cap(p[0]))
		}
		for _, op := range p[0] {
			if op.Path == "evil.md" {
				close(stop)
				wg.Wait()
				t.Fatal("caller append leaked into jcache slice backing array")
			}
		}
	}
	close(stop)
	wg.Wait()
	if concurrentErr != nil {
		t.Fatal(concurrentErr)
	}

	rs.jmu.Lock()
	var sum int64
	for _, c := range rs.jcache {
		sum += c.bytes
	}
	jbytes := rs.jbytes
	rs.jmu.Unlock()
	if jbytes != sum || sum != int64(len(raw)) {
		t.Fatalf("jbytes=%d sum=%d want=%d", jbytes, sum, len(raw))
	}
}

func TestQA10_HandleStorePutMetaChain(t *testing.T) {
	be := newTailTestMetaBackend(true)
	db, err := OpenProjectDB(filepath.Join(t.TempDir(), "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := db.GetOrCreate("qa10", "")
	if err != nil {
		t.Fatal(err)
	}
	blob := strings.Repeat("d", 64)
	_ = be.Put(context.Background(), p.ID+"/blobs/"+blob, strings.NewReader("x"), 1)
	srv := &Server{Root: be, Projects: db, Upload: UploadConfig{Enabled: true}}
	h := srv.Handler()
	api := "/api/p/" + p.ID + "/"

	line1 := []byte(fmt.Sprintf(`{"seq":1,"lamport":1,"time":"2026-11-20T00:00:00Z","device":"dev1","kind":"put","path":"a.md","blob":%q,"size":1}`+"\n", blob))
	line2 := []byte(fmt.Sprintf(`{"seq":2,"lamport":2,"time":"2026-11-20T00:00:01Z","device":"dev1","kind":"put","path":"b.md","blob":%q,"size":1}`+"\n", blob))

	// 1. Initial PUT creates journal/dev1.jsonl.
	if rec := tailQARequest(t, h, http.MethodPut, api+"store/object?key=journal/dev1.jsonl", line1, http.Header{"X-Bdrive-Device": {"dev1"}}); rec.Code != http.StatusOK {
		t.Fatalf("put 1: %d %s", rec.Code, rec.Body)
	}

	// 2. Prefix-extending PUT keeps chain=true pointing at len(line1).
	joined := append(append([]byte(nil), line1...), line2...)
	if rec := tailQARequest(t, h, http.MethodPut, api+"store/object?key=journal/dev1.jsonl", joined, http.Header{"X-Bdrive-Device": {"dev1"}}); rec.Code != http.StatusOK {
		t.Fatalf("put 2 (prefix extend): %d %s", rec.Code, rec.Body)
	}
	be.mu.Lock()
	obj2 := be.objs[p.ID+"/journal/dev1.jsonl"]
	be.mu.Unlock()
	chain2, ok := parseJournalAppendChain(remote.ObjectMeta{Size: int64(len(joined)), CRC32C: obj2.crc, Metadata: obj2.meta})
	if !ok || chain2.prefixes[int64(len(line1))].sha != qaHexSHA(line1) {
		t.Fatalf("prefix-extending PUT did not preserve prior prefix in chain: ok=%v chain=%+v", ok, chain2)
	}

	// 3. Idempotent PUT returns 200.
	if rec := tailQARequest(t, h, http.MethodPut, api+"store/object?key=journal/dev1.jsonl", joined, http.Header{"X-Bdrive-Device": {"dev1"}}); rec.Code != http.StatusOK {
		t.Fatalf("put 3 (idempotent): %d %s", rec.Code, rec.Body)
	}

	// 4. Valid non-prefix rewrite (same Seq ops, extra JSON spacing on line1) clears stale prefix offsets (chain=false).
	line1Alt := []byte(fmt.Sprintf(`{"seq": 1, "lamport":1, "time":"2026-11-20T00:00:00Z", "device":"dev1", "kind":"put", "path":"a.md", "blob":%q, "size":1}`+"\n", blob))
	rewritten := append(append([]byte(nil), line1Alt...), line2...)
	if rec := tailQARequest(t, h, http.MethodPut, api+"store/object?key=journal/dev1.jsonl", rewritten, http.Header{"X-Bdrive-Device": {"dev1"}}); rec.Code != http.StatusOK {
		t.Fatalf("put 4 (non-prefix rewrite): %d %s", rec.Code, rec.Body)
	}
	be.mu.Lock()
	obj4 := be.objs[p.ID+"/journal/dev1.jsonl"]
	be.mu.Unlock()
	chain4, ok := parseJournalAppendChain(remote.ObjectMeta{Size: int64(len(rewritten)), CRC32C: obj4.crc, Metadata: obj4.meta})
	if !ok || len(chain4.prefixes) != 1 {
		t.Fatalf("non-prefix rewrite must discard prior prefix offsets (chain=false), got ok=%v len=%d", ok, len(chain4.prefixes))
	}
}

func TestQA11_FilteredJournalTailGetAndStreamCap(t *testing.T) {
	ctx := context.Background()
	be := newTailTestMetaBackend(true)
	db, err := OpenProjectDB(filepath.Join(t.TempDir(), "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := db.GetOrCreate("qa11", "")
	if err != nil {
		t.Fatal(err)
	}
	raw, blob := seedLargeMetaJournal(t, be, p.ID, "dev1", 10000)
	srv := &Server{Root: be, Projects: db, Upload: UploadConfig{Enabled: true}}
	_, v, err := srv.projectVolume(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	rs := v.source.(*RemoteSource)
	filter := pathFilter{
		project: Project{Folders: []FolderRule{{Prefix: "secret/", Default: PermNone}}},
		email:   "bob@example.com",
		base:    PermRead,
		tag:     "bob",
		on:      true,
	}

	objs, err := rs.listJournals(ctx)
	if err != nil || len(objs) != 1 {
		t.Fatalf("listJournals: %d err=%v", len(objs), err)
	}
	d1, err := srv.filteredJournal(ctx, rs, objs[0], filter)
	if err != nil || len(d1) != len(raw) {
		t.Fatalf("initial filteredJournal: len=%d want=%d err=%v", len(d1), len(raw), err)
	}

	// Append 1 hidden line + 1 visible line via POST /store/append.
	t0 := time.Date(2026, 11, 20, 4, 0, 0, 0, time.UTC)
	secLine, _ := journal.Marshal([]journal.Op{{
		Seq: 10001, Lamport: 10001, Time: t0, Device: "dev1",
		Kind: journal.KindPut, Path: "secret/top.md", Blob: blob, Size: 14, Mode: 0o644,
	}})
	visLine, _ := journal.Marshal([]journal.Op{{
		Seq: 10002, Lamport: 10002, Time: t0.Add(time.Second), Device: "dev1",
		Kind: journal.KindPut, Path: "notes/pub.md", Blob: blob, Size: 14, Mode: 0o644,
	}})
	tail := append(append([]byte(nil), secLine...), visLine...)
	h := srv.Handler()
	api := "/api/p/" + p.ID + "/"
	u := fmt.Sprintf("%sstore/append?key=journal/dev1.jsonl&prev_size=%d&prev_sha256=%s", api, len(raw), qaHexSHA(raw))
	if rec := tailQARequest(t, h, http.MethodPost, u, tail, http.Header{"X-Bdrive-Device": {"dev1"}}); rec.Code != http.StatusOK {
		t.Fatalf("append: %d %s", rec.Code, rec.Body)
	}

	be.resetStats()
	objs2, err := rs.listJournals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := srv.filteredJournal(ctx, rs, objs2[0], filter)
	if err != nil {
		t.Fatalf("warm tail filteredJournal: %v", err)
	}
	if bytes.Contains(d2, []byte("secret/top.md")) || !bytes.Contains(d2, []byte("notes/pub.md")) {
		t.Fatalf("filteredJournal leaked secret or dropped visible line")
	}
	if len(d1) != len(raw) || bytes.Contains(d1, []byte("notes/pub.md")) {
		t.Fatal("warm tail mutated earlier returned filtered slice")
	}
	fg, fb, rg, _ := be.stats()
	if fg != 0 || fb != 0 || rg == 0 {
		t.Fatalf("expected range-only filtered tail read, got fullGets=%d (%d B) rangeGets=%d", fg, fb, rg)
	}

	// Verify VersionedRangeGetter-only backend skips GetVersionedRange on handleStoreGet cold candidate.
	vrgRaw := newTailTestMetaBackend(false)
	vrgDB, _ := OpenProjectDB(filepath.Join(t.TempDir(), "vrg-projects.json"))
	vp, _, _ := vrgDB.GetOrCreate("vrg", "")
	_, _ = vrgRaw, vp
	vrgBe := &vrgOnlyTestBackend{inner: vrgRaw}
	vrgSrv := &Server{Root: vrgBe, Projects: vrgDB, Upload: UploadConfig{Enabled: true}}
	_ = vrgRaw.Put(ctx, vp.ID+"/journal/dev1.jsonl", bytes.NewReader(raw), int64(len(raw)))
	vrgRaw.resetStats()
	coldTailURL := fmt.Sprintf("/api/p/%s/store/object?key=journal/dev1.jsonl&offset=100&prefix_sha256=%s", vp.ID, qaHexSHA(raw[:100]))
	rec := tailQARequest(t, vrgSrv.Handler(), http.MethodGet, coldTailURL, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("vrgOnly cold tail GET: %d", rec.Code)
	}
	if _, _, rg2, _ := vrgRaw.stats(); rg2 != 0 {
		t.Fatalf("vrgOnly backend must skip GetVersionedRange when hasJournalPrefixCandidate is false, got rangeGets=%d", rg2)
	}

	// Verify journalOps enforces SafePath & ReservedPath via ParseReader.
	f, err := os.CreateTemp(t.TempDir(), "jops-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, _ = f.WriteString(`{"seq":1,"kind":"put","path":".git/hooks/pre-commit","blob":"` + blob + `"}` + "\n")
	_, _ = f.Seek(0, io.SeekStart)
	if _, err := journalOps("journal/dev1.jsonl", f); err == nil {
		t.Fatal("journalOps accepted .git reserved path")
	}
}

func TestQA12_JournalVersionRingProtectsWriteFromLaggingReaders(t *testing.T) {
	rs := &RemoteSource{}
	sha := strings.Repeat("a", 64)
	rs.storeJournalWriteMeta("journal/dev1.jsonl", "v-write", 2048, sha, 123, 500, true, true, 1024, sha, true, nil)

	// Record lagging readers across 8 distinct older versions.
	for i := 1; i <= 8; i++ {
		rs.storeJournalPrefixMark("journal/dev1.jsonl", remote.Version(fmt.Sprintf("v-read-%d", i)), int64(100*i), sha, true)
	}

	if e, ok := rs.loadJournalAppendMeta("journal/dev1.jsonl", "v-write"); !ok || e.storedMax != 500 {
		t.Fatalf("lagging readers evicted newest write entry: ok=%v entry=%+v", ok, e)
	}
	rs.jverMu.Lock()
	ringLen := len(rs.jverCache["journal/dev1.jsonl"].entries)
	rs.jverMu.Unlock()
	if ringLen > maxJournalVersionsPerKey {
		t.Fatalf("ring length %d > maxJournalVersionsPerKey (%d)", ringLen, maxJournalVersionsPerKey)
	}

	// Check 1024-key cap.
	for i := 0; i < maxJournalVersionCacheKeys+50; i++ {
		rs.storeJournalPrefixMark(fmt.Sprintf("journal/k-%04d.jsonl", i), "v1", 10, sha, true)
	}
	rs.jverMu.Lock()
	keyCount := len(rs.jverCache)
	rs.jverMu.Unlock()
	if keyCount > maxJournalVersionCacheKeys {
		t.Fatalf("keyCount %d > maxJournalVersionCacheKeys (%d)", keyCount, maxJournalVersionCacheKeys)
	}

	// lookupVerifiedTail & lookupVerifiedPrefixForGet on older versions must not
	// evict or demote any of 4 active write entries in journalVersionRing.
	rsChain := &RemoteSource{}
	for w := 1; w <= 4; w++ {
		v := remote.Version(fmt.Sprintf("w%d", w))
		rsChain.storeJournalWriteMeta("journal/w.jsonl", v, int64(w*100), sha, 0, int64(w*10), true, true, 0, "", false, nil)
	}
	chainStr := formatJournalAppendChain(50, sha, true, 0, map[int64]journalPrefixMark{25: {sha: sha, lineBoundary: true}})
	readMeta := remote.ObjectMeta{
		Version:  "r-old",
		Size:     50,
		Metadata: map[string]string{journalAppendChainMetaKey: chainStr},
	}
	if !rsChain.lookupVerifiedPrefixForGet("journal/w.jsonl", readMeta, 25, sha) {
		t.Fatal("lookupVerifiedPrefixForGet failed on valid chain")
	}
	if _, _, ok := rsChain.lookupVerifiedTail("journal/w.jsonl", readMeta, 25, sha, true); !ok {
		t.Fatal("lookupVerifiedTail failed on valid chain")
	}
	// Repeating a read lookup on an active write version w1 must preserve hasAppend & storedMax without promoting w1 past w4.
	w1Meta := remote.ObjectMeta{
		Version:  "w1",
		Size:     100,
		Metadata: map[string]string{journalAppendChainMetaKey: formatJournalAppendChain(100, sha, true, 0, map[int64]journalPrefixMark{50: {sha: sha, lineBoundary: true}})},
	}
	if !rsChain.lookupVerifiedPrefixForGet("journal/w.jsonl", w1Meta, 50, sha) {
		t.Fatal("lookupVerifiedPrefixForGet failed on w1 chain")
	}
	for w := 1; w <= 4; w++ {
		v := remote.Version(fmt.Sprintf("w%d", w))
		e, ok := rsChain.loadJournalAppendMeta("journal/w.jsonl", v)
		if !ok || e.storedMax != int64(w*10) {
			t.Fatalf("write entry %s evicted or corrupted after read chain lookups: ok=%v entry=%+v", v, ok, e)
		}
	}
	rsChain.jverMu.Lock()
	newest := rsChain.jverCache["journal/w.jsonl"].entries[len(rsChain.jverCache["journal/w.jsonl"].entries)-1].version
	rsChain.jverMu.Unlock()
	if newest != "w4" {
		t.Fatalf("newest write entry demoted after reading w1: newest=%q want w4", newest)
	}
}

func TestQA13_ParseJournalAppendChainStrict(t *testing.T) {
	sha1 := strings.Repeat("1", 64)
	sha2 := strings.Repeat("2", 64)
	sha3 := strings.Repeat("3", 64)

	validMeta := formatJournalAppendChain(300, sha3, true, 0x0a0b0c0d, map[int64]journalPrefixMark{
		100: {sha: sha1, lineBoundary: true},
		200: {sha: sha2, lineBoundary: false},
	})
	chain, ok := parseJournalAppendChain(remote.ObjectMeta{
		Size:     300,
		CRC32C:   0x0a0b0c0d,
		Metadata: map[string]string{journalAppendChainMetaKey: validMeta},
	})
	if !ok || chain.headSize != 300 || chain.headSHA != sha3 || !chain.headNL || chain.prefixes[100].sha != sha1 || !chain.prefixes[100].lineBoundary || chain.prefixes[200].lineBoundary {
		t.Fatalf("valid parse failed: ok=%v chain=%+v", ok, chain)
	}

	// Malformed / forged cases must all fail closed.
	badCases := []struct {
		name string
		meta remote.ObjectMeta
	}{
		{"wrong version prefix", remote.ObjectMeta{Size: 300, Metadata: map[string]string{journalAppendChainMetaKey: strings.Replace(validMeta, "v1;", "v2;", 1)}}},
		{"headSize mismatch", remote.ObjectMeta{Size: 301, CRC32C: 0x0a0b0c0d, Metadata: map[string]string{journalAppendChainMetaKey: validMeta}}},
		{"zero CRC forgery when meta CRC non-zero", remote.ObjectMeta{Size: 300, CRC32C: 0x0a0b0c0d, Metadata: map[string]string{journalAppendChainMetaKey: fmt.Sprintf("v1;h=300:%s:1:00000000", sha3)}}},
		{"non-zero CRC mismatch", remote.ObjectMeta{Size: 300, CRC32C: 0x11111111, Metadata: map[string]string{journalAppendChainMetaKey: validMeta}}},
		{"non-monotonic offsets", remote.ObjectMeta{Size: 300, Metadata: map[string]string{journalAppendChainMetaKey: fmt.Sprintf("v1;h=300:%s:1:00000000;p=200:%s:1,100:%s:1", sha3, sha2, sha1)}}},
		{"duplicate offsets", remote.ObjectMeta{Size: 300, Metadata: map[string]string{journalAppendChainMetaKey: fmt.Sprintf("v1;h=300:%s:1:00000000;p=100:%s:1,100:%s:1", sha3, sha1, sha1)}}},
		{"offset >= headSize", remote.ObjectMeta{Size: 300, Metadata: map[string]string{journalAppendChainMetaKey: fmt.Sprintf("v1;h=300:%s:1:00000000;p=300:%s:1", sha3, sha1)}}},
		{"bad nl bit", remote.ObjectMeta{Size: 300, Metadata: map[string]string{journalAppendChainMetaKey: fmt.Sprintf("v1;h=300:%s:2:00000000", sha3)}}},
		{"invalid sha", remote.ObjectMeta{Size: 300, Metadata: map[string]string{journalAppendChainMetaKey: "v1;h=300:zzzz:1:00000000"}}},
		{"bad hex crc", remote.ObjectMeta{Size: 300, Metadata: map[string]string{journalAppendChainMetaKey: fmt.Sprintf("v1;h=300:%s:1:GGGGGGGG", sha3)}}},
		{"oversized metadata", remote.ObjectMeta{Size: 300, Metadata: map[string]string{journalAppendChainMetaKey: validMeta + strings.Repeat("x", maxMetaChainBytes)}}},
	}
	for _, tc := range badCases {
		if _, ok := parseJournalAppendChain(tc.meta); ok {
			t.Fatalf("%s unexpectedly accepted", tc.name)
		}
	}
}

func TestQA16_TryIncrementalHistIndexReadsTailOnly(t *testing.T) {
	ctx := context.Background()
	be := newTailTestMetaBackend(true)
	db, err := OpenProjectDB(filepath.Join(t.TempDir(), "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := db.GetOrCreate("qa16", "")
	if err != nil {
		t.Fatal(err)
	}
	raw, blob := seedLargeMetaJournal(t, be, p.ID, "dev1", 10000)
	srv := &Server{Root: be, Projects: db, Upload: UploadConfig{Enabled: true}}
	_, v, err := srv.projectVolume(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	rs := v.source.(*RemoteSource)

	if _, err := rs.historyIndex(ctx); err != nil {
		t.Fatalf("warm historyIndex: %v", err)
	}

	// Append 2 ops to dev1 and introduce brand-new dev2 journal.
	t0 := time.Date(2026, 11, 20, 5, 0, 0, 0, time.UTC)
	tail1, _ := journal.Marshal([]journal.Op{
		{Seq: 10001, Lamport: 10001, Time: t0, Device: "dev1", Kind: journal.KindPut, Path: "notes/h-1.md", Blob: blob, Size: 14},
		{Seq: 10002, Lamport: 10002, Time: t0.Add(time.Second), Device: "dev1", Kind: journal.KindPut, Path: "notes/h-2.md", Blob: blob, Size: 14},
	})
	h := srv.Handler()
	api := "/api/p/" + p.ID + "/"
	u := fmt.Sprintf("%sstore/append?key=journal/dev1.jsonl&prev_size=%d&prev_sha256=%s", api, len(raw), qaHexSHA(raw))
	if rec := tailQARequest(t, h, http.MethodPost, u, tail1, http.Header{"X-Bdrive-Device": {"dev1"}}); rec.Code != http.StatusOK {
		t.Fatalf("append dev1: %d %s", rec.Code, rec.Body)
	}
	dev2Line, _ := journal.Marshal([]journal.Op{
		{Seq: 1, Lamport: 10003, Time: t0.Add(2 * time.Second), Device: "dev2", Kind: journal.KindPut, Path: "notes/dev2-new.md", Blob: blob, Size: 14},
	})
	if err := be.Put(ctx, p.ID+"/journal/dev2.jsonl", bytes.NewReader(dev2Line), int64(len(dev2Line))); err != nil {
		t.Fatal(err)
	}

	be.resetStats()
	gotIdx, err := rs.historyIndex(ctx)
	if err != nil {
		t.Fatalf("incremental historyIndex: %v", err)
	}
	// Note dev2 (brand new) performs 1 small full GET; dev1 (2+ MiB) MUST take 0 full GETs!
	fg, fb, rg, _ := be.stats()
	if fg != 1 || fb != int64(len(dev2Line)) || rg == 0 {
		t.Fatalf("expected 1 small GET for new dev2 only and range GET for dev1: fullGets=%d fullBytes=%d rangeGets=%d (dev2=%d)", fg, fb, rg, len(dev2Line))
	}

	coldRS := &RemoteSource{Backend: remote.Prefixed(be, p.ID)}
	wantIdx, err := coldRS.historyIndex(ctx)
	if err != nil {
		t.Fatalf("cold historyIndex: %v", err)
	}
	if !reflect.DeepEqual(gotIdx.all, wantIdx.all) ||
		!reflect.DeepEqual(gotIdx.kinds, wantIdx.kinds) ||
		!reflect.DeepEqual(gotIdx.prevs, wantIdx.prevs) ||
		!reflect.DeepEqual(gotIdx.byTime, wantIdx.byTime) {
		t.Fatalf("incremental histIndex diverged from cold rebuild: got=%d want=%d", len(gotIdx.all), len(wantIdx.all))
	}
}

func TestQA17_ConcurrentTailReadsAndAppendsRace(t *testing.T) {
	ctx := context.Background()
	be := newTailTestMetaBackend(true)
	db, err := OpenProjectDB(filepath.Join(t.TempDir(), "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := db.GetOrCreate("qa17", "")
	if err != nil {
		t.Fatal(err)
	}
	raw, blob := seedLargeMetaJournal(t, be, p.ID, "dev1", 10000)
	srv := &Server{Root: be, Projects: db, Upload: UploadConfig{Enabled: true}}
	h := srv.Handler()
	api := "/api/p/" + p.ID + "/"
	_, v, err := srv.projectVolume(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	rs := v.source.(*RemoteSource)
	filter := pathFilter{
		project: Project{Folders: []FolderRule{{Prefix: "secret/", Default: PermNone}}},
		email:   "bob@example.com",
		base:    PermRead,
		tag:     "bob",
		on:      true,
	}

	// Warm initial fold, historyIndex, and filteredJournal caches.
	if _, err := rs.Files(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.historyIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if objs, err := rs.listJournals(ctx); err != nil {
		t.Fatal(err)
	} else if _, err := srv.filteredJournal(ctx, rs, objs[0], filter); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	errCh := make(chan error, 8)
	var wg sync.WaitGroup

	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if _, err := rs.Files(ctx); err != nil {
						errCh <- err
						return
					}
					if _, err := rs.historyIndex(ctx); err != nil {
						errCh <- err
						return
					}
					objs, err := rs.listJournals(ctx)
					if err != nil {
						errCh <- err
						return
					}
					if len(objs) > 0 {
						data, err := srv.filteredJournal(ctx, rs, objs[0], filter)
						if err != nil {
							errCh <- err
							return
						}
						if bytes.Contains(data, []byte("secret/")) {
							errCh <- fmt.Errorf("filteredJournal leaked secret path")
							return
						}
					}
				}
			}
		}()
	}

	cur := append([]byte(nil), raw...)
	t0 := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	for step := 1; step <= 10; step++ {
		path := fmt.Sprintf("notes/race-%02d.md", step)
		if step%3 == 0 {
			path = fmt.Sprintf("secret/race-%02d.md", step)
		}
		tail, _ := journal.Marshal([]journal.Op{{
			Seq: int64(10000 + step), Lamport: int64(10000 + step), Time: t0.Add(time.Duration(step) * time.Second),
			Device: "dev1", Kind: journal.KindPut, Path: path, Blob: blob, Size: 14, Mode: 0o644,
		}})
		u := fmt.Sprintf("%sstore/append?key=journal/dev1.jsonl&prev_size=%d&prev_sha256=%s", api, len(cur), qaHexSHA(cur))
		if rec := tailQARequest(t, h, http.MethodPost, u, tail, http.Header{"X-Bdrive-Device": {"dev1"}}); rec.Code != http.StatusOK {
			close(stop)
			wg.Wait()
			t.Fatalf("append %d: %d %s", step, rec.Code, rec.Body)
		}
		cur = append(cur, tail...)
	}
	close(stop)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

func BenchmarkJournalTailAppend(b *testing.B) {
	ctx := context.Background()
	be := newTailTestMetaBackend(true)
	rs := &RemoteSource{Backend: be}
	blob := strings.Repeat("e", 64)
	baseTime := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	const seedN = 18000 // ~4 MiB
	ops := make([]journal.Op, 0, seedN)
	for i := 1; i <= seedN; i++ {
		ops = append(ops, journal.Op{
			Seq: int64(i), Lamport: int64(i), Time: baseTime.Add(time.Duration(i) * time.Second),
			Device: "dev1", User: "alice@example.com", Kind: journal.KindPut,
			Path: fmt.Sprintf("notes/b-%05d.md", i), Blob: blob, Size: 14, Mode: 0o644,
		})
	}
	cur, err := journal.Marshal(ops)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := putIfWithMeta(ctx, be, be, "journal/dev1.jsonl", bytes.NewReader(cur), int64(len(cur)), remote.VersionAbsent, nil); err != nil {
		b.Fatal(err)
	}
	objs, err := rs.listJournals(ctx)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := rs.parsedJournals(ctx, objs); err != nil {
		b.Fatal(err)
	}
	_, prevMeta, err := be.GetVersionedWithMeta(ctx, "journal/dev1.jsonl")
	if err != nil {
		b.Fatal(err)
	}
	prevSize := int64(len(cur))
	prevSHA := qaHexSHA(cur)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		seq := int64(seedN + i + 1)
		tail, _ := journal.Marshal([]journal.Op{{
			Seq: seq, Lamport: seq, Time: baseTime.Add(time.Duration(seq) * time.Second),
			Device: "dev1", User: "alice@example.com", Kind: journal.KindPut,
			Path: fmt.Sprintf("notes/b-%06d.md", seq), Blob: blob, Size: 14, Mode: 0o644,
		}})
		next := make([]byte, len(cur)+len(tail))
		copy(next, cur)
		copy(next[len(cur):], tail)
		fullSHA := qaHexSHA(next)
		fullCRC := crc32cSum(next)
		prefixes, putMeta := rs.prepareJournalWriteMeta("journal/dev1.jsonl", prevMeta, prevSize, prevSHA, true, int64(len(next)), fullSHA, true, fullCRC, true)
		newMeta, err := putIfWithMeta(ctx, be, be, "journal/dev1.jsonl", bytes.NewReader(next), int64(len(next)), prevMeta.Version, putMeta)
		if err != nil {
			b.Fatal(err)
		}
		rs.storeJournalWriteMeta("journal/dev1.jsonl", newMeta.Version, int64(len(next)), fullSHA, fullCRC, seq, true, true, prevSize, prevSHA, true, prefixes)
		cur = next
		prevMeta = newMeta
		prevSize = int64(len(next))
		prevSHA = fullSHA

		listObjs, err := rs.listJournals(ctx)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := rs.parsedJournals(ctx, listObjs); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLargeJournalParse(b *testing.B) {
	blob := strings.Repeat("f", 64)
	baseTime := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	const seedN = 18000 // ~4 MiB
	ops := make([]journal.Op, 0, seedN)
	for i := 1; i <= seedN; i++ {
		ops = append(ops, journal.Op{
			Seq: int64(i), Lamport: int64(i), Time: baseTime.Add(time.Duration(i) * time.Second),
			Device: "dev1", User: "alice@example.com", Kind: journal.KindPut,
			Path: fmt.Sprintf("notes/p-%05d.md", i), Blob: blob, Size: 14, Mode: 0o644,
		})
	}
	raw, err := journal.Marshal(ops)
	if err != nil {
		b.Fatal(err)
	}
	sizeHint := int64(len(raw))
	b.SetBytes(sizeHint)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parsed, err := journal.ParseReaderSized(bytes.NewReader(raw), sizeHint)
		if err != nil || len(parsed) != seedN {
			b.Fatalf("ParseReaderSized: len=%d err=%v", len(parsed), err)
		}
	}
}

// Keep unused errors import impossible if refactored.
var _ = errors.New
