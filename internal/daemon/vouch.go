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
}

// newPulledVouch starts a worker's vouch with no marker: one left by an
// earlier worker, even in this same process, does not cover this worker
// until it has passed itself.
func newPulledVouch(volDir string) *pulledVouch {
	store.ClearPulled(volDir)
	return &pulledVouch{volDir: volDir}
}

// passed records a tick that pulled mounts from the hub, unless an
// announcement is still queued (queued reports it), which the pass predates.
// It shares the lock with announced, so a pass and an announcement order one
// way or the other, never interleave.
func (v *pulledVouch) passed(mounts []string, queued func() bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if queued() {
		return
	}
	v.mounts = mounts
	_ = store.MarkPulled(v.volDir, mounts)
}

// announced withdraws the vouch for a peer's announcement and, still holding
// the lock, queues it for the worker (enqueue).
func (v *pulledVouch) announced(enqueue func()) {
	v.mu.Lock()
	defer v.mu.Unlock()
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

// renew rewrites the marker if the last complete pass still stands and the
// stream is alive, which is what lets the files on disk stay vouched for. A
// stream that has delivered no frame yet has not shown it is alive.
func (v *pulledVouch) renew() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.mounts == nil || v.stream == nil {
		return
	}
	last := v.stream.StreamFrame()
	if last.IsZero() || time.Since(last) >= streamAliveFor {
		return
	}
	_ = store.MarkPulled(v.volDir, v.mounts)
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
