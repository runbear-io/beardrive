package syncer

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/webapp"
)

// newHub spins up a bdrive web hub over a fresh storage root and returns the
// test server plus one project.
func newHub(t *testing.T, storage remote.Backend, upload bool) (*httptest.Server, webapp.Project) {
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
		Upload: webapp.UploadConfig{Enabled: upload},
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, p
}

// A device syncing through a bdrive web server (https:// remote) must
// converge with a device talking to the object store directly: the server is
// just a broker, not a different sync model.
func TestSyncThroughWebServer(t *testing.T) {
	storage := sharedRemote(t) // the object store only the server knows about
	ts, p := newHub(t, storage, true)

	viaServer, err := remote.Open(context.Background(), ts.URL+"/p/"+p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer viaServer.Close()

	a := newDevice(t, "deva", viaServer)                      // storage-blind client
	b := newDevice(t, "devb", remote.Prefixed(storage, p.ID)) // direct-to-storage device

	// client → server → storage → direct device
	write(t, a.Folder, "notes/from-client.md", "hello via server")
	cycle(t, a)
	res := cycle(t, b)
	if res.PulledOps != 1 || read(t, b.Folder, "notes/from-client.md") != "hello via server" {
		t.Fatalf("b did not receive client's file: %+v", res)
	}

	// direct device → storage → server → client
	time.Sleep(10 * time.Millisecond)
	write(t, b.Folder, "notes/from-direct.md", "hello back")
	write(t, b.Folder, "notes/from-client.md", "edited directly")
	cycle(t, b)
	cycle(t, a)
	if read(t, a.Folder, "notes/from-direct.md") != "hello back" {
		t.Fatal("client did not receive direct device's file")
	}
	if read(t, a.Folder, "notes/from-client.md") != "edited directly" {
		t.Fatal("client did not receive the edit")
	}

	// deletes propagate through the server too
	os.Remove(filepath.Join(a.Folder, "notes", "from-direct.md"))
	cycle(t, a)
	cycle(t, b)
	if _, err := os.Stat(filepath.Join(b.Folder, "notes", "from-direct.md")); !os.IsNotExist(err) {
		t.Fatal("delete via server did not propagate")
	}
}

// With uploads disabled on the server, a client can still pull (read-only
// follower) — its pushes degrade to offline instead of failing the cycle.
func TestReadOnlyServerClientStillPulls(t *testing.T) {
	storage := sharedRemote(t)
	ts, p := newHub(t, storage, false) // read-only hub

	viaServer, err := remote.Open(context.Background(), ts.URL+"/p/"+p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer viaServer.Close()

	b := newDevice(t, "devb", remote.Prefixed(storage, p.ID))
	write(t, b.Folder, "shared.md", "server-side truth")
	cycle(t, b)

	a := newDevice(t, "deva", viaServer)
	write(t, a.Folder, "local-only.md", "cannot push this")
	res, err := a.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Offline {
		t.Fatalf("push against read-only server should degrade to offline: %+v", res)
	}
	if read(t, a.Folder, "shared.md") != "server-side truth" {
		t.Fatal("client should still pull from a read-only server")
	}
}
