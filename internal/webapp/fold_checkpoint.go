package webapp

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"io"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

const hubFoldCheckpointKey = "hub-cache/fold-v1.bin.gz"
const minFoldCheckpointBytes int64 = 1 << 20

type ckptJournalMeta struct {
	Size         int64
	ModTime      time.Time
	PrefixSHA256 string
	Ops          int
}

type winOp struct {
	Lamport int64
	Time    time.Time
	Device  string
	Seq     int64
}

type foldCheckpoint struct {
	Journals   map[string]ckptJournalMeta
	Files      map[string]FileInfo
	Tombstones map[string]winOp
	Moves      moveIndex
}

type checkpointPathEvent struct {
	At   time.Time
	To   string
	ToAt time.Time
}

type checkpointWire struct {
	Journals   map[string]ckptJournalMeta
	Files      map[string]FileInfo
	Tombstones map[string]winOp
	Moves      map[string][]checkpointPathEvent
}

func encodeFoldCheckpoint(cp foldCheckpoint) ([]byte, error) {
	wire := checkpointWire{
		Journals: cp.Journals, Files: cp.Files, Tombstones: cp.Tombstones,
		Moves: make(map[string][]checkpointPathEvent, len(cp.Moves)),
	}
	for p, events := range cp.Moves {
		out := make([]checkpointPathEvent, len(events))
		for i, e := range events {
			out[i] = checkpointPathEvent{At: e.At, To: e.To, ToAt: e.ToAt}
		}
		wire.Moves[p] = out
	}
	var raw bytes.Buffer
	zw := gzip.NewWriter(&raw)
	if err := gob.NewEncoder(zw).Encode(wire); err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return raw.Bytes(), nil
}

func decodeFoldCheckpoint(raw []byte) (foldCheckpoint, error) {
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return foldCheckpoint{}, err
	}
	var wire checkpointWire
	err = gob.NewDecoder(zr).Decode(&wire)
	closeErr := zr.Close()
	if err != nil {
		return foldCheckpoint{}, err
	}
	if closeErr != nil {
		return foldCheckpoint{}, closeErr
	}
	cp := foldCheckpoint{
		Journals: wire.Journals, Files: wire.Files, Tombstones: wire.Tombstones,
		Moves: make(moveIndex, len(wire.Moves)),
	}
	for p, events := range wire.Moves {
		out := make([]pathEvent, len(events))
		for i, e := range events {
			out[i] = pathEvent{At: e.At, To: e.To, ToAt: e.ToAt}
		}
		cp.Moves[p] = out
	}
	return cp, nil
}

func winOpOf(op journal.Op) winOp {
	return winOp{Lamport: op.Lamport, Time: op.Time, Device: op.Device, Seq: op.Seq}
}

func (w winOp) op(path, kind string) journal.Op {
	return journal.Op{Lamport: w.Lamport, Time: w.Time, Device: w.Device, Seq: w.Seq, Path: path, Kind: kind}
}

func (w winOp) less(op journal.Op) bool {
	return journal.Less(w.op(op.Path, op.Kind), op)
}

func cloneFiles(in map[string]FileInfo) map[string]FileInfo {
	out := make(map[string]FileInfo, len(in))
	for p, fi := range in {
		out[p] = fi
	}
	return out
}

func cloneTombstones(in map[string]winOp) map[string]winOp {
	out := make(map[string]winOp, len(in))
	for p, op := range in {
		out[p] = op
	}
	return out
}

func cloneMoves(in moveIndex) moveIndex {
	out := make(moveIndex, len(in))
	for p, events := range in {
		out[p] = append([]pathEvent(nil), events...)
	}
	return out
}

func fileInfoFromOp(op journal.Op) FileInfo {
	return FileInfo{
		Blob: op.Blob, Size: op.Size, Time: op.Time, Link: op.Link,
		User: op.User, UserName: op.UserName, Author: op.Author,
		Device: op.DeviceName, Lamport: op.Lamport, Seq: op.Seq,
		DeviceID: op.Device, Mode: op.Mode,
	}
}

func foldFilesAndTombstones(ops []journal.Op) (map[string]FileInfo, map[string]winOp) {
	files := make(map[string]FileInfo)
	tombs := make(map[string]winOp)
	for _, op := range ops {
		switch op.Kind {
		case journal.KindPut:
			if op.Link == "" && !isBlobSHA(op.Blob) {
				continue
			}
			if tomb, ok := tombs[op.Path]; ok && !tomb.less(op) {
				continue
			}
			files[op.Path] = fileInfoFromOp(op)
		case journal.KindDelete:
			w := winOpOf(op)
			if old, ok := tombs[op.Path]; !ok || old.less(op) {
				tombs[op.Path] = w
			}
			if fi, ok := files[op.Path]; ok && journal.Less(fi.winOp(op.Path), op) {
				delete(files, op.Path)
			}
		}
	}
	return files, tombs
}

func applyFoldOp(files map[string]FileInfo, tombs map[string]winOp, op journal.Op) bool {
	switch op.Kind {
	case journal.KindDelete:
		if old, ok := tombs[op.Path]; ok && !old.less(op) {
			return false
		}
		tombs[op.Path] = winOpOf(op)
		if fi, ok := files[op.Path]; ok && journal.Less(fi.winOp(op.Path), op) {
			delete(files, op.Path)
		}
		return true
	case journal.KindPut:
		if op.Link == "" && !isBlobSHA(op.Blob) {
			return false
		}
		if tomb, ok := tombs[op.Path]; ok && !tomb.less(op) {
			return false
		}
		if fi, ok := files[op.Path]; ok && !journal.Less(fi.winOp(op.Path), op) {
			return false
		}
		files[op.Path] = fileInfoFromOp(op)
		return true
	}
	return false
}

func (r *RemoteSource) tombstoneFor(path string) (winOp, bool) {
	r.foldMu.Lock()
	defer r.foldMu.Unlock()
	w, ok := r.foldTombstones[path]
	return w, ok
}

func checkpointMeta(keep []remote.Object) map[string]ckptJournalMeta {
	out := make(map[string]ckptJournalMeta, len(keep))
	for _, o := range keep {
		out[o.Key] = ckptJournalMeta{Size: o.Size, ModTime: o.Modified}
	}
	return out
}

func (r *RemoteSource) checkpointFor(keep []remote.Object, files map[string]FileInfo, tombs map[string]winOp, moves moveIndex) (foldCheckpoint, bool) {
	cp := foldCheckpoint{Journals: checkpointMeta(keep), Files: cloneFiles(files), Tombstones: cloneTombstones(tombs), Moves: cloneMoves(moves)}
	r.jmu.Lock()
	defer r.jmu.Unlock()
	for _, o := range keep {
		c, ok := r.jcache[o.Key]
		if !ok || c.size != o.Size || !c.mod.Equal(o.Modified) {
			return foldCheckpoint{}, false
		}
		cp.Journals[o.Key] = ckptJournalMeta{
			Size: o.Size, ModTime: o.Modified, Ops: len(c.ops),
			PrefixSHA256: hex.EncodeToString(c.rawPrefixHash[:]),
		}
	}
	return cp, true
}

func (r *RemoteSource) scheduleFoldCheckpoint(_ context.Context, fp string, keep []remote.Object, files map[string]FileInfo, tombs map[string]winOp, moves moveIndex) {
	var total int64
	for _, o := range keep {
		total += o.Size
	}
	if total < minFoldCheckpointBytes {
		return
	}
	cp, ok := r.checkpointFor(keep, files, tombs, moves)
	if !ok {
		return
	}
	raw, err := encodeFoldCheckpoint(cp)
	if err != nil {
		return
	}
	r.ckptMu.Lock()
	if r.ckptWriting || r.ckptFP == fp {
		r.ckptMu.Unlock()
		return
	}
	r.ckptWriting, r.ckptFP = true, fp
	r.ckptMu.Unlock()
	go func() {
		err := r.Backend.Put(context.Background(), hubFoldCheckpointKey, bytes.NewReader(raw), int64(len(raw)))
		r.ckptMu.Lock()
		r.ckptWriting = false
		if err != nil {
			r.ckptFP = ""
		}
		r.ckptMu.Unlock()
	}()
}

func sameCheckpointJournals(cp map[string]ckptJournalMeta, keep []remote.Object) bool {
	if len(keep) != len(cp) {
		return false
	}
	for _, o := range keep {
		old, ok := cp[o.Key]
		if !ok || o.Size != old.Size || old.PrefixSHA256 == "" || (!o.Modified.IsZero() && !old.ModTime.IsZero() && !o.Modified.Equal(old.ModTime)) {
			return false
		}
	}
	return true
}

func (r *RemoteSource) verifyJournalSHA256(ctx context.Context, o remote.Object, wantHex string) bool {
	if wantHex == "" {
		return false
	}
	r.jmu.Lock()
	c, ok := r.jcache[o.Key]
	r.jmu.Unlock()
	if ok && c.size == o.Size && !o.Modified.IsZero() && c.mod.Equal(o.Modified) {
		return hex.EncodeToString(c.rawPrefixHash[:]) == wantHex
	}
	rc, err := r.Backend.Get(ctx, o.Key)
	if err != nil {
		return false
	}
	h := sha256.New()
	n, copyErr := io.Copy(h, io.LimitReader(rc, o.Size+1))
	closeErr := rc.Close()
	if copyErr != nil || closeErr != nil || n != o.Size {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == wantHex
}

func (r *RemoteSource) restoreFoldCheckpoint(ctx context.Context, keep []remote.Object, fp string) (map[string]FileInfo, map[string]winOp, moveIndex, bool) {
	var total int64
	for _, o := range keep {
		total += o.Size
	}
	if total < minFoldCheckpointBytes {
		return nil, nil, nil, false
	}
	rc, err := r.Backend.Get(ctx, hubFoldCheckpointKey)
	if err != nil {
		return nil, nil, nil, false
	}
	raw, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return nil, nil, nil, false
	}
	cp, err := decodeFoldCheckpoint(raw)
	if err != nil {
		return nil, nil, nil, false
	}
	if !sameCheckpointJournals(cp.Journals, keep) {
		return nil, nil, nil, false
	}
	for _, o := range keep {
		old := cp.Journals[o.Key]
		if !r.verifyJournalSHA256(ctx, o, old.PrefixSHA256) {
			return nil, nil, nil, false
		}
	}
	files, tombs, moves := cloneFiles(cp.Files), cloneTombstones(cp.Tombstones), cloneMoves(cp.Moves)
	updatedMeta := cloneJournalMetas(cp.Journals)
	r.foldMu.Lock()
	r.foldFP, r.foldFiles, r.foldTombstones, r.foldMoves, r.foldJournals = fp, files, tombs, moves, updatedMeta
	r.foldMu.Unlock()
	return files, tombs, moves, true
}

func cloneJournalMetas(in map[string]ckptJournalMeta) map[string]ckptJournalMeta {
	out := make(map[string]ckptJournalMeta, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (r *RemoteSource) tryIncrementalFold(ctx context.Context, keep []remote.Object, fp string) (map[string]FileInfo, map[string]winOp, moveIndex, bool) {
	r.foldMu.Lock()
	prevFiles, prevTombs, prevMoves, prevMeta := r.foldFiles, r.foldTombstones, r.foldMoves, cloneJournalMetas(r.foldJournals)
	r.foldMu.Unlock()
	if prevFiles == nil || len(prevMeta) == 0 {
		return nil, nil, nil, false
	}
	for _, o := range keep {
		old, ok := prevMeta[o.Key]
		if !ok || o.Size < old.Size || (o.Size == old.Size && !o.Modified.Equal(old.ModTime)) {
			return nil, nil, nil, false
		}
	}
	if len(prevMeta) != len(keep) {
		return nil, nil, nil, false
	}
	r.jmu.Lock()
	counts := make(map[string]int, len(keep))
	for _, o := range keep {
		c, ok := r.jcache[o.Key]
		if !ok || c.size != prevMeta[o.Key].Size || !c.mod.Equal(prevMeta[o.Key].ModTime) ||
			(prevMeta[o.Key].PrefixSHA256 != "" && hex.EncodeToString(c.rawPrefixHash[:]) != prevMeta[o.Key].PrefixSHA256) {
			r.jmu.Unlock()
			return nil, nil, nil, false
		}
		counts[o.Key] = len(c.ops)
	}
	r.jmu.Unlock()
	parsed, err := r.parsedJournals(ctx, keep)
	if err != nil {
		return nil, nil, nil, false
	}
	files, tombs := cloneFiles(prevFiles), cloneTombstones(prevTombs)
	for i, o := range keep {
		start := counts[o.Key]
		if start > len(parsed[i]) {
			return nil, nil, nil, false
		}
		for _, op := range parsed[i][start:] {
			if op.Kind == journal.KindDelete {
				return nil, nil, nil, false
			}
			if op.Kind == journal.KindPut {
				if _, hadTomb := tombs[op.Path]; hadTomb {
					return nil, nil, nil, false
				}
				if _, ok := files[op.Path]; !ok || !applyFoldOp(files, tombs, op) {
					return nil, nil, nil, false
				}
			}
		}
	}
	meta := checkpointMeta(keep)
	for _, o := range keep {
		r.jmu.Lock()
		c := r.jcache[o.Key]
		r.jmu.Unlock()
		if c.size == o.Size {
			m := meta[o.Key]
			m.Ops = len(c.ops)
			m.PrefixSHA256 = hex.EncodeToString(c.rawPrefixHash[:])
			meta[o.Key] = m
		}
	}
	r.foldMu.Lock()
	r.foldFP, r.foldFiles, r.foldTombstones, r.foldMoves, r.foldJournals = fp, files, tombs, prevMoves, meta
	r.foldMu.Unlock()
	return files, tombs, prevMoves, true
}
