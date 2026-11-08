package daemon

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/store"
)

// The turn hook trusts the files on disk only while the daemon's last pass
// against the hub is recent, so a completed pass must record itself.
func TestWorkerRecordsEachPassAgainstTheHub(t *testing.T) {
	hub := newFakeHub(t)
	m := watchMountWithRemote(t, strings.TrimSuffix(hub.URL, "/")+"/p/p-test")
	w := startWatchSafetyWorker(t, m, time.Hour, nil, nil)
	w.wait("a recorded pass against the hub", func() bool {
		at, _ := store.PulledAt(m.volDir)
		return !at.IsZero()
	})
	at, by := store.PulledAt(m.volDir)
	if age := time.Since(at); age > time.Minute {
		t.Errorf("recorded pass is %s old", age)
	}
	if by != os.Getpid() {
		t.Errorf("pass recorded by pid %d, want the daemon's own %d", by, os.Getpid())
	}
}

// A daemon that cannot reach the hub keeps its lock but must not claim a pass.
func TestWorkerOfflineRecordsNoPass(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	down.Close()
	m := watchMountWithRemote(t, down.URL+"/p/p-test")
	w := startWatchSafetyWorker(t, m, 50*time.Millisecond, nil, nil)
	w.wait("a few offline cycles", func() bool { return w.cycles.Load() >= 3 })
	if at, _ := store.PulledAt(m.volDir); !at.IsZero() {
		t.Errorf("an offline daemon recorded a pass at %s", at)
	}
}
