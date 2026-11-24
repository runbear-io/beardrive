package webapp

import (
	"context"
	"encoding/hex"
	"slices"
	"sort"
	"sync"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

const (
	histKindAdd uint8 = iota
	histKindEdit
	histKindDelete
)

type histIndex struct {
	sources  []sourcedJournal // borrows jcache slices (+ any incremental tails)
	baseOffs []int            // starting index in jcache slice for each entry in sources
	order    []opRef          // sorted by journal.Less
	kinds    []uint8
	prevs    []string
	byTime   []int32 // indices into order, sorted by histLess
	journals map[string]ckptJournalMeta
	exists   map[string]bool
	last     map[string]string

	movesOnce sync.Once
	moves     moveIndex
}

func (x *histIndex) len() int { return len(x.order) }

func (x *histIndex) opAt(i int) (*journal.Op, string) {
	ref := x.order[i]
	sj := &x.sources[ref.j]
	return &sj.Ops[ref.i], sj.From
}

func (x *histIndex) kindAt(i int) string {
	switch x.kinds[i] {
	case histKindAdd:
		return "add"
	case histKindEdit:
		return "edit"
	case histKindDelete:
		return "delete"
	default:
		return ""
	}
}

func (x *histIndex) moveIndex() moveIndex {
	x.movesOnce.Do(func() {
		x.moves = buildMoveIndexFrom(len(x.order), func(i int) *journal.Op {
			ref := x.order[i]
			return &x.sources[ref.j].Ops[ref.i]
		})
	})
	return x.moves
}

func buildHistIndex(sources []sourcedJournal) *histIndex {
	n := 0
	for j := range sources {
		n += len(sources[j].Ops)
	}
	order := make([]opRef, 0, n)
	for j := range sources {
		ops := sources[j].Ops
		for i := range ops {
			order = append(order, opRef{j: int32(j), i: int32(i)})
		}
	}
	slices.SortStableFunc(order, func(a, b opRef) int {
		oa, ob := &sources[a.j].Ops[a.i], &sources[b.j].Ops[b.i]
		switch {
		case journal.Less(*oa, *ob):
			return -1
		case journal.Less(*ob, *oa):
			return 1
		}
		return 0
	})

	kinds := make([]uint8, len(order))
	prevs := make([]string, len(order))
	exists := make(map[string]bool, min(len(order), 65536))
	last := make(map[string]string, min(len(order), 65536))
	for i, ref := range order {
		op := &sources[ref.j].Ops[ref.i]
		if op.Kind != journal.KindDelete || exists[op.Path] {
			prevs[i] = last[op.Path]
		}
		if op.Kind != journal.KindDelete {
			last[op.Path] = op.Blob
		}
		switch {
		case op.Kind == journal.KindDelete:
			kinds[i] = histKindDelete
			exists[op.Path] = false
		case exists[op.Path]:
			kinds[i] = histKindEdit
		default:
			kinds[i] = histKindAdd
			exists[op.Path] = true
		}
	}
	byTime := make([]int32, len(order))
	for i := range byTime {
		byTime[i] = int32(i)
	}
	sort.Slice(byTime, func(i, j int) bool {
		ra, rb := order[byTime[i]], order[byTime[j]]
		return histLess(sources[ra.j].Ops[ra.i], sources[rb.j].Ops[rb.i])
	})
	return &histIndex{sources: sources, order: order, kinds: kinds, prevs: prevs, byTime: byTime, exists: exists, last: last}
}

func cMatchesMeta(c cachedJournal, m ckptJournalMeta) bool {
	if m.PrefixSHA256 == "" || c.size != m.Size || !c.mod.Equal(m.ModTime) || len(c.ops) != m.Ops {
		return false
	}
	return hex.EncodeToString(c.rawPrefixHash[:]) == m.PrefixSHA256
}

func (r *RemoteSource) tryIncrementalHistIndex(ctx context.Context, keep []remote.Object) (*histIndex, bool, uint64) {
	prev := histCache.peek(r)
	if prev == nil || len(prev.journals) == 0 || prev.exists == nil || prev.last == nil {
		return nil, false, 0
	}
	tails, updatedMeta, grew, ok := r.verifyAndReadTails(ctx, keep, prev.journals, false)
	if !ok || !grew {
		return nil, false, 0
	}
	baseSources := len(prev.sources)
	var tailSources []sourcedJournal
	var tailOffs []int
	var tailRefs []opRef
	for i, o := range keep {
		if len(tails[i]) == 0 {
			continue
		}
		jIdx := int32(baseSources + len(tailSources))
		tailOps := tails[i][:len(tails[i]):len(tails[i])]
		tailSources = append(tailSources, sourcedJournal{
			Key:  o.Key,
			From: journalDevice(o.Key),
			Ops:  tailOps,
		})
		tailOffs = append(tailOffs, prev.journals[o.Key].Ops)
		for k := range tailOps {
			tailRefs = append(tailRefs, opRef{j: jIdx, i: int32(k)})
		}
	}
	if len(tailRefs) == 0 {
		return nil, false, 0
	}
	slices.SortStableFunc(tailRefs, func(a, b opRef) int {
		oa := &tailSources[int(a.j)-baseSources].Ops[a.i]
		ob := &tailSources[int(b.j)-baseSources].Ops[b.i]
		switch {
		case journal.Less(*oa, *ob):
			return -1
		case journal.Less(*ob, *oa):
			return 1
		}
		return 0
	})
	baseLen := len(prev.order)
	if baseLen > 0 {
		lastPrev, _ := prev.opAt(baseLen - 1)
		firstTail := &tailSources[int(tailRefs[0].j)-baseSources].Ops[tailRefs[0].i]
		if !journal.Less(*lastPrev, *firstTail) {
			return nil, false, 0
		}
	}

	sources := make([]sourcedJournal, baseSources+len(tailSources))
	copy(sources, prev.sources)
	copy(sources[baseSources:], tailSources)
	baseOffs := make([]int, len(sources))
	copy(baseOffs, prev.baseOffs)
	copy(baseOffs[baseSources:], tailOffs)
	// Require an active jcache generation before taking ownership of prev's
	// maps, and re-anchor both prior sources and prior tails onto jcache's
	// latest slices when prefixes match so grown arrays are not pinned.
	r.jmu.Lock()
	jgen, hasAll := r.jgen, len(r.jcache) > 0
	if hasAll {
		for k := range prev.journals {
			if _, ok := r.jcache[k]; !ok {
				hasAll = false
				break
			}
		}
	}
	if !hasAll || jgen == 0 {
		r.jmu.Unlock()
		return nil, false, 0
	}
	for j := range sources {
		key := sources[j].Key
		off := baseOffs[j]
		oldLen := len(sources[j].Ops)
		matched := false
		if key != "" {
			if c, has := r.jcache[key]; has && off+oldLen <= len(c.ops) &&
				(cMatchesMeta(c, updatedMeta[key]) || cMatchesMeta(c, prev.journals[key])) {
				sources[j].Ops = c.ops[off : off+oldLen : off+oldLen]
				matched = true
			}
		}
		if !matched && j < baseSources && oldLen > 0 {
			r.jmu.Unlock()
			return nil, false, 0
		}
	}
	r.jmu.Unlock()

	// Take exclusive ownership of prev.exists and prev.last only after all
	// fallible guards succeed so a fallback keeps prev reusable.
	exists, last := prev.exists, prev.last
	prev.exists, prev.last = nil, nil

	totalLen := baseLen + len(tailRefs)
	order := make([]opRef, totalLen)
	copy(order, prev.order)
	copy(order[baseLen:], tailRefs)

	kinds := make([]uint8, totalLen)
	copy(kinds, prev.kinds)
	prevs := make([]string, totalLen)
	copy(prevs, prev.prevs)
	tailIdx := make([]int32, len(tailRefs))
	for j, ref := range tailRefs {
		op := &sources[ref.j].Ops[ref.i]
		idx := baseLen + j
		if op.Kind != journal.KindDelete || exists[op.Path] {
			prevs[idx] = last[op.Path]
		}
		if op.Kind != journal.KindDelete {
			last[op.Path] = op.Blob
		}
		switch {
		case op.Kind == journal.KindDelete:
			kinds[idx] = histKindDelete
			exists[op.Path] = false
		case exists[op.Path]:
			kinds[idx] = histKindEdit
		default:
			kinds[idx] = histKindAdd
			exists[op.Path] = true
		}
		tailIdx[j] = int32(idx)
	}

	sort.Slice(tailIdx, func(i, j int) bool {
		ra, rb := order[tailIdx[i]], order[tailIdx[j]]
		return histLess(sources[ra.j].Ops[ra.i], sources[rb.j].Ops[rb.i])
	})
	// Merge sorted tailIdx (length K) and sorted prev.byTime (length baseLen) in O(N + K).
	byTime := make([]int32, 0, totalLen)
	ti, pi := 0, 0
	for ti < len(tailIdx) && pi < len(prev.byTime) {
		ra, rb := order[tailIdx[ti]], order[prev.byTime[pi]]
		if histLess(sources[ra.j].Ops[ra.i], sources[rb.j].Ops[rb.i]) {
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
		sources: sources, baseOffs: baseOffs, order: order, kinds: kinds, prevs: prevs, byTime: byTime,
		journals: updatedMeta, exists: exists, last: last,
	}, true, jgen
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
			if inc, ok, jgen := r.tryIncrementalHistIndex(context.WithoutCancel(ctx), keep); ok {
				histCache.putIfJournal(r, fp, inc, jgen, true)
				return histFlightResult{fp: fp, x: inc}, nil
			}
			sources, jgen, allCached, err := r.sourcedJournalsOfGen(context.WithoutCancel(ctx), keep)
			if err != nil {
				return nil, err
			}
			jmeta, metaOK := r.journalMetaFromCache(keep)
			if !metaOK {
				allCached = false
			}
			// Drop the previous histIndex before allocating the new one so two full
			// indexes for the same project never coexist in memory.
			histCache.clearSource(r)
			x := buildHistIndex(sources)
			x.journals = jmeta
			histCache.putIfJournal(r, fp, x, jgen, allCached)
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

const histOpBytes = 64

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
	hubCaches.touch(src, historyCache, journalCache)
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
	hubCaches.uncharge(src, historyCache)
	c.mu.Unlock()
}

func (c *histLRU) put(src *RemoteSource, fp string, x *histIndex) {
	src.jmu.Lock()
	jgen, hasJ := src.jgen, len(src.jcache) > 0
	src.jmu.Unlock()
	c.putIfJournal(src, fp, x, jgen, hasJ)
}

func (c *histLRU) putIfJournal(src *RemoteSource, fp string, x *histIndex, expectedJGen uint64, hasJournal bool) {
	bytes := int64(len(x.order)) * histOpBytes
	c.mu.Lock()
	var gen uint64
	var evict []evicted
	if bytes == 0 {
		gen, evict = hubCaches.charge(src, historyCache, 0)
	} else {
		if !hasJournal {
			c.mu.Unlock()
			return
		}
		var ok bool
		gen, evict, ok = hubCaches.chargeHistoryIfJournal(src, bytes, expectedJGen)
		if !ok {
			c.mu.Unlock()
			return
		}
	}
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
