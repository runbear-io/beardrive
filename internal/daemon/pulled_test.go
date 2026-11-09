package daemon

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/syncer"
)

func projectIDOf(t *testing.T, m watchFixture) string {
	t.Helper()
	proj, ok, err := config.LoadProject(m.folder)
	if err != nil || !ok {
		t.Fatalf("load project: ok=%v err=%v", ok, err)
	}
	return proj.ID
}

// The turn hook trusts the files on disk only while the daemon vouches for
// them, so a complete pass must record itself, its pid and its mounts.
func TestWorkerRecordsEachPassAgainstTheHub(t *testing.T) {
	hub := newFakeHub(t)
	m := watchMountWithRemote(t, strings.TrimSuffix(hub.URL, "/")+"/p/p-test")
	w := startWatchSafetyWorker(t, m, time.Hour, nil, nil)
	w.wait("a recorded pass against the hub", func() bool {
		return !store.LoadPulled(m.volDir).At.IsZero()
	})
	p := store.LoadPulled(m.volDir)
	if age := time.Since(p.At); age > time.Minute {
		t.Errorf("recorded pass is %s old", age)
	}
	if p.PID != os.Getpid() {
		t.Errorf("pass recorded by pid %d, want the daemon's own %d", p.PID, os.Getpid())
	}
	if id := projectIDOf(t, m); !p.Covers(id) {
		t.Errorf("marker mounts = %v, want %s", p.Mounts, id)
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
	if p := store.LoadPulled(m.volDir); !p.At.IsZero() {
		t.Errorf("an offline daemon recorded a pass at %s", p.At)
	}
}

// With the change stream open the hub is polled minutes apart, so a quiet,
// live stream must keep the marker fresh in between, and a peer's
// announcement must withdraw it before the cycle that pulls the push starts.
func TestWorkerVouchesWhileTheStreamIsAliveAndWithdrawsOnAnnouncement(t *testing.T) {
	oldRefresh := vouchRefresh
	vouchRefresh = 20 * time.Millisecond
	t.Cleanup(func() { vouchRefresh = oldRefresh })
	hub := newFakeHub(t)
	proxy := newRemoteWakeProxy(t, hub)
	m := watchMountWithRemote(t, strings.TrimSuffix(proxy.URL, "/")+"/p/p-test")
	var armed atomic.Bool
	sawWithdrawn := make(chan bool, 1)
	w := startWatchSafetyWorker(t, m, time.Hour, nil, func() {
		if armed.CompareAndSwap(true, false) {
			sawWithdrawn <- store.LoadPulled(m.volDir).At.IsZero()
		}
	})
	select {
	case <-proxy.connected:
	case <-time.After(5 * time.Second):
		t.Fatal("remote watcher did not connect")
	}
	w.wait("a recorded pass", func() bool { return !store.LoadPulled(m.volDir).At.IsZero() })
	first := store.LoadPulled(m.volDir).At
	w.wait("a renewal with no poll", func() bool { return store.LoadPulled(m.volDir).At.After(first) })

	armed.Store(true)
	proxy.wake <- struct{}{}
	select {
	case withdrawn := <-sawWithdrawn:
		if !withdrawn {
			t.Error("the marker still vouched when the announcement's cycle started")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no cycle after the announcement")
	}
	w.wait("a pass recorded after the announcement", func() bool {
		return !store.LoadPulled(m.volDir).At.IsZero()
	})
}

// The marker must only follow a tick that pulled every mount: the merged
// result of a multi-mount volume can be read-only and offline at once.
func TestPassPulledNeedsEveryMountPulled(t *testing.T) {
	for _, c := range []struct {
		name      string
		doRemote  bool
		connected bool
		err       error
		res       syncer.Result
		want      bool
	}{
		{"remote pass", true, true, nil, syncer.Result{}, true},
		{"read-only pass", true, true, nil, syncer.Result{ReadOnly: true}, true},
		{"read-only beside an offline mount", true, true, nil, syncer.Result{ReadOnly: true, Offline: true}, false},
		{"offline", true, true, nil, syncer.Result{Offline: true}, false},
		{"access revoked", true, true, nil, syncer.Result{NoAccess: true}, false},
		{"backfill still landing", true, true, nil, syncer.Result{Backfilling: true}, false},
		{"cycle error", true, true, errors.New("boom"), syncer.Result{}, false},
		{"local-only tick", false, true, nil, syncer.Result{}, false},
		{"no backend", true, false, nil, syncer.Result{}, false},
	} {
		if got := passPulled(c.doRemote, c.connected, c.err, c.res); got != c.want {
			t.Errorf("%s: passPulled = %v, want %v", c.name, got, c.want)
		}
	}
}

type fakeStream struct{ last time.Time }

func (f *fakeStream) StreamFrame() time.Time { return f.last }

// renew only extends a standing pass over a live stream.
func TestVouchRenewsOnlyOverALiveStream(t *testing.T) {
	dir := t.TempDir()
	v := &pulledVouch{volDir: dir}
	stream := &fakeStream{last: time.Now()}

	v.renew()
	if !store.LoadPulled(dir).At.IsZero() {
		t.Fatal("renewed with no pass to stand on")
	}
	v.passed([]string{"p-a"}, v.heardSoFar())
	v.streaming(stream)
	old := time.Now().Add(-time.Minute)
	os.Chtimes(dir+"/pulled", old, old)

	stream.last = time.Now()
	v.renew()
	if at := store.LoadPulled(dir).At; !at.After(old.Add(time.Second)) {
		t.Error("did not renew over a live stream")
	}
	v.withdraw()
	v.renew()
	if !store.LoadPulled(dir).At.IsZero() {
		t.Error("renewed after a withdrawal")
	}
}

// A stream that closed may have missed announcements: the pass before it
// cannot be renewed over the stream that replaces it.
func TestVouchDoesNotSurviveAStreamReconnect(t *testing.T) {
	dir := t.TempDir()
	v := newPulledVouch(dir)
	v.passed([]string{"p-a"}, v.heardSoFar())
	v.streaming(&fakeStream{last: time.Now()})
	v.streaming(nil)
	if !store.LoadPulled(dir).At.IsZero() {
		t.Error("the marker outlived its stream")
	}
	v.streaming(&fakeStream{last: time.Now()})
	v.renew()
	if !store.LoadPulled(dir).At.IsZero() {
		t.Error("a new stream renewed the pass from before the reconnect")
	}
}

// A stream that has delivered nothing yet has not shown it is alive.
func TestVouchDoesNotRenewOverASilentNewStream(t *testing.T) {
	dir := t.TempDir()
	v := newPulledVouch(dir)
	v.passed([]string{"p-a"}, v.heardSoFar())
	v.streaming(&fakeStream{})
	old := time.Now().Add(-time.Minute)
	os.Chtimes(dir+"/pulled", old, old)
	v.renew()
	if at := store.LoadPulled(dir).At; at.After(old.Add(time.Second)) {
		t.Error("renewed over a stream with no frame yet")
	}
}

// A replacement worker in the same process must not inherit the marker its
// predecessor left: same pid, different worker, no pass yet.
func TestANewWorkerVouchStartsWithNoMarker(t *testing.T) {
	dir := t.TempDir()
	if err := store.MarkPulled(dir, []string{"p-a"}); err != nil {
		t.Fatal(err)
	}
	newPulledVouch(dir)
	if !store.LoadPulled(dir).At.IsZero() {
		t.Error("a new worker's vouch kept its predecessor's marker")
	}
}

// A pass and an announcement order one way or the other: an announcement
// heard after the cycle took its count may name a push the pull missed.
func TestVouchPassDoesNotCoverAnAnnouncementHeardSince(t *testing.T) {
	dir := t.TempDir()
	v := newPulledVouch(dir)
	since := v.heardSoFar()
	v.hear()
	v.passed([]string{"p-a"}, since)
	if !store.LoadPulled(dir).At.IsZero() {
		t.Error("a pass marked over an announcement heard while it pulled")
	}
	v.passed([]string{"p-a"}, v.heardSoFar())
	if store.LoadPulled(dir).At.IsZero() {
		t.Error("a pass begun after the announcement did not mark")
	}
	v.announced(func() {})
	if !store.LoadPulled(dir).At.IsZero() {
		t.Error("an announcement left the marker in place")
	}
}

// A stream that goes quiet past streamAliveFor may be dead with announcements
// lost in it: the vouch is withdrawn, not merely left to age.
func TestVouchWithdrawsWhenTheStreamGoesQuiet(t *testing.T) {
	dir := t.TempDir()
	v := newPulledVouch(dir)
	stream := &fakeStream{last: time.Now()}
	v.streaming(stream)
	v.passed([]string{"p-a"}, v.heardSoFar())
	stream.last = time.Now().Add(-2 * streamAliveFor)
	v.renew()
	if !store.LoadPulled(dir).At.IsZero() {
		t.Error("the marker outlived a stream gone quiet")
	}
	stream.last = time.Now()
	v.renew()
	if !store.LoadPulled(dir).At.IsZero() {
		t.Error("a frame after the withdrawal renewed a pass that no longer stands")
	}
}

// No marker stands past the moment its stream would stop counting as alive,
// however recently it was written.
func TestVouchMarkerEndsWithTheStreamsLiveness(t *testing.T) {
	dir := t.TempDir()
	v := newPulledVouch(dir)
	last := time.Now().Add(-streamAliveFor + time.Second)
	v.streaming(&fakeStream{last: last})
	v.passed([]string{"p-a"}, v.heardSoFar())
	p := store.LoadPulled(dir)
	if want := last.Add(streamAliveFor); !p.Until.Equal(want) {
		t.Fatalf("marker until = %s, want %s", p.Until, want)
	}
	if !p.FreshWithin(time.Minute) {
		t.Fatal("marker not fresh before its deadline")
	}
	time.Sleep(1100 * time.Millisecond)
	if store.LoadPulled(dir).FreshWithin(time.Minute) {
		t.Error("marker still fresh past its stream's liveness")
	}
}

// A worker replacing another in the same process announces the same pid, so
// the marker its predecessor left must be gone before that pid can be read.
func TestAReplacementWorkerIsNotVouchedForByItsPredecessor(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	down.Close()
	m := watchMountWithRemote(t, down.URL+"/p/p-test")
	if err := store.MarkPulled(m.volDir, []string{projectIDOf(t, m)}); err != nil {
		t.Fatal(err)
	}
	vouchedOnArrival := make(chan bool, 1)
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if pid, running := Running(m.volDir); running && pid == os.Getpid() {
				vouchedOnArrival <- !store.LoadPulled(m.volDir).At.IsZero()
				return
			}
		}
		close(vouchedOnArrival)
	}()
	startWatchSafetyWorker(t, m, time.Hour, nil, nil)
	stale, ok := <-vouchedOnArrival
	if !ok {
		t.Fatal("the worker never announced itself")
	}
	if stale {
		t.Error("the new worker's pid was readable while its predecessor's marker still stood")
	}
}
