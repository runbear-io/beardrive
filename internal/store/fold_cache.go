package store

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

const checkpointStride = 256
const maxCachedWinnersTotal = 500_000

type cacheEntry struct {
	id   fileID
	sha  [32]byte
	racy bool
	data map[string]CachedFile
}
type cachedJournalPrefix struct {
	dev          uint64
	ino          uint64
	byteOffset   int64
	prefixSHA256 string
	tailCheck    [64]byte
	tailCheckLen int
}

type compactWinner struct {
	Lamport    int64
	Time       time.Time
	Device     string
	Seq        int64
	Size       int64
	Mode       uint32
	IsPut      bool
	HasRawBlob bool
	RawBlob    [32]byte
	BlobStr    string
	BaseStr    string
	Link       string
}

func toCompactWinner(op journal.Op) compactWinner {
	w := compactWinner{Lamport: op.Lamport, Time: op.Time, Device: op.Device, Seq: op.Seq, Size: op.Size, Mode: op.Mode, IsPut: op.Kind == journal.KindPut, BlobStr: op.Blob, BaseStr: op.Base, Link: op.Link}
	if len(op.Blob) == 64 {
		var raw [32]byte
		if n, err := hex.Decode(raw[:], []byte(op.Blob)); n == 32 && err == nil && hex.EncodeToString(raw[:]) == op.Blob {
			w.HasRawBlob, w.RawBlob, w.BlobStr = true, raw, ""
		}
	}
	return w
}

func (w compactWinner) blob() string {
	if w.HasRawBlob {
		return hex.EncodeToString(w.RawBlob[:])
	}
	return w.BlobStr
}

func (w compactWinner) toOp(path string) journal.Op {
	kind := journal.KindDelete
	if w.IsPut {
		kind = journal.KindPut
	}
	return journal.Op{Lamport: w.Lamport, Time: w.Time, Device: w.Device, Seq: w.Seq, Kind: kind, Path: path, Blob: w.blob(), Base: w.BaseStr, Size: w.Size, Mode: w.Mode, Link: w.Link}
}

func lessCompact(aPath string, a compactWinner, bPath string, b compactWinner) bool {
	return journal.Less(a.toOp(aPath), b.toOp(bPath))
}

type opCheckpoint struct {
	OpIdx   int
	ByteOff int64
}

type deviceFold struct {
	ID              fileID
	ReadAtNS        int64
	Racy            bool
	TailCheck       [64]byte
	TailCheckLen    int
	ContentSHA256   [32]byte
	HashState       []byte
	OpCount         int
	MaxValidLamport int64
	MaxSeq          int64
	MonotonicDevice string
	HasIgnorePut    bool
	MaxIgnorePut    journal.Op
	Checkpoints     []opCheckpoint
	Winners         map[string]compactWinner
	DirtySidecar    bool
	VerifiedInCycle bool
	gen             uint64
}

func (s *Store) bumpGenLocked() uint64 {
	s.nextFoldGen++
	return s.nextFoldGen
}
func (s *Store) storeFoldLocked(dev string, f *deviceFold) {
	if len(f.Winners) > maxCachedWinnersTotal {
		delete(s.folds, dev)
		s.cachedFoldOK = false
		_ = os.Remove(s.foldPath(dev))
		return
	}
	f.gen = s.bumpGenLocked()
	s.folds[dev] = f
}

func foldOpsInto(f *deviceFold, ops []journal.Op) {
	for _, op := range ops {
		if op.Kind != journal.KindPut && op.Kind != journal.KindDelete {
			continue
		}
		if f.OpCount == 0 {
			if op.Device == "" {
				f.MonotonicDevice = "*"
			} else {
				f.MonotonicDevice = op.Device
				f.MaxSeq = op.Seq
			}
		} else if f.MonotonicDevice != "*" {
			if op.Device != f.MonotonicDevice || op.Seq <= f.MaxSeq {
				f.MonotonicDevice = "*"
			} else {
				f.MaxSeq = op.Seq
			}
		}
		f.OpCount++
		if op.Lamport > 0 && op.Lamport <= journal.MaxValidLamport && op.Lamport > f.MaxValidLamport {
			f.MaxValidLamport = op.Lamport
		}
		if op.Path == ".bdriveignore" && op.Kind == journal.KindPut && (!f.HasIgnorePut || journal.Less(f.MaxIgnorePut, op)) {
			f.HasIgnorePut, f.MaxIgnorePut = true, op
		}
		if cur, ok := f.Winners[op.Path]; ok && journal.Less(op, cur.toOp(op.Path)) {
			continue
		}
		f.Winners[op.Path] = toCompactWinner(op)
	}
}

func tailBytes(data []byte) (out [64]byte, n int) {
	if len(data) > len(out) {
		data = data[len(data)-len(out):]
	}
	copy(out[:], data)
	return out, len(data)
}

func buildDeviceFold(data []byte, id fileID, readAt int64) *deviceFold {
	f := &deviceFold{ID: id, ReadAtNS: readAt, Racy: readAt-id.mtimeNS < int64(2*time.Second), DirtySidecar: true, Winners: make(map[string]compactWinner)}
	h := sha256.New()
	_, _ = h.Write(data)
	copy(f.ContentSHA256[:], h.Sum(nil))
	if len(data) == 0 || data[len(data)-1] == '\n' {
		if bm, ok := h.(encoding.BinaryMarshaler); ok {
			f.HashState, _ = bm.MarshalBinary()
		}
	}
	f.TailCheck, f.TailCheckLen = tailBytes(data)
	ops, cps := journal.ParseWithCheckpoints(data, checkpointStride)
	for _, cp := range cps {
		f.Checkpoints = append(f.Checkpoints, opCheckpoint{OpIdx: int(cp[0]), ByteOff: cp[1]})
	}
	foldOpsInto(f, ops)
	return f
}

func appendCheckpoints(f *deviceFold, preSize int64, ops []journal.Op, cps [][2]int64) bool {
	cpIdx := 0
	curIdx := f.OpCount
	for _, op := range ops {
		if op.Kind != journal.KindPut && op.Kind != journal.KindDelete {
			continue
		}
		if cpIdx >= len(cps) {
			return false
		}
		if curIdx%checkpointStride == 0 {
			f.Checkpoints = append(f.Checkpoints, opCheckpoint{OpIdx: curIdx, ByteOff: preSize + cps[cpIdx][1]})
		}
		curIdx++
		cpIdx++
	}
	return cpIdx == len(cps)
}

func tailBytesFromEnd(f *deviceFold, data []byte) (out [64]byte, n int) {
	if len(data) >= len(out) {
		return tailBytes(data)
	}
	combined := make([]byte, 0, f.TailCheckLen+len(data))
	combined = append(combined, f.TailCheck[:f.TailCheckLen]...)
	combined = append(combined, data...)
	return tailBytes(combined)
}

func (s *Store) foldPath(dev string) string { return filepath.Join(s.dir, "fold-cache", dev+".fold") }
func safeDeviceName(dev string) bool {
	if dev == "" || dev == "." || dev == ".." || strings.ContainsAny(dev, `/\\`) {
		return false
	}
	return filepath.Base(dev) == dev
}

type sidecarFileID struct {
	Dev     uint64
	Ino     uint64
	Size    int64
	MTimeNS int64
	CTimeNS int64
	Valid   bool
}

func encodeFileID(id fileID) sidecarFileID {
	return sidecarFileID{Dev: id.dev, Ino: id.ino, Size: id.size, MTimeNS: id.mtimeNS, CTimeNS: id.ctimeNS, Valid: id.valid}
}
func decodeFileID(id sidecarFileID) fileID {
	return fileID{dev: id.Dev, ino: id.Ino, size: id.Size, mtimeNS: id.MTimeNS, ctimeNS: id.CTimeNS, valid: id.Valid}
}

type foldSidecar struct {
	Version         int
	ID              sidecarFileID
	ReadAtNS        int64
	Racy            bool
	TailCheck       [64]byte
	TailCheckLen    int
	ContentSHA256   [32]byte
	HashState       []byte
	OpCount         int
	MaxSeq          int64
	MonotonicDevice string
	MaxValidLamport int64
	HasIgnorePut    bool
	MaxIgnorePut    journal.Op
	Checkpoints     []opCheckpoint
	Winners         map[string]compactWinner
}

func sidecarFrom(f *deviceFold) foldSidecar {
	return foldSidecar{3, encodeFileID(f.ID), f.ReadAtNS, f.Racy, f.TailCheck, f.TailCheckLen, f.ContentSHA256, f.HashState, f.OpCount, f.MaxSeq, f.MonotonicDevice, f.MaxValidLamport, f.HasIgnorePut, f.MaxIgnorePut, f.Checkpoints, f.Winners}
}
func (s foldSidecar) fold() *deviceFold {
	return &deviceFold{ID: decodeFileID(s.ID), ReadAtNS: s.ReadAtNS, Racy: s.Racy, TailCheck: s.TailCheck, TailCheckLen: s.TailCheckLen, ContentSHA256: s.ContentSHA256, HashState: s.HashState, OpCount: s.OpCount, MaxSeq: s.MaxSeq, MonotonicDevice: s.MonotonicDevice, MaxValidLamport: s.MaxValidLamport, HasIgnorePut: s.HasIgnorePut, MaxIgnorePut: s.MaxIgnorePut, Checkpoints: s.Checkpoints, Winners: s.Winners}
}

func hashFile(f *os.File) ([32]byte, error) {
	h := sha256.New()
	if _, err := f.Seek(0, 0); err != nil {
		return [32]byte{}, err
	}
	if _, err := io.Copy(h, f); err != nil {
		return [32]byte{}, err
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

func (s *Store) getOrLoadDeviceFold(dev string) (*deviceFold, error) {
	lp := s.JournalPath(dev)
	now := time.Now().UnixNano()
	id, err := statFileID(lp)
	if err != nil {
		if os.IsNotExist(err) {
			id := fileID{valid: true}
			e := buildDeviceFold(nil, id, now)
			s.mu.Lock()
			s.storeFoldLocked(dev, e)
			s.mu.Unlock()
			return e, nil
		}
		return nil, err
	}
	s.mu.Lock()
	entry := s.folds[dev]
	if entry != nil && entry.ID == id && (!entry.Racy || entry.VerifiedInCycle) {
		s.mu.Unlock()
		return entry, nil
	}
	s.mu.Unlock()
	f, err := os.Open(lp)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dataTail := make([]byte, 64)
	n, _ := f.ReadAt(dataTail, max64(id.size-64, 0))
	if n < 0 {
		n = 0
	}
	var tail [64]byte
	copy(tail[:], dataTail[:n])
	s.mu.Lock()
	entry = s.folds[dev]
	if entry != nil && entry.ID == id && entry.TailCheckLen == n && bytes.Equal(entry.TailCheck[:n], tail[:n]) {
		if !entry.Racy || entry.VerifiedInCycle {
			s.mu.Unlock()
			return entry, nil
		}
		s.mu.Unlock()
		sum, herr := hashFile(f)
		post, serr := statFileID(lp)
		s.mu.Lock()
		if entry == s.folds[dev] && herr == nil && serr == nil && post == id && sum == entry.ContentSHA256 {
			if now-id.mtimeNS >= int64(2*time.Second) {
				entry.Racy = false
				entry.ReadAtNS = now
				entry.DirtySidecar = true
			}
			s.mu.Unlock()
			return entry, nil
		}
	}
	s.mu.Unlock()
	if safeDeviceName(dev) {
		if fi, serr := os.Stat(s.foldPath(dev)); serr == nil && fi.Size() <= 64<<20 {
			if raw, rerr := os.ReadFile(s.foldPath(dev)); rerr == nil {
				var sc foldSidecar
				valid := func() (ok bool) {
					defer func() { _ = recover() }()
					return gob.NewDecoder(bytes.NewReader(raw)).Decode(&sc) == nil
				}()
				if valid && sc.Version == 3 && sc.ID.Ino == id.ino && sc.ID.Size == id.size && sc.ID.MTimeNS == id.mtimeNS && sc.TailCheckLen == n && bytes.Equal(sc.TailCheck[:n], tail[:n]) && len(sc.Winners) <= maxCachedWinnersTotal {
					e := sc.fold()
					if sc.ReadAtNS-id.mtimeNS >= int64(2*time.Second) {
						s.mu.Lock()
						s.storeFoldLocked(dev, e)
						s.mu.Unlock()
						return e, nil
					}
					sum, herr := hashFile(f)
					post, serr := statFileID(lp)
					if herr == nil && serr == nil && post == id && sum == sc.ContentSHA256 {
						if now-id.mtimeNS >= int64(2*time.Second) {
							e.Racy = false
							e.ReadAtNS = now
							e.DirtySidecar = true
						}
						s.mu.Lock()
						s.storeFoldLocked(dev, e)
						s.mu.Unlock()
						return e, nil
					}
				}
			}
		}
	}
	data, err := os.ReadFile(lp)
	if err != nil {
		return nil, err
	}
	e := buildDeviceFold(data, id, now)
	s.mu.Lock()
	s.storeFoldLocked(dev, e)
	s.mu.Unlock()
	return e, nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (s *Store) AppendOps(device string, ops []journal.Op) error {
	if len(ops) == 0 {
		return nil
	}
	data, err := journal.Marshal(ops)
	if err != nil {
		return err
	}
	path := s.JournalPath(device)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	pre, err := f.Stat()
	if err != nil {
		return err
	}
	preID, preIDErr := statFileID(path)
	if preIDErr == nil {
		s.mu.Lock()
		entry, ok := s.folds[device]
		valid := ok && (!entry.Racy || entry.VerifiedInCycle) &&
			entry.ContentSHA256 != [32]byte{} &&
			entry.TailCheckLen > 0 && entry.TailCheck[entry.TailCheckLen-1] == '\n' &&
			entry.ID == preID
		if valid {
			if s.prefixCache == nil {
				s.prefixCache = make(map[string]map[int64]cachedJournalPrefix)
			}
			if s.prefixCache[device] == nil {
				s.prefixCache[device] = make(map[int64]cachedJournalPrefix)
			}
			s.prefixCache[device][int64(entry.OpCount)] = cachedJournalPrefix{
				dev:          preID.dev,
				ino:          preID.ino,
				byteOffset:   entry.ID.size,
				prefixSHA256: hex.EncodeToString(entry.ContentSHA256[:]),
				tailCheck:    entry.TailCheck,
				tailCheckLen: entry.TailCheckLen,
			}
		}
		s.mu.Unlock()
	}
	if pre.Size() > 0 {
		var b [1]byte
		if _, e := f.ReadAt(b[:], pre.Size()-1); e == nil && b[0] != '\n' {
			data = append([]byte{'\n'}, data...)
		}
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	postID, postErr := statFileID(path)
	s.mu.Lock()
	entry, ok := s.folds[device]
	fast := ok && preIDErr == nil && (!entry.Racy || entry.VerifiedInCycle) &&
		entry.ID.dev == preID.dev && entry.ID.ino == preID.ino && entry.ID.size == pre.Size() &&
		postErr == nil && postID.size == pre.Size()+int64(len(data)) &&
		(pre.Size() == 0 || len(entry.HashState) > 0)
	if fast {
		h := sha256.New()
		if len(entry.HashState) > 0 {
			um, ok := h.(encoding.BinaryUnmarshaler)
			if !ok || um.UnmarshalBinary(entry.HashState) != nil {
				fast = false
			}
		}
		if fast {
			_, _ = h.Write(data)
			copy(entry.ContentSHA256[:], h.Sum(nil))
			if bm, ok := h.(encoding.BinaryMarshaler); ok {
				entry.HashState, _ = bm.MarshalBinary()
			} else {
				fast = false
			}
		}
	}
	if fast {
		parsed, cps := journal.ParseWithCheckpoints(data, 1)
		if len(parsed) != countValidOps(ops) || !appendCheckpoints(entry, pre.Size(), ops, cps) {
			fast = false
		} else {
			foldOpsInto(entry, ops)
			entry.ID = postID
			entry.TailCheck, entry.TailCheckLen = tailBytesFromEnd(entry, data)
			entry.DirtySidecar = true
			entry.VerifiedInCycle = true
			entry.gen = s.bumpGenLocked()
			s.cachedFoldOK = false
		}
	}
	if !fast {
		delete(s.folds, device)
		delete(s.prefixCache, device)
		s.cachedFoldOK = false
		_ = os.Remove(s.foldPath(device))
	}
	s.mu.Unlock()
	return nil
}

func countValidOps(ops []journal.Op) int {
	n := 0
	for _, op := range ops {
		if op.Kind == journal.KindPut || op.Kind == journal.KindDelete {
			n++
		}
	}
	return n
}

func cloneFoldResult(r FoldResult) FoldResult {
	r.Target = maps.Clone(r.Target)
	return r
}

func (s *Store) ReplayFold() (FoldResult, error) {
	devs, err := s.Devices()
	if err != nil {
		return FoldResult{}, err
	}
	devSet := make(map[string]bool, len(devs))
	for _, dev := range devs {
		devSet[dev] = true
	}
	s.mu.Lock()
	for dev := range s.folds {
		if !devSet[dev] {
			delete(s.folds, dev)
			s.cachedFoldOK = false
			_ = os.Remove(s.foldPath(dev))
		}
	}
	s.mu.Unlock()

	entries := make([]*deviceFold, len(devs))
	for i, dev := range devs {
		f, e := s.getOrLoadDeviceFold(dev)
		if e != nil {
			return FoldResult{}, e
		}
		entries[i] = f
	}

	s.mu.Lock()
	if s.cachedFoldOK && slices.Equal(s.cachedFoldDevs, devs) && len(s.cachedFoldGens) == len(entries) {
		same := true
		for i, f := range entries {
			if f.gen == 0 || f.gen != s.cachedFoldGens[i] {
				same = false
				break
			}
		}
		if same {
			out := cloneFoldResult(s.cachedFold)
			s.mu.Unlock()
			return out, nil
		}
	}
	result := FoldResult{Target: make(map[string]journal.FileState)}
	winners := make(map[string]compactWinner)
	gens := make([]uint64, len(entries))
	allCached := true
	for i, f := range entries {
		gens[i] = f.gen
		if f.gen == 0 {
			allCached = false
		}
		result.TotalOps += f.OpCount
		if f.MaxValidLamport > result.MaxLamport {
			result.MaxLamport = f.MaxValidLamport
		}
		if f.HasIgnorePut && (!result.HasIgnorePut || journal.Less(result.LatestIgnorePut, f.MaxIgnorePut)) {
			result.LatestIgnorePut = f.MaxIgnorePut
			result.HasIgnorePut = true
		}
		for p, w := range f.Winners {
			cur, ok := winners[p]
			if !ok || !journal.Less(w.toOp(p), cur.toOp(p)) {
				winners[p] = w
			}
		}
	}
	for p, w := range winners {
		if w.IsPut {
			result.Target[p] = journal.FileState{Blob: w.blob(), Size: w.Size, Mode: w.Mode, Link: w.Link}
		}
		if p == ".bdriveignore" {
			result.IgnoreWinner = w.toOp(p)
			result.HasIgnoreWinner = true
		}
	}
	if allCached {
		s.cachedFold = cloneFoldResult(result)
		s.cachedFoldDevs = slices.Clone(devs)
		s.cachedFoldGens = gens
		s.cachedFoldOK = true
	} else {
		s.cachedFoldOK = false
	}
	s.mu.Unlock()
	return result, nil
}

type FoldResult struct {
	Target          map[string]journal.FileState
	TotalOps        int
	MaxLamport      int64
	LatestIgnorePut journal.Op
	HasIgnorePut    bool
	IgnoreWinner    journal.Op
	HasIgnoreWinner bool
}

func (s *Store) OwnOpCount(device string) (int, error) {
	f, e := s.getOrLoadDeviceFold(device)
	if e != nil {
		return 0, e
	}
	s.mu.Lock()
	n := f.OpCount
	s.mu.Unlock()
	return n, nil
}

func (s *Store) OwnOpsFrom(device string, startIdx int64) ([]journal.Op, error) {
	f, e := s.getOrLoadDeviceFold(device)
	if e != nil {
		return nil, e
	}
	if startIdx < 0 {
		startIdx = 0
	}
	s.mu.Lock()
	opCount := f.OpCount
	fID := f.ID
	cp := opCheckpoint{OpIdx: 0, ByteOff: 0}
	for _, c := range f.Checkpoints {
		if int64(c.OpIdx) <= startIdx {
			cp = c
		} else {
			break
		}
	}
	s.mu.Unlock()
	if startIdx >= int64(opCount) {
		return nil, nil
	}
	lp := s.JournalPath(device)
	if id, serr := statFileID(lp); serr == nil && id == fID && cp.ByteOff >= 0 && cp.ByteOff <= id.size {
		if file, oerr := os.Open(lp); oerr == nil {
			buf := make([]byte, id.size-cp.ByteOff)
			n, rerr := io.ReadFull(io.NewSectionReader(file, cp.ByteOff, id.size-cp.ByteOff), buf)
			_ = file.Close()
			if rerr == nil || errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
				ops, perr := journal.Parse(buf[:n])
				if perr == nil && len(ops) == opCount-cp.OpIdx {
					relIdx := int(startIdx) - cp.OpIdx
					if relIdx >= 0 && relIdx <= len(ops) {
						return ops[relIdx:], nil
					}
				}
			}
		}
	}
	ops, e := s.DeviceOps(device)
	if e != nil {
		return nil, e
	}
	if startIdx >= int64(len(ops)) {
		return nil, nil
	}
	return ops[startIdx:], nil
}

// JournalPrefixAt returns the byte boundary and digest for the first opCount
// valid journal operations.
func (s *Store) JournalPrefixAt(device string, opCount int64) (byteOffset int64, prefixSHA256 string, ok bool) {
	if opCount < 0 {
		return 0, "", false
	}
	f, err := os.Open(s.JournalPath(device))
	if err != nil {
		return 0, "", false
	}
	defer f.Close()
	if opCount == 0 {
		sum := sha256.Sum256(nil)
		return 0, hex.EncodeToString(sum[:]), true
	}
	lp := s.JournalPath(device)
	curID, curIDErr := statFileID(lp)
	s.mu.Lock()
	cp, cached := s.prefixCache[device][opCount]
	s.mu.Unlock()
	if cached && curIDErr == nil && curID.dev == cp.dev && curID.ino == cp.ino && curID.size >= cp.byteOffset && cp.tailCheckLen > 0 && cp.byteOffset >= int64(cp.tailCheckLen) {
		var tailBuf [64]byte
		if _, rerr := f.ReadAt(tailBuf[:cp.tailCheckLen], cp.byteOffset-int64(cp.tailCheckLen)); rerr == nil && bytes.Equal(tailBuf[:cp.tailCheckLen], cp.tailCheck[:cp.tailCheckLen]) {
			return cp.byteOffset, cp.prefixSHA256, true
		}
		s.mu.Lock()
		delete(s.prefixCache, device)
		s.mu.Unlock()
	}
	r := bufio.NewReader(f)
	h := sha256.New()
	var found, offset int64
	for {
		line, rerr := r.ReadBytes('\n')
		if len(line) == 0 || line[len(line)-1] != '\n' {
			break
		}
		_, _ = h.Write(line)
		offset += int64(len(line))
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			var op journal.Op
			if json.Unmarshal(trimmed, &op) == nil && (op.Kind == journal.KindPut || op.Kind == journal.KindDelete) {
				found++
				if found == opCount {
					resultOffset, resultSHA := offset, hex.EncodeToString(h.Sum(nil))
					if curIDErr == nil && resultOffset > 0 {
						tLen := int(min(int64(64), resultOffset))
						var tBuf [64]byte
						if _, terr := f.ReadAt(tBuf[:tLen], resultOffset-int64(tLen)); terr == nil {
							s.mu.Lock()
							if s.prefixCache == nil {
								s.prefixCache = make(map[string]map[int64]cachedJournalPrefix)
							}
							if s.prefixCache[device] == nil {
								s.prefixCache[device] = make(map[int64]cachedJournalPrefix)
							}
							s.prefixCache[device][opCount] = cachedJournalPrefix{
								dev:          curID.dev,
								ino:          curID.ino,
								byteOffset:   resultOffset,
								prefixSHA256: resultSHA,
								tailCheck:    tBuf,
								tailCheckLen: tLen,
							}
							s.mu.Unlock()
						}
					}
					return resultOffset, resultSHA, true
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	return 0, "", false
}

func (s *Store) JournalPrefixStat(device string) (size int64, shaHex string, endsNewline bool, ok bool) {
	lp := s.JournalPath(device)
	id, err := statFileID(lp)
	if err != nil || id.size <= 0 {
		return 0, "", false, false
	}
	s.mu.Lock()
	entry, has := s.folds[device]
	if has && (!entry.Racy || entry.VerifiedInCycle) && entry.ID == id {
		sum := entry.ContentSHA256
		endsNL := entry.TailCheckLen > 0 && entry.TailCheck[entry.TailCheckLen-1] == '\n'
		s.mu.Unlock()
		return id.size, hex.EncodeToString(sum[:]), endsNL, true
	}
	s.mu.Unlock()
	f, err := os.Open(lp)
	if err != nil {
		return 0, "", false, false
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, id.size))
	if err != nil || n != id.size {
		return 0, "", false, false
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], id.size-1); err != nil {
		return 0, "", false, false
	}
	return id.size, hex.EncodeToString(h.Sum(nil)), last[0] == '\n', true
}

func (s *Store) AppendValidatedPeerTail(device string, prevStat os.FileInfo, localSize int64, tailData []byte, fresh []journal.Op) bool {
	if prevStat == nil || prevStat.Size() != localSize || localSize <= 0 || len(tailData) == 0 {
		return false
	}
	entry, err := s.getOrLoadDeviceFold(device)
	if err != nil || entry == nil {
		return false
	}
	s.mu.Lock()
	if entry.ID.size != localSize || entry.ID.mtimeNS != prevStat.ModTime().UnixNano() ||
		entry.TailCheckLen == 0 || entry.TailCheck[entry.TailCheckLen-1] != '\n' ||
		entry.MonotonicDevice == "*" {
		s.mu.Unlock()
		return false
	}
	dev := entry.MonotonicDevice
	maxSeq := entry.MaxSeq
	hasOps := entry.OpCount > 0
	for _, op := range fresh {
		if op.Kind != journal.KindPut && op.Kind != journal.KindDelete {
			continue
		}
		if op.Device == "" {
			s.mu.Unlock()
			return false
		}
		if dev == "" {
			dev = op.Device
		} else if dev != op.Device {
			s.mu.Unlock()
			return false
		}
		if op.Seq <= 0 || (hasOps && op.Seq <= maxSeq) {
			s.mu.Unlock()
			return false
		}
		hasOps = true
		maxSeq = op.Seq
	}
	s.mu.Unlock()

	lp := s.JournalPath(device)
	tmp, err := os.CreateTemp(filepath.Dir(lp), ".bdrive-tmp-")
	if err != nil {
		return false
	}
	tmpName := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return false
	}

	prepared := false
	if cloneErr := tryCloneFile(lp, tmpName); cloneErr == nil {
		f, openErr := os.OpenFile(tmpName, os.O_WRONLY, 0o600)
		if openErr == nil {
			valid := false
			if fi, statErr := f.Stat(); statErr == nil && fi.Size() == localSize {
				if _, seekErr := f.Seek(localSize, io.SeekStart); seekErr == nil {
					if _, writeErr := f.Write(tailData); writeErr == nil {
						valid = f.Chmod(0o600) == nil
					}
				}
			}
			closeErr := f.Close()
			prepared = valid && closeErr == nil
		}
	}
	if !prepared {
		src, openErr := os.Open(lp)
		if openErr != nil {
			_ = os.Remove(tmpName)
			return false
		}
		dst, createErr := os.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if createErr != nil {
			_ = src.Close()
			_ = os.Remove(tmpName)
			return false
		}
		copied, copyErr := io.Copy(dst, io.LimitReader(src, localSize))
		var writeErr, chmodErr error
		if copyErr == nil && copied == localSize {
			_, writeErr = dst.Write(tailData)
			if writeErr == nil {
				chmodErr = dst.Chmod(0o600)
			}
		}
		closeDstErr := dst.Close()
		closeSrcErr := src.Close()
		if copyErr != nil || copied != localSize || writeErr != nil || chmodErr != nil || closeDstErr != nil || closeSrcErr != nil {
			_ = os.Remove(tmpName)
			return false
		}
	}
	if err := os.Rename(tmpName, lp); err != nil {
		_ = os.Remove(tmpName)
		return false
	}
	postID, postErr := statFileID(lp)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cachedFoldOK = false
	if postErr != nil || postID.size != localSize+int64(len(tailData)) {
		delete(s.folds, device)
		_ = os.Remove(s.foldPath(device))
		return true
	}
	cur, ok := s.folds[device]
	if !ok || cur != entry || len(entry.HashState) == 0 {
		delete(s.folds, device)
		_ = os.Remove(s.foldPath(device))
		return true
	}
	h := sha256.New()
	if um, ok := h.(encoding.BinaryUnmarshaler); !ok || um.UnmarshalBinary(entry.HashState) != nil {
		delete(s.folds, device)
		_ = os.Remove(s.foldPath(device))
		return true
	}
	_, _ = h.Write(tailData)
	copy(entry.ContentSHA256[:], h.Sum(nil))
	if tailData[len(tailData)-1] == '\n' {
		if bm, ok := h.(encoding.BinaryMarshaler); ok {
			entry.HashState, _ = bm.MarshalBinary()
		} else {
			entry.HashState = nil
		}
	} else {
		entry.HashState = nil
	}
	_, cps := journal.ParseWithCheckpoints(tailData, 1)
	_ = appendCheckpoints(entry, localSize, fresh, cps)
	foldOpsInto(entry, fresh)
	if len(entry.Winners) > maxCachedWinnersTotal {
		delete(s.folds, device)
		_ = os.Remove(s.foldPath(device))
		return true
	}
	entry.ID = postID
	entry.TailCheck, entry.TailCheckLen = tailBytesFromEnd(entry, tailData)
	entry.DirtySidecar = true
	entry.VerifiedInCycle = true
	entry.gen = s.bumpGenLocked()
	return true
}

func (s *Store) CanFastAppendPeer(device string, prevStat os.FileInfo, localBytes []byte, fresh []journal.Op) bool {
	if prevStat == nil || int64(len(localBytes)) != prevStat.Size() {
		return false
	}
	localTail, localTailLen := tailBytes(localBytes)
	s.mu.Lock()
	entry, ok := s.folds[device]
	matches := ok && (!entry.Racy || entry.VerifiedInCycle) &&
		entry.ID.size == int64(len(localBytes)) && entry.ID.mtimeNS == prevStat.ModTime().UnixNano() &&
		entry.TailCheckLen == localTailLen && bytes.Equal(entry.TailCheck[:localTailLen], localTail[:localTailLen])
	s.mu.Unlock()
	if !matches {
		loaded, err := s.getOrLoadDeviceFold(device)
		if err != nil {
			return false
		}
		entry = loaded
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry == nil || entry.ID.size != int64(len(localBytes)) ||
		entry.TailCheckLen != localTailLen || !bytes.Equal(entry.TailCheck[:localTailLen], localTail[:localTailLen]) ||
		entry.MonotonicDevice == "*" {
		return false
	}
	dev := entry.MonotonicDevice
	maxSeq := entry.MaxSeq
	hasOps := entry.OpCount > 0
	for _, op := range fresh {
		if op.Kind != journal.KindPut && op.Kind != journal.KindDelete {
			continue
		}
		if op.Device == "" {
			return false
		}
		if !hasOps {
			dev = op.Device
			maxSeq = op.Seq
			hasOps = true
			continue
		}
		if op.Device != dev || op.Seq <= maxSeq {
			return false
		}
		maxSeq = op.Seq
	}
	return true
}

func (s *Store) FoldPeerAppend(device string, prevStat os.FileInfo, localBytes, newData []byte, fresh []journal.Op, newStat os.FileInfo) {
	lp := s.JournalPath(device)
	postID, postErr := statFileID(lp)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.folds == nil {
		s.folds = make(map[string]*deviceFold)
	}
	s.cachedFoldOK = false
	if postErr != nil || postID.size != int64(len(newData)) {
		delete(s.folds, device)
		_ = os.Remove(s.foldPath(device))
		return
	}
	localTail, localTailLen := tailBytes(localBytes)
	entry, ok := s.folds[device]
	fast := ok && prevStat != nil && (!entry.Racy || entry.VerifiedInCycle) &&
		entry.ID.size == int64(len(localBytes)) && entry.ID.mtimeNS == prevStat.ModTime().UnixNano() &&
		entry.TailCheckLen == localTailLen && bytes.Equal(entry.TailCheck[:localTailLen], localTail[:localTailLen])
	if fast {
		tailData := newData[len(localBytes):]
		if len(entry.HashState) > 0 {
			h := sha256.New()
			if um, ok := h.(encoding.BinaryUnmarshaler); ok && um.UnmarshalBinary(entry.HashState) == nil {
				_, _ = h.Write(tailData)
				copy(entry.ContentSHA256[:], h.Sum(nil))
				if len(newData) == 0 || newData[len(newData)-1] == '\n' {
					if bm, ok := h.(encoding.BinaryMarshaler); ok {
						entry.HashState, _ = bm.MarshalBinary()
					} else {
						entry.HashState = nil
					}
				} else {
					entry.HashState = nil
				}
			} else {
				entry.ContentSHA256 = sha256.Sum256(newData)
				entry.HashState = nil
			}
		} else {
			entry.ContentSHA256 = sha256.Sum256(newData)
		}
		_, cps := journal.ParseWithCheckpoints(tailData, 1)
		_ = appendCheckpoints(entry, int64(len(localBytes)), fresh, cps)
		foldOpsInto(entry, fresh)
		if len(entry.Winners) > maxCachedWinnersTotal {
			delete(s.folds, device)
			_ = os.Remove(s.foldPath(device))
			return
		}
		entry.ID = postID
		entry.TailCheck, entry.TailCheckLen = tailBytes(newData)
		entry.DirtySidecar = true
		entry.VerifiedInCycle = true
		entry.gen = s.bumpGenLocked()
		return
	}
	built := buildDeviceFold(newData, postID, time.Now().UnixNano())
	built.VerifiedInCycle = true
	s.storeFoldLocked(device, built)
}

func (s *Store) InvalidateJournal(device string) {
	s.mu.Lock()
	delete(s.folds, device)
	delete(s.prefixCache, device)
	s.cachedFoldOK = false
	_ = os.Remove(s.foldPath(device))
	s.mu.Unlock()
}

func (s *Store) EvictPeerJournals(ownDevice string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cachedFoldOK = false
	for d := range s.folds {
		if d != ownDevice {
			delete(s.folds, d)
			delete(s.prefixCache, d)
			_ = os.Remove(s.foldPath(d))
		}
	}
	if entries, err := os.ReadDir(filepath.Join(s.dir, "fold-cache")); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".fold") {
				d := strings.TrimSuffix(e.Name(), ".fold")
				if d != ownDevice {
					_ = os.Remove(s.foldPath(d))
				}
			}
		}
	}
}
func (s *Store) SaveChangedFoldsLocked() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.folds == nil {
		return
	}
	_ = os.MkdirAll(filepath.Join(s.dir, "fold-cache"), 0o700)
	for dev, f := range s.folds {
		f.VerifiedInCycle = false
		if !f.DirtySidecar || !safeDeviceName(dev) || len(f.Winners) > maxCachedWinnersTotal {
			continue
		}
		var b bytes.Buffer
		if gob.NewEncoder(&b).Encode(sidecarFrom(f)) != nil {
			continue
		}
		if WriteFileAtomic(s.foldPath(dev), b.Bytes(), 0o600) == nil {
			f.DirtySidecar = false
		}
	}
}

func SyncStateEqual(a, b SyncState) bool {
	return a.Remote == b.Remote && a.Device == b.Device && a.Lamport == b.Lamport && a.PushedOps == b.PushedOps && a.Access == b.Access && a.AccessReason == b.AccessReason && a.IgnoreAccepted == b.IgnoreAccepted && a.IgnorePulled == b.IgnorePulled && a.ScopeTag == b.ScopeTag && slices.Equal(a.ReadOnly, b.ReadOnly) && slices.Equal(a.Denied, b.Denied)
}
func CloneSyncState(st SyncState) SyncState {
	st.ReadOnly = slices.Clone(st.ReadOnly)
	st.Denied = slices.Clone(st.Denied)
	return st
}

func (s *Store) loadCacheInternal(mountID string, clone bool) (map[string]CachedFile, error) {
	p, e := s.cachePath(mountID)
	if e != nil {
		return nil, e
	}
	now := time.Now().UnixNano()
	id, serr := statFileID(p)
	if serr != nil {
		if os.IsNotExist(serr) {
			return map[string]CachedFile{}, nil
		}
		return nil, serr
	}
	s.mu.Lock()
	ce, ok := s.stateCaches[mountID]
	if ok && ce.id == id && !ce.racy {
		data := ce.data
		s.mu.Unlock()
		if clone {
			return maps.Clone(data), nil
		}
		return data, nil
	}
	s.mu.Unlock()
	raw, rerr := os.ReadFile(p)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return map[string]CachedFile{}, nil
		}
		return nil, rerr
	}
	postID, postErr := statFileID(p)
	sum := sha256.Sum256(raw)
	if ok && postErr == nil && postID == id && ce.id == id && sum == ce.sha {
		s.mu.Lock()
		if cur, stillOK := s.stateCaches[mountID]; stillOK && cur.id == id && cur.sha == sum {
			if now-id.mtimeNS >= int64(2*time.Second) {
				cur.racy = false
				s.stateCaches[mountID] = cur
			}
			data := cur.data
			s.mu.Unlock()
			if clone {
				return maps.Clone(data), nil
			}
			return data, nil
		}
		s.mu.Unlock()
	}
	out := map[string]CachedFile{}
	if e := json.Unmarshal(raw, &out); e != nil {
		return nil, e
	}
	for k := range out {
		if !cleanRel(k) {
			delete(out, k)
		}
	}
	if postErr == nil && postID == id {
		s.mu.Lock()
		s.stateCaches[mountID] = cacheEntry{
			id:   id,
			sha:  sum,
			racy: now-id.mtimeNS < int64(2*time.Second),
			data: maps.Clone(out),
		}
		s.mu.Unlock()
	}
	if clone {
		return maps.Clone(out), nil
	}
	return out, nil
}

func (s *Store) LoadCache(mountID string) (map[string]CachedFile, error) {
	return s.loadCacheInternal(mountID, true)
}

func (s *Store) LoadCacheReadOnly(mountID string) (map[string]CachedFile, error) {
	return s.loadCacheInternal(mountID, false)
}

func (s *Store) SaveCache(mountID string, c map[string]CachedFile) error {
	p, e := s.cachePath(mountID)
	if e != nil {
		return e
	}
	cleaned := make(map[string]CachedFile, len(c))
	for k, v := range c {
		if cleanRel(k) {
			cleaned[k] = v
		}
	}
	raw, e := json.MarshalIndent(cleaned, "", "  ")
	if e != nil {
		return e
	}
	if e = WriteFileAtomic(p, raw, 0o600); e != nil {
		s.mu.Lock()
		delete(s.stateCaches, mountID)
		delete(s.ignoreFingerprints, mountID)
		s.mu.Unlock()
		return e
	}
	now := time.Now().UnixNano()
	id, e := statFileID(p)
	s.mu.Lock()
	if e == nil {
		s.stateCaches[mountID] = cacheEntry{
			id:   id,
			sha:  sha256.Sum256(raw),
			racy: now-id.mtimeNS < int64(2*time.Second),
			data: maps.Clone(cleaned),
		}
	} else {
		delete(s.stateCaches, mountID)
	}
	s.mu.Unlock()
	return e
}
func (s *Store) IgnoreFingerprintMatches(mountID string, sum [32]byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.ignoreFingerprints[mountID]
	return ok && v == sum
}
func (s *Store) SetIgnoreFingerprint(mountID string, sum [32]byte) {
	s.mu.Lock()
	if s.ignoreFingerprints == nil {
		s.ignoreFingerprints = make(map[string][32]byte)
	}
	s.ignoreFingerprints[mountID] = sum
	s.mu.Unlock()
}
func (s *Store) ClearIgnoreFingerprint(mountID string) {
	s.mu.Lock()
	delete(s.ignoreFingerprints, mountID)
	s.mu.Unlock()
}
