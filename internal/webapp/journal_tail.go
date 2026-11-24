package webapp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

const (
	journalAppendChainMetaKey   = "bdrive-append-chain"
	maxJournalVersionCacheKeys  = 1024
	maxJournalVersionsPerKey    = 4
	maxJournalVersionPrefixes   = 64
	maxMetaChainPrefixes        = 14
	maxMetaChainBytes           = 1400
)

var castagnoliTable = crc32.MakeTable(crc32.Castagnoli)

func crc32cSum(data []byte) uint32 {
	return crc32.Checksum(data, castagnoliTable)
}

type journalPrefixMark struct {
	sha          string
	lineBoundary bool
}

// journalVersionEntry remembers verified metadata for one storage version of a
// journal on a RemoteSource. Keying a tiny ring of recent versions per journal
// prevents a lagging reader from clobbering a newer append entry while keeping
// memory strictly bounded.
type journalVersionEntry struct {
	version    remote.Version
	size       int64
	sha256     string
	crc32c     uint32
	storedMax  int64
	endsWithNL bool
	hasAppend  bool
	isWrite    bool
	prefixes   map[int64]journalPrefixMark
}

type journalVersionRing struct {
	entries []journalVersionEntry
}

type knownLineBoundary uint8

const (
	nlUnknown knownLineBoundary = iota
	nlFalse
	nlTrue
)

type journalChain struct {
	headSize int64
	headSHA  string
	headNL   bool
	crc32c   uint32
	prefixes map[int64]journalPrefixMark
}

// parseJournalAppendChain strictly parses the bdrive-append-chain object
// metadata. Any malformed field, size/CRC mismatch, duplicate/unsorted offset,
// or invalid SHA fails closed and returns false.
func parseJournalAppendChain(meta remote.ObjectMeta) (journalChain, bool) {
	if meta.Size <= 0 || len(meta.Metadata) == 0 {
		return journalChain{}, false
	}
	var raw string
	found := false
	for k, v := range meta.Metadata {
		if strings.EqualFold(k, journalAppendChainMetaKey) {
			raw = v
			found = true
			break
		}
	}
	if !found || len(raw) > maxMetaChainBytes {
		return journalChain{}, false
	}
	rest, ok := strings.CutPrefix(raw, "v1;")
	if !ok {
		return journalChain{}, false
	}
	parts := strings.Split(rest, ";")
	if len(parts) < 1 || len(parts) > 2 {
		return journalChain{}, false
	}
	headSpec, ok := strings.CutPrefix(parts[0], "h=")
	if !ok {
		return journalChain{}, false
	}
	hp := strings.Split(headSpec, ":")
	if len(hp) != 4 {
		return journalChain{}, false
	}
	headSize, err := strconv.ParseInt(hp[0], 10, 64)
	if err != nil || headSize <= 0 || headSize != meta.Size {
		return journalChain{}, false
	}
	headSHA := hp[1]
	if !isBlobSHA(headSHA) {
		return journalChain{}, false
	}
	var headNL bool
	switch hp[2] {
	case "1":
		headNL = true
	case "0":
		headNL = false
	default:
		return journalChain{}, false
	}
	if len(hp[3]) != 8 {
		return journalChain{}, false
	}
	for i := 0; i < len(hp[3]); i++ {
		c := hp[3][i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return journalChain{}, false
		}
	}
	crcVal, err := strconv.ParseUint(hp[3], 16, 32)
	if err != nil {
		return journalChain{}, false
	}
	if meta.CRC32C != 0 && uint32(crcVal) != meta.CRC32C {
		return journalChain{}, false
	}
	prefixes := map[int64]journalPrefixMark{
		headSize: {sha: headSHA, lineBoundary: headNL},
	}
	if len(parts) == 2 {
		pSpec, ok := strings.CutPrefix(parts[1], "p=")
		if !ok || pSpec == "" {
			return journalChain{}, false
		}
		var prevOff int64
		for _, item := range strings.Split(pSpec, ",") {
			fields := strings.Split(item, ":")
			if len(fields) != 3 {
				return journalChain{}, false
			}
			off, err := strconv.ParseInt(fields[0], 10, 64)
			if err != nil || off <= 0 || off >= headSize || off <= prevOff {
				return journalChain{}, false
			}
			if !isBlobSHA(fields[1]) {
				return journalChain{}, false
			}
			var nl bool
			switch fields[2] {
			case "1":
				nl = true
			case "0":
				nl = false
			default:
				return journalChain{}, false
			}
			prefixes[off] = journalPrefixMark{sha: fields[1], lineBoundary: nl}
			prevOff = off
		}
	}
	return journalChain{
		headSize: headSize,
		headSHA:  headSHA,
		headNL:   headNL,
		crc32c:   uint32(crcVal),
		prefixes: prefixes,
	}, true
}

type prefixOffsetEntry struct {
	offset int64
	mark   journalPrefixMark
}

func formatJournalAppendChain(headSize int64, headSHA string, headNL bool, crc32c uint32, prefixes map[int64]journalPrefixMark) string {
	if headSize <= 0 || !isBlobSHA(headSHA) {
		return ""
	}
	nlBit := "0"
	if headNL {
		nlBit = "1"
	}
	headStr := fmt.Sprintf("v1;h=%d:%s:%s:%08x", headSize, headSHA, nlBit, crc32c)
	if len(headStr) > maxMetaChainBytes {
		return ""
	}
	var list []prefixOffsetEntry
	for off, mark := range prefixes {
		if off > 0 && off < headSize && isBlobSHA(mark.sha) {
			list = append(list, prefixOffsetEntry{offset: off, mark: mark})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].offset < list[j].offset })
	for len(list) > maxMetaChainPrefixes {
		if len(list) > 1 && headSize-list[0].offset <= 2*minCheckpointDeltaBytes {
			// Keep the checkpoint-era anchor offset while shedding intermediate hops.
			list = append(list[:1], list[2:]...)
		} else {
			list = list[1:]
		}
	}
	for len(list) > 0 {
		var b strings.Builder
		b.WriteString(headStr)
		b.WriteString(";p=")
		for i, item := range list {
			if i > 0 {
				b.WriteByte(',')
			}
			bit := "0"
			if item.mark.lineBoundary {
				bit = "1"
			}
			fmt.Fprintf(&b, "%d:%s:%s", item.offset, item.mark.sha, bit)
		}
		if b.Len() <= maxMetaChainBytes {
			return b.String()
		}
		if len(list) > 1 && headSize-list[0].offset <= 2*minCheckpointDeltaBytes {
			list = append(list[:1], list[2:]...)
		} else {
			list = list[1:]
		}
	}
	return headStr
}

func prunePrefixMap(in map[int64]journalPrefixMark, keepA, keepB int64, maxEntries int) map[int64]journalPrefixMark {
	if len(in) == 0 {
		return nil
	}
	out := make(map[int64]journalPrefixMark, min(len(in), maxEntries))
	for k, v := range in {
		if k > 0 && isBlobSHA(v.sha) {
			out[k] = v
		}
	}
	if len(out) <= maxEntries {
		return out
	}
	keys := make([]int64, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	preserveAnchor := len(keys) > 0 && keepB > 0 && keepB-keys[0] <= 2*minCheckpointDeltaBytes
	for i, k := range keys {
		if len(out) <= maxEntries {
			break
		}
		if k == keepA || k == keepB || (i == 0 && preserveAnchor) {
			continue
		}
		delete(out, k)
	}
	return out
}

func (r *RemoteSource) ensureJournalRingLocked(key string) *journalVersionRing {
	if r.jverCache == nil {
		r.jverCache = make(map[string]*journalVersionRing)
	}
	ring, exists := r.jverCache[key]
	if !exists {
		if len(r.jverOrder) >= maxJournalVersionCacheKeys {
			oldest := r.jverOrder[0]
			r.jverOrder = r.jverOrder[1:]
			delete(r.jverCache, oldest)
		}
		r.jverOrder = append(r.jverOrder, key)
		ring = &journalVersionRing{}
		r.jverCache[key] = ring
	}
	return ring
}

func (ring *journalVersionRing) find(ver remote.Version) (int, bool) {
	for i := len(ring.entries) - 1; i >= 0; i-- {
		if ring.entries[i].version == ver {
			return i, true
		}
	}
	return -1, false
}

func (r *RemoteSource) loadJournalAppendMeta(key string, ver remote.Version) (journalVersionEntry, bool) {
	if ver == remote.VersionAbsent {
		return journalVersionEntry{}, false
	}
	r.jverMu.Lock()
	defer r.jverMu.Unlock()
	ring, ok := r.jverCache[key]
	if !ok {
		return journalVersionEntry{}, false
	}
	idx, ok := ring.find(ver)
	if !ok || !ring.entries[idx].hasAppend {
		return journalVersionEntry{}, false
	}
	return ring.entries[idx], true
}

func (r *RemoteSource) findJournalVersionEntryLocked(key string, ver remote.Version) (journalVersionEntry, bool) {
	ring, ok := r.jverCache[key]
	if !ok {
		return journalVersionEntry{}, false
	}
	if ver != remote.VersionAbsent {
		if idx, found := ring.find(ver); found {
			return ring.entries[idx], true
		}
	}
	if len(ring.entries) > 0 {
		return ring.entries[len(ring.entries)-1], true
	}
	return journalVersionEntry{}, false
}

// prepareJournalWriteMeta computes the self-verified prefix map and encoded
// atomic object metadata before writing a new journal revision. When chain is
// false (e.g. a non-prefix rewrite), older offsets are discarded.
func (r *RemoteSource) prepareJournalWriteMeta(
	key string,
	prevMeta remote.ObjectMeta,
	prevSize int64,
	prevSHA string,
	prevLineBoundary bool,
	newSize int64,
	newSHA string,
	newLineBoundary bool,
	newCRC32C uint32,
	chain bool,
) (map[int64]journalPrefixMark, map[string]string) {
	newSHA = strings.ToLower(newSHA)
	prevSHA = strings.ToLower(prevSHA)
	if newSize <= 0 || !isBlobSHA(newSHA) {
		return nil, nil
	}
	merged := map[int64]journalPrefixMark{
		newSize: {sha: newSHA, lineBoundary: newLineBoundary},
	}
	if chain && prevSize > 0 && isBlobSHA(prevSHA) {
		// Self-verify against in-memory cache first, then atomic object metadata.
		r.jverMu.Lock()
		if prevEntry, ok := r.findJournalVersionEntryLocked(key, prevMeta.Version); ok {
			matchCache := (prevEntry.size == prevSize && prevEntry.sha256 == prevSHA)
			if !matchCache {
				if m, ok2 := prevEntry.prefixes[prevSize]; ok2 && m.sha == prevSHA {
					matchCache = true
				}
			}
			if matchCache {
				for off, m := range prevEntry.prefixes {
					if off > 0 && off < prevSize && isBlobSHA(m.sha) {
						merged[off] = m
					}
				}
			}
		}
		r.jverMu.Unlock()
		if parsed, ok := parseJournalAppendChain(prevMeta); ok {
			matchMeta := (parsed.headSize == prevSize && parsed.headSHA == prevSHA)
			if !matchMeta {
				if m, ok2 := parsed.prefixes[prevSize]; ok2 && m.sha == prevSHA {
					matchMeta = true
				}
			}
			if matchMeta {
				for off, m := range parsed.prefixes {
					if off > 0 && off < prevSize && isBlobSHA(m.sha) {
						if existing, had := merged[off]; had && existing.lineBoundary {
							continue
						}
						merged[off] = m
					}
				}
			}
		}
		merged[prevSize] = journalPrefixMark{sha: prevSHA, lineBoundary: prevLineBoundary}
	}
	merged = prunePrefixMap(merged, prevSize, newSize, maxJournalVersionPrefixes)
	encoded := formatJournalAppendChain(newSize, newSHA, newLineBoundary, newCRC32C, merged)
	if encoded == "" {
		return merged, nil
	}
	return merged, map[string]string{journalAppendChainMetaKey: encoded}
}

func insertReaderEntryLocked(ring *journalVersionRing, entry journalVersionEntry) {
	if len(ring.entries) < maxJournalVersionsPerKey {
		// Place lagging reader entries before newer write entries.
		ring.entries = append([]journalVersionEntry{entry}, ring.entries...)
		return
	}
	// Replace an existing non-write slot if available; never evict newer write entries.
	for i := 0; i < len(ring.entries)-1; i++ {
		if !ring.entries[i].isWrite {
			ring.entries = append(ring.entries[:i], ring.entries[i+1:]...)
			ring.entries = append([]journalVersionEntry{entry}, ring.entries...)
			return
		}
	}
}

func (r *RemoteSource) storeJournalWriteMeta(
	key string,
	ver remote.Version,
	size int64,
	sha256Hex string,
	crc32c uint32,
	storedMax int64,
	endsWithNL bool,
	hasAppend bool,
	prevSize int64,
	prevSHA string,
	prevLineBoundary bool,
	prefixes map[int64]journalPrefixMark,
) {
	r.storeJournalMeta(key, ver, size, sha256Hex, crc32c, storedMax, endsWithNL, hasAppend, true, prevSize, prevSHA, prevLineBoundary, prefixes)
}

func (r *RemoteSource) storeJournalMeta(
	key string,
	ver remote.Version,
	size int64,
	sha256Hex string,
	crc32c uint32,
	storedMax int64,
	endsWithNL bool,
	hasAppend bool,
	isWrite bool,
	prevSize int64,
	prevSHA string,
	prevLineBoundary bool,
	prefixes map[int64]journalPrefixMark,
) {
	if ver == remote.VersionAbsent || size <= 0 {
		return
	}
	sha256Hex = strings.ToLower(sha256Hex)
	prevSHA = strings.ToLower(prevSHA)
	if !isBlobSHA(sha256Hex) {
		return
	}
	copied := make(map[int64]journalPrefixMark, len(prefixes)+2)
	for off, m := range prefixes {
		if off > 0 && off <= size && isBlobSHA(m.sha) {
			copied[off] = m
		}
	}
	if prevSize > 0 && isBlobSHA(prevSHA) {
		copied[prevSize] = journalPrefixMark{sha: prevSHA, lineBoundary: prevLineBoundary}
	}
	copied[size] = journalPrefixMark{sha: sha256Hex, lineBoundary: endsWithNL}
	copied = prunePrefixMap(copied, prevSize, size, maxJournalVersionPrefixes)

	r.jverMu.Lock()
	defer r.jverMu.Unlock()
	ring := r.ensureJournalRingLocked(key)
	entry := journalVersionEntry{
		version:    ver,
		size:       size,
		sha256:     sha256Hex,
		crc32c:     crc32c,
		storedMax:  storedMax,
		endsWithNL: endsWithNL,
		hasAppend:  hasAppend,
		isWrite:    isWrite,
		prefixes:   copied,
	}
	if idx, found := ring.find(ver); found {
		existing := ring.entries[idx]
		mergedPrefixes := make(map[int64]journalPrefixMark, len(existing.prefixes)+len(copied))
		for off, m := range existing.prefixes {
			if off > 0 && off <= size && isBlobSHA(m.sha) {
				mergedPrefixes[off] = m
			}
		}
		for off, m := range copied {
			if prev, had := mergedPrefixes[off]; had && prev.sha == m.sha && prev.lineBoundary {
				m.lineBoundary = true
			}
			mergedPrefixes[off] = m
		}
		entry.prefixes = prunePrefixMap(mergedPrefixes, prevSize, size, maxJournalVersionPrefixes)
		if !hasAppend && existing.hasAppend && existing.size == size && existing.sha256 == sha256Hex {
			entry.hasAppend = true
			entry.storedMax = existing.storedMax
		}
		if !isWrite {
			entry.isWrite = existing.isWrite
			if existing.crc32c != 0 && entry.crc32c == 0 {
				entry.crc32c = existing.crc32c
			}
			ring.entries[idx] = entry
			return
		}
		ring.entries[idx] = entry
		if idx != len(ring.entries)-1 {
			ring.entries = append(ring.entries[:idx], ring.entries[idx+1:]...)
			ring.entries = append(ring.entries, entry)
		}
		return
	}
	if !isWrite {
		insertReaderEntryLocked(ring, entry)
		return
	}
	if len(ring.entries) >= maxJournalVersionsPerKey {
		ring.entries = append(ring.entries[1:], entry)
	} else {
		ring.entries = append(ring.entries, entry)
	}
}

func (r *RemoteSource) storeJournalAppendMeta(key string, ver remote.Version, size int64, sha256Hex string, storedMax int64, endsWithNL bool, prevSize int64, prevSHA string) {
	r.storeJournalWriteMeta(key, ver, size, sha256Hex, 0, storedMax, endsWithNL, true, prevSize, prevSHA, endsWithNL, nil)
}

// storeJournalPrefixMark records a verified prefix offset observed by a read
// path without displacing newer write entries in the per-key version ring.
func (r *RemoteSource) storeJournalPrefixMark(key string, ver remote.Version, offset int64, prefixSHA string, lineBoundary bool) {
	if ver == remote.VersionAbsent || offset <= 0 {
		return
	}
	prefixSHA = strings.ToLower(prefixSHA)
	if !isBlobSHA(prefixSHA) {
		return
	}
	r.jverMu.Lock()
	defer r.jverMu.Unlock()
	ring := r.ensureJournalRingLocked(key)
	if idx, found := ring.find(ver); found {
		e := ring.entries[idx]
		if e.prefixes == nil {
			e.prefixes = make(map[int64]journalPrefixMark, 2)
		}
		if prev, ok := e.prefixes[offset]; ok && prev.sha == prefixSHA && prev.lineBoundary {
			lineBoundary = true
		}
		e.prefixes[offset] = journalPrefixMark{sha: prefixSHA, lineBoundary: lineBoundary}
		e.prefixes = prunePrefixMap(e.prefixes, offset, e.size, maxJournalVersionPrefixes)
		ring.entries[idx] = e
		return
	}
	insertReaderEntryLocked(ring, journalVersionEntry{
		version:  ver,
		prefixes: map[int64]journalPrefixMark{offset: {sha: prefixSHA, lineBoundary: lineBoundary}},
	})
}

func (r *RemoteSource) storeJournalPrefixSHA(key string, ver remote.Version, offset int64, prefixSHA string) {
	r.storeJournalPrefixMark(key, ver, offset, prefixSHA, false)
}

func (r *RemoteSource) loadJournalPrefixMark(key string, ver remote.Version, offset int64) (journalPrefixMark, bool) {
	if ver == remote.VersionAbsent || offset <= 0 {
		return journalPrefixMark{}, false
	}
	r.jverMu.Lock()
	defer r.jverMu.Unlock()
	ring, ok := r.jverCache[key]
	if !ok {
		return journalPrefixMark{}, false
	}
	idx, found := ring.find(ver)
	if !found || ring.entries[idx].prefixes == nil {
		return journalPrefixMark{}, false
	}
	m, ok := ring.entries[idx].prefixes[offset]
	return m, ok
}

func (r *RemoteSource) loadJournalPrefixSHA(key string, ver remote.Version, offset int64) (string, bool) {
	m, ok := r.loadJournalPrefixMark(key, ver, offset)
	return m.sha, ok
}

func (r *RemoteSource) lookupAppendedJournalSHA(key string, ver remote.Version, prevSize int64, prevSHA string, newSize int64) (string, bool) {
	fullSHA, _, ok := r.lookupVerifiedTail(key, remote.ObjectMeta{Version: ver, Size: newSize}, prevSize, prevSHA, false)
	return fullSHA, ok
}

// hasJournalPrefixCandidate reports whether calling GetVersionedRange on a
// non-metadata backend has a plausible cached prefix entry, preventing cold
// localBackend calls from hashing the entire file under flock only to miss.
func (r *RemoteSource) hasJournalPrefixCandidate(key string, offset int64, prefixSHA string, listVer remote.Version) bool {
	if offset < 0 {
		return false
	}
	prefixSHA = strings.ToLower(prefixSHA)
	if offset > 0 && !isBlobSHA(prefixSHA) {
		return false
	}
	r.jverMu.Lock()
	defer r.jverMu.Unlock()
	ring, ok := r.jverCache[key]
	if !ok {
		return false
	}
	if listVer != remote.VersionAbsent {
		idx, found := ring.find(listVer)
		if !found {
			return false
		}
		e := ring.entries[idx]
		if offset == 0 {
			return true
		}
		m, ok := e.prefixes[offset]
		return ok && m.sha == prefixSHA
	}
	for i := len(ring.entries) - 1; i >= 0; i-- {
		e := ring.entries[i]
		if offset == 0 {
			return true
		}
		if m, ok := e.prefixes[offset]; ok && m.sha == prefixSHA {
			return true
		}
	}
	return false
}

// lookupVerifiedTail verifies that storage revision meta extends prevSize (with
// SHA-256 prevSHA) up to meta.Size using either jverCache or atomic object
// metadata. On a metadata hit it also populates jverCache for subsequent hits.
func (r *RemoteSource) lookupVerifiedTail(
	key string,
	meta remote.ObjectMeta,
	prevSize int64,
	prevSHA string,
	requireLineBoundary bool,
) (fullSHA string, prevNL bool, ok bool) {
	prevSHA = strings.ToLower(prevSHA)
	if meta.Version == remote.VersionAbsent || prevSize <= 0 || !isBlobSHA(prevSHA) {
		return "", false, false
	}
	r.jverMu.Lock()
	if ring, exists := r.jverCache[key]; exists {
		if idx, found := ring.find(meta.Version); found {
			e := ring.entries[idx]
			if e.size == meta.Size && e.sha256 != "" {
				if m, hasPrev := e.prefixes[prevSize]; hasPrev && m.sha == prevSHA && (!requireLineBoundary || m.lineBoundary) {
					r.jverMu.Unlock()
					return e.sha256, m.lineBoundary, true
				}
			}
		}
	}
	r.jverMu.Unlock()

	chain, parsed := parseJournalAppendChain(meta)
	if !parsed || chain.headSize != meta.Size {
		return "", false, false
	}
	m, hasPrev := chain.prefixes[prevSize]
	if !hasPrev || m.sha != prevSHA {
		return "", false, false
	}
	if requireLineBoundary && !m.lineBoundary {
		return "", false, false
	}
	// Cache the verified chain for this version so subsequent reads skip parsing,
	// without evicting or promoting over newer write entries.
	r.storeJournalMeta(key, meta.Version, chain.headSize, chain.headSHA, chain.crc32c, 0, chain.headNL, false, false, prevSize, prevSHA, m.lineBoundary, chain.prefixes)
	return chain.headSHA, m.lineBoundary, true
}

// lookupVerifiedPrefixForGet verifies an arbitrary ?offset=&prefix_sha256= query
// against jverCache or atomic object metadata.
func (r *RemoteSource) lookupVerifiedPrefixForGet(key string, meta remote.ObjectMeta, offset int64, prefixSHA string) bool {
	prefixSHA = strings.ToLower(prefixSHA)
	if meta.Version == remote.VersionAbsent || offset <= 0 || !isBlobSHA(prefixSHA) {
		return false
	}
	if m, hit := r.loadJournalPrefixMark(key, meta.Version, offset); hit && m.sha == prefixSHA {
		return true
	}
	chain, parsed := parseJournalAppendChain(meta)
	if !parsed {
		return false
	}
	m, ok := chain.prefixes[offset]
	if !ok || m.sha != prefixSHA {
		return false
	}
	r.storeJournalMeta(key, meta.Version, chain.headSize, chain.headSHA, chain.crc32c, 0, chain.headNL, false, false, 0, "", false, chain.prefixes)
	return true
}

func cloneSHA256State(state []byte) (hash.Hash, error) {
	if len(state) == 0 {
		return nil, nil
	}
	h := sha256.New()
	u, ok := h.(encoding.BinaryUnmarshaler)
	if !ok {
		return nil, fmt.Errorf("sha256 does not support BinaryUnmarshaler")
	}
	if err := u.UnmarshalBinary(state); err != nil {
		return nil, err
	}
	return h, nil
}

func marshalSHA256State(h hash.Hash) []byte {
	m, ok := h.(encoding.BinaryMarshaler)
	if !ok {
		return nil
	}
	b, err := m.MarshalBinary()
	if err != nil {
		return nil
	}
	return b
}

type byteCounterAndLast struct {
	n        int64
	lastByte byte
	h        hash.Hash
}

func (b *byteCounterAndLast) Write(p []byte) (int, error) {
	if len(p) > 0 {
		b.n += int64(len(p))
		b.lastByte = p[len(p)-1]
		if b.h != nil {
			_, _ = b.h.Write(p)
		}
	}
	return len(p), nil
}

type verifiedTailResult struct {
	ops           []journal.Op
	fullSHA       string
	fullHash      [32]byte
	hashState     []byte
	endsInNewline bool
}

// readVerifiedJournalTail fetches only the appended tail [prevSize, o.Size)
// when storage proves the prefix [0, prevSize) matches prevSHA and ends with a
// newline. When prevBoundary == nlUnknown and jverCache does not already prove
// the newline, it reads 1 extra byte at prevSize-1 directly from the pinned
// storage object to confirm the newline boundary without reading the prefix.
func (r *RemoteSource) readVerifiedJournalTail(
	ctx context.Context,
	o remote.Object,
	prevSize int64,
	prevSHA string,
	prevHashState []byte,
	prevBoundary knownLineBoundary,
) (verifiedTailResult, bool) {
	prevSHA = strings.ToLower(prevSHA)
	if prevBoundary == nlFalse || prevSize <= 0 || o.Size <= prevSize || !isBlobSHA(prevSHA) {
		return verifiedTailResult{}, false
	}
	metaBe, isMeta := r.Backend.(remote.VersionedMetaBackend)
	var vrg remote.VersionedRangeGetter
	if !isMeta {
		var ok bool
		vrg, ok = r.Backend.(remote.VersionedRangeGetter)
		if !ok {
			return verifiedTailResult{}, false
		}
		if !r.hasJournalPrefixCandidate(o.Key, prevSize, prevSHA, o.Version) {
			return verifiedTailResult{}, false
		}
	}

	readOffset := prevSize
	needNLProofByte := false
	if prevBoundary == nlUnknown {
		knownNL := false
		if o.Version != remote.VersionAbsent {
			if m, hit := r.loadJournalPrefixMark(o.Key, o.Version, prevSize); hit && m.sha == prevSHA && m.lineBoundary {
				knownNL = true
			}
		}
		if !knownNL {
			readOffset = prevSize - 1
			needNLProofByte = true
		}
	}

	wantTail := o.Size - prevSize
	var (
		rc   io.ReadCloser
		meta remote.ObjectMeta
		err  error
	)
	if isMeta {
		rc, meta, err = metaBe.GetVersionedRangeWithMeta(ctx, o.Key, readOffset, o.Size-readOffset)
		if err != nil {
			return verifiedTailResult{}, false
		}
		if meta.Size != o.Size {
			_ = rc.Close()
			return verifiedTailResult{}, false
		}
	} else {
		var ver remote.Version
		rc, ver, err = vrg.GetVersionedRange(ctx, o.Key, readOffset, o.Size-readOffset)
		if err != nil {
			return verifiedTailResult{}, false
		}
		meta = remote.ObjectMeta{Version: ver, Size: o.Size}
	}
	defer rc.Close()

	if o.Version != remote.VersionAbsent && meta.Version != o.Version {
		return verifiedTailResult{}, false
	}

	requireCacheNL := !needNLProofByte
	fullSHA, _, ok := r.lookupVerifiedTail(o.Key, meta, prevSize, prevSHA, requireCacheNL)
	if !ok {
		return verifiedTailResult{}, false
	}

	if needNLProofByte {
		var nl [1]byte
		if _, err := io.ReadFull(rc, nl[:]); err != nil || nl[0] != '\n' {
			return verifiedTailResult{}, false
		}
	}

	var clonedHash hash.Hash
	if len(prevHashState) > 0 {
		clonedHash, err = cloneSHA256State(prevHashState)
		if err != nil {
			return verifiedTailResult{}, false
		}
	}

	meter := &byteCounterAndLast{h: clonedHash}
	ops, perr := journal.ParseReaderSized(io.TeeReader(io.LimitReader(rc, wantTail+1), meter), wantTail)
	if perr != nil || meter.n != wantTail {
		return verifiedTailResult{}, false
	}

	res := verifiedTailResult{
		ops:           ops,
		fullSHA:       fullSHA,
		endsInNewline: meter.lastByte == '\n',
	}
	if clonedHash != nil {
		sum := clonedHash.Sum(nil)
		copy(res.fullHash[:], sum)
		if hex.EncodeToString(sum) != fullSHA {
			return verifiedTailResult{}, false
		}
		res.hashState = marshalSHA256State(clonedHash)
	}
	return res, true
}

// readVerifiedJournalTailRaw streams only the raw appended tail bytes for
// filteredJournal when the full journal prefix is already cached in fcache.
func (r *RemoteSource) readVerifiedJournalTailRaw(
	ctx context.Context,
	o remote.Object,
	prevSize int64,
	prevSHA string,
	prevEndsNL bool,
	prevHashState []byte,
) ([]byte, [32]byte, []byte, bool, bool) {
	if !prevEndsNL || prevSize <= 0 || o.Size <= prevSize {
		return nil, [32]byte{}, nil, false, false
	}
	prevSHA = strings.ToLower(prevSHA)
	if !isBlobSHA(prevSHA) {
		return nil, [32]byte{}, nil, false, false
	}
	var clonedHash hash.Hash
	if len(prevHashState) > 0 {
		var herr error
		clonedHash, herr = cloneSHA256State(prevHashState)
		if herr != nil {
			return nil, [32]byte{}, nil, false, false
		}
	}
	metaBe, isMeta := r.Backend.(remote.VersionedMetaBackend)
	var vrg remote.VersionedRangeGetter
	if !isMeta {
		var ok bool
		vrg, ok = r.Backend.(remote.VersionedRangeGetter)
		if !ok || !r.hasJournalPrefixCandidate(o.Key, prevSize, prevSHA, o.Version) {
			return nil, [32]byte{}, nil, false, false
		}
	}
	wantTail := o.Size - prevSize
	var (
		rc   io.ReadCloser
		meta remote.ObjectMeta
		err  error
	)
	if isMeta {
		rc, meta, err = metaBe.GetVersionedRangeWithMeta(ctx, o.Key, prevSize, wantTail)
		if err != nil {
			return nil, [32]byte{}, nil, false, false
		}
		if meta.Size != o.Size {
			_ = rc.Close()
			return nil, [32]byte{}, nil, false, false
		}
	} else {
		var ver remote.Version
		rc, ver, err = vrg.GetVersionedRange(ctx, o.Key, prevSize, wantTail)
		if err != nil {
			return nil, [32]byte{}, nil, false, false
		}
		meta = remote.ObjectMeta{Version: ver, Size: o.Size}
	}
	defer rc.Close()
	if o.Version != remote.VersionAbsent && meta.Version != o.Version {
		return nil, [32]byte{}, nil, false, false
	}
	fullSHA, _, ok := r.lookupVerifiedTail(o.Key, meta, prevSize, prevSHA, true)
	if !ok {
		return nil, [32]byte{}, nil, false, false
	}
	tailBytes, err := io.ReadAll(io.LimitReader(rc, wantTail+1))
	if err != nil || int64(len(tailBytes)) != wantTail {
		return nil, [32]byte{}, nil, false, false
	}
	var fullArr [32]byte
	var newState []byte
	if clonedHash != nil {
		_, _ = clonedHash.Write(tailBytes)
		sum := clonedHash.Sum(nil)
		if hex.EncodeToString(sum) != fullSHA {
			return nil, [32]byte{}, nil, false, false
		}
		copy(fullArr[:], sum)
		newState = marshalSHA256State(clonedHash)
	} else {
		rawFull, err := hex.DecodeString(fullSHA)
		if err != nil || len(rawFull) != 32 {
			return nil, [32]byte{}, nil, false, false
		}
		copy(fullArr[:], rawFull)
	}
	return tailBytes, fullArr, newState, tailBytes[len(tailBytes)-1] == '\n', true
}

// filterJournalReader streams raw journal bytes from r line-by-line without
// buffering the whole journal in heap, dropping paths hidden by f while
// counting raw bytes and tracking the final raw byte.
func filterJournalReader(r io.Reader, f pathFilter) ([]byte, int64, bool, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var (
		out      []byte
		rawSize  int64
		lastByte byte
		lineBuf  []byte
		accum    bool
	)
	hides := f.hides()
	emitLine := func(lineWithNL []byte) {
		if !hides {
			out = append(out, lineWithNL...)
			return
		}
		if len(bytes.TrimSpace(lineWithNL)) == 0 {
			return
		}
		ops, err := journal.Parse(lineWithNL)
		if err != nil || len(ops) != 1 {
			return
		}
		if f.canRead(ops[0].Path) {
			out = append(out, lineWithNL...)
		}
	}
	for {
		frag, err := br.ReadSlice('\n')
		if len(frag) > 0 {
			rawSize += int64(len(frag))
			lastByte = frag[len(frag)-1]
		}
		switch err {
		case nil:
			if accum {
				lineBuf = append(lineBuf, frag...)
				emitLine(lineBuf)
				accum = false
				if cap(lineBuf) > 1<<20 {
					lineBuf = nil
				} else {
					lineBuf = lineBuf[:0]
				}
			} else {
				emitLine(frag)
			}
		case bufio.ErrBufferFull:
			if !accum {
				accum = true
				lineBuf = lineBuf[:0]
			}
			lineBuf = append(lineBuf, frag...)
		case io.EOF:
			if accum {
				lineBuf = append(lineBuf, frag...)
				emitLine(lineBuf)
			} else if len(frag) > 0 {
				emitLine(frag)
			}
			if out == nil {
				out = []byte{}
			}
			return out, rawSize, rawSize > 0 && lastByte == '\n', nil
		default:
			return nil, 0, false, err
		}
	}
}

// bytesSlicesReader streams non-empty byte slices as a single io.ReadSeeker
// without copying or concatenating them, allowing HTTP S3 payload hashing and
// SDK retries to rewind cleanly.
type bytesSlicesReader struct {
	slices [][]byte
	total  int64
	pos    int64
}

func newBytesSlicesReader(parts ...[]byte) *bytesSlicesReader {
	var kept [][]byte
	var total int64
	for _, p := range parts {
		if len(p) > 0 {
			kept = append(kept, p)
			total += int64(len(p))
		}
	}
	return &bytesSlicesReader{slices: kept, total: total}
}

func (r *bytesSlicesReader) Read(p []byte) (int, error) {
	if r.pos >= r.total {
		return 0, io.EOF
	}
	off := r.pos
	idx := 0
	for idx < len(r.slices) && off >= int64(len(r.slices[idx])) {
		off -= int64(len(r.slices[idx]))
		idx++
	}
	copied := 0
	for copied < len(p) && idx < len(r.slices) {
		n := copy(p[copied:], r.slices[idx][off:])
		copied += n
		r.pos += int64(n)
		idx++
		off = 0
	}
	return copied, nil
}

func (r *bytesSlicesReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.pos + offset
	case io.SeekEnd:
		abs = r.total + offset
	default:
		return 0, errors.New("bytesSlicesReader.Seek: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("bytesSlicesReader.Seek: negative position")
	}
	r.pos = abs
	return abs, nil
}

func readVersionedWithMeta(ctx context.Context, be remote.Backend, cp remote.ConditionalPutter, key string) (io.ReadCloser, remote.ObjectMeta, error) {
	if mb, ok := be.(remote.VersionedMetaBackend); ok {
		return mb.GetVersionedWithMeta(ctx, key)
	}
	rc, ver, err := cp.GetVersioned(ctx, key)
	if err != nil {
		return nil, remote.ObjectMeta{}, err
	}
	return rc, remote.ObjectMeta{Version: ver}, nil
}

func readBytesVersionedWithMeta(ctx context.Context, be remote.Backend, cp remote.ConditionalPutter, key string) ([]byte, remote.ObjectMeta, error) {
	rc, meta, err := readVersionedWithMeta(ctx, be, cp, key)
	if err != nil {
		if remote.IsNotExist(err) {
			return nil, remote.ObjectMeta{Version: remote.VersionAbsent}, nil
		}
		return nil, remote.ObjectMeta{}, err
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, remote.ObjectMeta{}, err
	}
	if meta.Size == 0 {
		meta.Size = int64(len(body))
	}
	return body, meta, nil
}

func putIfWithMeta(ctx context.Context, be remote.Backend, cp remote.ConditionalPutter, key string, r io.Reader, size int64, expect remote.Version, meta map[string]string) (remote.ObjectMeta, error) {
	if mb, ok := be.(remote.VersionedMetaBackend); ok {
		return mb.PutIfWithMeta(ctx, key, r, size, expect, meta)
	}
	ver, err := cp.PutIf(ctx, key, r, size, expect)
	if err != nil {
		return remote.ObjectMeta{}, err
	}
	return remote.ObjectMeta{Version: ver, Size: size, Metadata: meta}, nil
}

// suppress unused warning if time import becomes used only by future helpers
var _ = time.Time{}
