package webapp

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// spyHub is a proxy in front of an existing hub that watches ONE client: it
// counts the bodies both ways and remembers which content encodings crossed.
//
// Per-client, and deliberately not countingHub with a mark taken between
// phases. `bdrive init` starts a daemon, so a mark placed after it measures
// whatever the daemon had not already done — which is how the first draft of
// this test reported a 282x pull ratio and proved nothing. Giving the client
// its own front door makes every counted byte and every observed header that
// client's, whenever it happened.
type spy struct {
	up, down atomic.Int64
	mu       sync.Mutex
	sentEnc  map[string]bool // Content-Encoding the client PUT with
	gotEnc   map[string]bool // Content-Encoding the hub answered it with
}

func (s *spy) note(m map[string]bool, enc string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m[enc] = true
}

func (s *spy) saw(m map[string]bool, enc string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return m[enc]
}

func spyHub(t *testing.T, inner *httptest.Server) (*httptest.Server, *spy) {
	t.Helper()
	sp := &spy{sentEnc: map[string]bool{}, gotEnc: map[string]bool{}}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/store/") {
			sp.note(sp.sentEnc, r.Header.Get("Content-Encoding"))
		}
		r.Body = &countingReader{r: r.Body, n: &sp.up}
		rec := &encSpyRW{ResponseWriter: w, sp: sp}
		inner.Config.Handler.ServeHTTP(rec, r)
	}))
	t.Cleanup(proxy.Close)
	return proxy, sp
}

type encSpyRW struct {
	http.ResponseWriter
	sp    *spy
	noted bool
}

func (e *encSpyRW) WriteHeader(code int) {
	e.note()
	e.ResponseWriter.WriteHeader(code)
}

func (e *encSpyRW) Write(p []byte) (int, error) {
	e.note()
	n, err := e.ResponseWriter.Write(p)
	e.sp.down.Add(int64(n))
	return n, err
}

func (e *encSpyRW) note() {
	if !e.noted {
		e.noted = true
		e.sp.note(e.sp.gotEnc, e.Header().Get("Content-Encoding"))
	}
}

// Transport compression against the REAL binary that predates it (oldBinRef,
// pinned at the commit before delta sync — which is also the commit before
// this). Both halves of the mixed-fleet claim, driven end to end rather than
// argued:
//
//   - the old binary's PULL is compressed with no client change, because
//     net/http asks for gzip and inflates on its own. This is the whole reason
//     the read leg needed no negotiation.
//   - the old binary's PUSH stays raw and still converges, because it never
//     reads the accept_encoding sign() now answers — the same field an old HUB
//     omits, which is what keeps a new client from posting gzip bytes to a hub
//     that would store them under the sha256 of the plaintext.
func TestCompressionE2E_OldBinaryPullsCompressedAndPushesRaw(t *testing.T) {
	inner := startTestHub(t)
	fresh := newCLIEnvOn(t, inner)
	oldDoor, sp := spyHub(t, inner)
	old := newCLIEnvBin(t, oldDoor, buildOldBinary(t))

	dirA := filepath.Join(t.TempDir(), "proj")
	initProject(t, fresh, dirA, "compress-e2e", false)
	raw := 0
	for i := 0; i < 40; i++ {
		body := []byte(fmt.Sprintf("# note %d\n\n%s", i, markdownish(i)))
		if err := os.WriteFile(filepath.Join(dirA, fmt.Sprintf("note-%02d.md", i)), body, 0o644); err != nil {
			t.Fatal(err)
		}
		raw += len(body)
	}
	syncNow(t, fresh, dirA)

	// The old binary pulls the corpus it has never seen. Everything it has
	// ever received is behind its own door, so no window has to be guessed.
	dirOld := filepath.Join(t.TempDir(), "proj")
	initProject(t, old, dirOld, "compress-e2e", true)
	syncNow(t, old, dirOld)
	for i := 0; i < 40; i++ {
		want := fmt.Sprintf("# note %d\n\n%s", i, markdownish(i))
		got, err := os.ReadFile(filepath.Join(dirOld, fmt.Sprintf("note-%02d.md", i)))
		if err != nil || string(got) != want {
			t.Fatalf("old binary did not converge on note %d: %v", i, err)
		}
	}
	pulled := sp.down.Load()
	t.Logf("old binary received %d bytes total for a %d-byte corpus (%.2fx)", pulled, raw, float64(raw)/float64(pulled))
	if !sp.saw(sp.gotEnc, "gzip") {
		t.Fatal("the hub never answered the old binary with Content-Encoding: gzip — the free win is not happening")
	}
	// Everything that client has ever been sent, sign-in and listings included,
	// against the corpus alone.
	if ratio := float64(raw) / float64(pulled); ratio < 2.5 {
		t.Fatalf("old binary received %d bytes for a %d-byte corpus (%.2fx), want at least 2.5x", pulled, raw, ratio)
	}

	// The old binary pushes. It never learned to read accept_encoding, so every
	// body it PUT must be raw — asserted on the headers rather than on a byte
	// count, so a daemon tick cannot change the answer.
	back := []byte("# from the old client\n\n" + markdownish(99))
	if err := os.WriteFile(filepath.Join(dirOld, "from-old.md"), back, 0o644); err != nil {
		t.Fatal(err)
	}
	syncNow(t, old, dirOld)
	if sp.saw(sp.sentEnc, "gzip") {
		t.Fatal("the old binary sent a compressed body — it cannot know how, so the hub must have been asked to inflate one it never sent")
	}
	if !sp.saw(sp.sentEnc, "") {
		t.Fatal("the old binary never PUT anything; the push half was not exercised")
	}
	syncNow(t, fresh, dirA)
	if got, err := os.ReadFile(filepath.Join(dirA, "from-old.md")); err != nil || !bytes.Equal(got, back) {
		t.Fatalf("the new client did not converge on the old binary's raw push: %v", err)
	}
}

// Delta sync grew the key space by two classes after this feature was written.
// Both ride the same compressed wire, and both keep the gates that make them
// safe — because those gates read the spooled PLAINTEXT, below the inflate.
//
// The manifest is the one that matters: it is never presigned, so in production
// it ALWAYS goes through the relay path that compresses. A large file that
// syncs as chunks + manifest is the case where a gzip body would otherwise have
// been the thing the write-once compare and the chunks-exist gate saw.
func TestCompressionE2E_ChunksAndManifestsOverGzip(t *testing.T) {
	inner := startTestHub(t)
	door, sp := spyHub(t, inner)
	a := newCLIEnvBin(t, door, "")
	b := newCLIEnvOn(t, inner)

	dirA := filepath.Join(t.TempDir(), "proj")
	initProject(t, a, dirA, "compress-chunks", false)
	// Past the chunking threshold, and compressible — so the manifest, the
	// chunks and the journal all take the gzip path rather than the skip path.
	var big []byte
	for len(big) < 12<<20 {
		big = append(big, fmt.Sprintf("line %d: %s\n", len(big), markdownish(len(big)%7))...)
	}
	if err := os.WriteFile(filepath.Join(dirA, "big.md"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	syncNow(t, a, dirA)
	// The push really did compress — otherwise the rest of this test is only
	// re-testing delta sync.
	if !sp.saw(sp.sentEnc, "gzip") {
		t.Fatal("the chunked push sent nothing compressed")
	}

	dirB := filepath.Join(t.TempDir(), "proj")
	initProject(t, b, dirB, "compress-chunks", true)
	syncNow(t, b, dirB)
	got, err := os.ReadFile(filepath.Join(dirB, "big.md"))
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("chunked file did not converge over the compressed wire: %v, %d bytes", err, len(got))
	}

	// Edit it: the second push re-puts a manifest under a NEW key and re-sends
	// only the changed chunks, all compressed. If the write-once compare or the
	// chunks-exist gate had been reading gzip bytes, this is where it breaks.
	copy(big[6<<20:], []byte("EDITED"))
	if err := os.WriteFile(filepath.Join(dirA, "big.md"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	syncNow(t, a, dirA)
	syncNow(t, b, dirB)
	if got, err := os.ReadFile(filepath.Join(dirB, "big.md")); err != nil || !bytes.Equal(got, big) {
		t.Fatalf("the edit did not converge: %v", err)
	}
	// A repeat sync must be a no-op, not a manifest conflict: an identical
	// re-put is the retry path, and it is compared on the plaintext.
	syncNow(t, a, dirA)
}

// markdownish is a stand-in for the real corpus — markdown and source, the
// content that made compression worth doing.
func markdownish(seed int) string {
	var b []byte
	for i := 0; i < 60; i++ {
		b = append(b, fmt.Sprintf("- item %d/%d: the sync wire carries markdown and source files\n", seed, i)...)
	}
	return string(b)
}
