package remote

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

type gcsStubObj struct {
	data     []byte
	gen      int64
	crc32c   uint32
	metadata map[string]string
	encoding string
}

type gcsStub struct {
	mu           sync.Mutex
	objs         map[string]gcsStubObj
	nextGen      int64
	rangeCalls   int
	attrsCalls   int
	uploadStarts int
	uploadAborts int
}

func newGCSStub() *gcsStub {
	return &gcsStub{objs: make(map[string]gcsStubObj), nextGen: 100}
}

func crc32cOf(b []byte) uint32 {
	return crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli))
}

func encodeGCSCRC(v uint32) string {
	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], v)
	return base64.StdEncoding.EncodeToString(raw[:])
}

func (s *gcsStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasPrefix(path, "/upload/storage/v1/b/") && strings.HasSuffix(path, "/o"):
			s.handleUpload(w, r)
		case strings.HasPrefix(path, "/storage/v1/b/") && strings.HasSuffix(path, "/o"):
			s.handleList(w, r)
		case strings.HasPrefix(path, "/storage/v1/b/") && strings.Contains(path, "/o/"):
			idx := strings.Index(path, "/o/")
			key := path[idx+3:]
			if r.URL.Query().Get("alt") == "media" {
				s.handleDownload(w, r, key)
			} else {
				s.handleAttrs(w, r, key)
			}
		case strings.HasPrefix(path, "/download/storage/v1/b/") && strings.Contains(path, "/o/"):
			idx := strings.Index(path, "/o/")
			s.handleDownload(w, r, path[idx+3:])
		default:
			// XML reader paths: /<bucket>/<key...>
			parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
			if len(parts) == 2 && r.Method == http.MethodGet {
				s.handleDownload(w, r, parts[1])
				return
			}
			http.NotFound(w, r)
		}
	})
}

func (s *gcsStub) writeJSONErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": msg},
	})
}

func (s *gcsStub) handleAttrs(w http.ResponseWriter, r *http.Request, key string) {
	s.mu.Lock()
	s.attrsCalls++
	obj, ok := s.objs[key]
	s.mu.Unlock()
	if !ok {
		s.writeJSONErr(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"kind":           "storage#object",
		"bucket":         "b",
		"name":           key,
		"size":           strconv.Itoa(len(obj.data)),
		"generation":     strconv.FormatInt(obj.gen, 10),
		"metageneration": "1",
		"crc32c":         encodeGCSCRC(obj.crc32c),
		"metadata":       obj.metadata,
	})
}

func (s *gcsStub) handleList(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	s.mu.Lock()
	var items []map[string]any
	for k, obj := range s.objs {
		if strings.HasPrefix(k, prefix) {
			items = append(items, map[string]any{
				"kind":       "storage#object",
				"bucket":     "b",
				"name":       k,
				"size":       strconv.Itoa(len(obj.data)),
				"generation": strconv.FormatInt(obj.gen, 10),
				"crc32c":     encodeGCSCRC(obj.crc32c),
				"metadata":   obj.metadata,
			})
		}
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"kind":  "storage#objects",
		"items": items,
	})
}

func (s *gcsStub) handleDownload(w http.ResponseWriter, r *http.Request, key string) {
	rng := r.Header.Get("Range")
	s.mu.Lock()
	if rng != "" {
		s.rangeCalls++
	}
	obj, ok := s.objs[key]
	s.mu.Unlock()
	if !ok {
		s.writeJSONErr(w, http.StatusNotFound, "not found")
		return
	}
	if genQ := r.URL.Query().Get("generation"); genQ != "" && genQ != strconv.FormatInt(obj.gen, 10) {
		s.writeJSONErr(w, http.StatusNotFound, "generation not found")
		return
	}
	if obj.encoding == "gzip" {
		// Simulate GCS decompressive transcoding ignoring Range and serving from byte 0.
		w.Header().Set("X-Goog-Generation", strconv.FormatInt(obj.gen, 10))
		w.Header().Set("X-Goog-Stored-Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(obj.data)
		return
	}
	w.Header().Set("X-Goog-Generation", strconv.FormatInt(obj.gen, 10))
	if rng == "" {
		w.Header().Set("Content-Length", strconv.Itoa(len(obj.data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(obj.data)
		return
	}
	spec := strings.TrimPrefix(rng, "bytes=")
	parts := strings.SplitN(spec, "-", 2)
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= int64(len(obj.data)) {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(obj.data)))
		s.writeJSONErr(w, http.StatusRequestedRangeNotSatisfiable, "invalid range")
		return
	}
	end := int64(len(obj.data)) - 1
	if parts[1] != "" {
		if e, eerr := strconv.ParseInt(parts[1], 10, 64); eerr == nil && e < end {
			end = e
		}
	}
	slice := obj.data[start : end+1]
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(obj.data)))
	w.Header().Set("Content-Length", strconv.Itoa(len(slice)))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(slice)
}

func (s *gcsStub) handleUpload(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.uploadStarts++
	s.mu.Unlock()

	ct := r.Header.Get("Content-Type")
	_, params, err := mime.ParseMediaType(ct)
	if err != nil || params["boundary"] == "" {
		s.writeJSONErr(w, http.StatusBadRequest, "expected multipart upload")
		return
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	var metaPart struct {
		Name     string            `json:"name"`
		Metadata map[string]string `json:"metadata"`
	}
	p1, err := mr.NextPart()
	if err != nil {
		s.mu.Lock()
		s.uploadAborts++
		s.mu.Unlock()
		return
	}
	if err := json.NewDecoder(p1).Decode(&metaPart); err != nil {
		s.writeJSONErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p2, err := mr.NextPart()
	if err != nil {
		s.mu.Lock()
		s.uploadAborts++
		s.mu.Unlock()
		return
	}
	body, err := io.ReadAll(p2)
	if err != nil {
		s.mu.Lock()
		s.uploadAborts++
		s.mu.Unlock()
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	cur, exists := s.objs[metaPart.Name]
	if r.URL.Query().Get("ifGenerationMatch") != "" {
		wantGen, _ := strconv.ParseInt(r.URL.Query().Get("ifGenerationMatch"), 10, 64)
		if (wantGen == 0 && exists) || (wantGen != 0 && (!exists || cur.gen != wantGen)) {
			s.writeJSONErr(w, http.StatusPreconditionFailed, "precondition failed")
			return
		}
	}
	s.nextGen++
	gen := s.nextGen
	crc := crc32cOf(body)
	metaCopy := make(map[string]string, len(metaPart.Metadata))
	for k, v := range metaPart.Metadata {
		metaCopy[k] = v
	}
	s.objs[metaPart.Name] = gcsStubObj{data: body, gen: gen, crc32c: crc, metadata: metaCopy}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"kind":           "storage#object",
		"bucket":         "b",
		"name":           metaPart.Name,
		"size":           strconv.Itoa(len(body)),
		"generation":     strconv.FormatInt(gen, 10),
		"metageneration": "1",
		"crc32c":         encodeGCSCRC(crc),
		"metadata":       metaCopy,
	})
}

func TestGCSBackendRangeVersionedAndMetadata(t *testing.T) {
	ctx := context.Background()
	stub := newGCSStub()
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	client, err := gcs.NewClient(ctx, option.WithEndpoint(ts.URL+"/storage/v1/"), option.WithoutAuthentication(), option.WithHTTPClient(ts.Client()))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	be := &gcsBackend{client: client, bucket: client.Bucket("b"), prefix: "proj"}

	// PutIfWithMeta normalizes metadata keys and round-trips through Attrs, List, GetVersionedWithMeta, and GetVersionedRangeWithMeta.
	body := "line1\nline2\n"
	om, err := be.PutIfWithMeta(ctx, "journal/dev.jsonl", strings.NewReader(body), int64(len(body)), VersionAbsent, map[string]string{
		"Bdrive-Append-Chain": "v1;test",
	})
	if err != nil {
		t.Fatalf("PutIfWithMeta: %v", err)
	}
	if om.Version == "" || om.Metadata["bdrive-append-chain"] != "v1;test" || om.CRC32C != crc32cOf([]byte(body)) {
		t.Fatalf("PutIfWithMeta meta = %+v", om)
	}

	objs, err := be.List(ctx, "journal/")
	if err != nil || len(objs) != 1 {
		t.Fatalf("List = (%+v, %v)", objs, err)
	}
	if objs[0].Version != om.Version || !objs[0].ImmutableVersion || objs[0].CRC32C != om.CRC32C {
		t.Fatalf("List Object revision fields = %+v, want ver=%q crc=%d", objs[0], om.Version, om.CRC32C)
	}

	// Range read returns exact tail + metadata.
	rc, rmeta, err := be.GetVersionedRangeWithMeta(ctx, "journal/dev.jsonl", 6, 6)
	if err != nil {
		t.Fatalf("GetVersionedRangeWithMeta: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "line2\n" || rmeta.Version != om.Version || rmeta.Metadata["bdrive-append-chain"] != "v1;test" {
		t.Fatalf("range read = (%q, %+v)", got, rmeta)
	}

	// offset == size and length == 0 must NOT trigger any Range HTTP request (1 Attrs RPC only, zero 416).
	stub.mu.Lock()
	stub.rangeCalls = 0
	stub.attrsCalls = 0
	stub.mu.Unlock()
	for _, tc := range []struct {
		off, length int64
	}{
		{int64(len(body)), -1},
		{6, 0},
	} {
		rc0, m0, err := be.GetVersionedRangeWithMeta(ctx, "journal/dev.jsonl", tc.off, tc.length)
		if err != nil {
			t.Fatalf("zero-byte range (%d,%d): %v", tc.off, tc.length, err)
		}
		b0, _ := io.ReadAll(rc0)
		_ = rc0.Close()
		if len(b0) != 0 || m0.Version != om.Version {
			t.Fatalf("zero-byte range (%d,%d) = (%q, %+v)", tc.off, tc.length, b0, m0)
		}
	}
	stub.mu.Lock()
	rcalls, acalls := stub.rangeCalls, stub.attrsCalls
	stub.mu.Unlock()
	if rcalls != 0 || acalls != 2 {
		t.Fatalf("expected 0 range HTTP calls and 2 Attrs RPCs for offset==size/length==0, got range=%d attrs=%d", rcalls, acalls)
	}

	// GetRange at offset == size recovers cleanly without error; negative/OOB fails closed.
	rEq, err := be.GetRange(ctx, "journal/dev.jsonl", int64(len(body)), -1)
	if err != nil {
		t.Fatalf("GetRange(offset==size): %v", err)
	}
	bEq, _ := io.ReadAll(rEq)
	_ = rEq.Close()
	if len(bEq) != 0 {
		t.Fatalf("GetRange(offset==size) = %q, want empty", bEq)
	}
	for _, off := range []int64{-1, int64(len(body)) + 1} {
		if r, _, err := be.GetVersionedRangeWithMeta(ctx, "journal/dev.jsonl", off, -1); err == nil {
			_ = r.Close()
			t.Fatalf("GetVersionedRangeWithMeta(%d) succeeded, want error", off)
		}
		if r, err := be.GetRange(ctx, "journal/dev.jsonl", off, -1); err == nil {
			_ = r.Close()
			t.Fatalf("GetRange(%d) succeeded, want error", off)
		}
	}

	// Decompressive transcoding (Decompressed=true) must be rejected instead of serving byte 0 as offset.
	stub.mu.Lock()
	stub.objs["proj/journal/gzipped.jsonl"] = gcsStubObj{data: []byte(body), gen: 555, crc32c: crc32cOf([]byte(body)), encoding: "gzip"}
	stub.mu.Unlock()
	if r, _, err := be.GetVersionedRangeWithMeta(ctx, "journal/gzipped.jsonl", 6, 6); err == nil {
		_ = r.Close()
		t.Fatal("expected error when GCS served decompressed stream from offset 0")
	}

	// Aborting io.Copy mid-upload must NOT commit a partial object.
	errBoom := errors.New("boom read error")
	if _, err := be.PutIfWithMeta(ctx, "journal/aborted.jsonl", iotest.ErrReader(errBoom), 10, VersionAbsent, map[string]string{"k": "v"}); !errors.Is(err, errBoom) {
		t.Fatalf("aborted PutIfWithMeta = %v, want %v", err, errBoom)
	}
	if exists, err := be.Exists(ctx, "journal/aborted.jsonl"); err != nil || exists {
		t.Fatalf("aborted upload committed partial object: exists=%v err=%v", exists, err)
	}

	// Missing object maps to IsNotExist.
	if _, _, err := be.GetVersionedRangeWithMeta(ctx, "journal/missing.jsonl", 0, -1); !IsNotExist(err) {
		t.Fatalf("missing GetVersionedRangeWithMeta = %v, want IsNotExist", err)
	}
}
