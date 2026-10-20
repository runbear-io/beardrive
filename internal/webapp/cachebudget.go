package webapp

import (
	"math"
	"runtime/debug"
	"sync"
)

// hubCaches is the memory the hub's per-project caches share: parsed journals
// (counted in raw journal bytes) and, through histOpBytes, history indexes.
// Over budget, the projects used least recently are dropped first. The project
// being served is never dropped to make room for itself: a fold or a history
// request needs every op of it in memory either way, so evicting it would save
// nothing and turn its next request into a re-download of the whole project.
var hubCaches = &cacheBudget{limit: defaultCacheBytes(), entries: map[*RemoteSource]*budgetEntry{}}

// defaultCacheBytes is a quarter of the runtime's memory limit (GOMEMLIMIT)
// when one is set, else 256 MiB.
func defaultCacheBytes() int64 {
	if lim := debug.SetMemoryLimit(-1); lim > 0 && lim < math.MaxInt64 {
		return lim / 4
	}
	return 256 << 20
}

type cacheBudget struct {
	mu      sync.Mutex
	limit   int64
	entries map[*RemoteSource]*budgetEntry
	total   int64
	tick    uint64
}

type budgetEntry struct {
	bytes int64
	used  uint64
}

func (b *cacheBudget) limitBytes() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limit
}

func (b *cacheBudget) setLimit(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.limit = n
}

// charge records that src's journal cache now holds bytes, marks it most
// recently used, and returns the other sources whose caches must be dropped
// for the total to fit. They are already uncharged; the caller drops them
// without holding b.mu.
func (b *cacheBudget) charge(src *RemoteSource, bytes int64) []*RemoteSource {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[src]
	if e == nil {
		e = &budgetEntry{}
		b.entries[src] = e
	}
	b.total += bytes - e.bytes
	e.bytes = bytes
	b.tick++
	e.used = b.tick
	var drop []*RemoteSource
	for b.total > b.limit {
		var victim *RemoteSource
		for s, x := range b.entries {
			if s != src && x.bytes > 0 && (victim == nil || x.used < b.entries[victim].used) {
				victim = s
			}
		}
		if victim == nil {
			break
		}
		b.total -= b.entries[victim].bytes
		delete(b.entries, victim)
		drop = append(drop, victim)
	}
	return drop
}

// dropJournals empties a source's parsed-journal cache after the budget has
// chosen it.
func (r *RemoteSource) dropJournals() {
	r.jmu.Lock()
	r.jcache, r.jbytes = nil, 0
	r.jmu.Unlock()
}
