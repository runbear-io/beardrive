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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
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
	Note string
	// Prune makes this cycle reconcile the hub against the shared ignore
	// rules: every path the remote still holds that .bdriveignore (or a
	// builtin never-sync rule) now excludes is journaled as a delete, so it
	// leaves the hub while staying on disk on every device. Off by default —
	// plain `bdrive sync` and the daemon never set it, because pruning must
	// be a deliberate act, never a side effect of editing .bdriveignore.
	Prune   bool
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
//
// Offline, ReadOnly, and NoAccess are three different answers and must not be
// conflated: offline means the hub could not be reached and everything should
// be retried; ReadOnly means it refused our push (we keep pulling, local ops
// stay journaled and unpushed); NoAccess means it refused our pull too, so the
// cycle does nothing at all and leaves the working folder alone. Regaining
// access self-heals on a later cycle with no manual step.
type Result struct {
	LocalOps     int  // local changes committed to the journal
	PulledOps    int  // ops received from other devices
	Conflicts    int  // conflict copies created
	Pruned       int  // paths removed from the hub by --prune (kept on disk)
	Materialized int  // files written/removed in the working folder
	Pushed       bool // own journal/blobs uploaded
	Offline      bool // remote configured but unreachable this cycle
	OfflineErr   error
	ReadOnly     bool // the hub refused our push: pull-only from here
	NoAccess     bool // the hub refused our pull: sync paused, nothing touched
	AccessErr    error
}

func (r *Result) Activity() bool {
	return r.LocalOps > 0 || r.PulledOps > 0 || r.Conflicts > 0 || r.Pruned > 0 || r.Materialized > 0
}

// The builtin exclusions (.bdrive — the mount's local identity, syncing it
// would let one device silently repoint another — and .git) are defined once
// in config, because the hub enforces the same set on the paths clients
// upload. Local aliases so the walk and the path checks below read plainly.
func ignoredDir(name string) bool  { return config.ReservedDir(name) }
func ignoredFile(name string) bool { return config.ReservedName(name) }

// maxLamport caps what this device's clock will absorb from a peer. Cycle
// raises st.Lamport to any value it pulls and scan increments it per local op,
// so one op carrying math.MaxInt64 wraps the clock negative and every op this
// device ever writes again sorts before everything it has already seen — a
// silent, permanent write lock installed by one line of JSON. A value this
// large is not a clock reading, so it is ignored rather than absorbed. Pulled
// ops are never rewritten: replay must agree between a device and its remote
// copy.
const maxLamport = int64(1) << 62

// absorbLamport advances the local clock to a peer's reading, ignoring absurd
// ones. tickLamport is the local increment, which stops at the ceiling rather
// than wrapping.
func absorbLamport(cur, peer int64) int64 {
	if peer > cur && peer <= maxLamport {
		return peer
	}
	return cur
}

// tickLamport is the local increment. The cap is on what this device ABSORBS,
// not on what it writes: a device that legitimately absorbed the ceiling must
// still be able to write an op that sorts after it, or the clock is frozen
// there forever and every later local edit falls through to Time — which the
// peer that sent the ceiling also chose. That is the same silent write lock
// the cap exists to prevent, reachable with the one value the cap accepts.
// Ticking past the ceiling is safe: it only ever climbs by one per local op,
// so only the wrap itself is refused.
func tickLamport(cur int64) int64 {
	if cur == math.MaxInt64 {
		return cur
	}
	return cur + 1
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
		switch {
		case err == nil:
		case errors.Is(err, remote.ErrForbidden):
			// Access to this project was revoked. Stop here: materializing a
			// replay we can no longer refresh would look like the hub
			// reverting the user's files. Nothing is pushed, nothing is
			// deleted, and the next cycle re-checks.
			res.NoAccess, res.AccessErr = true, err
			st.Access = store.AccessNone
			return res, s.finish(cache, st)
		default:
			res.Offline = true
			res.OfflineErr = err
		}
		res.PulledOps = len(pulled)
		for _, op := range pulled {
			st.Lamport = absorbLamport(st.Lamport, op.Lamport)
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

	// The ignore rules sync like any other file, so a peer can receive the new
	// .bdriveignore and the delete ops it justifies in the same batch. The
	// filter was loaded at the top of the cycle, before the pull, so write the
	// rules first and reload from them — otherwise materialize's delete loop
	// runs against stale rules, its filter guard never fires, and it unlinks
	// files that merely left sync scope.
	if want, ok := target[IgnoreFile]; ok && len(pulled) > 0 {
		wrote, err := s.materializeFile(IgnoreFile, want, cache)
		if err != nil {
			log.Printf("beardrive: could not write %s this cycle: %v", IgnoreFile, err)
		}
		if wrote {
			res.Materialized++
			// The reload rebuilds the rules from the new file — and only the
			// rules. Filter.nested is what walkFolder discovered during the
			// scan at the top of this cycle: it marks subfolders that sync
			// through their OWN project, with their own member list, so it is
			// a project boundary rather than an ignore rule. A fresh filter's
			// empty nested list would let this project's ops write into that
			// one, where its daemon picks them up and pushes them on.
			nested := filter.nested
			if filter, err = loadFilter(s.Folder, proj.Include); err != nil {
				return nil, fmt.Errorf("load %s: %w", IgnoreFile, err)
			}
			filter.nested = nested
		}
		// If the blob isn't fetched yet materializeFile skips it and the old
		// rules stand: the usual retry-next-cycle posture, and the guard in
		// materialize still protects the files either way.
	}

	// 4b. Prune: remove from the hub what the shared rules now exclude.
	if s.Prune {
		pruneOps, err := s.pruneOps(target, &st, int64(len(myOps)))
		if err != nil {
			return nil, err
		}
		if len(pruneOps) > 0 {
			if err := s.Store.AppendOps(s.Device.ID, pruneOps); err != nil {
				return nil, fmt.Errorf("append prune ops: %w", err)
			}
			myOps = append(myOps, pruneOps...)
			res.Pruned = len(pruneOps)
		}
	}

	n, err := s.materialize(target, cache, filter)
	if err != nil {
		return nil, fmt.Errorf("materialize: %w", err)
	}
	res.Materialized += n

	// 5. Push our blobs and journal.
	if s.Backend != nil && !res.Offline && int64(len(myOps)) > st.PushedOps {
		switch err := s.push(ctx, myOps, &st); {
		case err == nil:
			res.Pushed = true
		case errors.Is(err, remote.ErrForbidden):
			// Read-only on this project: pull and materialize already ran, so
			// pull-only is the steady state. Our own ops stay in the local
			// journal — never pushed, never dropped. The push is still
			// attempted once per remote interval (no hot loop, and a re-grant
			// self-heals).
			res.ReadOnly, res.AccessErr = true, err
		default:
			res.Offline = true
			res.OfflineErr = err
		}
	}

	// 6. Drain the agent read spool to the hub (read heatmap telemetry).
	// Strictly best-effort: a failed report keeps the batch queued for the
	// next cycle and never fails — or even marks offline — this one.
	if rr, ok := s.Backend.(remote.ReadReporter); ok && !res.Offline {
		if evs, err := s.Store.PendingReads(); err == nil && len(evs) > 0 {
			reads := make([]remote.ReadEvent, len(evs))
			for i, e := range evs {
				reads[i] = remote.ReadEvent{Path: e.Path, Time: e.Time}
			}
			if rr.ReportReads(ctx, reads) == nil {
				s.Store.ClearPendingReads()
			}
		}
	}

	st.Access = store.AccessOK
	if res.ReadOnly {
		st.Access = store.AccessReadOnly
	}
	if err := s.finish(cache, st); err != nil {
		return nil, err
	}
	return res, nil
}

// finish persists the two pieces of state a cycle mutates. Saving the cache
// matters even on a cut-short cycle: the scan already journaled local edits,
// and dropping the cache would make the next scan journal them all again.
func (s *Session) finish(cache map[string]store.CachedFile, st store.SyncState) error {
	if err := s.Store.SaveCache(s.mountID(), cache); err != nil {
		return err
	}
	return s.Store.SaveSync(st)
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
		st.Lamport = tickLamport(st.Lamport)
		seqBase++
		return journal.Op{
			Seq: seqBase, Lamport: st.Lamport, Time: time.Now().UTC(),
			Device: s.Device.ID, DeviceName: s.Device.Name, Author: s.Device.Author,
			User: s.Account.Email, UserName: s.Account.Name,
			Kind: kind, Path: rel, Note: note,
		}
	}

	err := walkFolder(s.Folder, filter, func(p, rel string, d fs.DirEntry, v verdict) error {
		if v != vSync {
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
		op.Mtime = info.ModTime().UTC()
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
		// Resume from a BYTE offset, never from an op count. A peer owns its
		// journal object and can rewrite it, and Parse drops a line it cannot
		// decode — so "skip the first len(prev) ops" let the peer choose how
		// far each device's cursor jumped: replace one already-counted line
		// with junk and every appended op shifts down by one, so a device that
		// synced earlier silently never sees it while a first-time device
		// does. Two devices, one journal, permanently different states.
		//
		// What we hold locally is the exact bytes we last accepted. If the
		// object still extends them, the new ops are what the extension parses
		// to; if it does not, the peer rewrote its log and every op in it is
		// treated as new. Re-applying ops is idempotent (Replay is a fold), so
		// the rewritten case is only ever slow, never divergent.
		local, err := os.ReadFile(lp)
		if err != nil && !os.IsNotExist(err) {
			return newOps, err
		}
		if bytes.Equal(local, data) {
			continue
		}
		tail := data
		if len(local) > 0 && bytes.HasPrefix(data, local) {
			tail = data[len(local):]
		}
		fresh, err := journal.Parse(tail)
		if err != nil {
			continue // corrupt remote journal; ignore rather than break sync
		}
		if err := store.WriteFileAtomic(lp, data, 0o644); err != nil {
			return newOps, err
		}
		newOps = append(newOps, fresh...)
	}

	// Fetch content for new ops. Blobs are uploaded before journals on push,
	// so anything referenced should exist — but Op.Blob is a string a peer
	// chose, so "missing" is a case this loop has to survive rather than a
	// contradiction. A blob that cannot be fetched is left unfetched:
	// materializeFile skips a path whose content is not in the store yet and
	// the next cycle retries, which is this package's posture for everything
	// transient. Abandoning the loop instead meant one op naming a blob that
	// was never pushed stopped every complete op behind it from ever landing.
	for _, op := range newOps {
		if op.Kind != journal.KindPut || op.Blob == "" || s.Store.HasBlob(op.Blob) {
			continue
		}
		rc, err := s.Backend.Get(ctx, "blobs/"+op.Blob)
		if err != nil {
			continue
		}
		sum, _, err := s.Store.PutBlobReader(rc)
		rc.Close()
		if err != nil {
			return newOps, err
		}
		if sum != op.Blob {
			return newOps, fmt.Errorf("blob %s corrupt on remote (got %s)", shortSha(op.Blob), shortSha(sum))
		}
	}
	return newOps, nil
}

// shortSha trims a blob string for a message. Op.Blob is arbitrary JSON off a
// peer's journal, not necessarily 64 hex characters, so slicing it directly
// panicked the daemon on every device that pulled the line.
func shortSha(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
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
		st.Lamport = tickLamport(st.Lamport)
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

// conflictName builds the copy's name. Both variable parts are bounded: the
// loser's DeviceName is an unvalidated string off a peer's journal, and the
// result has to be a name the filesystem accepts (NAME_MAX is 255 everywhere
// beardrive runs). An unwritable name is worse than an ugly one — the op is
// already in this device's own journal by the time the write is attempted, so
// it would replay and fail on every cycle from then on, triggered by one
// ordinary concurrent edit.
func conflictName(p, deviceName string, t time.Time) string {
	suffix := ".bdrive-conflict-" + clip(sanitize(deviceName), 32) + "-" + t.UTC().Format("20060102T150405Z")
	dir, base := path.Split(p)
	return dir + clip(base, 255-len(suffix)) + suffix
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
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
// A path that cannot be written is skipped, not fatal. Op.Path is a peer's
// string and the working folder is a real filesystem: a NUL byte, a 400-byte
// segment, a put for "docs/child.md" after a put for "docs", or a directory in
// the way are all ordinary refusals from the kernel. Failing the cycle on one
// of them wedged the device permanently — the op stays in the pulled journal,
// so every later cycle replayed it and died at the same line, and Cycle
// returns before finish() so the state cache was never saved either.
func (s *Session) materialize(target map[string]journal.FileState, cache map[string]store.CachedFile, filter *Filter) (int, error) {
	changed, skipped := 0, 0
	var firstErr error
	skip := func(err error) {
		skipped++
		if firstErr == nil {
			firstErr = err
		}
	}
	defer func() {
		if skipped > 0 {
			log.Printf("beardrive: %d path(s) could not be written this cycle (first: %v)", skipped, firstErr)
		}
	}()
	for rel, want := range target {
		// neverSync as well as the ignore filter: the builtin exclusions are
		// what keep .bdrive/ and .git/ off this device's disk, and an op
		// naming one arrives from a peer's journal — where the scan-side
		// check never ran. Writing it would let one device repoint another's
		// mount (or drop a git hook that runs on the next commit).
		if filter.Skip(rel) || neverSync(rel) {
			continue
		}
		wrote, err := s.materializeFile(rel, want, cache)
		if err != nil {
			skip(err)
			continue
		}
		if wrote {
			changed++
		}
	}

	for rel, c := range cache {
		if _, ok := target[rel]; ok {
			continue
		}
		if filter.Skip(rel) {
			// The path left sync scope rather than being deleted — someone
			// ignored it, or `--prune` removed it from the hub. Stop tracking
			// it; the file itself is ours to keep. Without this guard a prune
			// (or any delete op for a now-filtered path) unlinks every peer's
			// local copy, which is the data loss the feature exists to avoid.
			delete(cache, rel)
			continue
		}
		abs := filepath.Join(s.Folder, filepath.FromSlash(rel))
		if fi, err := os.Stat(abs); err == nil {
			if fi.Size() != c.Size || fi.ModTime().UnixNano() != c.MTimeNS {
				continue // dirty; do not delete fresh local edits
			}
			if err := os.Remove(abs); err != nil {
				skip(err)
				continue
			}
			pruneEmptyDirs(s.Folder, filepath.Dir(abs))
		}
		delete(cache, rel)
		changed++
	}
	return changed, nil
}

// materializeFile writes one path of the merged state into the working
// folder, reporting whether it wrote. It never clobbers a file that changed
// since the scan earlier in this cycle. Split out of materialize so the cycle
// can land .bdriveignore on its own, before the rules are needed.
func (s *Session) materializeFile(rel string, want journal.FileState, cache map[string]store.CachedFile) (bool, error) {
	want.Mode = safeMode(want.Mode) // before the cache compare, or every cycle rewrites
	c, ok := cache[rel]
	if ok && c.Blob == want.Blob && c.Mode == want.Mode {
		return false, nil
	}
	abs := filepath.Join(s.Folder, filepath.FromSlash(rel))
	if fi, err := os.Stat(abs); err == nil {
		if ok && (fi.Size() != c.Size || fi.ModTime().UnixNano() != c.MTimeNS) {
			return false, nil // dirty: changed mid-cycle, next scan commits it
		}
		if !ok {
			// Untracked file already at this path: adopt if identical,
			// otherwise leave it for the next scan to journal.
			sum, err := hashFile(abs)
			if err != nil || sum != want.Blob {
				return false, nil
			}
		}
	}
	if !s.Store.HasBlob(want.Blob) {
		return false, nil // content not fetched yet; retry next cycle
	}
	if err := s.writeFile(abs, want); err != nil {
		return false, fmt.Errorf("write %s: %w", rel, err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return false, err
	}
	cache[rel] = store.CachedFile{Blob: want.Blob, Size: fi.Size(), Mode: want.Mode, MTimeNS: fi.ModTime().UnixNano()}
	return true, nil
}

// pruneOps journals a delete for every path the hub still holds that the
// shared rules now exclude, and drops it from target so this cycle does not
// write it back. Peers keep their copies: the delete arrives alongside the
// rules that explain it, and materialize's filter guard turns it into
// "stop tracking" rather than "unlink".
//
// It reconciles against the replayed remote state, not the local cache. A
// path filtered out in some earlier cycle was dropped from the cache back
// then and is invisible locally today — which is exactly the leak --prune
// exists to clean up.
//
// The rules are deliberately ignore-only. .bdriveignore syncs, so every
// device agrees on it; the include list lives in this device's own
// .bdrive/config.json and does not sync. Never reuse the cycle's main filter
// here: a device with a legacy include-list scope would delete files a
// whole-folder teammate legitimately syncs.
func (s *Session) pruneOps(target map[string]journal.FileState, st *store.SyncState, seqBase int64) ([]journal.Op, error) {
	shared, err := loadFilter(s.Folder, nil) // nil include: shared rules only
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", IgnoreFile, err)
	}
	// The `!` refusal has to be made against the rules the prune is about to
	// APPLY. The CLI's pruneSafe reads .bdriveignore before the cycle; the
	// pull then materializes whatever version a peer pushed, and this reads it
	// again — two reads of two different files, so a teammate running `bdrive
	// scope`/`--only` (which writes exactly these rules) turned a cleared
	// prune into a hub-wide delete of everything outside their scope. No
	// malice required. The CLI check stays, as a nicer early error.
	if shared.Negated() {
		return nil, nil
	}
	var paths []string
	for rel := range target {
		if shared.Skip(rel) || neverSync(rel) {
			paths = append(paths, rel)
		}
	}
	sort.Strings(paths) // map order is random; keep the journal reproducible
	ops := make([]journal.Op, 0, len(paths))
	for _, rel := range paths {
		st.Lamport = tickLamport(st.Lamport)
		seqBase++
		ops = append(ops, journal.Op{
			Seq: seqBase, Lamport: st.Lamport, Time: time.Now().UTC(),
			Device: s.Device.ID, DeviceName: s.Device.Name, Author: s.Device.Author,
			User: s.Account.Email, UserName: s.Account.Name,
			Kind: journal.KindDelete, Path: rel,
			Note: "pruned: excluded by " + IgnoreFile,
		})
		delete(target, rel)
	}
	return ops, nil
}

// neverSync reports whether a path is one the scan walk never uploads at all
// — the builtin exclusions, which prune treats exactly like ignore rules — or
// one no journal may name at all (see unsafeRel). Every path that reaches the
// working folder routes through here.
func neverSync(rel string) bool {
	if unsafeRel(rel) {
		return true
	}
	parts := strings.Split(rel, "/")
	for _, dir := range parts[:len(parts)-1] {
		if ignoredDir(dir) {
			return true
		}
	}
	return ignoredFile(parts[len(parts)-1])
}

// unsafeRel reports whether an op's Path escapes the mount root. scan only
// ever produces clean relative paths, but Op.Path is arbitrary JSON off a
// peer's journal and materialize resolves it with filepath.Join(s.Folder, …),
// which walks above the root without complaint — one pushed line would reach
// ~/.ssh/authorized_keys on every teammate's machine. Anything that is not
// already a clean relative slash path is refused rather than normalized:
// normalizing would land two different journal paths on one file.
func unsafeRel(rel string) bool {
	return rel == "" || rel == ".." || strings.HasPrefix(rel, "../") ||
		path.IsAbs(rel) || filepath.IsAbs(rel) || path.Clean(rel) != rel
}

// safeMode is the only mode materialize will apply. scan records
// info.Mode().Perm(), but Op.Mode is a raw uint32 off the wire and
// fs.ModeSetuid/ModeSetgid live in that same word — os.Chmod would turn a
// peer's op into a setuid binary in every teammate's folder. Group/other write
// goes too: a synced file is never a drop box for other users on the machine.
func safeMode(m uint32) uint32 { return m & 0o777 &^ 0o022 }

func (s *Session) writeFile(abs string, want journal.FileState) error {
	// unsafeRel judged the path's SPELLING; this is the same boundary on disk.
	// MkdirAll, CreateTemp and Rename all follow symlinks, so a directory
	// inside the mount that is a symlink makes "docs/x.md" a perfectly clean
	// relative path landing outside the mount — and walkFolder refuses to
	// descend into one, so such a directory is a one-way door: it takes peer
	// writes and never reports them. Checked before MkdirAll, so a refused op
	// does not even build the parent chain on the far side.
	if !store.UnderRoot(s.Folder, abs) {
		return fmt.Errorf("resolves outside the mount root")
	}
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

// DisplayTime is the timestamp to show a human for an op: when the file was
// written if we know it, otherwise when the op was committed. Ops written
// before Op.Mtime existed, and deletes (no file left to stat), fall back.
func DisplayTime(op journal.Op) time.Time {
	// Clamped to the moment the op was journaled. Mtime comes off the
	// filesystem, so a peer chooses it: an op stamped in the year 9999 sits
	// above every real entry in `bdrive log` forever, and a handful of them
	// pushes the genuine history off a screen that prints 50 rows. Lagging
	// Time is legitimate (an old file, journaled today); leading it is not a
	// write time, it is a sort key someone picked.
	if !op.Mtime.IsZero() && !op.Mtime.After(op.Time) {
		return op.Mtime
	}
	return op.Time
}

// SortForDisplay orders ops newest-first by DisplayTime — the timestamp the
// user actually sees, so the list reads as a timeline. Ties fall back to
// reversed journal.Less to stay deterministic. This is deliberately NOT the
// replay order: LogEntries keeps returning causal order because
// bdrive restore walks it to find a file's previous version.
func SortForDisplay(ops []journal.Op) {
	sort.SliceStable(ops, func(i, j int) bool {
		ti, tj := DisplayTime(ops[i]), DisplayTime(ops[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return journal.Less(ops[j], ops[i])
	})
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
