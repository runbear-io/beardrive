package remote

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type s3StubObj struct {
	data     []byte
	etag     string
	mod      time.Time
	metadata map[string]string
}

type s3Stub struct {
	mu                       sync.Mutex
	objs                     map[string]s3StubObj
	headCalls                int
	getCalls                 int
	force200NoContentRange   bool
	forceOnce416             bool
	forceOversizedRange      bool
}

func newS3Stub() *s3Stub {
	return &s3Stub{objs: make(map[string]s3StubObj)}
}

func s3ETag(b []byte) string {
	sum := md5.Sum(b)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func (s *s3Stub) writeXMLError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, msg)
}

func (s *s3Stub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trimmed := strings.TrimPrefix(r.URL.Path, "/")
		parts := strings.SplitN(trimmed, "/", 2)
		if len(parts) < 1 || parts[0] == "" {
			http.NotFound(w, r)
			return
		}
		if len(parts) == 1 || parts[1] == "" {
			if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
				s.handleList(w, r)
				return
			}
			http.NotFound(w, r)
			return
		}
		key := parts[1]
		switch r.Method {
		case http.MethodPut:
			s.handlePut(w, r, key)
		case http.MethodHead:
			s.handleHead(w, key)
		case http.MethodGet:
			s.handleGet(w, r, key)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

func (s *s3Stub) handleList(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	type contentsItem struct {
		Key          string `xml:"Key"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
		Size         int64  `xml:"Size"`
	}
	type listResult struct {
		XMLName     xml.Name       `xml:"ListBucketResult"`
		Xmlns       string         `xml:"xmlns,attr"`
		Name        string         `xml:"Name"`
		Prefix      string         `xml:"Prefix"`
		IsTruncated bool           `xml:"IsTruncated"`
		Contents    []contentsItem `xml:"Contents"`
	}
	s.mu.Lock()
	res := listResult{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/", Name: "b", Prefix: prefix}
	for k, o := range s.objs {
		if strings.HasPrefix(k, prefix) {
			res.Contents = append(res.Contents, contentsItem{
				Key: k, LastModified: o.mod.UTC().Format(time.RFC3339), ETag: o.etag, Size: int64(len(o.data)),
			})
		}
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(res)
}

func (s *s3Stub) handlePut(w http.ResponseWriter, r *http.Request, key string) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	meta := map[string]string{}
	for hk, vs := range r.Header {
		if k, ok := strings.CutPrefix(strings.ToLower(hk), "x-amz-meta-"); ok && len(vs) > 0 {
			meta[k] = vs[0]
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, exists := s.objs[key]
	if r.Header.Get("If-None-Match") == "*" && exists {
		s.writeXMLError(w, http.StatusPreconditionFailed, "PreconditionFailed", "exists")
		return
	}
	if im := r.Header.Get("If-Match"); im != "" && (!exists || cur.etag != im) {
		s.writeXMLError(w, http.StatusPreconditionFailed, "PreconditionFailed", "mismatch")
		return
	}
	etag := s3ETag(data)
	s.objs[key] = s3StubObj{data: data, etag: etag, mod: time.Now().UTC(), metadata: meta}
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

func (s *s3Stub) writeObjHeaders(w http.ResponseWriter, obj s3StubObj) {
	w.Header().Set("ETag", obj.etag)
	for k, v := range obj.metadata {
		w.Header().Set("x-amz-meta-"+k, v)
	}
}

func (s *s3Stub) handleHead(w http.ResponseWriter, key string) {
	s.mu.Lock()
	s.headCalls++
	obj, ok := s.objs[key]
	s.mu.Unlock()
	if !ok {
		s.writeXMLError(w, http.StatusNotFound, "NotFound", "missing")
		return
	}
	s.writeObjHeaders(w, obj)
	w.Header().Set("Content-Length", strconv.Itoa(len(obj.data)))
	w.WriteHeader(http.StatusOK)
}

func (s *s3Stub) handleGet(w http.ResponseWriter, r *http.Request, key string) {
	rng := r.Header.Get("Range")
	s.mu.Lock()
	s.getCalls++
	obj, ok := s.objs[key]
	force200 := s.force200NoContentRange
	force416 := false
	if rng != "" && s.forceOnce416 {
		force416 = true
		s.forceOnce416 = false
	}
	forceOversized := s.forceOversizedRange
	s.mu.Unlock()
	if !ok {
		s.writeXMLError(w, http.StatusNotFound, "NoSuchKey", "missing")
		return
	}
	s.writeObjHeaders(w, obj)
	if force416 {
		w.Header().Set("Content-Range", "bytes */0")
		s.writeXMLError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "stale 416")
		return
	}
	if rng == "" {
		w.Header().Set("Content-Length", strconv.Itoa(len(obj.data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(obj.data)
		return
	}
	if force200 {
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
		s.writeXMLError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "oob")
		return
	}
	end := int64(len(obj.data)) - 1
	if parts[1] != "" {
		e, eerr := strconv.ParseInt(parts[1], 10, 64)
		if eerr != nil || e < start {
			s.writeXMLError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "bad end")
			return
		}
		if e < end {
			end = e
		}
	}
	slice := obj.data[start : end+1]
	reportedEnd := end
	if forceOversized {
		reportedEnd = int64(len(obj.data)) - 1
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, reportedEnd, len(obj.data)))
	w.Header().Set("Content-Length", strconv.Itoa(len(slice)))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(slice)
}

func TestS3BackendRangeVersionedAndMetadata(t *testing.T) {
	ctx := context.Background()
	stub := newS3Stub()
	ts := httptest.NewServer(stub.handler())
	defer ts.Close()

	client := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(ts.URL),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("AK", "SK", ""),
	})
	be := &s3Backend{client: client, bucket: "b", prefix: "proj"}

	body := "line1\nline2\n"
	om, err := be.PutIfWithMeta(ctx, "journal/dev.jsonl", strings.NewReader(body), int64(len(body)), VersionAbsent, map[string]string{
		"Bdrive-Append-Chain": "v1;chain",
	})
	if err != nil {
		t.Fatalf("PutIfWithMeta: %v", err)
	}
	if om.Version == "" || om.Metadata["bdrive-append-chain"] != "v1;chain" {
		t.Fatalf("PutIfWithMeta = %+v", om)
	}
	// Stale Expect fails with ErrVersionMismatch.
	if _, err := be.PutIfWithMeta(ctx, "journal/dev.jsonl", strings.NewReader(body), int64(len(body)), `"wrong"`, nil); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("stale PutIfWithMeta = %v, want ErrVersionMismatch", err)
	}

	objs, err := be.List(ctx, "journal/")
	if err != nil || len(objs) != 1 {
		t.Fatalf("List = (%+v, %v)", objs, err)
	}
	if objs[0].Version != om.Version || objs[0].ImmutableVersion {
		t.Fatalf("List Object = %+v (ImmutableVersion must be false on S3)", objs[0])
	}

	// Range read with length > 0 and length < 0.
	rc, rmeta, err := be.GetVersionedRangeWithMeta(ctx, "journal/dev.jsonl", 6, 3)
	if err != nil {
		t.Fatalf("GetVersionedRangeWithMeta(6,3): %v", err)
	}
	got3, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got3) != "lin" || rmeta.Size != int64(len(body)) || rmeta.Version != om.Version || rmeta.Metadata["bdrive-append-chain"] != "v1;chain" {
		t.Fatalf("range (6,3) = (%q, %+v)", got3, rmeta)
	}

	// length == 0 uses HeadObject (0 GetObject calls) and never reads 1 byte.
	stub.mu.Lock()
	stub.getCalls = 0
	stub.headCalls = 0
	stub.mu.Unlock()
	rc0, m0, err := be.GetVersionedRangeWithMeta(ctx, "journal/dev.jsonl", 6, 0)
	if err != nil {
		t.Fatalf("length==0: %v", err)
	}
	b0, _ := io.ReadAll(rc0)
	_ = rc0.Close()
	stub.mu.Lock()
	gc, hc := stub.getCalls, stub.headCalls
	stub.mu.Unlock()
	if len(b0) != 0 || m0.Version != om.Version || gc != 0 || hc != 1 {
		t.Fatalf("length==0 got (%q, %+v, get=%d, head=%d)", b0, m0, gc, hc)
	}

	// offset == size handles 416 via HeadObject and returns empty reader + meta.
	rcEq, mEq, err := be.GetVersionedRangeWithMeta(ctx, "journal/dev.jsonl", int64(len(body)), -1)
	if err != nil {
		t.Fatalf("offset==size: %v", err)
	}
	bEq, _ := io.ReadAll(rcEq)
	_ = rcEq.Close()
	if len(bEq) != 0 || mEq.Size != int64(len(body)) {
		t.Fatalf("offset==size = (%q, %+v)", bEq, mEq)
	}

	// Negative / OOB offsets fail closed.
	for _, off := range []int64{-1, int64(len(body)) + 1} {
		if r, _, err := be.GetVersionedRangeWithMeta(ctx, "journal/dev.jsonl", off, -1); err == nil {
			_ = r.Close()
			t.Fatalf("offset %d succeeded, want error", off)
		}
	}

	// Non-206 proxy response missing Content-Range must be rejected when offset > 0.
	stub.mu.Lock()
	stub.force200NoContentRange = true
	stub.mu.Unlock()
	if r, _, err := be.GetVersionedRangeWithMeta(ctx, "journal/dev.jsonl", 6, -1); err == nil {
		_ = r.Close()
		t.Fatal("expected error when S3 response omitted Content-Range for offset > 0")
	}
	stub.mu.Lock()
	stub.force200NoContentRange = false
	stub.mu.Unlock()

	// 416 followed by HeadObject seeing concurrent growth retries once and succeeds.
	stub.mu.Lock()
	stub.forceOnce416 = true
	stub.getCalls = 0
	stub.headCalls = 0
	stub.mu.Unlock()
	rcRetry, mRetry, err := be.GetVersionedRangeWithMeta(ctx, "journal/dev.jsonl", 6, 3)
	if err != nil {
		t.Fatalf("416 retry failed: %v", err)
	}
	gotRetry, _ := io.ReadAll(rcRetry)
	_ = rcRetry.Close()
	stub.mu.Lock()
	gcR, hcR := stub.getCalls, stub.headCalls
	stub.mu.Unlock()
	if string(gotRetry) != "lin" || mRetry.Size != int64(len(body)) || gcR != 2 || hcR != 1 {
		t.Fatalf("416 retry got (%q, %+v, gets=%d, heads=%d)", gotRetry, mRetry, gcR, hcR)
	}

	// Oversized Content-Range exceeding requested length must be rejected.
	stub.mu.Lock()
	stub.forceOversizedRange = true
	stub.mu.Unlock()
	if r, _, err := be.GetVersionedRangeWithMeta(ctx, "journal/dev.jsonl", 6, 3); err == nil {
		_ = r.Close()
		t.Fatal("expected error on oversized Content-Range")
	}
	stub.mu.Lock()
	stub.forceOversizedRange = false
	stub.mu.Unlock()

	// Missing key maps to remote.IsNotExist.
	if _, _, err := be.GetVersionedRangeWithMeta(ctx, "journal/missing.jsonl", 0, -1); !IsNotExist(err) {
		t.Fatalf("missing S3 key = %v, want IsNotExist", err)
	}
}
