// Package syncer drives a volume's sync cycle:
//
//	scan → commit local ops → pull peer journals → preserve conflicts →
//	materialize merged state → push blobs + own journal
//
// Scanning always happens before pulling, so local edits are committed to the
// journal (and their content captured in the blob store) before any remote
// state can overwrite the working folder. Concurrent edits resolve
// deterministically last-writer-wins; the losing local version is preserved
// as a "<name>.bdrive-conflict-<device>-<time>" file that syncs like any other.
package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/store"
)

// pushConcurrency bounds how many blobs upload at once. The initial import of
// many files is latency-bound on serial round-trips, so uploading in parallel
// is the main speedup.
const pushConcurrency = 16

// Progress reports upload progress during a cycle's push phase, so the CLI can
// draw a bar. Total/TotalBytes are set once when the push starts; Done/Bytes
// climb as blobs finish. Nil OnProgress means no reporting (the daemon).
type Progress struct {
	Done, Total    int
	Bytes, ToBytes int64
}

// Session ties a working folder to its volume store and (optionally) remote.
type Session struct {
	Folder  string
	MountID string // the stable project mount id from .bdrive/config.json
	Store   *store.Store
	Device  config.Device
	// Account is the signed-in user (from `bdrive login`); ops carry it so
	// history shows who changed what. Zero on offline/no-auth setups —
	// Device.Author remains the fallback identity.
	Account config.Settings
	// Note, when set, is stamped into every op this session commits — session
	// context like "claude-code session <id>". Empty means fall back to the
	// store's persisted session note (store.LoadNote), which lets a one-shot
	// `bdrive sync --note` leave context that the daemon's later scans also
	// stamp. Conflict-copy ops keep their own explanatory note.
	Note    string
	Backend remote.Backend // nil = work offline
	// OnProgress, when set, is called during push with upload progress. It may
	// be invoked concurrently from upload workers, so it must be safe to call
	// from multiple goroutines.
	OnProgress func(Progress)
}

func (s *Session) mountID() string {
	if s.MountID != "" {
		return s.MountID
	}
	// Fallback for sessions built without a project (tests): key the state
	// cache by the folder path.
	sum := sha256.Sum256([]byte(s.Folder))
	return hex.EncodeToString(sum[:])[:12]
}

// Result summarizes one sync cycle.
type Result struct {
	LocalOps     int  // local changes committed to the journal
	PulledOps    int  // ops received from other devices
	Conflicts    int  // conflict copies created
	Materialized int  // files written/removed in the working folder
	Pushed       bool // own journal/blobs uploaded
	Offline      bool // remote configured but unreachable this cycle
	OfflineErr   error
}

func (r *Result) Activity() bool {
	return r.LocalOps > 0 || r.PulledOps > 0 || r.Conflicts > 0 || r.Materialized > 0
}

// The .bdrive settings dir (config.ProjectDir) never syncs: it is the
// mount's local identity, and syncing it would let one device silently
// repoint another.
var ignoreNames = map[string]bool{".DS_Store": true}
var ignoreDirs = map[string]bool{".git": true, config.ProjectDir: true}

func ignoredFile(name string) bool {
	return ignoreNames[name] || strings.HasPrefix(name, ".bdrive-tmp-")
}

// Cycle runs one full scan/sync/materialize pass under the volume lock.
func (s *Session) Cycle(ctx context.Context) (*Result, error) {
	unlock, err := s.Store.Lock()
	if err != nil {
		return nil, fmt.Errorf("lock volume: %w", err)
	}
	defer unlock()

	res := &Result{}
	cache, err := s.Store.LoadCache(s.mountID())
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	st, err := s.Store.LoadSync()
	if err != nil {
		return nil, fmt.Errorf("load sync state: %w", err)
	}
	myOps, err := s.Store.DeviceOps(s.Device.ID)
	if err != nil {
		return nil, fmt.Errorf("read own journal: %w", err)
	}
	proj, _, err := config.LoadProject(s.Folder)
	if err != nil {
		return nil, err
	}
	filter, err := loadFilter(s.Folder, proj.Include)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", IgnoreFile, err)
	}

	// 1. Scan the working folder and journal any local changes.
	localOps, err := s.scan(cache, &st, int64(len(myOps)), filter)
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	if len(localOps) > 0 {
		if err := s.Store.AppendOps(s.Device.ID, localOps); err != nil {
			return nil, fmt.Errorf("append journal: %w", err)
		}
		myOps = append(myOps, localOps...)
		res.LocalOps = len(localOps)
	}

	// 2. Pull journals + blobs from other devices.
	var pulled []journal.Op
	if s.Backend != nil {
		pulled, err = s.pull(ctx)
		if err != nil {
			res.Offline = true
			res.OfflineErr = err
		}
		res.PulledOps = len(pulled)
		for _, op := range pulled {
			if op.Lamport > st.Lamport {
				st.Lamport = op.Lamport
			}
		}
	}

	// 3. Preserve losing local edits as conflict copies.
	if len(pulled) > 0 {
		conflictOps, err := s.conflictCopies(myOps, st.PushedOps, pulled, &st)
		if err != nil {
			return nil, err
		}
		if len(conflictOps) > 0 {
			if err := s.Store.AppendOps(s.Device.ID, conflictOps); err != nil {
				return nil, fmt.Errorf("append conflict ops: %w", err)
			}
			myOps = append(myOps, conflictOps...)
			res.Conflicts = len(conflictOps)
		}
	}

	// 4. Materialize the merged state into the working folder.
	all, err := s.Store.AllOps()
	if err != nil {
		return nil, fmt.Errorf("read journals: %w", err)
	}
	target := journal.Replay(all)
	n, err := s.materialize(target, cache, filter)
	if err != nil {
		return nil, fmt.Errorf("materialize: %w", err)
	}
	res.Materialized = n

	// 5. Push our blobs and journal.
	if s.Backend != nil && !res.Offline && int64(len(myOps)) > st.PushedOps {
		if err := s.push(ctx, myOps, &st); err != nil {
			res.Offline = true
			res.OfflineErr = err
		} else {
			res.Pushed = true
		}
	}

	if err := s.Store.SaveCache(s.mountID(), cache); err != nil {
		return nil, err
	}
	if err := s.Store.SaveSync(st); err != nil {
		return nil, err
	}
	return res, nil
}

// scan diffs the working folder against the state cache and returns ops for
// every local change, storing new content in the blob store. Filtered paths
// are neither journaled nor deleted: a path that becomes ignored is dropped
// from the cache without a delete op, so opting out locally never removes
// the file from other devices.
func (s *Session) scan(cache map[string]store.CachedFile, st *store.SyncState, seqBase int64, filter *Filter) ([]journal.Op, error) {
	seen := make(map[string]bool, len(cache))
	var ops []journal.Op
	note := s.Note
	if note == "" {
		note = s.Store.LoadNote()
	}
	nextOp := func(kind, rel string) journal.Op {
		st.Lamport++
		seqBase++
		return journal.Op{
			Seq: seqBase, Lamport: st.Lamport, Time: time.Now().UTC(),
			Device: s.Device.ID, DeviceName: s.Device.Name, Author: s.Device.Author,
			User: s.Account.Email, UserName: s.Account.Name,
			Kind: kind, Path: rel, Note: note,
		}
	}

	err := filepath.WalkDir(s.Folder, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip unreadable entries
		}
		rel, err := filepath.Rel(s.Folder, p)
		if err != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if ignoreDirs[d.Name()] || filter.PruneDir(rel) {
				return fs.SkipDir
			}
			if config.IsMount(p) {
				// A mount of its own: it syncs through its own project.
				filter.addNestedMount(rel)
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || ignoredFile(d.Name()) || filter.Skip(rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		seen[rel] = true
		size, mt := info.Size(), info.ModTime().UnixNano()
		mode := uint32(info.Mode().Perm())
		c, ok := cache[rel]
		if ok && c.Size == size && c.MTimeNS == mt {
			return nil // unchanged (cheap path)
		}
		sum, n, err := s.Store.PutBlobFile(p)
		if err != nil {
			return nil // file vanished or unreadable; next cycle
		}
		if ok && c.Blob == sum {
			// content unchanged, just touched
			c.Size, c.MTimeNS, c.Mode = n, mt, mode
			cache[rel] = c
			return nil
		}
		op := nextOp(journal.KindPut, rel)
		op.Blob, op.Size, op.Mode = sum, n, mode
		ops = append(ops, op)
		cache[rel] = store.CachedFile{Blob: sum, Size: n, Mode: mode, MTimeNS: mt}
		return nil
	})
	if err != nil {
		return nil, err
	}

	for rel := range cache {
		if seen[rel] {
			continue
		}
		if filter.Skip(rel) {
			delete(cache, rel) // newly filtered, not deleted: stop tracking silently
			continue
		}
		ops = append(ops, nextOp(journal.KindDelete, rel))
		delete(cache, rel)
	}
	return ops, nil
}

// pull fetches journals that grew on the remote and any blobs we are missing
// for the new ops. Returns only the ops we had not seen before.
func (s *Session) pull(ctx context.Context) ([]journal.Op, error) {
	objs, err := s.Backend.List(ctx, "journal/")
	if err != nil {
		return nil, err
	}
	var newOps []journal.Op
	for _, o := range objs {
		name := strings.TrimPrefix(o.Key, "journal/")
		if !strings.HasSuffix(name, ".jsonl") || strings.Contains(name, "/") {
			continue
		}
		dev := strings.TrimSuffix(name, ".jsonl")
		if dev == s.Device.ID {
			continue
		}
		lp := s.Store.JournalPath(dev)
		var localSize int64
		if fi, err := os.Stat(lp); err == nil {
			localSize = fi.Size()
		}
		if o.Size <= localSize && localSize > 0 {
			continue
		}
		rc, err := s.Backend.Get(ctx, o.Key)
		if err != nil {
			return newOps, err
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return newOps, err
		}
		fresh, err := journal.Parse(data)
		if err != nil {
			continue // corrupt remote journal; ignore rather than break sync
		}
		prev, err := s.Store.DeviceOps(dev)
		if err != nil {
			return newOps, err
		}
		if len(fresh) <= len(prev) {
			continue
		}
		if err := store.WriteFileAtomic(lp, data, 0o644); err != nil {
			return newOps, err
		}
		newOps = append(newOps, fresh[len(prev):]...)
	}

	// Fetch content for new ops. Blobs are uploaded before journals on push,
	// so anything referenced should exist.
	for _, op := range newOps {
		if op.Kind != journal.KindPut || op.Blob == "" || s.Store.HasBlob(op.Blob) {
			continue
		}
		rc, err := s.Backend.Get(ctx, "blobs/"+op.Blob)
		if err != nil {
			return newOps, fmt.Errorf("fetch blob %s: %w", op.Blob[:12], err)
		}
		sum, _, err := s.Store.PutBlobReader(rc)
		rc.Close()
		if err != nil {
			return newOps, err
		}
		if sum != op.Blob {
			return newOps, fmt.Errorf("blob %s corrupt on remote (got %s)", op.Blob[:12], sum[:12])
		}
	}
	return newOps, nil
}

// conflictCopies detects paths edited concurrently — we hold a not-yet-pushed
// op and just pulled a competing op for the same path. Last-writer-wins
// resolves the path itself deterministically; here the device that observed
// the concurrency preserves the losing version (ours or the pulled one) as a
// conflict-copy file so no content is silently dropped.
func (s *Session) conflictCopies(myOps []journal.Op, pushed int64, pulled []journal.Op, st *store.SyncState) ([]journal.Op, error) {
	if pushed > int64(len(myOps)) {
		pushed = int64(len(myOps))
	}
	unpushed := map[string]journal.Op{}
	for _, op := range myOps[pushed:] {
		unpushed[op.Path] = op // latest local op per path
	}
	pulledLatest := map[string]journal.Op{}
	for _, op := range pulled {
		if _, ok := unpushed[op.Path]; !ok {
			continue
		}
		if prev, ok := pulledLatest[op.Path]; !ok || journal.Less(prev, op) {
			pulledLatest[op.Path] = op
		}
	}
	if len(pulledLatest) == 0 {
		return nil, nil
	}
	all, err := s.Store.AllOps()
	if err != nil {
		return nil, err
	}
	state := journal.Replay(all)
	seqBase := int64(len(myOps))
	var out []journal.Op
	for p, theirs := range pulledLatest {
		mine := unpushed[p]
		cur, exists := state[p]
		mineWon := (mine.Kind == journal.KindPut && exists && cur.Blob == mine.Blob) ||
			(mine.Kind == journal.KindDelete && !exists)
		loser := mine
		if mineWon {
			loser = theirs
		}
		if loser.Kind != journal.KindPut || loser.Blob == "" {
			continue // a lost delete needs no preservation
		}
		if exists && cur.Blob == loser.Blob {
			continue // identical content; nothing actually lost
		}
		if !s.Store.HasBlob(loser.Blob) {
			continue // content unavailable (partial pull); skip rather than fail
		}
		st.Lamport++
		seqBase++
		out = append(out, journal.Op{
			Seq: seqBase, Lamport: st.Lamport, Time: time.Now().UTC(),
			Device: s.Device.ID, DeviceName: s.Device.Name, Author: s.Device.Author,
			User: s.Account.Email, UserName: s.Account.Name,
			Kind: journal.KindPut, Path: conflictName(p, loser.DeviceName, loser.Time),
			Blob: loser.Blob, Size: loser.Size, Mode: loser.Mode,
			Note: "conflict copy of " + p,
		})
	}
	return out, nil
}

func conflictName(p, deviceName string, t time.Time) string {
	return p + ".bdrive-conflict-" + sanitize(deviceName) + "-" + t.UTC().Format("20060102T150405Z")
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, s)
}

// materialize applies the merged state to the working folder, never
// clobbering files that changed since the scan earlier in this cycle.
// Filtered paths are not written: other devices' files that match the local
// ignore/include rules simply don't appear here.
func (s *Session) materialize(target map[string]journal.FileState, cache map[string]store.CachedFile, filter *Filter) (int, error) {
	changed := 0
	for rel, want := range target {
		if filter.Skip(rel) {
			continue
		}
		c, ok := cache[rel]
		if ok && c.Blob == want.Blob && c.Mode == want.Mode {
			continue
		}
		abs := filepath.Join(s.Folder, filepath.FromSlash(rel))
		if fi, err := os.Stat(abs); err == nil {
			if ok && (fi.Size() != c.Size || fi.ModTime().UnixNano() != c.MTimeNS) {
				continue // dirty: changed mid-cycle, next scan commits it
			}
			if !ok {
				// Untracked file already at this path: adopt if identical,
				// otherwise leave it for the next scan to journal.
				sum, err := hashFile(abs)
				if err != nil || sum != want.Blob {
					continue
				}
			}
		}
		if !s.Store.HasBlob(want.Blob) {
			continue // content not fetched yet; retry next cycle
		}
		if err := s.writeFile(abs, want); err != nil {
			return changed, fmt.Errorf("write %s: %w", rel, err)
		}
		fi, err := os.Stat(abs)
		if err != nil {
			return changed, err
		}
		cache[rel] = store.CachedFile{Blob: want.Blob, Size: fi.Size(), Mode: want.Mode, MTimeNS: fi.ModTime().UnixNano()}
		changed++
	}

	for rel, c := range cache {
		if _, ok := target[rel]; ok {
			continue
		}
		abs := filepath.Join(s.Folder, filepath.FromSlash(rel))
		if fi, err := os.Stat(abs); err == nil {
			if fi.Size() != c.Size || fi.ModTime().UnixNano() != c.MTimeNS {
				continue // dirty; do not delete fresh local edits
			}
			if err := os.Remove(abs); err != nil {
				return changed, err
			}
			pruneEmptyDirs(s.Folder, filepath.Dir(abs))
		}
		delete(cache, rel)
		changed++
	}
	return changed, nil
}

func (s *Session) writeFile(abs string, want journal.FileState) error {
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	src, err := s.Store.OpenBlob(want.Blob)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".bdrive-tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	mode := os.FileMode(want.Mode)
	if mode == 0 {
		mode = 0o644
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), abs)
}

// push uploads blobs referenced by unpushed ops, then the journal itself.
// Blob-before-journal ordering means peers never see an op whose content is
// missing.
func (s *Session) push(ctx context.Context, myOps []journal.Op, st *store.SyncState) error {
	if st.PushedOps > int64(len(myOps)) {
		st.PushedOps = int64(len(myOps))
	}
	// Collect the unique, not-yet-pushed blobs to upload (deduped by content
	// hash). The backend's Put is idempotent and already skips content that's
	// present remotely (the hub reports it during signing), so we don't pay a
	// separate existence round-trip per blob.
	seen := map[string]bool{}
	type blobJob struct {
		blob string
		size int64
	}
	var jobs []blobJob
	var totalBytes int64
	for _, op := range myOps[st.PushedOps:] {
		if op.Kind != journal.KindPut || op.Blob == "" || seen[op.Blob] {
			continue
		}
		seen[op.Blob] = true
		jobs = append(jobs, blobJob{op.Blob, op.Size})
		totalBytes += op.Size
	}

	var done, bytesDone int64
	report := func() {
		if s.OnProgress != nil {
			s.OnProgress(Progress{
				Done: int(atomic.LoadInt64(&done)), Total: len(jobs),
				Bytes: atomic.LoadInt64(&bytesDone), ToBytes: totalBytes,
			})
		}
	}
	report() // announce the total up front (0 / N)

	// Upload blobs in parallel — the initial import is bound on serial
	// round-trips, not bandwidth, so concurrency is the win.
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(pushConcurrency)
	for _, j := range jobs {
		g.Go(func() error {
			f, err := s.Store.OpenBlob(j.blob)
			if err != nil {
				return err
			}
			fi, err := f.Stat()
			if err != nil {
				f.Close()
				return err
			}
			err = s.Backend.Put(gctx, "blobs/"+j.blob, f, fi.Size())
			f.Close()
			if err != nil {
				return err
			}
			atomic.AddInt64(&done, 1)
			atomic.AddInt64(&bytesDone, fi.Size())
			report()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	jp := s.Store.JournalPath(s.Device.ID)
	f, err := os.Open(jp)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if err := s.Backend.Put(ctx, "journal/"+s.Device.ID+".jsonl", f, fi.Size()); err != nil {
		return err
	}
	st.PushedOps = int64(len(myOps))
	return nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func pruneEmptyDirs(root, dir string) {
	root = filepath.Clean(root)
	for {
		dir = filepath.Clean(dir)
		if dir == root || !strings.HasPrefix(dir, root+string(filepath.Separator)) {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// LogEntries returns the volume history, newest first.
func LogEntries(st *store.Store, pathFilter string, limit int) ([]journal.Op, error) {
	all, err := st.AllOps()
	if err != nil {
		return nil, err
	}
	journal.Sort(all)
	// reverse
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	if pathFilter != "" {
		filtered := all[:0]
		for _, op := range all {
			if op.Path == pathFilter || strings.HasPrefix(op.Path, pathFilter+"/") || path.Dir(op.Path) == pathFilter {
				filtered = append(filtered, op)
			}
		}
		all = filtered
	}
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}
