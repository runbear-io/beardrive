package webapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// seed writes a put op carrying a signed-in account, like a logged-in device
// does.
func (f *fakeRemote) putAs(dev, user, userName, path, content string) {
	f.t.Helper()
	f.put(dev, path, content)
	// rewrite the last op to carry the account (put() doesn't know it)
	p := filepath.Join(f.dir, "journal", dev+".jsonl")
	ops, err := journal.ReadFile(p)
	if err != nil || len(ops) == 0 {
		f.t.Fatal(err)
	}
	ops[len(ops)-1].User, ops[len(ops)-1].UserName = user, userName
	data, err := journal.Marshal(ops)
	if err != nil {
		f.t.Fatal(err)
	}
	writeFileT(f.t, p, data)
}

func writeFileT(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryAPI(t *testing.T) {
	srv, p, root := newHub(t, false, nil)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.putAs("dev1", "alice@x.io", "Alice", "notes/plan.md", "v1")
	f.putAs("dev1", "alice@x.io", "Alice", "notes/plan.md", "v2 longer")
	f.putAs("dev2", "bob@x.io", "Bob", "notes/other.md", "bob's file")
	f.del("dev2", "notes/other.md")
	f.putAs("dev1", "alice@x.io", "Alice", "readme.md", "top")

	// the server knows dev1 from its store traffic
	srv.Devices, _ = OpenDeviceRegistry(filepath.Join(t.TempDir(), "devices.json"))
	srv.Devices.Observe(DeviceInfo{ID: "dev1", Name: "alice-laptop", OS: "darwin/arm64", IP: "203.0.113.7"})

	h := srv.Handler()
	base := "/api/p/" + p.ID + "/"

	// one file's versions, newest first
	rec := do(t, h, "GET", base+"history?path=notes/plan.md", nil)
	if rec.Code != 200 {
		t.Fatalf("history: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Entries []HistoryEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(out.Entries))
	}
	newest, oldest := out.Entries[0], out.Entries[1]
	if newest.Size != int64(len("v2 longer")) || oldest.Size != int64(len("v1")) {
		t.Fatalf("order wrong: %+v", out.Entries)
	}
	if newest.User != "alice@x.io" || newest.UserName != "Alice" {
		t.Fatalf("user = %+v", newest)
	}
	// device joined from the registry: name, OS, server-observed IP
	if newest.Device.Name != "alice-laptop" || newest.Device.OS != "darwin/arm64" || newest.Device.IP != "203.0.113.7" {
		t.Fatalf("device = %+v", newest.Device)
	}
	if newest.Blob == "" || oldest.Blob == "" {
		t.Fatal("entries must link to their exact content")
	}

	// folder rollup: everything under notes/, deletes included
	rec = do(t, h, "GET", base+"history?prefix=notes/", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 4 {
		t.Fatalf("notes/ feed = %d entries, want 4", len(out.Entries))
	}
	if out.Entries[0].Kind != "delete" || out.Entries[0].Path != "notes/other.md" {
		t.Fatalf("newest notes/ entry = %+v, want the delete", out.Entries[0])
	}
	// a device the registry never saw falls back to the op's own info
	if out.Entries[0].Device.Name != "dev2" {
		t.Fatalf("unknown device fallback = %+v", out.Entries[0].Device)
	}

	// whole-project feed + n limit
	rec = do(t, h, "GET", base+"history?n=2", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 2 {
		t.Fatalf("n=2 gave %d entries", len(out.Entries))
	}

	// any old version is retrievable by content hash
	rec = do(t, h, "GET", base+"blob?sha="+oldest.Blob+"&name=plan.md", nil)
	if rec.Code != 200 || rec.Body.String() != "v1" {
		t.Fatalf("old version: %d %q", rec.Code, rec.Body)
	}
	rec = do(t, h, "GET", base+"blob?sha="+oldest.Blob+"&name=plan.md&download=1", nil)
	if cd := rec.Header().Get("Content-Disposition"); cd == "" {
		t.Fatal("download variant should attach")
	}
	if rec := do(t, h, "GET", base+"blob?sha=nothex", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad sha: %d, want 400", rec.Code)
	}
}

func TestDeviceRegistryObserve(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	r, err := OpenDeviceRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	r.Observe(DeviceInfo{ID: "d1", Name: "laptop", OS: "darwin/arm64", User: "a@x.io", IP: "198.51.100.4"})
	// identity survives a restart
	r2, err := OpenDeviceRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	d, ok := r2.Get("d1")
	if !ok || d.Name != "laptop" || d.IP != "198.51.100.4" || d.User != "a@x.io" {
		t.Fatalf("reloaded = %+v %v", d, ok)
	}
	if time.Since(d.LastSeen) > time.Minute {
		t.Fatalf("last_seen = %v", d.LastSeen)
	}
	// partial updates don't erase known fields
	r2.Observe(DeviceInfo{ID: "d1", IP: "198.51.100.9"})
	d, _ = r2.Get("d1")
	if d.Name != "laptop" || d.IP != "198.51.100.9" {
		t.Fatalf("merge = %+v", d)
	}
	// nil registry is a no-op
	var nilReg *DeviceRegistry
	nilReg.Observe(DeviceInfo{ID: "x"})
	if _, ok := nilReg.Get("x"); ok {
		t.Fatal("nil registry returned a device")
	}
}

// The store API records what it sees about devices (headers + observed IP +
// authenticated user).
func TestStoreObservesDevices(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	srv.Devices, _ = OpenDeviceRegistry(filepath.Join(t.TempDir(), "devices.json"))
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/api/p/"+p.ID+"/store/list?prefix=journal/", nil)
	req.Header.Set("X-Bdrive-Device", "dev-9")
	req.Header.Set("X-Bdrive-Device-Name", "build-box")
	req.Header.Set("X-Bdrive-Os", "linux/amd64")
	req.RemoteAddr = "192.0.2.55:41000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	d, ok := srv.Devices.Get("dev-9")
	if !ok || d.Name != "build-box" || d.OS != "linux/amd64" || d.IP != "192.0.2.55" {
		t.Fatalf("observed = %+v %v", d, ok)
	}
}
