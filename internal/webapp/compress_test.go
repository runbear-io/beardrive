package webapp

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// getEnc is get() with an Accept-Encoding, since that is the whole subject
// here.
func getEnc(t *testing.T, h http.Handler, url, accept string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", url, nil)
	if accept != "" {
		r.Header.Set("Accept-Encoding", accept)
	}
	h.ServeHTTP(rec, r)
	return rec
}

func TestJSONIsCompressedForClientsThatAskAndNotForOthers(t *testing.T) {
	// The tree carries names, not contents, so the fixture has to be MANY
	// files rather than big ones — which is also what the real payload is:
	// 5,734 nodes of highly repetitive paths, timestamps and author strings.
	files := map[string]string{}
	for i := 0; i < 120; i++ {
		files[fmt.Sprintf("areas/team/daily-meetings/2026-09/note-%03d.md", i)] = "x"
	}
	h := dirServer(t, files)

	asked := getEnc(t, h, "/api/tree", "gzip, deflate, br")
	if got := asked.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if !strings.Contains(asked.Header().Get("Vary"), "Accept-Encoding") {
		t.Error("a compressible response must Vary on Accept-Encoding")
	}
	if cl := asked.Header().Get("Content-Length"); cl != "" {
		t.Errorf("Content-Length %q survived compression — that is a truncated body", cl)
	}
	// Before the reader consumes it: this is the number that matters, and
	// comparing the raw body against the INFLATED one instead would be
	// comparing a thing to itself.
	onTheWire := asked.Body.Len()
	zr, err := gzip.NewReader(asked.Body)
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gzip body did not read to completion (missing trailer?): %v", err)
	}
	var root Node
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("decompressed body is not the tree: %v", err)
	}

	plain := getEnc(t, h, "/api/tree", "")
	if got := plain.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q for a client that did not ask", got)
	}
	if plain.Body.Len() != len(body) {
		t.Errorf("the two clients got different documents: raw %d, inflated %d",
			plain.Body.Len(), len(body))
	}
	// A tree is repetitive enough that anything short of a large factor means
	// the compressor is not really running. Production measures 11x.
	if plain.Body.Len() < onTheWire*3 {
		t.Errorf("raw %d vs %d on the wire — barely compressed",
			plain.Body.Len(), onTheWire)
	}
}

func TestGzipIsRefusedByQZero(t *testing.T) {
	h := dirServer(t, map[string]string{"a.md": "x"})
	rec := getEnc(t, h, "/api/tree", "gzip;q=0, identity")
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, but the client refused gzip", got)
	}
}

// flushRecorder counts flushes, which is the only way to tell a stream that
// arrives from one that is sitting in a compressor.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (f *flushRecorder) Flush() {
	f.flushes++
	f.ResponseRecorder.Flush()
}

/* An event stream is never compressed, and its flushes still reach the socket.

   This is the regression that shipped once behind an analytics middleware:
   a wrapper that buffers SSE leaves EventSource retrying a valid, empty,
   closed 200 forever — live updates and co-editing both dead, with nothing in
   any log to say so. See refuseUnstreamable in events.go. */
func TestEventStreamIsNeitherCompressedNorBuffered(t *testing.T) {
	h := gzipResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"type\":\"change\"}\n\n")
		w.(http.Flusher).Flush()
	}))

	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/events", nil))

	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("event stream was compressed (Content-Encoding %q)", got)
	}
	if rec.flushes != 1 {
		t.Fatalf("flushes = %d, want 1 — the frame never left the handler", rec.flushes)
	}
	if !strings.Contains(rec.Body.String(), `data: {"type":"change"}`) {
		t.Fatalf("frame did not arrive verbatim: %q", rec.Body.String())
	}
}

// The same guard events.go runs before it writes a byte, asked both ways.
//
// Both answers matter, and the second is the one that cost a 15-minute test
// hang to find: a wrapper that ALWAYS implements Flush answers for a writer
// that cannot, so the guard stops at the wrapper, believes the chain is
// streamable, and the handler streams into a void forever.
func TestStreamabilityIsReportedHonestly(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/events", nil)

	over := &gzipWriter{ResponseWriter: httptest.NewRecorder()} // recorder flushes
	if refuseUnstreamable(&flushingGzipWriter{over}, req) {
		t.Error("a flushable chain was refused — SSE would 500 for no reason")
	}

	// Nothing under here can flush, and the wrapper must not pretend it can.
	blind := &gzipWriter{ResponseWriter: unflushable{httptest.NewRecorder()}}
	if !refuseUnstreamable(blind, req) {
		t.Error("an unflushable chain read as streamable — the handler would " +
			"stream into a void, which is the bug refuseUnstreamable exists for")
	}
}

// unflushable hides the recorder's Flush without hiding the rest of it.
type unflushable struct{ rec *httptest.ResponseRecorder }

func (u unflushable) Header() http.Header         { return u.rec.Header() }
func (u unflushable) Write(b []byte) (int, error) { return u.rec.Write(b) }
func (u unflushable) WriteHeader(code int)        { u.rec.WriteHeader(code) }

func TestAlreadyEncodedBodiesArePassedThrough(t *testing.T) {
	// A handler that encoded its own body. Deliberately NOT a /store/ URL:
	// that path is skipped wholesale (see TestStoreWireIsNeverTouched), and
	// this has to exercise the Content-Encoding branch itself.
	h := gzipResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		io.WriteString(w, "pretend-this-is-already-gzip")
	}))
	rec := getEnc(t, h, "/api/p/x/history", "gzip")
	if got := rec.Header().Values("Content-Encoding"); len(got) != 1 || got[0] != "gzip" {
		t.Fatalf("Content-Encoding = %v, want exactly one gzip", got)
	}
	if rec.Body.String() != "pretend-this-is-already-gzip" {
		t.Fatalf("body was re-encoded: %q", rec.Body.String())
	}
}

/* The sync wire is skipped by PATH, not by inspecting what it answered.

   syncer.pull skips a journal that did not grow and then resumes at a byte
   offset, so a length that means anything other than "bytes on this socket"
   makes it re-download forever or resume mid-stream. store.go owns encoding
   there, end to end. */
func TestStoreWireIsNeverTouched(t *testing.T) {
	h := gzipResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, strings.Repeat(`{"key":"blobs/abc","size":12},`, 200))
	}))
	rec := getEnc(t, h, "/api/p/x/store/list", "gzip")
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("the sync wire was compressed by the middleware (%q)", got)
	}
	if rec.Header().Get("Vary") != "" {
		t.Errorf("Vary = %q on a path the middleware does not touch", rec.Header().Get("Vary"))
	}
}

func TestAlreadyCompressedFormatsAreLeftAlone(t *testing.T) {
	h := gzipResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("\x89PNG\r\n\x1a\n"))
	}))
	rec := getEnc(t, h, "/api/p/x/file?path=a.png", "gzip")
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("a PNG was gzipped (Content-Encoding %q) — that spends CPU to add bytes", got)
	}
}

/* The tree revalidates instead of re-sending.

   The saving is the transfer, not the work: the body is still built and
   hashed to answer a 304. On a real project that is ~150 KB compressed
   against 30 bytes, every time a reload or the safety-net refetch asks. */
func TestTreeRevalidatesWithAnETag(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("# A"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{Source: &DirSource{Root: root}, Volume: "local", Refresh: 0}
	h := s.Handler()

	first := get(t, h, "/api/tree")
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the tree")
	}
	if cc := first.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache (revalidate, not reuse blindly)", cc)
	}

	rec := httptest.NewRecorder()
	again := httptest.NewRequest("GET", "/api/tree", nil)
	again.Header.Set("If-None-Match", etag)
	h.ServeHTTP(rec, again)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("304 carried %d bytes of body", rec.Body.Len())
	}

	// A change has to break the tag, or the client would hold a stale tree
	// until something else invalidated it.
	if err := os.WriteFile(filepath.Join(root, "b.md"), []byte("# B"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	third := httptest.NewRequest("GET", "/api/tree", nil)
	third.Header.Set("If-None-Match", etag)
	h.ServeHTTP(rec, third)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d after a write, want 200", rec.Code)
	}
	if rec.Header().Get("ETag") == etag {
		t.Fatal("the ETag did not move when the tree did")
	}
}

func TestETagMatchesHandlesTheHeaderAsSent(t *testing.T) {
	for _, tc := range []struct {
		header, etag string
		want         bool
	}{
		{`"abc"`, `"abc"`, true},
		{`W/"abc"`, `"abc"`, true},
		{`"zzz", "abc"`, `"abc"`, true},
		{`*`, `"abc"`, true},
		{`"zzz"`, `"abc"`, false},
		{``, `"abc"`, false},
	} {
		if got := etagMatches(tc.header, tc.etag); got != tc.want {
			t.Errorf("etagMatches(%q, %q) = %v, want %v", tc.header, tc.etag, got, tc.want)
		}
	}
}

/* Compressing a body destroys Content-Length, and something was reading it.

   The file viewer decides a body is too large to render BEFORE reading it
   (useBlob.ts fetchBlobText) — the difference between a "too large" card and
   pulling a 50 MB file into a tab. The plaintext size moves aside under its
   own name so that check survives. */
func TestCompressionKeepsTheSizeHintTheViewerNeeds(t *testing.T) {
	body := strings.Repeat("# a markdown file, served to a browser\n", 500)
	h := gzipResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		io.WriteString(w, body)
	}))
	rec := getEnc(t, h, "/api/p/x/file?path=big.md", "gzip")
	if got := rec.Header().Get("Content-Length"); got != "" {
		t.Errorf("Content-Length %q describes bytes that are not there any more", got)
	}
	if got := rec.Header().Get("X-Uncompressed-Length"); got != fmt.Sprint(len(body)) {
		t.Fatalf("X-Uncompressed-Length = %q, want %d", got, len(body))
	}
}

// A range is an offset into the plaintext, so compressing the slice leaves
// Content-Range describing bytes that are no longer there. Reachable, not
// theoretical: http.FileServerFS serves the embedded assets and answers ranges.
func TestRangeResponsesAreNotCompressed(t *testing.T) {
	h := gzipResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Content-Range", "bytes 0-99/100000")
		w.WriteHeader(http.StatusPartialContent)
		io.WriteString(w, strings.Repeat("x", 100))
	}))
	rec := getEnc(t, h, "/assets/index-abc.js", "gzip")
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("a 206 was compressed (%q) — Content-Range now lies", got)
	}
	if rec.Body.Len() != 100 {
		t.Fatalf("partial body = %d bytes, want the 100 the range promised", rec.Body.Len())
	}
}
