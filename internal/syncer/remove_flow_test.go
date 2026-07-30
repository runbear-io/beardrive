package syncer

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/remote"
)

// The whole point of BEA-35: a file an agent run created can be un-created
// from the hub's History view, and the removal reaches every device like any
// other change. The hub writes ONE delete op into its OWN journal — the
// device's log is untouched, so one-writer-per-journal survives.
func TestHubRemoveReachesDevices(t *testing.T) {
	storage := sharedRemote(t)
	ts, p := newHub(t, storage, true)

	viaServer, err := remote.Open(context.Background(), ts.URL+"/p/"+p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer viaServer.Close()
	a := newDevice(t, "deva", viaServer)
	b := newDevice(t, "devb", remote.Prefixed(storage, p.ID))

	write(t, a.Folder, "ideas.md", "an agent dumped this")
	cycle(t, a)
	cycle(t, b)
	if read(t, b.Folder, "ideas.md") != "an agent dumped this" {
		t.Fatal("b never received the file")
	}
	ownBefore := journalBytes(t, a, "deva")

	post(t, ts.URL+"/api/p/"+p.ID+"/remove", `{"path":"ideas.md"}`)

	res := cycle(t, a)
	if res.PulledOps != 1 {
		t.Fatalf("a pulled %d ops, want the hub's one delete", res.PulledOps)
	}
	if _, err := os.Stat(filepath.Join(a.Folder, "ideas.md")); !os.IsNotExist(err) {
		t.Fatal("the hub's removal did not unlink the file on a")
	}
	if !bytes.Equal(journalBytes(t, a, "deva"), ownBefore) {
		t.Fatal("the hub's removal rewrote the device's own journal")
	}
	// and onward to a device that talks to storage directly
	cycle(t, b)
	if _, err := os.Stat(filepath.Join(b.Folder, "ideas.md")); !os.IsNotExist(err) {
		t.Fatal("the removal did not reach the direct-to-storage device")
	}
}

func post(t *testing.T, url, body string) {
	t.Helper()
	res, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("POST %s: %d", url, res.StatusCode)
	}
}
