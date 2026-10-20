package webapp

import (
	"math"
	"runtime/debug"
	"sync"
)

// hubCaches is the one memory budget the hub's per-project caches share:
// parsed journals (counted in raw journal bytes) and history indexes (at
// histOpBytes per op).
// Over budget, the projects used least recently are dropped first. The project
// being served is never dropped to make room for itself: a fold or a history
// request needs every op of it in memory either way, so evicting it would save
// nothing and turn its next request into a re-download of the whole project.
var hubCaches = &cacheBudget{limit: defaultCacheBytes(), entries: map[budgetKey]*budgetEntry{}}

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
	entries map[budgetKey]*budgetEntry
	total   int64
	tick    uint64
}

type cacheKind uint8

const (
	journalCache cacheKind = iota // RemoteSource.jcache, in raw journal bytes
	historyCache                  // histCache's entry, at histOpBytes per op
)

type budgetKey struct {
	src  *RemoteSource
	kind cacheKind
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

// held is what src's caches are charged, both kinds together.
func (b *cacheBudget) held(src *RemoteSource) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	var n int64
	for k, e := range b.entries {
		if k.src == src {
			n += e.bytes
		}
	}
	return n
}

func (b *cacheBudget) heldJournals(src *RemoteSource) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e := b.entries[budgetKey{src, journalCache}]; e != nil {
		return e.bytes
	}
	return 0
}

// touch marks one of src's caches most recently used.
func (b *cacheBudget) touch(src *RemoteSource, kind cacheKind) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e := b.entries[budgetKey{src, kind}]; e != nil {
		b.tick++
		e.used = b.tick
	}
}

// charge records that one of src's caches now holds bytes, marks it most
// recently used, and evicts caches of OTHER sources, least recently used
// first, until the total fits. Eviction runs after b.mu is released, since
// dropping a cache takes that cache's own lock.
func (b *cacheBudget) charge(src *RemoteSource, kind cacheKind, bytes int64) {
	b.mu.Lock()
	key := budgetKey{src, kind}
	e := b.entries[key]
	if e == nil {
		e = &budgetEntry{}
		b.entries[key] = e
	}
	b.total += bytes - e.bytes
	e.bytes = bytes
	b.tick++
	e.used = b.tick
	var drop []budgetKey
	for b.total > b.limit {
		var victim budgetKey
		var oldest *budgetEntry
		for k, x := range b.entries {
			if k.src != src && x.bytes > 0 && (oldest == nil || x.used < oldest.used) {
				victim, oldest = k, x
			}
		}
		if oldest == nil {
			break
		}
		b.total -= oldest.bytes
		delete(b.entries, victim)
		drop = append(drop, victim)
	}
	b.mu.Unlock()
	for _, k := range drop {
		switch k.kind {
		case journalCache:
			k.src.dropJournals()
		case historyCache:
			histCache.drop(k.src)
		}
	}
}

// dropJournals empties a source's parsed-journal cache after the budget has
// chosen it.
func (r *RemoteSource) dropJournals() {
	r.jmu.Lock()
	r.jcache, r.jbytes = nil, 0
	r.jmu.Unlock()
}
