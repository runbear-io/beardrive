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

// passed records a tick that pulled mounts from the hub.
func (v *pulledVouch) passed(mounts []string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.mounts = mounts
	_ = store.MarkPulled(v.volDir, mounts)
}

// withdraw stops vouching: a peer announced a push, or a pass failed.
func (v *pulledVouch) withdraw() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.mounts = nil
	store.ClearPulled(v.volDir)
}

// streaming records the change stream now open, or nil once it closes.
func (v *pulledVouch) streaming(s remote.StreamLiveness) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.stream = s
}

// renew rewrites the marker if the last complete pass still stands and the
// stream is alive, which is what lets the files on disk stay vouched for.
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
