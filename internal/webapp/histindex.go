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
	all, err := r.sourcedOpsOf(ctx, keep)
	if err != nil {
		return nil, err
	}
	x := buildHistIndex(all)
	histCache.put(r, fp, x)
	return x, nil
}

// maxHistIndexOps bounds the ops held by history indexes across the whole hub
// (a few hundred bytes each). Least recently used projects are dropped first;
// a dropped project's next history request rebuilds, which is what every
// request cost before the index existed.
const maxHistIndexOps = 300_000

type histEntry struct {
	src  *RemoteSource
	fp   string
	x    *histIndex
	used uint64
}

type histLRU struct {
	mu      sync.Mutex
	entries map[*RemoteSource]*histEntry
	ops     int
	tick    uint64
}

var histCache = &histLRU{entries: map[*RemoteSource]*histEntry{}}

func (c *histLRU) get(src *RemoteSource, fp string) *histIndex {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[src]
	if !ok || e.fp != fp {
		return nil
	}
	c.tick++
	e.used = c.tick
	return e.x
}

func (c *histLRU) put(src *RemoteSource, fp string, x *histIndex) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.entries[src]; ok {
		c.ops -= len(old.x.all)
		delete(c.entries, src)
	}
	if len(x.all) > maxHistIndexOps {
		return
	}
	for c.ops+len(x.all) > maxHistIndexOps {
		var victim *histEntry
		for _, e := range c.entries {
			if victim == nil || e.used < victim.used {
				victim = e
			}
		}
		c.ops -= len(victim.x.all)
		delete(c.entries, victim.src)
	}
	c.tick++
	c.entries[src] = &histEntry{src: src, fp: fp, x: x, used: c.tick}
	c.ops += len(x.all)
}
