package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
)

/* The poll that ran beside a healthy push channel.

   A hub-connected daemon holds an SSE change stream (remote.httpBackend.Watch)
   that announces every peer write. It ALSO re-listed every peer journal and
   re-asked its scope on a fixed timer, because doRemote gated on elapsed time
   alone and never consulted the stream. The daemon's own comment said so:
   "an accelerator only: every guarantee still rests on the tick."

   In production that cost 17,280 requests a day for one idle device on one
   project — 9.4% of the entire hub's daily traffic, to learn nothing, while a
   working stream sat open next to it saying the same nothing for free.

   These tests pin both halves of the contract: the poll stands down while the
   stream is healthy, and it comes straight back when the stream dies. The
   second half is the one that matters for safety — the stream is an
   optimisation, and every guarantee still has to survive losing it. */

func TestWatchSuppressesThePoll(t *testing.T) {
	hub := newFakeHub(t)
	m := watchMount(t, hub)

	done := watchRun(t, m)
	defer watchStop(t, m, done)

	// Long enough to be unambiguous: at remoteInterval=300ms an ungated poll
	// makes ~10 listings in here, and the watched cadence (30x) is 9s, so a
	// daemon that stood down makes the one it opens the stream with.
	time.Sleep(3 * time.Second)

	hub.mu.Lock()
	lists, scopes, events := hub.lists, hub.scopes, hub.events
	hub.mu.Unlock()

	if events == 0 {
		t.Fatalf("the daemon never opened the change stream, so this test proves nothing "+
			"about suppressing a poll beside one (lists=%d scopes=%d)", lists, scopes)
	}
	// Three, not one: the first cycle runs before a backend exists to open the
	// stream on, and a push of the seeded file costs another.
	if lists > 3 {
		t.Errorf("store/list ran %d times in 3s with a live change stream open; "+
			"the poll is not standing down (scope=%d, events=%d)", lists, scopes, events)
	}
	if scopes > 3 {
		t.Errorf("scope ran %d times in 3s with a live change stream open (lists=%d)", scopes, lists)
	}
}

func TestPollResumesWhenTheStreamDies(t *testing.T) {
	hub := newFakeHub(t)
	m := watchMount(t, hub)

	done := watchRun(t, m)
	defer watchStop(t, m, done)

	// Let it settle into the watched cadence first, or this measures the
	// startup burst rather than the recovery.
	time.Sleep(1500 * time.Millisecond)
	hub.mu.Lock()
	before := hub.lists
	hub.mu.Unlock()

	hub.killStreams()

	// remoteInterval is 300ms, so an unwatched daemon makes several listings
	// in here. If the daemon stayed on the 9s watched cadence after losing
	// its stream, it would make none — which is the dangerous direction.
	time.Sleep(2 * time.Second)

	hub.mu.Lock()
	after := hub.lists
	hub.mu.Unlock()

	if after-before < 2 {
		t.Errorf("store/list ran %d times in the 2s after the change stream was killed; "+
			"the daemon did not fall back to polling and is now blind to peer writes",
			after-before)
	}
}

// --- fake hub ---------------------------------------------------------------

// fakeHub answers the handful of routes a syncing daemon calls, counts them,
// and can sever every open change stream on demand.
type fakeHub struct {
	*httptest.Server

	mu                    sync.Mutex
	lists, scopes, events int
	streams               []chan struct{}
	dead                  bool
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	h := &fakeHub{}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/p/{project}/scope", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.scopes++
		h.mu.Unlock()
		writeJSON(w, map[string]any{"read_only": []string{}, "deny": []string{}, "tag": "t1"})
	})

	mux.HandleFunc("/api/p/{project}/store/list", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.lists++
		h.mu.Unlock()
		writeJSON(w, map[string]any{"objects": []any{}})
	})

	// Pushes: accept and forget. What this test measures is the polling, and a
	// push only happens when there is something to push.
	mux.HandleFunc("/api/p/{project}/store/object", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/p/{project}/store/sign", func(w http.ResponseWriter, r *http.Request) {
		// An empty plan means "no presigned URL, relay the bytes through me",
		// which keeps this fixture to one upload path. A non-200 here would
		// fail the push, and a failed push takes the cycle Offline — which
		// closes the change stream and would quietly turn this into a test of
		// the reconnect path instead of the poll.
		writeJSON(w, map[string]any{})
	})
	mux.HandleFunc("/api/p/{project}/store/exists", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"exists": false})
	})
	mux.HandleFunc("/api/p/{project}/reads", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// The change stream. Held open, silent apart from keepalive comments —
	// exactly the shape of a hub with nothing to report, which is the case the
	// poll beside it was wasting requests on.
	mux.HandleFunc("/api/p/{project}/events", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		if h.dead {
			h.mu.Unlock()
			http.Error(w, "no stream", http.StatusServiceUnavailable)
			return
		}
		h.events++
		kill := make(chan struct{})
		h.streams = append(h.streams, kill)
		h.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		fmt.Fprint(w, ": keepalive\n\n")
		rc.Flush()
		for {
			select {
			case <-kill:
				return
			case <-r.Context().Done():
				return
			case <-time.After(100 * time.Millisecond):
				if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
					return
				}
				if rc.Flush() != nil {
					return
				}
			}
		}
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })

	h.Server = httptest.NewServer(mux)
	t.Cleanup(func() {
		h.killStreams()
		h.Close()
	})
	return h
}

// killStreams severs every open change stream and refuses new ones, which is
// what a hub going away looks like from the daemon's side.
func (h *fakeHub) killStreams() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dead = true
	for _, c := range h.streams {
		close(c)
	}
	h.streams = nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// --- fixture ----------------------------------------------------------------

type watchFixture struct {
	folder string
	volDir string
}

func watchMount(t *testing.T, hub *fakeHub) watchFixture {
	t.Helper()
	t.Setenv("BDRIVE_HOME", t.TempDir())
	t.Setenv("BDRIVE_TOKEN", "test-token")
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "notes.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	remote := strings.TrimSuffix(hub.URL, "/") + "/p/p-test"
	p, err := config.SaveProject(folder, config.Project{Remote: remote})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(folder); err != nil {
		t.Fatal(err)
	}
	vdir, err := config.VolumeDir(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return watchFixture{folder: folder, volDir: vdir}
}

func watchRun(t *testing.T, m watchFixture) chan error {
	t.Helper()
	done := make(chan error, 1)
	// remoteInterval 300ms keeps an ungated poll loud (~10 listings in 3s)
	// while the watched cadence derived from it stays well outside the window.
	go func() { done <- Run(m.folder, 100*time.Millisecond, 300*time.Millisecond) }()
	return done
}

func watchStop(t *testing.T, m watchFixture, done chan error) {
	t.Helper()
	// The documented shutdown: the config vanishing makes the daemon exit
	// cleanly without propagating deletes.
	os.RemoveAll(filepath.Join(m.folder, ".bdrive"))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Log("daemon did not exit within 5s")
	}
}
