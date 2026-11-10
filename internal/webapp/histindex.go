package webapp

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

type histIndex struct {
	all      []sourcedOp
	kinds    []string
	prevs    []string
	byTime   []int32
	journals map[string]ckptJournalMeta
	exists   map[string]bool
	last     map[string]string

	movesOnce sync.Once
	moves     moveIndex
}

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
	kinds := make([]string, len(all), len(all)+512)
	prevs := make([]string, len(all), len(all)+512)
	exists := make(map[string]bool, min(len(all), 65536))
	last := make(map[string]string, min(len(all), 65536))
	for i, so := range all {
		op := so.Op
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
	byTime := make([]int32, len(all))
	for i := range byTime {
		byTime[i] = int32(i)
	}
	sort.Slice(byTime, func(i, j int) bool {
		return histLess(all[byTime[i]].Op, all[byTime[j]].Op)
	})
	return &histIndex{all: all, kinds: kinds, prevs: prevs, byTime: byTime, exists: exists, last: last}
}

func (r *RemoteSource) tryIncrementalHistIndex(ctx context.Context, keep []remote.Object) (*histIndex, bool) {
	prev := histCache.peek(r)
	if prev == nil || len(prev.journals) == 0 || prev.exists == nil || prev.last == nil {
		return nil, false
	}
	tails, updatedMeta, grew, ok := r.verifyAndReadTails(ctx, keep, prev.journals, false)
	if !ok || !grew {
		return nil, false
	}
	var tailAll []sourcedOp
	for i, o := range keep {
		if len(tails[i]) == 0 {
			continue
		}
		from := strings.TrimSuffix(strings.TrimPrefix(o.Key, "journal/"), ".jsonl")
		for _, op := range tails[i] {
			tailAll = append(tailAll, sourcedOp{Op: op, From: from})
		}
	}
	if len(tailAll) == 0 {
		return nil, false
	}
	sort.SliceStable(tailAll, func(i, j int) bool { return journal.Less(tailAll[i].Op, tailAll[j].Op) })
	if len(prev.all) > 0 && !journal.Less(prev.all[len(prev.all)-1].Op, tailAll[0].Op) {
		return nil, false
	}

	// Take exclusive ownership of prev.exists and prev.last so no map clone is needed
	exists, last := prev.exists, prev.last
	prev.exists, prev.last = nil, nil

	baseLen := len(prev.all)
	totalLen := baseLen + len(tailAll)
	// Three-index slice on prev.all/kinds/prevs so appending allocates Only if cap is exceeded,
	// or if spare capacity exists, writes only into [baseLen:totalLen] beyond prev's length.
	all := make([]sourcedOp, totalLen)
	copy(all, prev.all[:baseLen])
	copy(all[baseLen:], tailAll)
	kinds := make([]string, len(tailAll))
	prevs := make([]string, len(tailAll))
	tailIdx := make([]int32, len(tailAll))
	for j, so := range tailAll {
		op := so.Op
		if op.Kind != journal.KindDelete || exists[op.Path] {
			prevs[j] = last[op.Path]
		}
		if op.Kind != journal.KindDelete {
			last[op.Path] = op.Blob
		}
		switch {
		case op.Kind == journal.KindDelete:
			kinds[j] = "delete"
			exists[op.Path] = false
		case exists[op.Path]:
			kinds[j] = "edit"
		default:
			kinds[j] = "add"
			exists[op.Path] = true
		}
		tailIdx[j] = int32(baseLen + j)
	}
	fullKinds := make([]string, totalLen)
	copy(fullKinds, prev.kinds[:baseLen])
	copy(fullKinds[baseLen:], kinds)
	fullPrevs := make([]string, totalLen)
	copy(fullPrevs, prev.prevs[:baseLen])
	copy(fullPrevs[baseLen:], prevs)

	sort.Slice(tailIdx, func(i, j int) bool {
		return histLess(all[tailIdx[i]].Op, all[tailIdx[j]].Op)
	})
	// Merge sorted tailIdx (length K) and sorted prev.byTime (length baseLen) in O(N + K)
	byTime := make([]int32, 0, totalLen)
	ti, pi := 0, 0
	for ti < len(tailIdx) && pi < len(prev.byTime) {
		if histLess(all[tailIdx[ti]].Op, all[prev.byTime[pi]].Op) {
			byTime = append(byTime, tailIdx[ti])
			ti++
		} else {
			byTime = append(byTime, prev.byTime[pi])
			pi++
		}
	}
	byTime = append(byTime, tailIdx[ti:]...)
	byTime = append(byTime, prev.byTime[pi:]...)

	return &histIndex{
		all: all, kinds: fullKinds, prevs: fullPrevs, byTime: byTime,
		journals: updatedMeta, exists: exists, last: last,
	}, true
}

type histFlightResult struct {
	fp string
	x  *histIndex
}

func (r *RemoteSource) historyIndex(ctx context.Context) (*histIndex, error) {
	keep, err := r.listJournals(ctx)
	if err != nil {
		return nil, err
	}
	fp := journalFingerprint(keep)
	for {
		if x := histCache.get(r, fp); x != nil {
			return x, nil
		}
		res, err, _ := r.flight.Do("hist", func() (any, error) {
			if cur := histCache.get(r, fp); cur != nil {
				return histFlightResult{fp: fp, x: cur}, nil
			}
			if inc, ok := r.tryIncrementalHistIndex(context.WithoutCancel(ctx), keep); ok {
				histCache.put(r, fp, inc)
				return histFlightResult{fp: fp, x: inc}, nil
			}
			all, err := r.sourcedOpsOf(context.WithoutCancel(ctx), keep)
			if err != nil {
				return nil, err
			}
			jmeta, _ := r.journalMetaFromCache(keep)
			if int64(len(all))*histOpBytes+hubCaches.heldJournals(r) > hubCaches.limitBytes() {
				r.jmu.Lock()
				jgen := r.jgen
				r.jmu.Unlock()
				r.dropJournals(jgen)
			}
			// Drop the previous histIndex before allocating the new one so two full
			// indexes for the same project never coexist in memory.
			histCache.clearSource(r)
			x := buildHistIndex(all)
			x.journals = jmeta
			histCache.put(r, fp, x)
			return histFlightResult{fp: fp, x: x}, nil
		})
		if err != nil {
			return nil, err
		}
		hr := res.(histFlightResult)
		if hr.fp == fp {
			return hr.x, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
}

const histOpBytes = 400

type histEntry struct {
	fp  string
	x   *histIndex
	gen uint64
}

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

func (c *histLRU) peek(src *RemoteSource) *histIndex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[src]; ok {
		return e.x
	}
	return nil
}

func (c *histLRU) clearSource(src *RemoteSource) {
	c.mu.Lock()
	delete(c.entries, src)
	c.mu.Unlock()
}

func (c *histLRU) put(src *RemoteSource, fp string, x *histIndex) {
	c.mu.Lock()
	gen, evict := hubCaches.charge(src, historyCache, int64(len(x.all))*histOpBytes)
	c.entries[src] = &histEntry{fp: fp, x: x, gen: gen}
	c.mu.Unlock()
	dropEvicted(evict)
}

func (c *histLRU) drop(src *RemoteSource, gen uint64) {
	c.mu.Lock()
	if e := c.entries[src]; e != nil && e.gen == gen {
		delete(c.entries, src)
	}
	c.mu.Unlock()
}
