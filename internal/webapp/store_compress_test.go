package webapp

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

// Transport compression on /store/*. Everything here is about one invariant
// holding while the wire changes underneath it: content addressing is over the
// UNCOMPRESSED bytes. The hub inflates before it hashes, stores plaintext, and
// bills plaintext — the only thing that is ever compressed is the transfer.

const textCorpus = "# release notes\n\nthe corpus is markdown and source files, " +
	"five to ten kilobytes each, which is exactly what gzip is good at.\n"

// meterQuota records what the hub bills, separately per meter: usage is a
// storage bill (uncompressed), egress is a bandwidth bill (compressed).
type meterQuota struct {
	UnlimitedQuota
	mu     sync.Mutex
	usage  int64
	egress int64
}

func (q *meterQuota) RecordUsage(_ string, b int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.usage += b
}

func (q *meterQuota) RecordEgress(_ string, b int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.egress += b
}

// doRaw sends an exact body with exact headers — reads_test.go's doHdr
// JSON-marshals what it is given, and here the bytes on the wire are the
// subject.
func doRaw(t *testing.T, h http.Handler, method, url string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, url, bytes.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gunzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func incompressible(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(7)).Read(b)
	return b
}

// The pull leg. It needs no client change at all — net/http sends
// Accept-Encoding: gzip on its own and inflates transparently — so this is what
// a device built before this feature existed gets for free, and it must still
// arrive as the exact stored bytes.
func TestStoreGetCompresses(t *testing.T) {
	srv, p, root := newHub(t, false, nil)
	q := &meterQuota{}
	srv.Quota = q
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	text := strings.Repeat(textCorpus, 200)
	f.put("deva", "notes.md", text)
	h := srv.Handler()
	key := "blobs/" + shaOf(text)
	url := "/api/p/" + p.ID + "/store/object?key=" + key

	rec := doRaw(t, h, "GET", url, nil, map[string]string{"Accept-Encoding": "gzip"})
	if rec.Code != 200 || rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip GET: %d %q", rec.Code, rec.Header().Get("Content-Encoding"))
	}
	wire := rec.Body.Bytes()
	if got := string(gunzipBytes(t, wire)); got != text {
		t.Fatal("the compressed response does not inflate to the stored bytes")
	}
	if ratio := float64(len(text)) / float64(len(wire)); ratio < 2.5 {
		t.Fatalf("wire ratio %.2fx, want at least 2.5x", ratio)
	}
	// The bandwidth meter reports what left the socket, not what was stored.
	// Inverted (counter inside the gzip writer) this reads len(text) and
	// nothing else fails.
	if q.egress != int64(len(wire)) {
		t.Fatalf("RecordEgress = %d, want the compressed %d", q.egress, len(wire))
	}

	// A caller that did not ask gets exactly what it always got.
	rec = doRaw(t, h, "GET", url, nil, nil)
	if rec.Code != 200 || rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != text {
		t.Fatalf("plain GET: %d %q", rec.Code, rec.Header().Get("Content-Encoding"))
	}
}

// Already-compressed content must cross untouched: gzip on a JPEG pays CPU to
// make the payload ~0.1% bigger.
func TestStoreGetSkipsIncompressible(t *testing.T) {
	srv, p, root := newHub(t, false, nil)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	raw := incompressible(300 << 10)
	f.put("deva", "photo.jpg", string(raw))
	h := srv.Handler()
	url := "/api/p/" + p.ID + "/store/object?key=blobs/" + shaOf(string(raw))

	for _, ae := range []string{"gzip", ""} {
		rec := doRaw(t, h, "GET", url, nil, map[string]string{"Accept-Encoding": ae})
		if rec.Code != 200 || rec.Header().Get("Content-Encoding") != "" {
			t.Fatalf("Accept-Encoding %q: %d %q", ae, rec.Code, rec.Header().Get("Content-Encoding"))
		}
		if n := rec.Body.Len(); float64(n) > float64(len(raw))*1.01 {
			t.Fatalf("wire carried %d bytes for a %d-byte payload", n, len(raw))
		}
		if !bytes.Equal(rec.Body.Bytes(), raw) {
			t.Fatal("body is not the stored bytes")
		}
	}
}

// The listing is the first call of every cycle on every device, and it is
// repetitive JSON.
func TestStoreListCompresses(t *testing.T) {
	srv, p, root := newHub(t, false, nil)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	for i := 0; i < 50; i++ {
		f.put("deva", "notes/"+strings.Repeat("x", i+1)+".md", textCorpus+strings.Repeat("y", i+1))
	}
	h := srv.Handler()
	url := "/api/p/" + p.ID + "/store/list?prefix=blobs/"

	rec := doRaw(t, h, "GET", url, nil, map[string]string{"Accept-Encoding": "gzip"})
	if rec.Code != 200 || rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip list: %d %q", rec.Code, rec.Header().Get("Content-Encoding"))
	}
	var list struct {
		Objects []remote.Object `json:"objects"`
	}
	if err := json.Unmarshal(gunzipBytes(t, rec.Body.Bytes()), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Objects) != 50 {
		t.Fatalf("objects = %d, want 50", len(list.Objects))
	}
	plain := doRaw(t, h, "GET", url, nil, nil)
	if plain.Header().Get("Content-Encoding") != "" {
		t.Fatal("a caller that did not ask for gzip got it anyway")
	}
	if rec.Body.Len() >= plain.Body.Len() {
		t.Fatalf("compressed listing is %d bytes, raw is %d", rec.Body.Len(), plain.Body.Len())
	}
}

// The push leg is negotiated, and this is the advertisement the client reads.
func TestStoreSignAdvertisesGzip(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	rec := do(t, h, "POST", "/api/p/"+p.ID+"/store/sign",
		map[string]any{"key": "blobs/" + shaOf("hi"), "size": 2})
	var plan struct {
		Mode           string   `json:"mode"`
		AcceptEncoding []string `json:"accept_encoding"`
	}
	mustJSON(t, rec, &plan)
	if plan.Mode != "server" || len(plan.AcceptEncoding) != 1 || plan.AcceptEncoding[0] != "gzip" {
		t.Fatalf("plan = %+v", plan)
	}
}

// A compressed PUT is stored as plaintext under the plaintext's hash, and
// billed for the plaintext — the object stored is uncompressed, so that is what
// the storage bill is for.
func TestStorePutInflatesBeforeItHashes(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	q := &meterQuota{}
	srv.Quota = q
	h := srv.Handler()
	text := strings.Repeat(textCorpus, 200)
	key := "blobs/" + shaOf(text)
	body := gzipBytes(t, []byte(text))
	if len(body) >= len(text) {
		t.Fatal("fixture is not actually compressed")
	}

	rec := doRaw(t, h, "PUT", "/api/p/"+p.ID+"/store/object?key="+key, body,
		map[string]string{"Content-Encoding": "gzip"})
	if rec.Code != 200 {
		t.Fatalf("gzip put: %d %s", rec.Code, rec.Body)
	}
	stored, err := os.ReadFile(filepath.Join(root, p.ID, key))
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != text {
		t.Fatal("storage does not hold the plaintext")
	}
	if q.usage != int64(len(text)) {
		t.Fatalf("RecordUsage = %d, want the uncompressed %d", q.usage, len(text))
	}
}

// Content addressing is unchanged: a compressed body that decodes to bytes
// which are not what the key names is refused, exactly like a raw one.
func TestStorePutGzipMustStillHashToItsKey(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	body := gzipBytes(t, []byte("the plaintext nobody asked for"))
	rec := doRaw(t, h, "PUT", "/api/p/"+p.ID+"/store/object?key=blobs/"+shaOf("something else"), body,
		map[string]string{"Content-Encoding": "gzip"})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "does not hash to its key") {
		t.Fatalf("mismatched gzip put: %d %s", rec.Code, rec.Body)
	}
}

// Content-Encoding severs the "a byte on the wire costs a byte on disk"
// relationship that made spool() safe unbounded, and the inflate lands before
// CheckWrite can refuse anything. A small body must not become an arbitrary
// hub-side write.
func TestStorePutRefusesAGzipBomb(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	h := srv.Handler()
	// ~256 MiB of zeros compresses to a few hundred KB — the shape of the bomb,
	// small enough to keep the test fast.
	bomb := gzipBytes(t, make([]byte, maxInflatedPut+1))
	if len(bomb) > 1<<20 {
		t.Fatalf("bomb fixture is %d bytes, expected it to compress much harder", len(bomb))
	}
	rec := doRaw(t, h, "PUT", "/api/p/"+p.ID+"/store/object?key=blobs/"+strings.Repeat("a", 64), bomb,
		map[string]string{"Content-Encoding": "gzip"})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("bomb: %d %s, want 413", rec.Code, rec.Body)
	}
	blobs, _ := os.ReadDir(filepath.Join(root, p.ID, "blobs"))
	if len(blobs) != 0 {
		t.Fatalf("the bomb stored %d objects", len(blobs))
	}
}

// The two ways a declared encoding can be the client's fault, kept apart from
// "the hub's storage broke".
func TestStorePutRejectsBadEncodings(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	url := "/api/p/" + p.ID + "/store/object?key=blobs/" + shaOf("hi")

	rec := doRaw(t, h, "PUT", url, []byte("not gzip at all"), map[string]string{"Content-Encoding": "gzip"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("lying Content-Encoding: %d %s, want 400", rec.Code, rec.Body)
	}
	rec = doRaw(t, h, "PUT", url, []byte("hi"), map[string]string{"Content-Encoding": "br"})
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("unsupported codec: %d %s, want 415", rec.Code, rec.Body)
	}
	// Truncated gzip: the header parses, the stream does not finish.
	full := gzipBytes(t, []byte(strings.Repeat(textCorpus, 50)))
	rec = doRaw(t, h, "PUT", url, full[:len(full)/2], map[string]string{"Content-Encoding": "gzip"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("truncated gzip: %d %s, want 400", rec.Code, rec.Body)
	}
}

// A journal body is read for its ops after the inflate, so every rule that
// protects the log still applies to a compressed push — including the
// append-only one, which is the invariant a compressed body could otherwise
// have smuggled past.
func TestStorePutGzippedJournalKeepsItsOps(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	url := "/api/p/" + p.ID + "/store/object?key=journal/deva.jsonl"
	hdr := map[string]string{"Content-Encoding": "gzip", "X-Bdrive-Device": "deva"}

	line := func(seq int64, path string) []byte {
		b, err := json.Marshal(journal.Op{
			Kind: journal.KindPut, Path: path, Seq: seq, Device: "deva",
			Blob: shaOf(path), Size: 1, Lamport: seq,
		})
		if err != nil {
			t.Fatal(err)
		}
		return append(b, '\n')
	}
	two := append(line(1, "a.md"), line(2, "b.md")...)
	if rec := doRaw(t, h, "PUT", url, gzipBytes(t, two), hdr); rec.Code != 200 {
		t.Fatalf("gzipped journal put: %d %s", rec.Code, rec.Body)
	}
	// Same journal, one op short: refused, compressed or not.
	if rec := doRaw(t, h, "PUT", url, gzipBytes(t, line(1, "a.md")), hdr); rec.Code != http.StatusConflict {
		t.Fatalf("truncating journal put: %d %s, want 409", rec.Code, rec.Body)
	}
}
