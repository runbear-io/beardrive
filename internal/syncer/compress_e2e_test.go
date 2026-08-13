package syncer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/webapp"
)

// The measurement that justifies the feature, taken where it is real: HTTP
// bodies in and out of the hub.
//
// It has to be measured HERE and not one layer up. A counter wrapping a
// remote.Backend sits ABOVE httpBackend and can only ever see the plaintext it
// hands down — it cannot observe transport encoding at all, and the file://
// backend those counters usually wrap is never compressed by this change.
// countingHub proxies the hub's own handler, so what it counts is the wire.

// countingHub is a hub whose HTTP bodies are counted in both directions.
func countingHub(t *testing.T, storage remote.Backend) (*httptest.Server, webapp.Project, *wireCount) {
	t.Helper()
	db, err := webapp.OpenProjectDB(filepath.Join(t.TempDir(), "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := db.GetOrCreate("vol", "")
	if err != nil {
		t.Fatal(err)
	}
	srv := &webapp.Server{
		Root: storage, Projects: db, Refresh: 0,
		Device: webapp.Identity{ID: "hubdev", Name: "hub", Author: "hub@test"},
		Upload: webapp.UploadConfig{Enabled: true},
	}
	n := &wireCount{}
	h := srv.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = &countingReadCloser{rc: r.Body, n: &n.up}
		h.ServeHTTP(&countingRW{ResponseWriter: w, n: &n.down}, r)
	}))
	t.Cleanup(ts.Close)
	return ts, p, n
}

type wireCount struct{ up, down atomic.Int64 }

func (w *wireCount) reset() { w.up.Store(0); w.down.Store(0) }

type countingReadCloser struct {
	rc interface {
		Read([]byte) (int, error)
		Close() error
	}
	n *atomic.Int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	c.n.Add(int64(n))
	return n, err
}
func (c *countingReadCloser) Close() error { return c.rc.Close() }

type countingRW struct {
	http.ResponseWriter
	n *atomic.Int64
}

func (c *countingRW) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// A text-heavy project must cross the wire at ≥2.5x reduction in BOTH
// directions, and the same project must still converge byte for byte.
func TestCompressionWireRatio(t *testing.T) {
	storage := sharedRemote(t)
	ts, p, wire := countingHub(t, storage)
	viaServer, err := remote.Open(context.Background(), ts.URL+"/p/"+p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer viaServer.Close()

	a := newDevice(t, "deva", viaServer)
	raw := 0
	for i := 0; i < 20; i++ {
		body := fmt.Sprintf("# note %d\n\n%s", i, textish(i))
		write(t, a.Folder, fmt.Sprintf("notes/note-%02d.md", i), body)
		raw += len(body)
	}
	wire.reset()
	cycle(t, a)
	up := wire.up.Load()
	t.Logf("push: %d bytes on the wire for a %d-byte corpus (%.2fx)", up, raw, float64(raw)/float64(up))
	if ratio := float64(raw) / float64(up); ratio < 2.5 {
		t.Fatalf("push carried %d bytes for a %d-byte corpus (%.2fx), want at least 2.5x", up, raw, ratio)
	}

	// A second device pulls the same corpus back down through the same hub.
	b, err := remote.Open(context.Background(), ts.URL+"/p/"+p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	dev := newDevice(t, "devb", b)
	wire.reset()
	if res := cycle(t, dev); res.PulledOps != 20 {
		t.Fatalf("pulled %d ops, want 20", res.PulledOps)
	}
	down := wire.down.Load()
	t.Logf("pull: %d bytes on the wire for a %d-byte corpus (%.2fx)", down, raw, float64(raw)/float64(down))
	if ratio := float64(raw) / float64(down); ratio < 2.5 {
		t.Fatalf("pull carried %d bytes for a %d-byte corpus (%.2fx), want at least 2.5x", down, raw, ratio)
	}
	// Compression is a transport concern: the bytes on disk are the bytes that
	// were written, or none of the above matters.
	for i := 0; i < 20; i++ {
		want := fmt.Sprintf("# note %d\n\n%s", i, textish(i))
		if got := read(t, dev.Folder, fmt.Sprintf("notes/note-%02d.md", i)); got != want {
			t.Fatalf("note %d did not converge", i)
		}
	}
}

// Already-compressed content must cross within ~1% of its raw size, in both
// directions: the skip path exists so gzip does not pay CPU to grow a JPEG.
func TestCompressionSkipsIncompressiblePayload(t *testing.T) {
	storage := sharedRemote(t)
	ts, p, wire := countingHub(t, storage)
	viaServer, err := remote.Open(context.Background(), ts.URL+"/p/"+p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer viaServer.Close()

	a := newDevice(t, "deva", viaServer)
	payload := string(pseudoJPEG(512 << 10))
	write(t, a.Folder, "photo.jpg", payload)
	wire.reset()
	cycle(t, a)
	if up := wire.up.Load(); float64(up) > float64(len(payload))*1.01+4096 {
		t.Fatalf("push carried %d bytes for a %d-byte incompressible payload", up, len(payload))
	}

	b, err := remote.Open(context.Background(), ts.URL+"/p/"+p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	dev := newDevice(t, "devb", b)
	wire.reset()
	cycle(t, dev)
	if down := wire.down.Load(); float64(down) > float64(len(payload))*1.01+4096 {
		t.Fatalf("pull carried %d bytes for a %d-byte incompressible payload", down, len(payload))
	}
	if read(t, dev.Folder, "photo.jpg") != payload {
		t.Fatal("the incompressible payload did not converge")
	}
}

// textish is a stand-in for the real corpus: markdown and source, 5–10 KB.
func textish(seed int) string {
	var b []byte
	for i := 0; i < 120; i++ {
		b = append(b, fmt.Sprintf("- item %d/%d: the sync wire carries markdown and source files\n", seed, i)...)
	}
	return string(b)
}

// pseudoJPEG stands in for content that is already compressed.
func pseudoJPEG(n int) []byte {
	b := make([]byte, n)
	x := uint32(2463534242)
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x)
	}
	return b
}
