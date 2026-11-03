package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/store"
)

// These workers use the real scanner, journal, watcher, and HTTP backend. The
// fake hub forgets uploads, so propagation is observed through the persisted
// push cursor as well as the journal's actual content-addressed bytes.
type watchSafetyWorker struct {
	t      *testing.T
	m      watchFixture
	st     *store.Store
	device string
	done   chan error
	cycles atomic.Int64
}

func startWatchSafetyWorker(t *testing.T, m watchFixture, remoteInterval time.Duration, now func() time.Time, beforeCycle func(), scanIntervals ...time.Duration) *watchSafetyWorker {
	t.Helper()
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
	w := &watchSafetyWorker{t: t, m: m, st: st, device: dev.ID, done: make(chan error, 1)}
	oldNow := localWatchNow
	if now != nil {
		localWatchNow = now
	}
	workerCycleHook = func(volDir string) {
		if volDir == m.volDir {
			w.cycles.Add(1)
			if beforeCycle != nil {
				beforeCycle()
			}
		}
	}
	scanInterval := 20 * time.Millisecond
	if len(scanIntervals) > 0 {
		scanInterval = scanIntervals[0]
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		w.done <- runVolumeWorker(ctx, m.volDir, proj.Remote, scanInterval, remoteInterval, nil)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-w.done:
			if err != nil {
				t.Errorf("worker shutdown: %v", err)
			}
			workerCycleHook = nil
			workerRemoteWakeHook = nil
			localWatchNow = oldNow
			setLocalWatchHooks(nil, nil, nil)
		case <-time.After(5 * time.Second):
			t.Error("worker did not stop; retaining global hooks rather than racing it")
		}
	})
	return w
}

func (w *watchSafetyWorker) wait(label string, ready func() bool) {
	w.t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-w.done:
			w.done <- err // Leave the result available for the cleanup join.
			w.t.Fatalf("worker exited before %s: %v", label, err)
		default:
		}
		if ready() {
			return
		}
		select {
		case <-deadline.C:
			w.t.Fatalf("timed out waiting for %s; cycles=%d; log=%s", label, w.cycles.Load(), w.log())
		case <-tick.C:
		}
	}
}

func (w *watchSafetyWorker) log() string {
	data, _ := os.ReadFile(LogPath(w.m.volDir))
	return string(data)
}

func (w *watchSafetyWorker) pushed(path, kind, contents string) bool {
	ops, err := w.st.DeviceOps(w.device)
	if err != nil {
		return false
	}
	state, err := w.st.LoadSync()
	if err != nil || state.PushedOps != int64(len(ops)) {
		return false
	}
	for i := len(ops) - 1; i >= 0; i-- {
		op := ops[i]
		if op.Path != path {
			continue
		}
		if op.Kind != kind {
			return false
		}
		if kind == journal.KindDelete {
			return true
		}
		data, err := os.ReadFile(w.st.BlobPath(op.Blob))
		return err == nil && string(data) == contents
	}
	return false
}

type remotePullGate struct {
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
	releaseOnce sync.Once
}

func (g *remotePullGate) signalStarted() {
	g.startedOnce.Do(func() { close(g.started) })
}

func (g *remotePullGate) unblock() {
	g.releaseOnce.Do(func() { close(g.release) })
}

type remoteWakeProxy struct {
	*httptest.Server
	wake      chan struct{}
	connected chan struct{}
	once      sync.Once
	pullMu    sync.Mutex
	pullGate  *remotePullGate
}

func (p *remoteWakeProxy) blockPull() *remotePullGate {
	gate := &remotePullGate{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	p.pullMu.Lock()
	p.pullGate = gate
	p.pullMu.Unlock()
	return gate
}

func (p *remoteWakeProxy) waitForPull(r *http.Request) bool {
	p.pullMu.Lock()
	gate := p.pullGate
	p.pullMu.Unlock()
	if gate == nil {
		return true
	}
	gate.signalStarted()
	select {
	case <-gate.release:
		return true
	case <-r.Context().Done():
		return false
	}
}

func newRemoteWakeProxy(t *testing.T, hub *fakeHub) *remoteWakeProxy {
	t.Helper()
	target, err := url.Parse(hub.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := &remoteWakeProxy{
		wake:      make(chan struct{}, 1),
		connected: make(chan struct{}),
	}
	forward := httputil.NewSingleHostReverseProxy(target)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/p/p-test/store/list", func(w http.ResponseWriter, r *http.Request) {
		if !proxy.waitForPull(r) {
			return
		}
		forward.ServeHTTP(w, r)
	})
	mux.HandleFunc("/api/p/p-test/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		fmt.Fprint(w, ": keepalive\n\n")
		flusher.Flush()
		proxy.once.Do(func() { close(proxy.connected) })
		select {
		case <-r.Context().Done():
			return
		case <-proxy.wake:
			fmt.Fprint(w, "data: remote wake\n\n")
			flusher.Flush()
		}
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(100 * time.Millisecond):
				fmt.Fprint(w, ": keepalive\n\n")
				flusher.Flush()
			}
		}
	})
	mux.Handle("/", forward)
	proxy.Server = httptest.NewServer(mux)
	t.Cleanup(proxy.Close)
	return proxy
}

func watchMountWithRemote(t *testing.T, remote string) watchFixture {
	t.Helper()
	t.Setenv("BDRIVE_HOME", t.TempDir())
	t.Setenv("BDRIVE_TOKEN", "test-token")
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "notes.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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

func seedRemoteNotes(t *testing.T, hub *fakeHub) string {
	t.Helper()
	const contents = "remote version wins after wake\n"
	sum := sha256.Sum256([]byte(contents))
	blob := hex.EncodeToString(sum[:])
	line, err := json.Marshal(journal.Op{
		Seq: 1, Lamport: 100, Time: time.Now().UTC(), Device: "peer",
		Kind: journal.KindPut, Path: "notes.md", Blob: blob,
		Size: int64(len(contents)), Mode: 0o644,
	})
	if err != nil {
		t.Fatal(err)
	}
	hub.mu.Lock()
	hub.objects["blobs/"+blob] = []byte(contents)
	hub.objects["journal/peer.jsonl"] = append(line, '\n')
	hub.mu.Unlock()
	return contents
}

func TestWorkerRemoteOnlyWakeDoesNotDiscoverUnhintedLocalFile(t *testing.T) {
	hub := newFakeHub(t)
	proxy := newRemoteWakeProxy(t, hub)
	m := watchMountWithRemote(t, strings.TrimSuffix(proxy.URL, "/")+"/p/p-test")
	held := filepath.Join(m.folder, "held")
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	setLocalWatchHooks(nil, nil, func(path string) {
		if path == held {
			enterOnce.Do(func() { close(entered) })
			<-release
		}
	})
	// Keep the safety deadline a full minute away throughout this scenario:
	// only the SSE wake and, after release, the directory event may discover work.
	now := time.Now()
	w := startWatchSafetyWorker(t, m, time.Hour, func() time.Time { return now }, nil)
	t.Cleanup(unblock)
	w.wait("completed initial full scan", func() bool {
		return w.pushed("notes.md", journal.KindPut, "hello\n") &&
			strings.Contains(w.log(), "local+1 pulled+0")
	})
	w.requireHealthyWatcher()
	select {
	case <-proxy.connected:
	case <-time.After(5 * time.Second):
		t.Fatal("remote watcher did not connect")
	}
	beforeOps, err := w.st.DeviceOps(w.device)
	if err != nil {
		t.Fatal(err)
	}
	beforeSync, err := w.st.LoadSync()
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(held, 0o755); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("new directory never reached recursive watcher installation")
	}
	// The directory's Create has not been queued and no watch exists inside
	// it yet. This file therefore cannot be discovered by a bounded local hint.
	const localPath = "held/unhinted.txt"
	const localContents = "user file awaiting its local watcher hint\n"
	if err := os.WriteFile(filepath.Join(m.folder, localPath), []byte(localContents), 0o644); err != nil {
		t.Fatal(err)
	}
	remoteContents := seedRemoteNotes(t, hub)
	gate := proxy.blockPull()
	t.Cleanup(gate.unblock)
	proxy.wake <- struct{}{}
	select {
	case <-gate.started:
	case <-time.After(5 * time.Second):
		t.Fatal("remote-only wake did not reach the pull gate")
	}
	gate.unblock()
	w.wait("completed remote cycle with peer content materialized", func() bool {
		return worktreeFileEquals(m.folder, "notes.md", remoteContents) &&
			strings.Contains(w.log(), "pulled+1")
	})
	w.requireHealthyWatcher()

	peerOps, err := w.st.DeviceOps("peer")
	if err != nil {
		t.Fatal(err)
	}
	if len(peerOps) != 1 || peerOps[0].Path != "notes.md" || peerOps[0].Kind != journal.KindPut {
		t.Fatalf("persisted peer journal = %+v, want one notes.md put", peerOps)
	}
	peerBytes, err := os.ReadFile(w.st.BlobPath(peerOps[0].Blob))
	if err != nil {
		t.Fatal(err)
	}
	if string(peerBytes) != remoteContents {
		t.Fatalf("persisted peer blob = %q, want %q", peerBytes, remoteContents)
	}
	afterOps, err := w.st.DeviceOps(w.device)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range afterOps {
		if op.Path == localPath {
			t.Fatalf("remote-only cycle discovered unhinted local file: %+v", op)
		}
	}
	if len(afterOps) != len(beforeOps) {
		t.Fatalf("remote-only cycle changed local journal length: %d -> %d", len(beforeOps), len(afterOps))
	}
	afterSync, err := w.st.LoadSync()
	if err != nil {
		t.Fatal(err)
	}
	if afterSync.PushedOps != beforeSync.PushedOps {
		t.Fatalf("remote-only cycle advanced local push cursor: %d -> %d", beforeSync.PushedOps, afterSync.PushedOps)
	}
	if !worktreeFileEquals(m.folder, localPath, localContents) {
		t.Fatal("remote-only cycle changed the unhinted user's file")
	}

	unblock()
	w.wait("local file uploaded after its directory hint was admitted", func() bool {
		return w.pushed(localPath, journal.KindPut, localContents)
	})
	if !worktreeFileEquals(m.folder, localPath, localContents) {
		t.Fatal("local discovery changed the user's file")
	}
	w.requireHealthyWatcher()
}

func TestWorkerRemoteCycleCannotExtendSafetyDeadline(t *testing.T) {
	hub := newFakeHub(t)
	proxy := newRemoteWakeProxy(t, hub)
	m := watchMountWithRemote(t, strings.TrimSuffix(proxy.URL, "/")+"/p/p-test")
	var offset atomic.Int64
	clock := func() time.Time {
		return time.Now().Add(time.Duration(offset.Load()))
	}
	held := filepath.Join(m.folder, "held")
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	setLocalWatchHooks(nil, nil, func(path string) {
		if path != held {
			return
		}
		enterOnce.Do(func() { close(entered) })
		<-release
	})
	w := startWatchSafetyWorker(t, m, time.Hour, clock, nil)
	t.Cleanup(unblock)
	w.wait("completed initial full scan", func() bool {
		return w.pushed("notes.md", journal.KindPut, "hello\n") &&
			strings.Contains(w.log(), "local+1 pulled+0")
	})
	w.requireHealthyWatcher()
	select {
	case <-proxy.connected:
	case <-time.After(5 * time.Second):
		t.Fatal("remote watcher did not connect")
	}

	offset.Store(int64(10 * time.Second))
	remoteContents := seedRemoteNotes(t, hub)
	gate := proxy.blockPull()
	t.Cleanup(gate.unblock)
	proxy.wake <- struct{}{}
	select {
	case <-gate.started:
	case <-time.After(5 * time.Second):
		t.Fatal("remote-only wake did not reach the pull gate")
	}
	// Pull follows the bounded local scan. Block event admission only now,
	// so this file must be discovered by a later full scan, never this cycle.
	if err := os.Mkdir(held, 0o755); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("new directory never reached recursive watcher installation")
	}
	const localPath = "held/missed.txt"
	const localContents = "unobserved user edit during remote cycle\n"
	if err := os.WriteFile(filepath.Join(m.folder, localPath), []byte(localContents), 0o644); err != nil {
		t.Fatal(err)
	}

	ops, err := w.st.DeviceOps(w.device)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Path != "notes.md" {
		t.Fatalf("remote-only cycle changed the local journal: %+v", ops)
	}
	offset.Store(int64(localWatchSafetyInterval + 5*time.Second))
	gate.unblock()

	// Do not release the watcher or send another wake: the completed remote
	// cycle must schedule safety discovery against the original deadline.
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if worktreeFileEquals(m.folder, "notes.md", remoteContents) &&
			w.pushed(localPath, journal.KindPut, localContents) {
			break
		}
		select {
		case err := <-w.done:
			w.done <- err
			t.Fatalf("worker exited before safety full scan: %v", err)
		case <-deadline.C:
			t.Fatalf("bounded remote cycle postponed safety discovery past its deadline; peer materialized=%v; log=%s",
				worktreeFileEquals(m.folder, "notes.md", remoteContents), w.log())
		case <-tick.C:
		}
	}
	if !worktreeFileEquals(m.folder, localPath, localContents) {
		t.Fatal("safety full scan changed the user's disk bytes")
	}
	w.requireHealthyWatcher()
	unblock()
}

func TestWorkerRemoteWakePreservesPendingLocalEdit(t *testing.T) {
	hub := newFakeHub(t)
	proxy := newRemoteWakeProxy(t, hub)
	m := watchMountWithRemote(t, strings.TrimSuffix(proxy.URL, "/")+"/p/p-test")

	const localContents = "local edit racing remote wake\n"
	var wakeOnce sync.Once
	barrier := filepath.Join(m.folder, "remote-wake-barrier")
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	hookErrors := make(chan error, 1)
	setLocalWatchHooks(nil, nil, func(path string) {
		if path == barrier {
			enterOnce.Do(func() { close(entered) })
			<-release
		}
	})
	w := startWatchSafetyWorker(t, m, time.Hour, nil, nil)
	t.Cleanup(unblock)
	workerRemoteWakeHook = func(volDir string) {
		if volDir != m.volDir {
			return
		}
		wakeOnce.Do(func() {
			defer unblock()
			if err := os.WriteFile(filepath.Join(m.folder, "notes.md"), []byte(localContents), 0o644); err != nil {
				hookErrors <- err
				return
			}
			// fsnotify delivers these events in order. Reaching the next
			// directory's walk proves notes.md's Write was queued, while the
			// barrier directory's own event remains unqueued. Release it only
			// after that proof so the remote-woken cycle can start.
			if err := os.Mkdir(barrier, 0o755); err != nil {
				hookErrors <- err
				return
			}
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				hookErrors <- errors.New("local watcher did not admit notes.md before the remote cycle")
			}
		})
	}
	w.wait("completed initial full scan", func() bool {
		return w.pushed("notes.md", journal.KindPut, "hello\n")
	})
	w.requireHealthyWatcher()
	select {
	case <-proxy.connected:
	case <-time.After(5 * time.Second):
		t.Fatal("remote watcher did not connect")
	}
	remoteContents := seedRemoteNotes(t, hub)
	gate := proxy.blockPull()
	t.Cleanup(gate.unblock)
	proxy.wake <- struct{}{}

	w.wait("remote pull to reach its blocking gate", func() bool {
		select {
		case err := <-hookErrors:
			t.Fatal(err)
			return false
		case <-gate.started:
			return true
		default:
			return false
		}
	})
	w.wait("local edit journaled before releasing remote pull", func() bool {
		select {
		case err := <-hookErrors:
			t.Fatal(err)
		default:
		}
		ops, err := w.st.DeviceOps(w.device)
		if err != nil {
			return false
		}
		for _, op := range ops {
			if op.Kind != journal.KindPut || op.Path != "notes.md" {
				continue
			}
			data, err := os.ReadFile(w.st.BlobPath(op.Blob))
			if err == nil && string(data) == localContents {
				return true
			}
		}
		return false
	})
	gate.unblock()
	w.wait("remote content materialized after the pull", func() bool {
		return w.pushed("notes.md", journal.KindPut, localContents) &&
			worktreeFileEquals(m.folder, "notes.md", remoteContents)
	})
}

func TestWorkerRemoteTimerPreservesQueuedLocalChanges(t *testing.T) {
	hub := newFakeHub(t)
	proxy := newRemoteWakeProxy(t, hub)
	m := watchMountWithRemote(t, strings.TrimSuffix(proxy.URL, "/")+"/p/p-test")
	if err := os.WriteFile(filepath.Join(m.folder, "remove.md"), []byte("delete me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const firstContents = "first local operation\n"
	const localContents = "second local edit racing remote timer\n"
	barrier := filepath.Join(m.folder, "remote-timer-barrier")
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	setLocalWatchHooks(nil, nil, func(path string) {
		if path == barrier {
			enterOnce.Do(func() {
				close(entered)
				<-release
			})
		}
	})

	initStarted := make(chan struct{})
	var initOnce sync.Once
	workerInitHook = func(volDir string) {
		if volDir == m.volDir {
			initOnce.Do(func() { close(initStarted) })
		}
	}
	t.Cleanup(func() { workerInitHook = nil })

	var remoteWakes atomic.Int32
	workerRemoteWakeHook = func(volDir string) {
		if volDir == m.volDir {
			remoteWakes.Add(1)
		}
	}

	var phase atomic.Int32
	hookErrors := make(chan error, 1)
	w := startWatchSafetyWorker(t, m, time.Hour, nil, func() {
		if !phase.CompareAndSwap(1, 2) {
			return
		}
		if err := os.WriteFile(filepath.Join(m.folder, "notes.md"), []byte(localContents), 0o644); err != nil {
			hookErrors <- err
			return
		}
		if err := os.Remove(filepath.Join(m.folder, "remove.md")); err != nil {
			hookErrors <- err
			return
		}
		if err := os.Mkdir(barrier, 0o755); err != nil {
			hookErrors <- err
			return
		}
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			hookErrors <- errors.New("local watcher did not queue the second edit and delete after hint collection")
		}
	}, 2*time.Second)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	select {
	case <-initStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not initialize")
	}
	w.wait("completed initial full scan", func() bool {
		return w.pushed("notes.md", journal.KindPut, "hello\n") &&
			w.pushed("remove.md", journal.KindPut, "delete me\n")
	})
	select {
	case <-proxy.connected:
	case <-time.After(5 * time.Second):
		t.Fatal("remote watcher did not connect")
	}

	gate := proxy.blockPull()
	t.Cleanup(gate.unblock)
	remoteContents := seedRemoteNotes(t, hub)
	phase.Store(1)
	if err := os.WriteFile(filepath.Join(m.folder, "first.md"), []byte(firstContents), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-entered:
	case err := <-hookErrors:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("local watcher did not queue the second edit and delete after the first cycle's hint collection")
	}
	select {
	case <-gate.started:
	case err := <-hookErrors:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("remote timer did not reach its pull gate")
	}
	if got := remoteWakes.Load(); got != 0 {
		t.Fatalf("remote wake hook fired %d times; expected timer-only ordering", got)
	}
	w.wait("first local operation journaled before remote pull", func() bool {
		ops, err := w.st.DeviceOps(w.device)
		if err != nil {
			return false
		}
		for _, op := range ops {
			if op.Kind != journal.KindPut || op.Path != "first.md" {
				continue
			}
			data, err := os.ReadFile(w.st.BlobPath(op.Blob))
			return err == nil && string(data) == firstContents
		}
		return false
	})
	w.wait("queued local changes journaled before remote pull", func() bool {
		ops, err := w.st.DeviceOps(w.device)
		if err != nil {
			return false
		}
		var edit, deleted bool
		for _, op := range ops {
			switch {
			case op.Kind == journal.KindPut && op.Path == "notes.md":
				data, err := os.ReadFile(w.st.BlobPath(op.Blob))
				edit = edit || (err == nil && string(data) == localContents)
			case op.Kind == journal.KindDelete && op.Path == "remove.md":
				deleted = true
			}
		}
		return edit && deleted
	})
	gate.unblock()
	w.wait("remote content materialized after the pull", func() bool {
		_, err := os.Stat(filepath.Join(m.folder, "remove.md"))
		return w.pushed("notes.md", journal.KindPut, localContents) &&
			w.pushed("remove.md", journal.KindDelete, "") &&
			worktreeFileEquals(m.folder, "notes.md", remoteContents) &&
			os.IsNotExist(err)
	})
}

func (w *watchSafetyWorker) requireHealthyWatcher() {
	w.t.Helper()
	for _, failure := range []string{"watcher stopped", "watcher unavailable", "cycle error:", "cycle panic", "offline, will retry:"} {
		if log := w.log(); strings.Contains(log, failure) {
			w.t.Fatalf("lost healthy-watcher precondition: %s", log)
		}
	}
}

func TestWorkerWatcherLossDiscoversUnqueuedWritesAndKeepsPolling(t *testing.T) {
	for _, watchErr := range []error{fsnotify.ErrEventOverflow, errors.New("fatal local watcher backend failure")} {
		t.Run(watchErr.Error(), func(t *testing.T) {
			hub := newFakeHub(t)
			m := watchMount(t, hub)
			injected := make(chan error, 1)
			setLocalWatchHooks(nil, func() <-chan error { return injected }, nil)
			entered, release := make(chan struct{}), make(chan struct{})
			var armed atomic.Bool
			var enterOnce, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			w := startWatchSafetyWorker(t, m, time.Hour, nil, func() {
				if armed.Load() {
					enterOnce.Do(func() {
						close(entered)
						<-release
					})
				}
			})
			t.Cleanup(unblock) // Release before the worker's cleanup join.
			w.wait("completed initial full scan", func() bool {
				return w.pushed("notes.md", journal.KindPut, "hello\n") && strings.Contains(w.log(), "local+1 pulled+0")
			})
			w.requireHealthyWatcher()
			armed.Store(true)
			injected <- watchErr
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("watcher loss did not immediately schedule fallback discovery")
			}
			// stopLocalWatch joins the failed watcher before this cycle begins.
			// Neither the directory nor these user writes can enter its queue.
			if err := os.Mkdir(filepath.Join(m.folder, "unwatched"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeWorktreeFile(t, m.folder, "unwatched/new.txt", "missed by the failed watcher\n")
			writeWorktreeFile(t, m.folder, "notes.md", "ordinary edit during watcher loss\n")
			unblock()
			w.wait("full fallback discovery and push", func() bool {
				return w.pushed("unwatched/new.txt", journal.KindPut, "missed by the failed watcher\n") &&
					w.pushed("notes.md", journal.KindPut, "ordinary edit during watcher loss\n")
			})
			writeWorktreeFile(t, m.folder, "unwatched/later.txt", "polling still discovers new writes\n")
			w.wait("ordinary write after fallback", func() bool {
				return w.pushed("unwatched/later.txt", journal.KindPut, "polling still discovers new writes\n")
			})
			if !worktreeFileEquals(m.folder, "notes.md", "ordinary edit during watcher loss\n") ||
				!worktreeFileEquals(m.folder, "unwatched/new.txt", "missed by the failed watcher\n") {
				t.Fatal("fallback discovery changed the user's disk bytes")
			}
		})
	}
}

func TestWorkerSafetyFullScanDeadlineSurvivesOtherCycles(t *testing.T) {
	hub := newFakeHub(t)
	m := watchMount(t, hub)
	var offset atomic.Int64
	clock := func() time.Time {
		return time.Now().Add(time.Duration(offset.Load()))
	}
	held := filepath.Join(m.folder, "held")
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	setLocalWatchHooks(nil, nil, func(path string) {
		if path == held {
			enterOnce.Do(func() {
				if err := os.WriteFile(filepath.Join(m.folder, "held/missed.txt"), []byte("unobserved user edit\n"), 0o644); err != nil {
					t.Errorf("write missed local edit: %v", err)
				}
				close(entered)
			})
			<-release
		}
	})
	w := startWatchSafetyWorker(t, m, time.Hour, clock, nil)
	t.Cleanup(unblock) // Release the watcher before the worker cleanup join.
	w.wait("completed initial full scan", func() bool {
		return w.pushed("notes.md", journal.KindPut, "hello\n") && strings.Contains(w.log(), "local+1 pulled+0")
	})

	offset.Store(int64(localWatchSafetyInterval - 2*time.Second))
	writeWorktreeFile(t, m.folder, "notes.md", "targeted edit before safety deadline\n")
	w.wait("targeted edit committed without moving full-scan deadline", func() bool {
		return w.pushed("notes.md", journal.KindPut, "targeted edit before safety deadline\n")
	})

	if err := os.Mkdir(held, 0o755); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("new directory never reached recursive watcher installation")
	}
	if w.pushed("held/missed.txt", journal.KindPut, "unobserved user edit\n") {
		t.Fatal("targeted cycle scanned the file before the watcher walk admitted its creation")
	}

	// No wake is sent: the existing timer must honor the full-scan deadline
	// despite the intervening targeted cycle.
	w.wait("safety full scan at the original deadline", func() bool {
		return w.pushed("held/missed.txt", journal.KindPut, "unobserved user edit\n")
	})
	if !worktreeFileEquals(m.folder, "held/missed.txt", "unobserved user edit\n") {
		t.Fatal("safety full scan changed the user's disk bytes")
	}
	w.requireHealthyWatcher()
	unblock()
}

func TestWorkerLocalWakeDebounceHonorsSafetyDeadline(t *testing.T) {
	hub := newFakeHub(t)
	m := watchMount(t, hub)
	var offset atomic.Int64
	clock := func() time.Time {
		return time.Now().Add(time.Duration(offset.Load()))
	}
	held := filepath.Join(m.folder, "held")
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	setLocalWatchHooks(nil, nil, func(path string) {
		if path != held {
			return
		}
		enterOnce.Do(func() {
			if err := os.WriteFile(filepath.Join(m.folder, "held/missed.txt"), []byte("unobserved user edit\n"), 0o644); err != nil {
				t.Errorf("write missed local edit: %v", err)
			}
			close(entered)
		})
		<-release
	})
	w := startWatchSafetyWorker(t, m, time.Hour, clock, nil, 15*time.Second)
	t.Cleanup(unblock) // Release the watcher before the worker cleanup join.
	w.wait("completed initial full scan", func() bool {
		return w.pushed("notes.md", journal.KindPut, "hello\n") &&
			strings.Contains(w.log(), "local+1 pulled+0")
	})
	beforeSync, err := w.st.LoadSync()
	if err != nil {
		t.Fatal(err)
	}
	offset.Store(int64(localWatchSafetyInterval - 2*time.Second))
	const localContents = "ordinary local edit before safety deadline\n"
	writeWorktreeFile(t, m.folder, "notes.md", localContents)

	if err := os.Mkdir(held, 0o755); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("new directory never reached recursive watcher installation")
	}
	const missedPath = "held/missed.txt"
	if w.pushed(missedPath, journal.KindPut, "unobserved user edit\n") {
		t.Fatal("safety scan discovered the unhinted file before the deadline")
	}
	ops, err := w.st.DeviceOps(w.device)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if op.Path == missedPath {
			t.Fatalf("unhinted file entered the local journal before the safety scan: %+v", op)
		}
	}
	afterSync, err := w.st.LoadSync()
	if err != nil {
		t.Fatal(err)
	}
	if afterSync.PushedOps != beforeSync.PushedOps {
		t.Fatalf("unhinted file advanced the push cursor before the safety scan: %d -> %d", beforeSync.PushedOps, afterSync.PushedOps)
	}

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if w.pushed(missedPath, journal.KindPut, "unobserved user edit\n") {
			break
		}
		select {
		case err := <-w.done:
			w.done <- err
			t.Fatalf("worker exited before safety full scan: %v", err)
		case <-deadline.C:
			t.Fatalf("safety full scan did not discover the unhinted file before the debounce interval; log=%s", w.log())
		case <-tick.C:
		}
	}
	if !w.pushed("notes.md", journal.KindPut, localContents) {
		t.Fatal("ordinary local edit was not uploaded with the safety full scan")
	}
	if !worktreeFileEquals(m.folder, missedPath, "unobserved user edit\n") {
		t.Fatal("safety full scan changed the user's disk bytes")
	}
	w.requireHealthyWatcher()
	unblock()
}

func TestWorkerNestedSameVolumeRootsJournalDirectoryRenameAndDelete(t *testing.T) {
	hub := newFakeHub(t)
	m := watchMount(t, hub)
	// The parent excludes the worktree itself, not its project-relative paths.
	// A misrouted nested event is therefore ignored rather than rescued by a
	// second scan of the same physical files under the wrong prefix.
	writeWorktreeFile(t, m.folder, ".bdriveignore", "nested/\n")
	nested := filepath.Join(m.folder, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.volDir, 0o755); err != nil {
		t.Fatal(err)
	}
	proj, ok, err := config.LoadProject(m.folder)
	if err != nil || !ok {
		t.Fatalf("load parent project: ok=%v err=%v", ok, err)
	}
	child, err := config.SaveProject(nested, config.Project{Remote: proj.Remote})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(nested); err != nil {
		t.Fatal(err)
	}
	childVolume, err := config.VolumeDir(child.ID)
	if err != nil || childVolume != m.volDir {
		t.Fatalf("nested mount volume = %q, want %q; err=%v", childVolume, m.volDir, err)
	}
	if err := os.Mkdir(filepath.Join(m.folder, "tree"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeWorktreeFile(t, m.folder, "tree/item.txt", "nested worktree content\n")
	base := time.Now()
	w := startWatchSafetyWorker(t, m, time.Hour, func() time.Time { return base }, nil)
	w.wait("initial nested worktree convergence", func() bool {
		return w.pushed("tree/item.txt", journal.KindPut, "nested worktree content\n") &&
			worktreeFileEquals(nested, "tree/item.txt", "nested worktree content\n") &&
			worktreeFileEquals(nested, ".bdriveignore", "nested/\n") &&
			strings.Contains(w.log(), "local+3 pulled+0")
	})
	if err := os.Rename(filepath.Join(nested, "tree"), filepath.Join(nested, "renamed")); err != nil {
		t.Fatal(err)
	}
	w.wait("nested directory rename journaled at project-relative paths", func() bool {
		_, oldErr := os.Stat(filepath.Join(m.folder, "tree/item.txt"))
		return w.pushed("tree/item.txt", journal.KindDelete, "") &&
			w.pushed("renamed/item.txt", journal.KindPut, "nested worktree content\n") &&
			os.IsNotExist(oldErr) && worktreeFileEquals(m.folder, "renamed/item.txt", "nested worktree content\n")
	})
	if err := os.RemoveAll(filepath.Join(nested, "renamed")); err != nil {
		t.Fatal(err)
	}
	w.wait("nested directory deletion propagated to the parent worktree", func() bool {
		_, err := os.Stat(filepath.Join(m.folder, "renamed/item.txt"))
		return w.pushed("renamed/item.txt", journal.KindDelete, "") && os.IsNotExist(err)
	})
	ops, err := w.st.DeviceOps(w.device)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if strings.HasPrefix(op.Path, "nested/") {
			t.Fatalf("nested root was journaled under the parent's physical prefix: %+v", op)
		}
		if op.Path == "notes.md" && op.Kind == journal.KindDelete {
			t.Fatal("nested rename/delete journaled deletion of an unrelated project file")
		}
	}
	if !worktreeFileEquals(m.folder, "notes.md", "hello\n") || !worktreeFileEquals(nested, "notes.md", "hello\n") {
		t.Fatal("nested rename/delete changed an unrelated project file")
	}
	w.requireHealthyWatcher()
}
