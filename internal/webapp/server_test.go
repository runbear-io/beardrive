package webapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

// fakeRemote builds a beardrive remote layout (journal/<dev>.jsonl + blobs/<sha>)
// in a temp dir and returns a Server over it.
type fakeRemote struct {
	t   *testing.T
	dir string
	seq map[string]int64
	lam int64
}

func newFakeRemote(t *testing.T) *fakeRemote {
	t.Helper()
	return newFakeRemoteAt(t, t.TempDir())
}

// newFakeRemoteAt builds the remote layout at a specific directory (e.g. a
// project prefix inside a hub's storage root).
func newFakeRemoteAt(t *testing.T, dir string) *fakeRemote {
	t.Helper()
	for _, d := range []string{"journal", "blobs"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &fakeRemote{t: t, dir: dir, seq: map[string]int64{}}
}

func (f *fakeRemote) put(dev, path, content string) {
	f.t.Helper()
	sum := sha256.Sum256([]byte(content))
	blob := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(f.dir, "blobs", blob), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.append(dev, journal.Op{
		Kind: journal.KindPut, Path: path,
		Blob: blob, Size: int64(len(content)), Mode: 0o644,
	})
}

func (f *fakeRemote) del(dev, path string) {
	f.t.Helper()
	f.append(dev, journal.Op{Kind: journal.KindDelete, Path: path})
}

func (f *fakeRemote) append(dev string, op journal.Op) {
	f.t.Helper()
	f.lam++
	f.seq[dev]++
	op.Seq, op.Lamport = f.seq[dev], f.lam
	op.Time = time.Now().UTC()
	op.Device, op.DeviceName, op.Author = dev, dev, dev+"@test"
	if err := journal.Append(filepath.Join(f.dir, "journal", dev+".jsonl"), []journal.Op{op}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeRemote) server() *Server {
	f.t.Helper()
	be, err := remote.Open(context.Background(), "file://"+f.dir)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { be.Close() })
	return &Server{Source: &RemoteSource{Backend: be}, Volume: "testvol", Refresh: 0}
}

func get(t *testing.T, h http.Handler, url string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
	return rec
}

func TestTreeAndFile(t *testing.T) {
	f := newFakeRemote(t)
	f.put("deva", "readme.md", "# Hello")
	f.put("deva", "notes/plan.md", "- step one")
	f.put("devb", "notes/img.png", "not-really-a-png")
	h := f.server().Handler()

	rec := get(t, h, "/api/tree")
	if rec.Code != 200 {
		t.Fatalf("tree: %d %s", rec.Code, rec.Body)
	}
	var root Node
	if err := json.Unmarshal(rec.Body.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	if len(root.Children) != 2 {
		t.Fatalf("root children = %d, want 2 (notes/, readme.md)", len(root.Children))
	}
	if !root.Children[0].Dir || root.Children[0].Name != "notes" {
		t.Fatalf("first child = %+v, want dir notes (folders first)", root.Children[0])
	}
	if got := len(root.Children[0].Children); got != 2 {
		t.Fatalf("notes/ children = %d, want 2", got)
	}

	rec = get(t, h, "/api/file?path=readme.md")
	if rec.Code != 200 || rec.Body.String() != "# Hello" {
		t.Fatalf("file: %d %q", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Fatalf("content-type = %q", ct)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	req := httptest.NewRequest("GET", "/api/file?path=readme.md", nil)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("etag revalidate: %d, want 304", rec.Code)
	}

	if rec := get(t, h, "/api/file?path=nope.md"); rec.Code != 404 {
		t.Fatalf("missing file: %d, want 404", rec.Code)
	}
}

func TestDeleteHidesFile(t *testing.T) {
	f := newFakeRemote(t)
	f.put("deva", "a.md", "a")
	f.put("deva", "b.md", "b")
	f.del("devb", "a.md")
	h := f.server().Handler()

	var root Node
	if err := json.Unmarshal(get(t, h, "/api/tree").Body.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	if len(root.Children) != 1 || root.Children[0].Name != "b.md" {
		t.Fatalf("tree after delete = %+v, want only b.md", root.Children)
	}
	if rec := get(t, h, "/api/file?path=a.md"); rec.Code != 404 {
		t.Fatalf("deleted file: %d, want 404", rec.Code)
	}
}

func TestRenderMarkdown(t *testing.T) {
	f := newFakeRemote(t)
	f.put("deva", "doc.md", "# Title\n\nsee [[plan]] and [[plan|the plan]]\n\n<script>x</script>")
	h := f.server().Handler()

	rec := get(t, h, "/api/render?path=doc.md")
	if rec.Code != 200 {
		t.Fatalf("render: %d %s", rec.Code, rec.Body)
	}
	var doc struct {
		HTML   string `json:"html"`
		Author string `json:"author"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<h1", "Title", `href="wiki:plan"`, ">the plan</a>"} {
		if !strings.Contains(doc.HTML, want) {
			t.Errorf("html missing %q:\n%s", want, doc.HTML)
		}
	}
	if strings.Contains(doc.HTML, "<script>") {
		t.Errorf("raw HTML not escaped:\n%s", doc.HTML)
	}
	if doc.Author != "deva@test" {
		t.Errorf("author = %q", doc.Author)
	}
}

func TestDownload(t *testing.T) {
	f := newFakeRemote(t)
	f.put("deva", "notes/plan.md", "content")
	h := f.server().Handler()

	rec := get(t, h, "/api/download?path=notes/plan.md")
	if rec.Code != 200 || rec.Body.String() != "content" {
		t.Fatalf("download: %d %q", rec.Code, rec.Body)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="plan.md"` {
		t.Fatalf("content-disposition = %q", cd)
	}
}

func TestFrontendServed(t *testing.T) {
	f := newFakeRemote(t)
	h := f.server().Handler()
	rec := get(t, h, "/")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<title>BearDrive</title>") {
		t.Fatalf("index: %d", rec.Code)
	}
}

func TestExpandWikilinks(t *testing.T) {
	got := string(expandWikilinks([]byte("a [[x y]] b [[u|v]] c [[no")))
	want := "a [x y](wiki:x%20y) b [v](wiki:u) c [[no"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
