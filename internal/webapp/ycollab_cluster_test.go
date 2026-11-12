package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"

	"github.com/runbear-io/beardrive/internal/remote"
)

/* Two hub processes holding one document.

   max_instance_count = 1 never meant one process: a rollout keeps the old
   revision serving its open websockets beside the new one, and Cloud Run
   overshoots the cap under load. These pin what two processes sharing a
   storage root must do — seed a file once between them, let exactly one
   write it, pass that job on when the writer's room ends, and converge over
   a relay — so that raising the cap is a configuration change. */

// casBackend adds compare-and-swap to a file:// root, so two Servers in one
// test process share it the way two hub instances share a bucket.
type casBackend struct {
	remote.Backend
	mu  sync.Mutex
	ver map[string]int
	// beforePut runs once, inside the next PutIf, standing in for another
	// process writing in the window between a read and its write.
	beforePut func(key string)
}

func (b *casBackend) GetVersioned(ctx context.Context, key string) (io.ReadCloser, remote.Version, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rc, err := b.Backend.Get(ctx, key)
	if err != nil {
		return nil, remote.VersionAbsent, err
	}
	return rc, remote.Version(strconv.Itoa(b.ver[key])), nil
}

func (b *casBackend) PutIf(ctx context.Context, key string, r io.Reader, size int64, expect remote.Version) (remote.Version, error) {
	if hook := b.beforePut; hook != nil {
		b.beforePut = nil
		hook(key)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	cur := remote.VersionAbsent
	if ok, _ := b.Backend.Exists(ctx, key); ok {
		cur = remote.Version(strconv.Itoa(b.ver[key]))
	}
	if cur != expect {
		return remote.VersionAbsent, remote.ErrVersionMismatch
	}
	if err := b.Backend.Put(ctx, key, r, size); err != nil {
		return remote.VersionAbsent, err
	}
	b.ver[key]++
	return remote.Version(strconv.Itoa(b.ver[key])), nil
}

var coEditor = User{Email: "editor@example.com", Name: "An Editor"}

// newHubPair is two hub processes on one storage root and one project.
func newHubPair(t *testing.T, quotas ...QuotaProvider) (a, b *Server, p Project) {
	t.Helper()
	fs, err := remote.Open(context.Background(), "file://"+t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	be := &casBackend{Backend: fs, ver: map[string]int{}}
	db, err := OpenProjectDB(filepath.Join(t.TempDir(), "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, _, err = db.GetOrCreate("proj", "")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(id string) *Server {
		s := &Server{
			Root: be, Projects: db, Device: Identity{ID: id, Name: id, Author: id + "@test"},
			Refresh: 0, Upload: UploadConfig{Enabled: true},
		}
		if len(quotas) > 0 {
			s.Quota = quotas[0]
			quotas = quotas[1:]
		}
		s.Handler()
		t.Cleanup(func() {
			s.Close(context.Background())
			s.rooms().settle()
		})
		return s
	}
	return mk("hub-a"), mk("hub-b"), p
}

func openVia(s *Server, room string) error {
	return s.ydocs().OnLoadDocument(context.Background(), room, crdt.New())
}

// This observer delegates every operation to the real file backend. Its
// channels only expose entry/return ordering for a selected CAS key.
type shutdownCASCall struct {
	key      string
	returned <-chan error
}

type shutdownCASBackend struct {
	remote.Backend
	cond    remote.ConditionalPutter
	mu      sync.Mutex
	watch   string
	entered chan shutdownCASCall
}

func (b *shutdownCASBackend) observe(key string) func(error) {
	b.mu.Lock()
	match := b.watch != "" && strings.Contains(key, b.watch)
	b.mu.Unlock()
	if !match {
		return func(error) {}
	}
	returned := make(chan error, 1)
	select {
	case b.entered <- shutdownCASCall{key: key, returned: returned}:
	default:
	}
	return func(err error) { returned <- err }
}

func (b *shutdownCASBackend) GetVersioned(ctx context.Context, key string) (io.ReadCloser, remote.Version, error) {
	finish := b.observe(key)
	rc, version, err := b.cond.GetVersioned(ctx, key)
	finish(err)
	return rc, version, err
}

func (b *shutdownCASBackend) PutIf(ctx context.Context, key string, r io.Reader, size int64, expect remote.Version) (remote.Version, error) {
	finish := b.observe(key)
	version, err := b.cond.PutIf(ctx, key, r, size, expect)
	finish(err)
	return version, err
}

func (b *shutdownCASBackend) watchKey(key string) {
	b.mu.Lock()
	b.watch = key
	b.mu.Unlock()
}

func newShutdownHub(t *testing.T) (*Server, Project, string, *shutdownCASBackend) {
	t.Helper()
	var observed *shutdownCASBackend
	srv, p, root := newHub(t, true, func(be remote.Backend) remote.Backend {
		cond, ok := be.(remote.ConditionalPutter)
		if !ok {
			t.Fatal("file backend does not support compare-and-swap")
		}
		observed = &shutdownCASBackend{Backend: be, cond: cond, entered: make(chan shutdownCASCall, 16)}
		return observed
	})
	return srv, p, root, observed
}

func holdCollabShutdownLock(t *testing.T, root string) (release func(), assertBlocked func(), assertFree func()) {
	t.Helper()
	path := filepath.Join(root, ".bdrive-tmp-cas.lock")
	holder, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		_ = holder.Close()
		t.Fatal(err)
	}
	probe, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		_ = holder.Close()
		t.Fatal(err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			_ = syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)
			_ = holder.Close()
		})
	}
	t.Cleanup(func() {
		release()
		_ = probe.Close()
	})
	assertBlocked = func() {
		t.Helper()
		err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EWOULDBLOCK) {
			if err == nil {
				_ = syscall.Flock(int(probe.Fd()), syscall.LOCK_UN)
			}
			t.Fatalf("external holder lost its lock: %v", err)
		}
	}
	assertFree = func() {
		t.Helper()
		if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatalf("CAS lock remains held after external holder release: %v", err)
		}
		_ = syscall.Flock(int(probe.Fd()), syscall.LOCK_UN)
	}
	return release, assertBlocked, assertFree
}

func waitShutdownCall(t *testing.T, call func() error, release func()) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- call() }()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		release()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
		t.Fatal("operation did not return while the external CAS lock was held")
		return nil
	}
}

func awaitShutdownCAS(t *testing.T, b *shutdownCASBackend) shutdownCASCall {
	t.Helper()
	select {
	case call := <-b.entered:
		return call
	case <-time.After(3 * time.Second):
		t.Fatal("operation never reached the real file CAS")
		return shutdownCASCall{}
	}
}

func awaitShutdownCASReturn(t *testing.T, call shutdownCASCall) {
	t.Helper()
	select {
	case err := <-call.returned:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("CAS %s returned %v, want context cancellation", call.key, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("CAS %s outlived shutdown", call.key)
	}
}

// The underlying bytes and CAS remain real; the selected I/O waits just as
// a context-aware network backend would, until its caller cancels it.
type shutdownIOBackend struct {
	*shutdownCASBackend
	mu      sync.Mutex
	method  string
	prefix  string
	entered chan struct{}
	abort   chan struct{}
	once    sync.Once
}

func (b *shutdownIOBackend) wait(ctx context.Context, method, key string) error {
	b.mu.Lock()
	match := b.method == method && strings.HasPrefix(key, b.prefix)
	b.mu.Unlock()
	if !match {
		return nil
	}
	select {
	case b.entered <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.abort:
		return errors.New("test storage guard released")
	}
}

func (b *shutdownIOBackend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := b.wait(ctx, "GET", key); err != nil {
		return nil, err
	}
	return b.Backend.Get(ctx, key)
}

func (b *shutdownIOBackend) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if err := b.wait(ctx, "PUT", key); err != nil {
		return err
	}
	return b.Backend.Put(ctx, key, r, size)
}

func (b *shutdownIOBackend) PutIf(ctx context.Context, key string, r io.Reader, size int64, expect remote.Version) (remote.Version, error) {
	if err := b.wait(ctx, "PUT", key); err != nil {
		return remote.VersionAbsent, err
	}
	return b.shutdownCASBackend.PutIf(ctx, key, r, size, expect)
}

func TestCollabClosePropagatesContextToRoomStorage(t *testing.T) {
	for _, step := range []string{"room_bytes", "room_history", "room_head"} {
		t.Run(step, func(t *testing.T) {
			var blocked *shutdownIOBackend
			srv, p, _ := newHub(t, true, func(be remote.Backend) remote.Backend {
				cond, ok := be.(remote.ConditionalPutter)
				if !ok {
					t.Fatal("file backend does not support CAS")
				}
				observed := &shutdownCASBackend{Backend: be, cond: cond, entered: make(chan shutdownCASCall, 16)}
				blocked = &shutdownIOBackend{shutdownCASBackend: observed, entered: make(chan struct{}, 1), abort: make(chan struct{})}
				return blocked
			})
			t.Cleanup(func() { blocked.once.Do(func() { close(blocked.abort) }) })
			h := srv.Handler()
			if rec := putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "before\n", ""); rec.Code != http.StatusOK {
				t.Fatalf("seed: %d", rec.Code)
			}
			doc := openRoom(t, srv, p.ID+"/notes.md")
			typeInto(doc, "dirty tail\n")
			blocked.mu.Lock()
			if step == "room_bytes" {
				blocked.method, blocked.prefix = "GET", p.ID+"/blobs/"
			} else if step == "room_head" {
				blocked.method, blocked.prefix = "PUT", p.ID+"/ydoc/head/"
			} else {
				blocked.method, blocked.prefix = "PUT", p.ID+"/ydoc/"
			}
			blocked.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			err := waitShutdownCall(t, func() error { return srv.Close(ctx) }, func() {
				blocked.mu.Lock()
				blocked.method = ""
				blocked.mu.Unlock()
				blocked.once.Do(func() { close(blocked.abort) })
			})
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Close = %v, want deadline exceeded", err)
			}
			select {
			case <-blocked.entered:
			default:
				t.Fatal("Close never reached the selected room storage operation")
			}
			blocked.mu.Lock()
			blocked.method = ""
			blocked.mu.Unlock()
		})
	}
}

func TestCollabCloseCancelsOnLoadStorageWhileRequestLives(t *testing.T) {
	for _, step := range []string{"room_head", "room_history"} {
		t.Run(step, func(t *testing.T) {
			var blocked *shutdownIOBackend
			srv, p, _ := newHub(t, true, func(be remote.Backend) remote.Backend {
				cond, ok := be.(remote.ConditionalPutter)
				if !ok {
					t.Fatal("file backend does not support CAS")
				}
				observed := &shutdownCASBackend{Backend: be, cond: cond, entered: make(chan shutdownCASCall, 16)}
				blocked = &shutdownIOBackend{shutdownCASBackend: observed, entered: make(chan struct{}, 1), abort: make(chan struct{})}
				return blocked
			})
			h := srv.Handler()
			if rec := putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "before\n", ""); rec.Code != http.StatusOK {
				t.Fatalf("seed: %d", rec.Code)
			}
			room := p.ID + "/notes.md"
			abort := func() {
				blocked.mu.Lock()
				blocked.method = ""
				blocked.mu.Unlock()
				blocked.once.Do(func() { close(blocked.abort) })
			}
			t.Cleanup(func() {
				abort()
				srv.rooms().drop(room)
				srv.rooms().settle()
			})
			blocked.mu.Lock()
			blocked.method = "GET"
			blocked.prefix = p.ID + "/ydoc/head/"
			if step == "room_history" {
				blocked.prefix = p.ID + "/ydoc/" + shaOf("before\n")
			}
			blocked.mu.Unlock()
			requestCtx, cancelRequest := context.WithCancel(context.Background())
			defer cancelRequest()
			loaded := make(chan error, 1)
			go func() { loaded <- srv.ydocs().OnLoadDocument(requestCtx, room, crdt.New()) }()
			select {
			case <-blocked.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("OnLoad never reached the selected seed storage read")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if err := waitShutdownCall(t, func() error { return srv.Close(ctx) }, abort); err != nil && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Close: %v", err)
			}
			if err := waitShutdownCall(t, func() error { return <-loaded }, abort); !errors.Is(err, context.Canceled) {
				t.Fatalf("OnLoad = %v, want lifecycle cancellation", err)
			}
			if requestCtx.Err() != nil {
				t.Fatalf("request context was canceled: %v", requestCtx.Err())
			}
		})
	}
}

func TestCollabCloseCancelsLeaseRenewalAndPendingSnapshot(t *testing.T) {
	oldTTL := leaseTTL
	leaseTTL = 120 * time.Millisecond
	t.Cleanup(func() { leaseTTL = oldTTL })
	fastRoom(t, 50*time.Millisecond, 100*time.Millisecond)
	srv, p, root, observed := newShutdownHub(t)
	h := srv.Handler()
	if rec := putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "before\n", ""); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d", rec.Code)
	}
	room := p.ID + "/notes.md"
	doc := crdt.New()
	if err := srv.ydocs().OnLoadDocument(context.Background(), room, doc); err != nil {
		t.Fatal(err)
	}
	srv.rooms().writer(room, coEditor)
	t.Cleanup(func() {
		srv.rooms().drop(room)
		srv.rooms().settle()
	})
	release, assertBlocked, assertFree := holdCollabShutdownLock(t, root)
	observed.watchKey("/lease/notes.md")
	typeInto(doc, "dirty tail\n")
	call := awaitShutdownCAS(t, observed)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := waitShutdownCall(t, func() error { return srv.Close(ctx) }, release); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want deadline exceeded", err)
	}
	awaitShutdownCASReturn(t, call)
	assertBlocked()
	leasePath := filepath.Join(root, p.ID, "lease", "notes.md")
	atClose, err := os.ReadFile(leasePath)
	if err != nil {
		t.Fatal(err)
	}
	// This crosses both timers, not a shutdown latency assertion.
	time.Sleep(snapshotMax + leaseTTL)
	if got, err := os.ReadFile(leasePath); err != nil || string(got) != string(atClose) {
		t.Fatalf("lease changed after Close while externally locked: %q, %v", got, err)
	}
	if got := get(t, h, "/api/p/"+p.ID+"/file?path=notes.md").Body.String(); got != "before\n" {
		t.Fatalf("pending snapshot wrote after Close: %q", got)
	}
	assertBlocked()
	release()
	assertFree()
	time.Sleep(snapshotMax + leaseTTL)
	if got, err := os.ReadFile(leasePath); err != nil || string(got) != string(atClose) {
		t.Fatalf("lease renewed after external unlock: %q, %v", got, err)
	}
	again, cancelAgain := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelAgain()
	if err := waitShutdownCall(t, func() error { return srv.Close(again) }, release); err != nil {
		t.Fatalf("uncontended second Close: %v", err)
	}
}

func TestCollabCloseCancelsOnLoadWithoutCancelingRequest(t *testing.T) {
	srv, p, root, observed := newShutdownHub(t)
	h := srv.Handler()
	if rec := putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "before\n", ""); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d", rec.Code)
	}
	t.Cleanup(func() {
		srv.rooms().drop(p.ID + "/notes.md")
		srv.rooms().settle()
	})
	release, assertBlocked, assertFree := holdCollabShutdownLock(t, root)
	observed.watchKey("/lease/notes.md")
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	loaded := make(chan error, 1)
	go func() {
		loaded <- srv.ydocs().OnLoadDocument(requestCtx, p.ID+"/notes.md", crdt.New())
	}()
	call := awaitShutdownCAS(t, observed)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := waitShutdownCall(t, func() error { return srv.Close(ctx) }, release); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close: %v", err)
	}
	awaitShutdownCASReturn(t, call)
	if err := waitShutdownCall(t, func() error { return <-loaded }, release); !errors.Is(err, context.Canceled) {
		t.Fatalf("OnLoad = %v, want server lifecycle cancellation", err)
	}
	if err := requestCtx.Err(); err != nil {
		t.Fatalf("request was canceled by Close: %v", err)
	}
	assertBlocked()
	release()
	assertFree()

	healthy, hp, _ := newHub(t, true, nil)
	hh := healthy.Handler()
	if rec := putContent(t, hh, "/api/p/"+hp.ID+"/upload/content?path=notes.md", "before\n", ""); rec.Code != http.StatusOK {
		t.Fatalf("healthy seed: %d", rec.Code)
	}
	doc := openRoom(t, healthy, hp.ID+"/notes.md")
	typeInto(doc, "caller tail\n")
	healthyCtx, cancelHealthy := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelHealthy()
	if err := waitShutdownCall(t, func() error { return healthy.Close(healthyCtx) }, release); err != nil {
		t.Fatalf("healthy Close: %v", err)
	}
	if got := get(t, hh, "/api/p/"+hp.ID+"/file?path=notes.md").Body.String(); got != "before\ncaller tail\n" {
		t.Fatalf("caller context did not save dirty tail: %q", got)
	}
}

func TestCollabParkCopyCallerCancellationPreservesBytes(t *testing.T) {
	srv, p, root, observed := newShutdownHub(t)
	h := srv.Handler()
	release, assertBlocked, assertFree := holdCollabShutdownLock(t, root)
	observed.watchKey("/journal/")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := waitShutdownCall(t, func() error {
		srv.parkCopy(ctx, coEditor, p.ID, "notes.md", "parked.md", "parked tail\n")
		return ctx.Err()
	}, release); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("park caller = %v, want deadline exceeded", err)
	}
	awaitShutdownCASReturn(t, awaitShutdownCAS(t, observed))
	// Cancel any delayed retry before releasing the fault injection.
	closeCtx, cancelClose := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelClose()
	if err := waitShutdownCall(t, func() error { return srv.Close(closeCtx) }, release); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertBlocked()
	if got := get(t, h, "/api/p/"+p.ID+"/file?path=parked.md"); got.Code != http.StatusNotFound {
		t.Fatalf("canceled caller parked bytes while locked: %d %q", got.Code, got.Body.String())
	}
	release()
	assertFree()
}

func TestCollabParkCopyCancellationUsesTheUploadCAS(t *testing.T) {
	for _, closedFirst := range []bool{false, true} {
		name := "inflight_retry"
		if closedFirst {
			name = "closed_before_failed_attempt"
		}
		t.Run(name, func(t *testing.T) {
			oldBackoff, oldMax := parkBackoff, parkBackoffMax
			parkBackoff, parkBackoffMax = 20*time.Millisecond, 20*time.Millisecond
			srv, p, root, observed := newShutdownHub(t)
			t.Cleanup(func() {
				_ = srv.Close(context.Background())
				parkBackoff, parkBackoffMax = oldBackoff, oldMax
			})
			srv.Handler()
			if closedFirst {
				if err := srv.Close(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			release, assertBlocked, assertFree := holdCollabShutdownLock(t, root)
			observed.watchKey("/journal/")
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := waitShutdownCall(t, func() error {
				srv.parkCopy(ctx, coEditor, p.ID, "notes.md", "parked.md", "parked tail\n")
				return nil
			}, release); err != nil {
				t.Fatal(err)
			}
			initial := awaitShutdownCAS(t, observed)
			awaitShutdownCASReturn(t, initial)
			if !closedFirst {
				call := awaitShutdownCAS(t, observed)
				closeCtx, cancelClose := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancelClose()
				if err := waitShutdownCall(t, func() error { return srv.Close(closeCtx) }, release); err != nil {
					t.Fatalf("Close with park retry: %v", err)
				}
				awaitShutdownCASReturn(t, call)
			}
			assertBlocked()
			time.Sleep(3 * parkBackoffMax)
			if got := get(t, srv.Handler(), "/api/p/"+p.ID+"/file?path=parked.md"); got.Code != http.StatusNotFound {
				t.Fatalf("canceled park wrote while externally locked: %d %q", got.Code, got.Body.String())
			}
			release()
			assertFree()
			time.Sleep(3 * parkBackoffMax)
			if got := get(t, srv.Handler(), "/api/p/"+p.ID+"/file?path=parked.md"); got.Code != http.StatusNotFound {
				t.Fatalf("park retry wrote after shutdown and unlock: %d %q", got.Code, got.Body.String())
			}
		})
	}
}

func TestCollabCloseWritesLiveRooms(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	h := srv.Handler()
	url := "/api/p/" + p.ID + "/file?path=notes.md"
	if rec := putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "before\n", ""); rec.Code != http.StatusOK {
		t.Fatalf("seed write: %d", rec.Code)
	}
	doc := openRoom(t, srv, p.ID+"/notes.md")
	typeInto(doc, "typed just before the deploy\n")
	if err := srv.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := get(t, h, url).Body.String(); got != "before\ntyped just before the deploy\n" {
		t.Fatalf("after Close the file says %q", got)
	}
	body, err := os.ReadFile(filepath.Join(root, p.ID, "lease", "notes.md"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err == nil {
		var lease leaseRecord
		if err := json.Unmarshal(body, &lease); err != nil {
			t.Fatal(err)
		}
		if lease.Holder != srv.nodeID() || !lease.Until.IsZero() {
			t.Fatalf("healthy Close left or replaced the room lease: %s", body)
		}
	}
}

func TestCollabCloseCancelsContendedFileCAS(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	h := srv.Handler()
	if rec := putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "before\n", ""); rec.Code != http.StatusOK {
		t.Fatalf("seed write: %d", rec.Code)
	}
	room := p.ID + "/notes.md"
	doc := crdt.New()
	if err := srv.ydocs().OnLoadDocument(context.Background(), room, doc); err != nil {
		t.Fatal(err)
	}
	srv.rooms().writer(room, coEditor)
	t.Cleanup(func() {
		srv.rooms().drop(room)
		srv.rooms().settle()
	})
	typeInto(doc, "dirty shutdown tail\n")
	if got := doc.GetText("body").ToString(); got != "before\ndirty shutdown tail\n" {
		t.Fatalf("dirty document = %q", got)
	}

	lockPath := filepath.Join(root, ".bdrive-tmp-cas.lock")
	holder, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	var unlockHolder sync.Once
	releaseHolder := func() {
		unlockHolder.Do(func() { _ = syscall.Flock(int(holder.Fd()), syscall.LOCK_UN) })
	}
	defer releaseHolder()

	probe, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Close(ctx) }()

	select {
	case err := <-done:
		if ctx.Err() != context.DeadlineExceeded || !errors.Is(err, ctx.Err()) {
			t.Fatalf("Close error = %v, context error = %v, want context deadline exceeded", err, ctx.Err())
		}
	case <-time.After(3 * time.Second):
		// The guard bounds a broken implementation; it is not a shutdown deadline assertion.
		releaseHolder()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
		t.Fatal("Close did not return while the external CAS lock was held")
	}

	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("third descriptor acquired CAS lock before holder release: %v", err)
	}
	if got := get(t, h, "/api/p/"+p.ID+"/file?path=notes.md").Body.String(); got != "before\n" {
		t.Fatalf("contended Close changed file to %q", got)
	}

	releaseHolder()
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("third descriptor could not acquire CAS lock after holder release: %v", err)
	}
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
}

func TestCollabSecondProcessRefusesARoomWithoutARelay(t *testing.T) {
	a, b, p := newHubPair(t)
	if rec := putContent(t, a.Handler(), "/api/p/"+p.ID+"/upload/content?path=notes.md", "base\n", ""); rec.Code != http.StatusOK {
		t.Fatalf("seed write: %d", rec.Code)
	}
	room := p.ID + "/notes.md"
	if err := openVia(a, room); err != nil {
		t.Fatal(err)
	}
	if err := openVia(b, room); !errors.Is(err, errRoomElsewhere) {
		t.Fatalf("a second process opened a held room: %v", err)
	}
	a.dropRoom(room)
	if err := openVia(b, room); err != nil {
		t.Fatalf("after the holder's room ended: %v", err)
	}
	b.dropRoom(room)
}

func TestCollabLeaseOutlivesADeadHolderOnlyByItsTTL(t *testing.T) {
	old := leaseTTL
	leaseTTL = 120 * time.Millisecond
	t.Cleanup(func() { leaseTTL = old })
	a, b, p := newHubPair(t)
	if rec := putContent(t, a.Handler(), "/api/p/"+p.ID+"/upload/content?path=notes.md", "base\n", ""); rec.Code != http.StatusOK {
		t.Fatalf("seed write: %d", rec.Code)
	}
	room := p.ID + "/notes.md"
	if err := openVia(a, room); err != nil {
		t.Fatal(err)
	}
	a.rooms().drop(room) // SIGKILLed: the room is gone, the lease is not released
	if err := openVia(b, room); !errors.Is(err, errRoomElsewhere) {
		t.Fatalf("a lease a dead process still holds was taken at once: %v", err)
	}
	time.Sleep(2 * leaseTTL)
	if err := openVia(b, room); err != nil {
		t.Fatalf("an expired lease was not taken: %v", err)
	}
	b.dropRoom(room)
}

// A seed is a set of items under a fresh client id, so two processes seeding
// one file from its bytes hold two documents that spell the same text — and
// the merge keeps both. The first to store ydoc/<sha> has seeded the file;
// the other must build from that.
func TestCollabSeedsAFileOnceAcrossProcesses(t *testing.T) {
	a, b, p := newHubPair(t)
	if rec := putContent(t, a.Handler(), "/api/p/"+p.ID+"/upload/content?path=notes.md", "base\n", ""); rec.Code != http.StatusOK {
		t.Fatalf("seed write: %d", rec.Code)
	}
	room := p.ID + "/notes.md"
	docA, docB := crdt.New(), crdt.New()
	be := a.Root.(*casBackend)
	// b seeds while a is between "no history yet" and claiming it.
	be.beforePut = func(key string) {
		if !strings.Contains(key, "/ydoc/") {
			t.Fatalf("first conditional write was %s, not the seed", key)
		}
		if err := b.seedDoc(context.Background(), room, docB); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.seedDoc(context.Background(), room, docA); err != nil {
		t.Fatal(err)
	}
	if err := docA.ApplyUpdate(docB.EncodeStateAsUpdate()); err != nil {
		t.Fatal(err)
	}
	if got := docA.GetText("body").ToString(); got != "base\n" {
		t.Fatalf("the two processes' seeds merged to %q", got)
	}
}

// A file taken back to an older version from outside, then reopened: the room
// must spell the file AND already know the edits the revert undid, so a relay
// replaying them (cluster/redis replays a room's retained log on activation)
// puts nothing back.
func TestCollabReopenedRoomKnowsTheEditsARevertUndid(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	room := p.ID + "/notes.md"
	write := func(text string) {
		t.Helper()
		if rec := putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", text, ""); rec.Code != http.StatusOK {
			t.Fatalf("write %q: %d %s", text, rec.Code, rec.Body.String())
		}
	}
	write("base\n")
	doc := openRoom(t, srv, room)
	typeInto(doc, "more\n")
	srv.snapshotRoom(context.Background(), room)
	if got := get(t, h, "/api/p/"+p.ID+"/file?path=notes.md").Body.String(); got != "base\nmore\n" {
		t.Fatalf("snapshot wrote %q", got)
	}
	later := doc.EncodeStateAsUpdate()
	srv.rooms().drop(room)

	write("base\n") // reverted from outside: this blob has a stored state from before "more"
	re := crdt.New()
	if err := srv.seedDoc(context.Background(), room, re); err != nil {
		t.Fatal(err)
	}
	if got := re.GetText("body").ToString(); got != "base\n" {
		t.Fatalf("reopened room spells %q, file says %q", got, "base\n")
	}
	if err := re.ApplyUpdate(later); err != nil {
		t.Fatal(err)
	}
	if got := re.GetText("body").ToString(); got != "base\n" {
		t.Fatalf("a replayed edit put the reverted text back: %q", got)
	}
}

// The comparison contains rests on: a document's own state diffed against its
// own state vector is the empty update, byte for byte.
func TestCollabEmptyDiffIsTheEmptyUpdate(t *testing.T) {
	doc := crdt.New()
	typeInto(doc, "some text")
	missing, err := crdt.DiffUpdateV1(doc.EncodeStateAsUpdate(), doc.StateVector())
	if err != nil {
		t.Fatal(err)
	}
	if string(missing) != string(emptyUpdate) {
		t.Fatalf("diff of a document against itself = %x, empty update = %x", missing, emptyUpdate)
	}
}

func TestCollabProcessesConvergeOverAMemRelay(t *testing.T) {
	a, b, p := newHubPair(t)
	relay := cluster.NewMemRelay()
	for _, s := range []*Server{a, b} {
		if err := s.useCollabRelay(relay); err != nil {
			t.Fatal(err)
		}
	}
	convergeAcross(t, a, b, p)
}

func TestCollabProcessesConvergeOverRedis(t *testing.T) {
	mr := miniredis.RunT(t)
	a, b, p := newHubPair(t)
	for _, s := range []*Server{a, b} {
		s.Collab.Redis = mr.Addr()
		if err := s.ConnectCollab(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	convergeAcross(t, a, b, p)
}

func TestConnectCollabRedisParseErrorOmitsSecrets(t *testing.T) {
	tests := []struct {
		name      string
		redis     string
		forbidden []string
	}{
		{
			name:  "malformed userinfo escape",
			redis: "redis://redis-parse-user:redis-parse-pass%zz@127.0.0.1:6379",
			forbidden: []string{
				"redis://redis-parse-user:redis-parse-pass%zz@127.0.0.1:6379",
				"redis-parse-user",
				"redis-parse-pass",
			},
		},
		{
			name:  "malformed path escape",
			redis: "redis://redis-path-user:redis-path-pass@127.0.0.1:6379/a%zz",
			forbidden: []string{
				"redis://redis-path-user:redis-path-pass@127.0.0.1:6379/a%zz",
				"redis-path-user",
				"redis-path-pass",
			},
		},
		{
			name:  "malformed scheme-less userinfo escape",
			redis: "redis-scheme-user%zz:redis-scheme-pass@127.0.0.1:6379",
			forbidden: []string{
				"redis-scheme-user%zz:redis-scheme-pass@127.0.0.1:6379",
				"redis-scheme-user",
				"redis-scheme-pass",
			},
		},
		{
			name:  "secret in invalid query value",
			redis: "redis://redis-query-user:redis-query-pass@127.0.0.1:6379?dial_timeout=redis-query-secret",
			forbidden: []string{
				"redis://redis-query-user:redis-query-pass@127.0.0.1:6379?dial_timeout=redis-query-secret",
				"redis-query-user",
				"redis-query-pass",
				"redis-query-secret",
			},
		},
		{
			name:  "invalid scheme control",
			redis: "http://redis-control-user:redis-control-pass@127.0.0.1:6379",
			forbidden: []string{
				"http://redis-control-user:redis-control-pass@127.0.0.1:6379",
				"redis-control-user",
				"redis-control-pass",
			},
		},
		{
			name:  "invalid database control",
			redis: "redis://redis-db-user:redis-db-pass@127.0.0.1:6379/notadb",
			forbidden: []string{
				"redis://redis-db-user:redis-db-pass@127.0.0.1:6379/notadb",
				"redis-db-user",
				"redis-db-pass",
			},
		},
		{
			name:  "invalid query control",
			redis: "redis://redis-query-control-user:redis-query-control-pass@127.0.0.1:6379?dial_timeout=not-a-duration",
			forbidden: []string{
				"redis://redis-query-control-user:redis-query-control-pass@127.0.0.1:6379?dial_timeout=not-a-duration",
				"redis-query-control-user",
				"redis-query-control-pass",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{Collab: CollabConfig{Redis: tc.redis}}
			err := srv.ConnectCollab(context.Background())
			if err == nil {
				t.Fatalf("malformed Redis configuration was accepted")
			}
			message := err.Error()
			for _, forbidden := range tc.forbidden {
				if strings.Contains(message, forbidden) {
					t.Fatalf("Redis configuration details appeared in the error")
				}
			}
		})
	}
}

// convergeAcross is one file edited from two processes: every edit reaches
// the other process, one process writes the file, and when its room ends the
// other takes the pen and builds on what was written instead of parking a
// conflict copy beside it.
func convergeAcross(t *testing.T, a, b *Server, p Project) {
	t.Helper()
	fastRoom(t, 50*time.Millisecond, 500*time.Millisecond)
	h := a.Handler()
	if rec := putContent(t, h, "/api/p/"+p.ID+"/upload/content?path=notes.md", "base\n", ""); rec.Code != http.StatusOK {
		t.Fatalf("seed write: %d", rec.Code)
	}
	room := p.ID + "/notes.md"
	url := "/api/p/" + p.ID + "/file?path=notes.md"
	for _, s := range []*Server{a, b} {
		s.rooms().writer(room, coEditor)
	}
	typeVia := func(s *Server, text string) {
		t.Helper()
		err := s.ydocs().Apply(context.Background(), room, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			txt := doc.GetText("body")
			transact(func(txn *crdt.Transaction) { txt.Insert(txn, txt.Len(), text, nil) })
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	textOf := func(s *Server) string {
		doc := s.ydocs().GetDoc(room)
		if doc == nil {
			return ""
		}
		return doc.GetText("body").ToString()
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s: a=%q b=%q file=%q", what, textOf(a), textOf(b), get(t, h, url).Body.String())
	}

	typeVia(a, "from A\n")
	waitFor("A writing the file", func() bool { return get(t, h, url).Body.String() == "base\nfrom A\n" })
	// B opens the room mid-session: built from what A stored, caught up by
	// the relay. (The Redis relay subscribes a node only to the rooms it
	// hosts, so nothing reaches B before this.)
	typeVia(b, "")
	waitFor("A's edit reaching B", func() bool { return textOf(b) == "base\nfrom A\n" })

	typeVia(b, "from B\n")
	waitFor("B's edit reaching A", func() bool { return textOf(a) == "base\nfrom A\nfrom B\n" })
	waitFor("A writing B's edit", func() bool { return get(t, h, url).Body.String() == "base\nfrom A\nfrom B\n" })

	// A's room ends. B must take the pen and build on A's last write.
	if err := a.ydocs().CloseRoom(room, true); err != nil {
		t.Fatal(err)
	}
	typeVia(b, "after A left\n")
	waitFor("B writing after taking over", func() bool { return get(t, h, url).Body.String() == "base\nfrom A\nfrom B\nafter A left\n" })

	// B's edit also reaches A once A's room is rebuilt — from the stored
	// state, so nothing is doubled.
	typeVia(a, "A is back\n")
	waitFor("A rebuilt and converged", func() bool {
		return textOf(a) == "base\nfrom A\nfrom B\nafter A left\nA is back\n" && textOf(b) == textOf(a)
	})

	var feed struct {
		Entries []struct{ Path string } `json:"entries"`
	}
	if err := json.Unmarshal(get(t, h, "/api/p/"+p.ID+"/history?prefix=&n=100").Body.Bytes(), &feed); err != nil {
		t.Fatal(err)
	}
	for _, e := range feed.Entries {
		if strings.Contains(e.Path, ".bdrive-conflict-") {
			t.Fatalf("converged editing still parked %s", e.Path)
		}
	}
	// Torn down here, before fastRoom's cleanup restores the timings: a
	// snapshot timer or lease renewer still running would read them racing
	// that write.
	for _, s := range []*Server{a, b} {
		s.Close(context.Background())
		s.rooms().settle()
	}
}
