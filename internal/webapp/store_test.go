package webapp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/remote"
)

// newHub builds a hub server over a fresh storage root with one project.
func newHub(t *testing.T, upload bool, wrap func(remote.Backend) remote.Backend) (*Server, Project, string) {
	t.Helper()
	root := t.TempDir()
	be, err := remote.Open(context.Background(), "file://"+root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { be.Close() })
	if wrap != nil {
		be = wrap(be)
	}
	db, err := OpenProjectDB(filepath.Join(t.TempDir(), "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := db.GetOrCreate("proj")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		Root: be, Projects: db, Device: webDevice,
		Refresh: 0, Upload: UploadConfig{Enabled: upload},
	}
	return srv, p, root
}

func TestProjectAPI(t *testing.T) {
	srv, seeded, _ := newHub(t, true, nil)
	h := srv.Handler()

	// create-or-join by name: new name creates, same name joins
	rec := do(t, h, "POST", "/api/projects", map[string]string{"name": "my-app"})
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Project Project `json:"project"`
		Created bool    `json:"created"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Created || out.Project.Name != "my-app" || !strings.HasPrefix(out.Project.ID, "p-") {
		t.Fatalf("create = %+v", out)
	}
	id := out.Project.ID

	rec = do(t, h, "POST", "/api/projects", map[string]string{"name": "my-app"})
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Created || out.Project.ID != id {
		t.Fatalf("re-create = %+v, want join of %s", out, id)
	}

	// list has both projects; get by id resolves
	rec = do(t, h, "GET", "/api/projects", nil)
	var list struct {
		Projects []Project `json:"projects"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Projects) != 2 {
		t.Fatalf("projects = %+v, want 2", list.Projects)
	}
	if rec := do(t, h, "GET", "/api/projects/"+seeded.ID, nil); rec.Code != 200 {
		t.Fatalf("get project: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/api/projects/p-00000000", nil); rec.Code != 404 {
		t.Fatalf("get missing project: %d, want 404", rec.Code)
	}

	// hub config declares hub mode and never names storage
	rec = do(t, h, "GET", "/api/config", nil)
	if !strings.Contains(rec.Body.String(), `"hub"`) || strings.Contains(rec.Body.String(), "file://") {
		t.Fatalf("config = %s", rec.Body)
	}

	// empty name is rejected
	if rec := do(t, h, "POST", "/api/projects", map[string]string{"name": "  "}); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty name: %d, want 400", rec.Code)
	}
}

func TestProjectCreateGatedByUpload(t *testing.T) {
	srv, _, _ := newHub(t, false, nil) // read-only hub
	h := srv.Handler()
	if rec := do(t, h, "POST", "/api/projects", map[string]string{"name": "x"}); rec.Code != http.StatusForbidden {
		t.Fatalf("create on read-only hub: %d, want 403", rec.Code)
	}
	// reads still work
	if rec := do(t, h, "GET", "/api/projects", nil); rec.Code != 200 {
		t.Fatalf("list on read-only hub: %d", rec.Code)
	}
}

func TestProjectDBPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projects.json")
	db, err := OpenProjectDB(path)
	if err != nil {
		t.Fatal(err)
	}
	p1, created, err := db.GetOrCreate("alpha")
	if err != nil || !created {
		t.Fatalf("create: %+v %v %v", p1, created, err)
	}
	// a fresh open (server restart) sees the same project
	db2, err := OpenProjectDB(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := db2.Get(p1.ID)
	if !ok || got.Name != "alpha" {
		t.Fatalf("reload = %+v %v", got, ok)
	}
	p2, created, err := db2.GetOrCreate("alpha")
	if err != nil || created || p2.ID != p1.ID {
		t.Fatalf("get-or-create after reload = %+v %v %v", p2, created, err)
	}
}

func TestStoreAPIReads(t *testing.T) {
	srv, p, root := newHub(t, false, nil) // reads work even on read-only hubs
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.put("deva", "readme.md", "hello")
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/store/"

	rec := do(t, h, "GET", base+"list?prefix=journal/", nil)
	if rec.Code != 200 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	var list struct {
		Objects []remote.Object `json:"objects"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Objects) != 1 || list.Objects[0].Key != "journal/deva.jsonl" {
		t.Fatalf("objects = %+v", list.Objects)
	}

	rec = do(t, h, "GET", base+"object?key=journal/deva.jsonl", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"readme.md"`) {
		t.Fatalf("get journal: %d %s", rec.Code, rec.Body)
	}
	blobKey := "blobs/" + shaOf("hello")
	rec = do(t, h, "GET", base+"exists?key="+blobKey, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "true") {
		t.Fatalf("exists: %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, "GET", base+"object?key="+blobKey, nil)
	if rec.Code != 200 || rec.Body.String() != "hello" {
		t.Fatalf("get blob: %d %q", rec.Code, rec.Body)
	}

	// the viewer works per project too
	rec = do(t, h, "GET", "/api/p/"+p.ID+"/file?path=readme.md", nil)
	if rec.Code != 200 || rec.Body.String() != "hello" {
		t.Fatalf("viewer file: %d %q", rec.Code, rec.Body)
	}
	// unknown project 404s
	if rec := do(t, h, "GET", "/api/p/p-00000000/store/list?prefix=journal/", nil); rec.Code != 404 {
		t.Fatalf("unknown project: %d, want 404", rec.Code)
	}
}

func TestStoreAPIKeyValidation(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/store/"
	bad := []string{
		"", "x", "blobs/short", "blobs/../../etc/passwd",
		"journal/../device.json", "journal/a/b.jsonl", "journal/dev.txt",
		"blobs/" + strings.Repeat("G", 64), // non-hex
	}
	for _, key := range bad {
		if rec := do(t, h, "GET", base+"object?key="+key, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("get %q: %d, want 400", key, rec.Code)
		}
		if rec := do(t, h, "PUT", base+"object?key="+key, []byte("x")); rec.Code != http.StatusBadRequest {
			t.Errorf("put %q: %d, want 400", key, rec.Code)
		}
	}
	if rec := do(t, h, "GET", base+"list?prefix=../", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("list bad prefix: %d, want 400", rec.Code)
	}
}

func TestStoreAPIWriteGating(t *testing.T) {
	srv, p, _ := newHub(t, false, nil)
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/store/"
	key := "blobs/" + shaOf("x")
	if rec := do(t, h, "PUT", base+"object?key="+key, []byte("x")); rec.Code != http.StatusForbidden {
		t.Fatalf("put with uploads off: %d, want 403", rec.Code)
	}
	if rec := do(t, h, "POST", base+"sign", map[string]any{"key": key, "size": 1}); rec.Code != http.StatusForbidden {
		t.Fatalf("sign with uploads off: %d, want 403", rec.Code)
	}
}

func TestSingleVolumeServerHostsNoProjects(t *testing.T) {
	srv := &Server{Source: &DirSource{Root: t.TempDir()}, Volume: "local",
		Upload: UploadConfig{Enabled: true}}
	h := srv.Handler()
	if rec := do(t, h, "GET", "/api/projects", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("projects on single server: %d, want 404", rec.Code)
	}
	if rec := do(t, h, "GET", "/api/p/p-00000000/tree", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("project tree on single server: %d, want 404", rec.Code)
	}
}

// Journals must never be presigned — they are mutable state, only blobs are
// immutable — so sign answers "server" for them even on a signing backend.
// Presigning must survive the project-prefix wrapper.
func TestStoreSignJournalAlwaysViaServer(t *testing.T) {
	var sb *signingBackend
	srv, p, _ := newHub(t, true, func(be remote.Backend) remote.Backend {
		sb = &signingBackend{Backend: be}
		return sb
	})
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/store/"

	rec := do(t, h, "POST", base+"sign", map[string]any{"key": "journal/dev1.jsonl", "size": 10})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"server"`) {
		t.Fatalf("sign journal: %d %s, want server mode", rec.Code, rec.Body)
	}
	blobKey := "blobs/" + shaOf("z")
	rec = do(t, h, "POST", base+"sign", map[string]any{"key": blobKey, "size": 1})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"direct"`) {
		t.Fatalf("sign blob: %d %s, want direct mode", rec.Code, rec.Body)
	}
	// the presigned key is namespaced under the project prefix
	if len(sb.signed) != 1 || sb.signed[0] != p.ID+"/"+blobKey {
		t.Fatalf("signed keys = %v, want [%s/%s]", sb.signed, p.ID, blobKey)
	}
}

// The full client path: an https:// remote backend (remote.Open on the
// server's /p/<id> URL) doing List/Get/Exists/Put against a live hub, and
// projects staying isolated from each other.
func TestHTTPBackendThroughServer(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.put("deva", "seed.md", "seeded")
	other, _, err := srv.Projects.GetOrCreate("other")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	be, err := remote.Open(context.Background(), ts.URL+"/p/"+p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()

	objs, err := be.List(context.Background(), "journal/")
	if err != nil || len(objs) != 1 {
		t.Fatalf("list = %v, %v", objs, err)
	}
	ok, err := be.Exists(context.Background(), "blobs/"+shaOf("seeded"))
	if err != nil || !ok {
		t.Fatalf("exists = %v, %v", ok, err)
	}

	// push a blob + read it back like a syncing device would
	content := "pushed through server"
	blobKey := "blobs/" + shaOf(content)
	if err := be.Put(context.Background(), blobKey, strings.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	rc, err := be.Get(context.Background(), blobKey)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != content {
		t.Fatalf("roundtrip = %q", got)
	}

	// project isolation: the other project sees none of it
	beOther, err := remote.Open(context.Background(), ts.URL+"/p/"+other.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer beOther.Close()
	objs, err = beOther.List(context.Background(), "journal/")
	if err != nil || len(objs) != 0 {
		t.Fatalf("other project journals = %v, %v; want none", objs, err)
	}
	ok, err = beOther.Exists(context.Background(), blobKey)
	if err != nil || ok {
		t.Fatalf("other project sees foreign blob: %v %v", ok, err)
	}
}
