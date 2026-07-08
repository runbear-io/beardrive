package webapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

var webDevice = Identity{ID: "webdev", Name: "webhost", Author: "web@test"}

// uploadServer returns an upload-enabled Server over the fake remote, with
// the backend optionally wrapped (e.g. to add signing).
func (f *fakeRemote) uploadServer(wrap func(remote.Backend) remote.Backend) *Server {
	f.t.Helper()
	be, err := remote.Open(context.Background(), "file://"+f.dir)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { be.Close() })
	if wrap != nil {
		be = wrap(be)
	}
	return &Server{
		Source: &RemoteSource{Backend: be, Device: webDevice},
		Volume: "testvol", Refresh: 0,
		Upload: UploadConfig{Enabled: true},
	}
}

// signingBackend fakes an object store that can presign uploads.
type signingBackend struct {
	remote.Backend
	signed []string // keys presigned so far
}

func (b *signingBackend) SignPut(_ context.Context, key string, size int64, ttl time.Duration) (*remote.SignedPut, error) {
	b.signed = append(b.signed, key)
	return &remote.SignedPut{
		URL: "https://storage.example/" + key + "?sig=abc", Method: "PUT",
		Headers: map[string]string{"Content-Length": fmt.Sprint(size)},
		Expires: time.Now().Add(ttl),
	}, nil
}

func do(t *testing.T, h http.Handler, method, url string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		data, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(data)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, url, rd))
	return rec
}

func shaOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func initReq(path, content string) map[string]any {
	return map[string]any{"path": path, "sha256": shaOf(content), "size": len(content)}
}

func TestConfigExposesNoStorageInfo(t *testing.T) {
	f := newFakeRemote(t)
	h := f.server().Handler()
	rec := do(t, h, "GET", "/api/config", nil)
	if rec.Code != 200 {
		t.Fatalf("config: %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, f.dir) || strings.Contains(body, "file://") {
		t.Fatalf("config leaks storage location: %s", body)
	}
	var cfg struct {
		Volume string `json:"volume"`
		Upload struct {
			Enabled bool `json:"enabled"`
		} `json:"upload"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Volume != "testvol" || cfg.Upload.Enabled {
		t.Fatalf("config = %+v, want volume testvol, upload disabled", cfg)
	}
	// the old endpoint that used to expose the remote URL must be gone
	if rec := do(t, h, "GET", "/api/volume", nil); rec.Code == 200 {
		t.Fatalf("/api/volume should not exist, got %d", rec.Code)
	}
}

func TestUploadDisabledByDefault(t *testing.T) {
	f := newFakeRemote(t)
	h := f.server().Handler() // read-only server
	if rec := do(t, h, "POST", "/api/upload/init", initReq("a.md", "x")); rec.Code != http.StatusForbidden {
		t.Fatalf("init on read-only server: %d, want 403", rec.Code)
	}
	if rec := do(t, h, "PUT", "/api/upload/content?path=a.md", []byte("x")); rec.Code != http.StatusForbidden {
		t.Fatalf("content on read-only server: %d, want 403", rec.Code)
	}
	if rec := do(t, h, "POST", "/api/upload/commit", initReq("a.md", "x")); rec.Code != http.StatusForbidden {
		t.Fatalf("commit on read-only server: %d, want 403", rec.Code)
	}
}

// A file:// backend cannot presign, so the server must direct the client to
// upload through it — and the upload must land as blob + journal op with the
// server's device identity and a lamport past every existing op.
func TestServerModeUpload(t *testing.T) {
	f := newFakeRemote(t)
	f.put("deva", "old.md", "existing") // pre-existing history from another device
	srv := f.uploadServer(nil)
	h := srv.Handler()

	rec := do(t, h, "POST", "/api/upload/init", initReq("notes/new.md", "hello"))
	if rec.Code != 200 {
		t.Fatalf("init: %d %s", rec.Code, rec.Body)
	}
	var plan struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "server" {
		t.Fatalf("mode = %q, want server (file backend cannot presign)", plan.Mode)
	}

	rec = do(t, h, "PUT", "/api/upload/content?path=notes/new.md", []byte("hello"))
	if rec.Code != 200 {
		t.Fatalf("content: %d %s", rec.Code, rec.Body)
	}

	// visible immediately (snapshot invalidated), served back intact
	if rec := do(t, h, "GET", "/api/file?path=notes/new.md", nil); rec.Code != 200 || rec.Body.String() != "hello" {
		t.Fatalf("file after upload: %d %q", rec.Code, rec.Body)
	}

	// blob is content-addressed in the store
	if _, err := os.Stat(filepath.Join(f.dir, "blobs", shaOf("hello"))); err != nil {
		t.Fatalf("blob not in store: %v", err)
	}
	// journaled under the server's own device, never anyone else's
	ops, err := journal.ReadFile(filepath.Join(f.dir, "journal", webDevice.ID+".jsonl"))
	if err != nil || len(ops) != 1 {
		t.Fatalf("web journal ops = %v, %v", ops, err)
	}
	op := ops[0]
	if op.Device != webDevice.ID || op.Author != webDevice.Author || op.Seq != 1 {
		t.Fatalf("op identity = %+v", op)
	}
	if op.Lamport <= 1 { // deva's put had lamport 1; ours must sort after it
		t.Fatalf("lamport = %d, want > 1", op.Lamport)
	}
	if op.Kind != journal.KindPut || op.Path != "notes/new.md" || op.Blob != shaOf("hello") || op.Size != 5 {
		t.Fatalf("op = %+v", op)
	}
}

func TestDirectModeUpload(t *testing.T) {
	f := newFakeRemote(t)
	var sb *signingBackend
	srv := f.uploadServer(func(be remote.Backend) remote.Backend {
		sb = &signingBackend{Backend: be}
		return sb
	})
	h := srv.Handler()
	content := "direct content"
	req := initReq("docs/d.md", content)

	rec := do(t, h, "POST", "/api/upload/init", req)
	if rec.Code != 200 {
		t.Fatalf("init: %d %s", rec.Code, rec.Body)
	}
	var plan struct {
		Mode, URL, Method string
		Exists            bool
		Expires           time.Time
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "direct" || plan.Exists {
		t.Fatalf("plan = %+v, want fresh direct upload", plan)
	}
	wantKey := "blobs/" + shaOf(content)
	if !strings.Contains(plan.URL, wantKey) || plan.Method != "PUT" {
		t.Fatalf("plan = %+v, want presigned PUT of %s", plan, wantKey)
	}
	if plan.Expires.IsZero() || plan.Expires.After(time.Now().Add(DefaultUploadTTL+time.Minute)) {
		t.Fatalf("expires = %v, want bounded by ttl", plan.Expires)
	}
	if len(sb.signed) != 1 || sb.signed[0] != wantKey {
		t.Fatalf("signed keys = %v", sb.signed)
	}

	// committing before the blob arrived must be refused: a journal op must
	// never point at missing content
	if rec := do(t, h, "POST", "/api/upload/commit", req); rec.Code != http.StatusConflict {
		t.Fatalf("commit without blob: %d %s, want 409", rec.Code, rec.Body)
	}

	// simulate the client's direct PUT to storage
	if err := os.WriteFile(filepath.Join(f.dir, "blobs", shaOf(content)), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec := do(t, h, "POST", "/api/upload/commit", req); rec.Code != 200 {
		t.Fatalf("commit: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", "/api/file?path=docs/d.md", nil); rec.Code != 200 || rec.Body.String() != content {
		t.Fatalf("file after direct upload: %d %q", rec.Code, rec.Body)
	}

	// re-uploading identical content: init should say it's already there
	rec = do(t, h, "POST", "/api/upload/init", initReq("copy.md", content))
	var again struct {
		Mode   string
		Exists bool
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &again); err != nil {
		t.Fatal(err)
	}
	if again.Mode != "direct" || !again.Exists {
		t.Fatalf("re-init = %+v, want direct+exists", again)
	}
}

func TestUploadPathValidation(t *testing.T) {
	f := newFakeRemote(t)
	h := f.uploadServer(nil).Handler()
	for _, bad := range []string{"", "/abs.md", "../escape.md", "a/../../b", "dir/", ".bdrive", "x/.bdrive-tmp-1"} {
		body := map[string]any{"path": bad, "sha256": shaOf("x"), "size": 1}
		if rec := do(t, h, "POST", "/api/upload/init", body); rec.Code != http.StatusBadRequest {
			t.Errorf("init path %q: %d, want 400", bad, rec.Code)
		}
		if rec := do(t, h, "PUT", "/api/upload/content?path="+bad, []byte("x")); rec.Code != http.StatusBadRequest {
			t.Errorf("content path %q: %d, want 400", bad, rec.Code)
		}
	}
	// bad sha256 values
	for _, sha := range []string{"", "xyz", strings.Repeat("A", 64)} {
		body := map[string]any{"path": "ok.md", "sha256": sha, "size": 1}
		if rec := do(t, h, "POST", "/api/upload/init", body); rec.Code != http.StatusBadRequest {
			t.Errorf("init sha %q: %d, want 400", sha, rec.Code)
		}
	}
}

func TestDirSourceUpload(t *testing.T) {
	root := t.TempDir()
	srv := &Server{
		Source: &DirSource{Root: root}, Volume: "local", Refresh: 0,
		Upload: UploadConfig{Enabled: true},
	}
	h := srv.Handler()

	// a plain folder cannot presign either → server mode
	rec := do(t, h, "POST", "/api/upload/init", initReq("sub/note.md", "hi"))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"server"`) {
		t.Fatalf("init: %d %s, want server mode", rec.Code, rec.Body)
	}
	if rec := do(t, h, "PUT", "/api/upload/content?path=sub/note.md", []byte("hi")); rec.Code != 200 {
		t.Fatalf("content: %d %s", rec.Code, rec.Body)
	}
	data, err := os.ReadFile(filepath.Join(root, "sub", "note.md"))
	if err != nil || string(data) != "hi" {
		t.Fatalf("file on disk = %q, %v", data, err)
	}
	// direct-mode commit makes no sense for a plain folder
	if rec := do(t, h, "POST", "/api/upload/commit", initReq("sub/note.md", "hi")); rec.Code != http.StatusBadRequest {
		t.Fatalf("commit on dir source: %d, want 400", rec.Code)
	}
	// upload config advertises enabled
	rec = do(t, h, "GET", "/api/config", nil)
	if !strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Fatalf("config = %s, want upload enabled", rec.Body)
	}
}

// Uploads from two requests racing must serialize on the journal: both ops
// survive with distinct seqs.
func TestConcurrentCommits(t *testing.T) {
	f := newFakeRemote(t)
	h := f.uploadServer(nil).Handler()
	done := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			content := fmt.Sprintf("body-%d", i)
			rec := do(t, h, "PUT", fmt.Sprintf("/api/upload/content?path=f%d.md", i), []byte(content))
			done <- rec.Code
		}(i)
	}
	for i := 0; i < 2; i++ {
		if code := <-done; code != 200 {
			t.Fatalf("concurrent upload: %d", code)
		}
	}
	ops, err := journal.ReadFile(filepath.Join(f.dir, "journal", webDevice.ID+".jsonl"))
	if err != nil || len(ops) != 2 {
		t.Fatalf("ops = %v, %v; want 2", ops, err)
	}
	if ops[0].Seq == ops[1].Seq || ops[0].Lamport == ops[1].Lamport {
		t.Fatalf("seq/lamport must be distinct: %+v %+v", ops[0], ops[1])
	}
}
