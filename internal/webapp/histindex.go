package webapp

import (
	"context"
	"sort"
	"sync"

	"github.com/runbear-io/beardrive/internal/journal"
)

// histIndex is everything the history feed derives from a project's journals
// that does not depend on who is asking: every op in journal.Less order, what
// each one was (add, edit or delete) and the blob it replaced, and the move
// index. Building it is a sort of every op in the project, which was most of
// what each history request cost; it is now built once per journal state and
// shared, read-only, by every request until a journal grows.
type histIndex struct {
	all   []sourcedOp
	kinds []string
	prevs []string

	movesOnce sync.Once
	moves     moveIndex
}

// moveIndex is built on first use: only ?path= feeds follow moves.
func (x *histIndex) moveIndex() moveIndex {
	x.movesOnce.Do(func() {
		ops := make([]journal.Op, len(x.all))
		for i, sop := range x.all {
			ops[i] = sop.Op
		}
		x.moves = buildMoveIndex(ops)
	})
	return x.moves
}

func buildHistIndex(all []sourcedOp) *histIndex {
	sort.SliceStable(all, func(i, j int) bool { return journal.Less(all[i].Op, all[j].Op) })
	// A put is an "add" when the path didn't exist just before it (first
	// version, or first after a delete), an "edit" otherwise. Existence is
	// replayed over ALL ops in journal order, before any path/prefix filter,
	// so a filtered view classifies the same as the full feed.
	kinds := make([]string, len(all))
	prevs := make([]string, len(all))
	exists := make(map[string]bool, len(all))
	last := make(map[string]string, len(all))
	for i, so := range all {
		op := so.Op
		// A delete of a path that is already gone removed nothing, so it
		// names nothing — even though the path once had content.
		if op.Kind != journal.KindDelete || exists[op.Path] {
			prevs[i] = last[op.Path]
		}
		if op.Kind != journal.KindDelete {
			last[op.Path] = op.Blob
		}
		switch {
		case op.Kind == journal.KindDelete:
			kinds[i] = "delete"
			exists[op.Path] = false
		case exists[op.Path]:
			kinds[i] = "edit"
		default:
			kinds[i] = "add"
			exists[op.Path] = true
		}
	}
	return &histIndex{all: all, kinds: kinds, prevs: prevs}
}

// historyIndex returns the project's history index, rebuilt only when the
// journal listing moved (see journalFingerprint).
func (r *RemoteSource) historyIndex(ctx context.Context) (*histIndex, error) {
	keep, err := r.listJournals(ctx)
	if err != nil {
		return nil, err
	}
	fp := journalFingerprint(keep)
	if x := histCache.get(r, fp); x != nil {
		return x, nil
	}
	x, err, _ := r.flight.Do("hist\x00"+fp, func() (any, error) {
		all, err := r.sourcedOpsOf(context.WithoutCancel(ctx), keep)
		if err != nil {
			return nil, err
		}
		x := buildHistIndex(all)
		histCache.put(r, fp, x)
		return x, nil
	})
	if err != nil {
		return nil, err
	}
	return x.(*histIndex), nil
}

// histOpBytes is roughly what one op costs in a history index: what an index
// is charged against hubCaches, which drops the least recently used projects'
// caches first. A dropped project's next history request rebuilds. The index
// just built is never dropped to make room for itself: rebuilding it per
// request costs the same memory plus the sort, every time.
const histOpBytes = 400

type histEntry struct {
	fp string
	x  *histIndex
}

// histLRU holds each project's current history index; hubCaches decides which
// ones stay.
type histLRU struct {
	mu      sync.Mutex
	entries map[*RemoteSource]*histEntry
}

var histCache = &histLRU{entries: map[*RemoteSource]*histEntry{}}

func (c *histLRU) get(src *RemoteSource, fp string) *histIndex {
	c.mu.Lock()
	e, ok := c.entries[src]
	c.mu.Unlock()
	if !ok || e.fp != fp {
		return nil
	}
	hubCaches.touch(src, historyCache)
	return e.x
}

func (c *histLRU) put(src *RemoteSource, fp string, x *histIndex) {
	c.mu.Lock()
	c.entries[src] = &histEntry{fp: fp, x: x}
	c.mu.Unlock()
	hubCaches.charge(src, historyCache, int64(len(x.all))*histOpBytes)
}

func (c *histLRU) drop(src *RemoteSource) {
	c.mu.Lock()
	delete(c.entries, src)
	c.mu.Unlock()
}
