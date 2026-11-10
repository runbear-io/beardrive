package webapp

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"io"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

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

func (r *RemoteSource) journalMetaFromCache(keep []remote.Object) (map[string]ckptJournalMeta, bool) {
	out := checkpointMeta(keep)
	r.jmu.Lock()
	defer r.jmu.Unlock()
	for _, o := range keep {
		c, ok := r.jcache[o.Key]
		if !ok || c.size != o.Size || !c.mod.Equal(o.Modified) {
			return nil, false
		}
		out[o.Key] = ckptJournalMeta{
			Size: o.Size, ModTime: o.Modified, Ops: len(c.ops),
			PrefixSHA256: hex.EncodeToString(c.rawPrefixHash[:]),
		}
	}
	return out, true
}

func (r *RemoteSource) checkpointFor(keep []remote.Object, files map[string]FileInfo, tombs map[string]winOp, moves moveIndex) (foldCheckpoint, bool) {
	meta, ok := r.journalMetaFromCache(keep)
	if !ok {
		return foldCheckpoint{}, false
	}
	return foldCheckpoint{
		Journals: meta, Files: cloneFiles(files), Tombstones: cloneTombstones(tombs), Moves: cloneMoves(moves),
	}, true
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
	r.scheduleFoldCheckpointWrite(fp, cp)
}

func (r *RemoteSource) scheduleFoldCheckpointFromMeta(fp string, meta map[string]ckptJournalMeta, files map[string]FileInfo, tombs map[string]winOp, moves moveIndex) {
	var total int64
	for _, m := range meta {
		if m.PrefixSHA256 == "" {
			return
		}
		total += m.Size
	}
	if total < minFoldCheckpointBytes {
		return
	}
	cp := foldCheckpoint{
		Journals: cloneJournalMetas(meta), Files: cloneFiles(files), Tombstones: cloneTombstones(tombs), Moves: cloneMoves(moves),
	}
	r.scheduleFoldCheckpointWrite(fp, cp)
}

const minCheckpointDeltaBytes int64 = 256 << 10

func (r *RemoteSource) shouldRefreshCheckpoint(meta map[string]ckptJournalMeta) bool {
	var cur int64
	for _, m := range meta {
		cur += m.Size
	}
	r.ckptMu.Lock()
	defer r.ckptMu.Unlock()
	return r.ckptBytes == 0 || cur-r.ckptBytes >= minCheckpointDeltaBytes
}

func (r *RemoteSource) scheduleFoldCheckpointWrite(fp string, cp foldCheckpoint) {
	var total int64
	for _, m := range cp.Journals {
		total += m.Size
	}
	r.ckptMu.Lock()
	if r.ckptWriting || r.ckptFP == fp {
		r.ckptMu.Unlock()
		return
	}
	r.ckptWriting = true
	r.ckptFP = fp
	r.ckptBytes = total
	r.ckptMu.Unlock()

	go func() {
		raw, err := encodeFoldCheckpoint(cp)
		if err == nil {
			err = r.Backend.Put(context.Background(), hubFoldCheckpointKey, bytes.NewReader(raw), int64(len(raw)))
		}
		r.ckptMu.Lock()
		r.ckptWriting = false
		if err != nil {
			r.ckptFP = ""
		}
		r.ckptMu.Unlock()
	}()
}

func compatibleCheckpointJournals(cp map[string]ckptJournalMeta, keep []remote.Object) bool {
	if len(keep) != len(cp) {
		return false
	}
	for _, o := range keep {
		old, ok := cp[o.Key]
		if !ok || o.Size < old.Size || old.PrefixSHA256 == "" {
			return false
		}
		if o.Size == old.Size && !o.Modified.IsZero() && !old.ModTime.IsZero() && !o.Modified.Equal(old.ModTime) {
			return false
		}
	}
	return true
}

// verifyAndReadTail streams o.Key to verify that the prefix [0, old.Size) hashes
// to old.PrefixSHA256 (and ends with a newline when o.Size > old.Size), then
// reads and parses only the appended tail [old.Size, o.Size).
func (r *RemoteSource) verifyAndReadTail(ctx context.Context, o remote.Object, old ckptJournalMeta, alwaysVerifyPrefix bool) (tailOps []journal.Op, newFullSHA string, ok bool) {
	if old.PrefixSHA256 == "" || o.Size < old.Size {
		return nil, "", false
	}
	if _, hasCAS := r.Backend.(remote.ConditionalPutter); !hasCAS {
		alwaysVerifyPrefix = true
	}
	r.jmu.Lock()
	c, inJCache := r.jcache[o.Key]
	r.jmu.Unlock()
	if inJCache && c.size == old.Size && !o.Modified.IsZero() && c.mod.Equal(old.ModTime) {
		if hex.EncodeToString(c.rawPrefixHash[:]) != old.PrefixSHA256 {
			return nil, "", false
		}
		if o.Size == old.Size && o.Modified.Equal(old.ModTime) {
			return nil, old.PrefixSHA256, true
		}
	} else if !alwaysVerifyPrefix && o.Size == old.Size && !o.Modified.IsZero() && !old.ModTime.IsZero() && o.Modified.Equal(old.ModTime) {
		return nil, old.PrefixSHA256, true
	}

	if o.Size > old.Size {
		if vrg, ok := r.Backend.(remote.VersionedRangeGetter); ok {
			if trc, ver, verr := vrg.GetVersionedRange(ctx, o.Key, old.Size, o.Size-old.Size); verr == nil {
				if fullSHA, ok := lookupAppendedJournalSHA(o.Key, ver, old.Size, old.PrefixSHA256, o.Size); ok {
					tailBytes, rerr := io.ReadAll(io.LimitReader(trc, (o.Size-old.Size)+1))
					_ = trc.Close()
					if rerr == nil && int64(len(tailBytes)) == o.Size-old.Size {
						if ops, perr := journal.Parse(tailBytes); perr == nil && len(ops) > 0 {
							return ops, fullSHA, true
						}
					}
				} else {
					_ = trc.Close()
				}
			}
		}
	}

	rc, err := r.Backend.Get(ctx, o.Key)
	if err != nil {
		return nil, "", false
	}
	defer rc.Close()

	h := sha256.New()
	if old.Size > 0 {
		if o.Size > old.Size {
			if old.Size > 1 {
				n, cerr := io.CopyN(h, rc, old.Size-1)
				if cerr != nil || n != old.Size-1 {
					return nil, "", false
				}
			}
			var last [1]byte
			if _, rerr := io.ReadFull(rc, last[:]); rerr != nil || last[0] != '\n' {
				return nil, "", false
			}
			_, _ = h.Write(last[:])
		} else {
			n, cerr := io.CopyN(h, rc, old.Size)
			if cerr != nil || n != old.Size {
				return nil, "", false
			}
		}
	}
	if hex.EncodeToString(h.Sum(nil)) != old.PrefixSHA256 {
		return nil, "", false
	}
	if o.Size == old.Size {
		var extra [1]byte
		if n, _ := rc.Read(extra[:]); n != 0 {
			return nil, "", false
		}
		return nil, old.PrefixSHA256, true
	}
	tailLen := o.Size - old.Size
	tailBytes, err := io.ReadAll(io.LimitReader(rc, tailLen+1))
	if err != nil || int64(len(tailBytes)) != tailLen {
		return nil, "", false
	}
	_, _ = h.Write(tailBytes)
	ops, err := journal.Parse(tailBytes)
	if err != nil || len(ops) == 0 {
		return nil, "", false
	}
	return ops, hex.EncodeToString(h.Sum(nil)), true
}

func (r *RemoteSource) verifyAndReadTails(ctx context.Context, keep []remote.Object, prevMeta map[string]ckptJournalMeta, alwaysVerifyPrefix bool) ([][]journal.Op, map[string]ckptJournalMeta, bool, bool) {
	if !compatibleCheckpointJournals(prevMeta, keep) {
		return nil, nil, false, false
	}
	tails := make([][]journal.Op, len(keep))
	updatedMeta := make(map[string]ckptJournalMeta, len(keep))
	var metaMu sync.Mutex
	grew := false

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(journalFetchConcurrency)
	for i, o := range keep {
		i, o := i, o
		old := prevMeta[o.Key]
		if o.Size > old.Size {
			grew = true
		}
		g.Go(func() error {
			ops, fullSHA, ok := r.verifyAndReadTail(gctx, o, old, alwaysVerifyPrefix)
			if !ok {
				return io.ErrUnexpectedEOF
			}
			tails[i] = ops
			metaMu.Lock()
			updatedMeta[o.Key] = ckptJournalMeta{
				Size:         o.Size,
				ModTime:      o.Modified,
				Ops:          old.Ops + len(ops),
				PrefixSHA256: fullSHA,
			}
			metaMu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, nil, false, false
	}
	return tails, updatedMeta, grew, true
}

func applyTailSlices(files map[string]FileInfo, tombs map[string]winOp, tails [][]journal.Op) bool {
	var merged []journal.Op
	for _, ops := range tails {
		merged = append(merged, ops...)
	}
	if len(merged) > 1 {
		sort.SliceStable(merged, func(i, j int) bool { return journal.Less(merged[i], merged[j]) })
	}
	for _, op := range merged {
		if op.Kind != journal.KindPut {
			return false
		}
		if _, ok := files[op.Path]; !ok || !applyFoldOp(files, tombs, op) {
			return false
		}
	}
	return true
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
	tails, updatedMeta, grew, ok := r.verifyAndReadTails(ctx, keep, cp.Journals, true)
	if !ok {
		return nil, nil, nil, false
	}
	files, tombs, moves := cloneFiles(cp.Files), cloneTombstones(cp.Tombstones), cloneMoves(cp.Moves)
	if grew && !applyTailSlices(files, tombs, tails) {
		return nil, nil, nil, false
	}
	var baseBytes int64
	for _, m := range cp.Journals {
		baseBytes += m.Size
	}
	r.ckptMu.Lock()
	if r.ckptBytes == 0 {
		r.ckptBytes = baseBytes
	}
	r.ckptMu.Unlock()
	r.foldMu.Lock()
	r.foldFP, r.foldFiles, r.foldTombstones, r.foldMoves, r.foldJournals = fp, files, tombs, moves, updatedMeta
	r.foldMu.Unlock()
	if grew && r.shouldRefreshCheckpoint(updatedMeta) {
		r.scheduleFoldCheckpointFromMeta(fp, updatedMeta, files, tombs, moves)
	}
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
	tails, updatedMeta, grew, ok := r.verifyAndReadTails(ctx, keep, prevMeta, false)
	if !ok {
		return nil, nil, nil, false
	}
	files, tombs := cloneFiles(prevFiles), cloneTombstones(prevTombs)
	if !applyTailSlices(files, tombs, tails) {
		return nil, nil, nil, false
	}
	r.foldMu.Lock()
	r.foldFP, r.foldFiles, r.foldTombstones, r.foldMoves, r.foldJournals = fp, files, tombs, prevMoves, updatedMeta
	r.foldMu.Unlock()
	if grew && r.shouldRefreshCheckpoint(updatedMeta) {
		r.scheduleFoldCheckpointFromMeta(fp, updatedMeta, files, tombs, prevMoves)
	}
	return files, tombs, prevMoves, true
}
