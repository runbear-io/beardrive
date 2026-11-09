package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/store"
)

// streamAliveFor is how recent the change stream's last frame must be for the
// stream to count as alive. The hub sends a keepalive every 20 s on an idle
// stream, so this tolerates one late keepalive and no more.
var streamAliveFor = 30 * time.Second

// vouchRefresh is how often a quiet, live stream renews the pulled marker. It
// must stay well under the hooks' freshness window (cmd/bdrive hookPullFresh).
var vouchRefresh = 5 * time.Second

// pulledVouch keeps one volume's pulled marker (store.MarkPulled) honest. A
// tick that pulled every mount writes it; with the hub's change stream open
// and alive, no announcement means nothing has changed, so a quiet stretch
// keeps it fresh between the stream-paced polls (minutes apart); a peer's
// announcement, a failed pass, or the stream going quiet ends it until the
// next complete pass.
type pulledVouch struct {
	volDir string
	mu     sync.Mutex
	mounts []string              // what the last complete pass pulled; nil = not vouching
	stream remote.StreamLiveness // the open change stream, nil when none
	heard  uint64                // announcements heard so far
}

// newPulledVouch starts a worker's vouch with no marker: one left by an
// earlier worker, even in this same process, does not cover this worker
// until it has passed itself.
func newPulledVouch(volDir string) *pulledVouch {
	store.ClearPulled(volDir)
	return &pulledVouch{volDir: volDir}
}

// heardSoFar is the count a cycle takes before it pulls and hands to passed.
func (v *pulledVouch) heardSoFar() uint64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.heard
}

// passed records a tick that pulled mounts from the hub, unless an
// announcement was heard since the tick took heardSoFar: the pull may have
// started before that push landed.
func (v *pulledVouch) passed(mounts []string, since uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.heard != since {
		return
	}
	v.mounts = mounts
	_ = store.MarkPulledUntil(v.volDir, mounts, v.deadline())
}

// hear withdraws the vouch for a peer's announcement. The change stream calls
// it as it reads one (remote.AnnounceNotifier), before the worker is woken,
// so no hook can take the volume lock between the two and still see it.
func (v *pulledVouch) hear() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.heard++
	v.mounts = nil
	store.ClearPulled(v.volDir)
}

// announced is hear for a stream that cannot report announcements as it
// reads them: it withdraws and, still holding the lock, queues the
// announcement for the worker (enqueue).
func (v *pulledVouch) announced(enqueue func()) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.heard++
	v.mounts = nil
	store.ClearPulled(v.volDir)
	enqueue()
}

// withdraw stops vouching: a pass failed, or the worker is stopping.
func (v *pulledVouch) withdraw() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.mounts = nil
	store.ClearPulled(v.volDir)
}

// streaming records the change stream now open, or nil once it closes. A
// closed stream withdraws the vouch: announcements may have been missed, so
// only a pass after the next stream opens can stand again.
func (v *pulledVouch) streaming(s remote.StreamLiveness) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.stream = s
	if s == nil {
		v.mounts = nil
		store.ClearPulled(v.volDir)
	}
}

// deadline is when the stream would stop counting as alive if it delivered
// nothing more, which is as long as any marker written now may stand; zero
// with no stream to judge by. Callers hold mu.
func (v *pulledVouch) deadline() time.Time {
	if v.stream == nil {
		return time.Time{}
	}
	if last := v.stream.StreamFrame(); !last.IsZero() {
		return last.Add(streamAliveFor)
	}
	return time.Time{}
}

// renew rewrites the marker if the last complete pass still stands and the
// stream is alive, which is what lets the files on disk stay vouched for. A
// stream that has delivered no frame yet has not shown it is alive; one that
// has gone quiet past streamAliveFor withdraws the vouch, since it may be
// dead with announcements lost in it.
func (v *pulledVouch) renew() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.mounts == nil || v.stream == nil {
		return
	}
	last := v.stream.StreamFrame()
	if last.IsZero() {
		return
	}
	if time.Since(last) >= streamAliveFor {
		v.mounts = nil
		store.ClearPulled(v.volDir)
		return
	}
	_ = store.MarkPulledUntil(v.volDir, v.mounts, v.deadline())
}

// run renews the marker until ctx ends, then withdraws it: a stopped daemon
// vouches for nothing.
func (v *pulledVouch) run(ctx context.Context) {
	tick := time.NewTicker(vouchRefresh)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			v.withdraw()
			return
		case <-tick.C:
			v.renew()
		}
	}
}
