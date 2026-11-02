package daemon

import (
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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/store"
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

func TestLocalFolderWatcherRejectsMissingRoot(t *testing.T) {
	_, err := startLocalFolderWatcher(context.Background(), []string{filepath.Join(t.TempDir(), "missing")})
	if err == nil {
		t.Fatal("watcher setup succeeded for a missing root")
	}
}

func TestLocalFolderWatcherWakesAndAddsDirectories(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lw, err := startLocalFolderWatcher(ctx, []string{root})
	if err != nil {
		t.Fatalf("filesystem watcher unavailable: %v", err)
	}
	defer lw.close()
	if err := os.WriteFile(filepath.Join(root, "first.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lw.wake:
	case <-time.After(time.Second):
		t.Fatal("watcher did not report a file change")
	}
	sub := filepath.Join(root, "nested")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lw.wake:
	case <-time.After(time.Second):
		t.Fatal("watcher did not report a new directory")
	}
	if err := os.WriteFile(filepath.Join(sub, "second.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lw.wake:
	case <-time.After(time.Second):
		t.Fatal("watcher did not add the new directory")
	}
	lw.close()
	lw.close()
}

func TestLocalFolderWatcherAddsOnlyCreatedDirectorySubtree(t *testing.T) {
	root := t.TempDir()
	unrelated := filepath.Join(root, "unrelated")
	if err := os.Mkdir(unrelated, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lw, err := startLocalFolderWatcher(ctx, []string{root})
	if err != nil {
		t.Fatalf("filesystem watcher unavailable: %v", err)
	}
	defer lw.close()

	var mu sync.Mutex
	var walked []string
	localWatchWalkHook = func(path string) {
		mu.Lock()
		walked = append(walked, path)
		mu.Unlock()
	}
	defer func() { localWatchWalkHook = nil }()

	created := filepath.Join(root, "created")
	if err := os.Mkdir(created, 0o755); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lw.wake:
	case <-time.After(time.Second):
		t.Fatal("watcher did not report a new directory")
	}
	mu.Lock()
	got := append([]string(nil), walked...)
	mu.Unlock()
	if len(got) != 1 || got[0] != created {
		t.Fatalf("incremental watcher walks = %v, want only %s", got, created)
	}
	if err := os.WriteFile(filepath.Join(created, "file.txt"), []byte("contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lw.wake:
	case <-time.After(time.Second):
		t.Fatal("watcher did not add the created directory")
	}
}

func TestLocalFolderWatcherReaddsRecreatedDirectoryTree(t *testing.T) {
	root := t.TempDir()
	recreated := filepath.Join(root, "nested")
	deep := filepath.Join(recreated, "deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lw, err := startLocalFolderWatcher(ctx, []string{root})
	if err != nil {
		t.Fatalf("filesystem watcher unavailable: %v", err)
	}
	defer lw.close()

	waitWake := func(label string) {
		t.Helper()
		select {
		case <-lw.wake:
		case <-time.After(time.Second):
			t.Fatalf("watcher did not report %s", label)
		}
	}

	if err := os.RemoveAll(recreated); err != nil {
		t.Fatal(err)
	}
	waitWake("directory removal")

	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	waitWake("recreated directory tree")
	select {
	case <-lw.wake:
	default:
	}

	if err := os.WriteFile(filepath.Join(deep, "file.txt"), []byte("contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitWake("write in recreated descendant")
}

func TestLocalFolderWatcherBatchesPerMount(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lw, err := startLocalFolderWatcher(ctx, []string{rootA, rootB})
	if err != nil {
		t.Fatalf("filesystem watcher unavailable: %v", err)
	}
	defer lw.close()

	pathA := filepath.Join(rootA, "a.txt")
	pathB := filepath.Join(rootB, "b.txt")
	if err := os.WriteFile(pathA, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathA, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathB, []byte("three"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lw.wake:
	case <-time.After(time.Second):
		t.Fatal("watcher did not wake")
	}
	time.Sleep(25 * time.Millisecond)
	a := lw.take(rootA)
	b := lw.take(rootB)
	if len(a.events) != 1 || a.events[0].path != pathA {
		t.Fatalf("mount A batch = %+v, want one coalesced event for %s", a.events, pathA)
	}
	if len(b.events) != 1 || b.events[0].path != pathB {
		t.Fatalf("mount B batch = %+v, want one event for %s", b.events, pathB)
	}
	if len(lw.take(rootA).events) != 0 {
		t.Fatal("mount A batch was consumed by a second take")
	}
}

func TestWatcherRebuildsOnChildENOENT(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lw, err := startLocalFolderWatcher(ctx, []string{root})
	if err != nil {
		t.Fatalf("filesystem watcher unavailable: %v", err)
	}

	failOnce := true
	localWatchAddHook = func(path string) error {
		if failOnce && filepath.Base(path) == "race" {
			failOnce = false
			return os.ErrNotExist
		}
		return nil
	}
	defer func() { localWatchAddHook = nil }()
	defer lw.close()
	raceDir := filepath.Join(root, "race")
	if err := os.Mkdir(raceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	select {
	case watchErr := <-lw.err:
		t.Fatalf("child ENOENT made watcher unhealthy: %v", watchErr)
	default:
	}
	if batch := lw.take(root); !batch.full {
		t.Fatal("child ENOENT rebuild did not force a full fallback batch")
	}
	if err := os.WriteFile(filepath.Join(raceDir, "after.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lw.wake:
	case watchErr := <-lw.err:
		t.Fatalf("rebuilt watcher failed: %v", watchErr)
	case <-time.After(time.Second):
		t.Fatal("rebuilt watcher did not report later child write")
	}
}

func TestWatcherKeepsPriorWatcherAfterRepeatedChildENOENT(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lw, err := startLocalFolderWatcher(ctx, []string{root})
	if err != nil {
		t.Fatalf("filesystem watcher unavailable: %v", err)
	}
	defer lw.close()

	failures := 2
	localWatchAddHook = func(path string) error {
		if failures > 0 && filepath.Base(path) == "race" {
			failures--
			return os.ErrNotExist
		}
		return nil
	}
	defer func() { localWatchAddHook = nil }()

	raceDir := filepath.Join(root, "race")
	if err := os.Mkdir(raceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lw.wake:
	case watchErr := <-lw.err:
		t.Fatalf("repeated child ENOENT made watcher unhealthy: %v", watchErr)
	case <-time.After(time.Second):
		t.Fatal("watcher did not recover from repeated child ENOENT")
	}
	if batch := lw.take(root); !batch.full {
		t.Fatal("repeated child ENOENT did not force a full fallback batch")
	}

	later := filepath.Join(root, "later.txt")
	if err := os.WriteFile(later, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lw.wake:
	case watchErr := <-lw.err:
		t.Fatalf("prior watcher failed after repeated child ENOENT: %v", watchErr)
	case <-time.After(time.Second):
		t.Fatal("prior watcher did not report a later root write")
	}
}

func TestEventsDuringScanAreFenced(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lw, err := startLocalFolderWatcher(ctx, []string{root})
	if err != nil {
		t.Fatalf("filesystem watcher unavailable: %v", err)
	}
	defer lw.close()

	echo := filepath.Join(root, "echo.txt")
	if err := os.WriteFile(echo, []byte("materialized"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lw.wake:
	case <-time.After(time.Second):
		t.Fatal("echo write did not wake watcher")
	}
	_ = lw.take(root)

	if err := os.WriteFile(echo, []byte("materialized again"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lw.wake:
	case <-time.After(time.Second):
		t.Fatal("post-cycle echo did not wake watcher")
	}
	fence := lw.fence()
	lw.suppress(root, fence, func(ev localWatchEvent) bool {
		return ev.path != echo
	})

	late := filepath.Join(root, "late.txt")
	if err := os.WriteFile(late, []byte("concurrent"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	foundLate := false
	for !foundLate {
		select {
		case _, ok := <-lw.wake:
			if !ok {
				t.Fatal("watcher stopped before post-fence event was observed")
			}
			batch := lw.take(root)
			for _, ev := range batch.events {
				if ev.path == echo {
					t.Fatal("fenced echo remained queued")
				}
				if ev.path == late {
					foundLate = true
				}
			}
		case <-deadline.C:
			t.Fatalf("post-fence event missing from batches before deadline")
		}
	}
}

func TestMaterializationEchoSuppressionKeepsChangedEvents(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lw, err := startLocalFolderWatcher(ctx, []string{root})
	if err != nil {
		t.Fatalf("filesystem watcher unavailable: %v", err)
	}
	defer lw.close()

	echo := filepath.Join(root, "echo.txt")
	changed := filepath.Join(root, "changed.txt")
	if err := os.WriteFile(echo, []byte("materialized"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(changed, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lw.wake:
	case <-time.After(time.Second):
		t.Fatal("initial writes did not wake watcher")
	}
	_ = lw.take(root)

	st, err := store.Open(filepath.Join(t.TempDir(), "volume"))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := func(path string) store.CachedFile {
		t.Helper()
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		return store.CachedFile{Size: info.Size(), MTimeNS: info.ModTime().UnixNano()}
	}
	cache := map[string]store.CachedFile{
		"echo.txt":    fingerprint(echo),
		"changed.txt": fingerprint(changed),
	}

	if err := os.WriteFile(echo, []byte("materialized"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lw.wake:
	case <-time.After(time.Second):
		t.Fatal("materialization echo did not wake watcher")
	}
	cache["echo.txt"] = fingerprint(echo)
	if err := os.WriteFile(changed, []byte("concurrent"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lw.wake:
	case <-time.After(time.Second):
		t.Fatal("concurrent change did not wake watcher")
	}
	if err := st.SaveCache("mount", cache); err != nil {
		t.Fatal(err)
	}
	suppressMaterializationEchoes(lw, root, root, "mount", st, lw.fence())

	batch := lw.take(root)
	if len(batch.events) != 1 || batch.events[0].path != changed {
		t.Fatalf("suppressed batch = %+v, want only concurrent change %s", batch.events, changed)
	}
}

func TestWorkerSuppressesLateMaterializationEchoes(t *testing.T) {
	for _, concurrentEdit := range []bool{false, true} {
		t.Run(fmt.Sprintf("concurrent_edit=%v", concurrentEdit), func(t *testing.T) {
			hub := newFakeHub(t)
			hub.killStreams()
			m := watchMount(t, hub)
			proj, ok, err := config.LoadProject(m.folder)
			if err != nil || !ok {
				t.Fatalf("load project: ok=%v err=%v", ok, err)
			}
			st, err := store.Open(m.volDir)
			if err != nil {
				t.Fatal(err)
			}
			dev, err := config.LoadDevice()
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			peerDir := filepath.Join(m.folder, "peer")
			// Hold recursive watch installation, not materialization: the peer
			// directory's Create must arrive in pending AFTER the cycle's
			// original post-materialization fence. This happens naturally when
			// Darwin is still adding watches for a newly populated directory.
			var enteredOnce sync.Once
			localWatchWalkHook = func(path string) {
				if path == peerDir {
					enteredOnce.Do(func() { close(entered) })
					select {
					case <-release:
					case <-ctx.Done():
					}
				}
			}
			var cycles atomic.Int64
			workerCycleHook = func(volDir string) {
				if volDir == m.volDir {
					cycles.Add(1)
				}
			}
			wake := make(chan struct{}, 1)
			done := make(chan error, 1)
			const scanInterval = 100 * time.Millisecond
			go func() {
				done <- runVolumeWorker(ctx, m.volDir, proj.Remote, scanInterval, time.Hour, wake)
			}()
			t.Cleanup(func() {
				unblock()
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("worker shutdown: %v", err)
					}
					workerCycleHook, localWatchWalkHook = nil, nil
				case <-time.After(5 * time.Second):
					t.Error("worker did not stop; leaving hooks installed to avoid racing it")
				}
			})
			waitFor := func(label string, ready func() bool) {
				t.Helper()
				deadline := time.Now().Add(10 * time.Second)
				for !ready() {
					select {
					case err := <-done:
						// Keep the result available to the cleanup join.
						done <- err
						t.Fatalf("worker exited before %s: %v", label, err)
					case <-time.After(10 * time.Millisecond):
					}
					if time.Now().After(deadline) {
						t.Fatalf("timed out waiting for %s; cycles=%d", label, cycles.Load())
					}
				}
			}
			logContains := func(text string) bool {
				data, err := os.ReadFile(LogPath(m.volDir))
				return err == nil && strings.Contains(string(data), text)
			}
			waitFor("initial push", func() bool { return logContains("local+1 pulled+0") })
			time.Sleep(3 * scanInterval)
			before := cycles.Load()

			const peerFiles = 20
			hub.mu.Lock()
			var lines []byte
			for i := range peerFiles {
				content := []byte(fmt.Sprintf("peer %d\n", i))
				sum := sha256.Sum256(content)
				blob := hex.EncodeToString(sum[:])
				line, err := json.Marshal(journal.Op{
					Device: "peer", Seq: int64(i + 1), Lamport: int64(i + 100),
					Time: time.Now().UTC(), Kind: journal.KindPut,
					Path: fmt.Sprintf("peer/nested/%03d.md", i), Blob: blob,
					Size: int64(len(content)), Mode: 0o644,
				})
				if err != nil {
					hub.mu.Unlock()
					t.Fatal(err)
				}
				hub.objects["blobs/"+blob] = content
				lines = append(append(lines, line...), '\n')
			}
			hub.objects["journal/peer.jsonl"] = lines
			hub.mu.Unlock()
			wake <- struct{}{}
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("peer directory never reached recursive watch installation")
			}
			// The activity log is written after Cycle, cache persistence, and
			// the old suppression call, so releasing cannot race that fence.
			waitFor("completed peer materialization", func() bool { return logContains("pulled+20") })
			if got := cycles.Load(); got != before+1 {
				t.Fatalf("peer pull ran %d cycles before delayed events, want 1", got-before)
			}
			for i := range peerFiles {
				data, err := os.ReadFile(filepath.Join(peerDir, "nested", fmt.Sprintf("%03d.md", i)))
				if err != nil || string(data) != fmt.Sprintf("peer %d\n", i) {
					t.Fatalf("materialized peer file %d: bytes=%q err=%v", i, data, err)
				}
			}

			wantCycles := before + 1
			if concurrentEdit {
				for _, path := range []string{"notes.md", "peer/user.md"} {
					if err := os.WriteFile(filepath.Join(m.folder, filepath.FromSlash(path)), []byte("concurrent user edit\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				// One local discovery and its immediate remote push are needed;
				// the peer directory echo must not add another discovery.
				wantCycles += 2
			}
			unblock()
			if concurrentEdit {
				waitFor("concurrent edits journaled and pushed", func() bool {
					ops, err := st.DeviceOps(dev.ID)
					if err != nil {
						return false
					}
					syncState, err := st.LoadSync()
					if err != nil || syncState.PushedOps != int64(len(ops)) {
						return false
					}
					found := make(map[string]bool)
					for _, op := range ops {
						if op.Path == "notes.md" || op.Path == "peer/user.md" {
							data, err := os.ReadFile(st.BlobPath(op.Blob))
							found[op.Path] = op.Kind == journal.KindPut && err == nil && string(data) == "concurrent user edit\n"
						}
					}
					return found["notes.md"] && found["peer/user.md"]
				})
			}
			time.Sleep(5 * scanInterval)
			if got := cycles.Load(); got != wantCycles {
				t.Fatalf("cycles after peer materialization = %d, want %d (no echo-only discovery)", got-before, wantCycles-before)
			}
			data, err := os.ReadFile(LogPath(m.volDir))
			if err != nil {
				t.Fatal(err)
			}
			for _, failure := range []string{"watcher stopped", "watcher unavailable", "cycle error:", "cycle panic", "offline, will retry:"} {
				if strings.Contains(string(data), failure) {
					t.Fatalf("worker lost healthy-watcher precondition: %s", data)
				}
			}
		})
	}
}

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

	// objects are peer journals and blobs the hub serves; failBlobs refuses
	// that many blob GETs before serving any.
	objects   map[string][]byte
	failBlobs int
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	h := &fakeHub{objects: map[string][]byte{}}
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
		prefix := r.URL.Query().Get("prefix")
		objs := []map[string]any{}
		for k, v := range h.objects {
			if strings.HasPrefix(k, prefix) {
				objs = append(objs, map[string]any{"Key": k, "Size": len(v)})
			}
		}
		h.mu.Unlock()
		writeJSON(w, map[string]any{"objects": objs})
	})

	// Pushes: accept and forget. What this test measures is the polling, and a
	// push only happens when there is something to push.
	mux.HandleFunc("/api/p/{project}/store/object", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			key := r.URL.Query().Get("key")
			h.mu.Lock()
			body, ok := h.objects[key]
			refuse := ok && strings.HasPrefix(key, "blobs/") && h.failBlobs > 0
			if refuse {
				h.failBlobs--
			}
			h.mu.Unlock()
			switch {
			case refuse:
				http.Error(w, "shedding load", http.StatusServiceUnavailable)
			case ok:
				w.Write(body)
			default:
				http.NotFound(w, r)
			}
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
