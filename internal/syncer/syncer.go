// Package syncer drives a volume's sync cycle:
//
//	re-derive the clock → scan → commit local ops → pull peer journals →
//	preserve conflicts → materialize merged state → push blobs + own journal
//
// The cycle opens by folding every op already in the volume's journal dir
// through absorbLamport. Nothing else re-derives the clock — a pull absorbs
// only the new tail it fetched, and a fully-downloaded peer journal yields
// none — so a device whose stored clock fell below the journals beside it
// would stay there forever, and every op it wrote would lose replay.
//
// Scanning always happens before pulling, so local edits are committed to the
// journal (and their content captured in the blob store) before any remote
// state can overwrite the working folder. Concurrent edits resolve
// deterministically last-writer-wins; the losing local version is preserved
// as a "<name>.bdrive-conflict-<device>-<time>" file that syncs like any other.
//
// The exception is a device's FIRST cycle on a volume, which is a join rather
// than an edit: a local file at a path the project already holds is adopted —
// the project's version wins everywhere and no conflict copy is made — while
// the local content is still journaled (below every clock the project can hold)
// so history keeps it. See step 1b in Cycle.
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
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/secrets"
	"github.com/runbear-io/beardrive/internal/store"
)

// pushConcurrency bounds how many blobs upload at once. The initial import of
// many files is latency-bound on serial round-trips, so uploading in parallel
// is the main speedup.
const pushConcurrency = 16

// pullConcurrency is pushConcurrency's mirror for fetchBlobs: a peer's large
// batch of real changes is just as latency-bound on the download side.
const pullConcurrency = 16

// Progress reports upload progress during a cycle's push phase, so the CLI can
// draw a bar. Total/TotalBytes are set once when the push starts; Done/Bytes
// climb as blobs finish. Nil OnProgress means no reporting (the daemon).
type Progress struct {
	Done, Total    int
	Bytes, ToBytes int64
}

// Session ties a working folder to its volume store and (optionally) remote.
type Session struct {
	Folder         string
	MountID        string // the stable project mount id from .bdrive/config.json
	ExpectedRemote string
	Store          *store.Store
	Device         config.Device
	// Account is the signed-in user (from `bdrive login`); ops carry it so
	// history shows who changed what. Zero on offline/no-auth setups —
	// Device.Author remains the fallback identity.
	Account config.Settings
	// Note, when set, is stamped into every op this session commits — session
	// context like "claude-code session <id>". Empty means fall back to the
	// store's persisted mount note (store.LoadNoteForMount), which lets a one-shot
	// `bdrive sync --note` leave context that the daemon's later scans also
	// stamp. Conflict-copy ops keep their own explanatory note.
	Note string
	// SessionID is the agent session every op this cycle commits is stamped
	// with (journal.Op.Session). Set only by `bdrive sync --hook`, and
	// deliberately NOT persisted the way Note is (store.SaveNote): the note
	// is context that outlives the hook turn, the session id is an identity
	// that must not be attached to changes the daemon commits on its own
	// later. So a daemon scan after the hook turn carries the note and no
	// session — the asymmetry is intended.
	SessionID string
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

	// readOnly are the folder prefixes the hub says this account may sync down
	// but never journal a change to (docs/folder-permissions-prd.md). Loaded
	// from the hub each cycle and persisted in SyncState, so an offline cycle
	// keeps honouring the last answer. Empty for a raw object-store remote,
	// for a hub with no folder rules, and for a hub too old to have them.
	readOnly []string
	// denied are prefixes this account may not read at all. Nothing
	// materializes there (the hub sends no ops), so this exists only to stop
	// the SCAN journaling a local file that happens to sit at such a path —
	// which the hub refuses, wedging this device's whole journal.
	denied []string
	// reverted names the read-only paths this cycle put back, for Result.
	reverted []string

	// inbound accumulates this cycle's materialized peer paths, for
	// Result.Inbound. Reset at the top of every cycle — a Session is reused
	// across cycles by the daemon and by the tests.
	inbound []store.InboundEvent

	// fetched holds the blobs this cycle has already asked the remote for, so
	// the backfill never downloads one twice in a cycle. Reset with inbound.
	fetched    map[string]bool
	cacheDirty bool
}

// ScanEventKind describes one local filesystem observation. These types are
// intentionally internal to the daemon/syncer boundary: ordinary callers do
// not need to know about watcher events and continue to use Cycle's full-scan
// default.
type ScanEventKind uint8

const (
	ScanWrite ScanEventKind = iota
	ScanCreate
	ScanRemove
	ScanRename
	ScanChmod
)

// ScanEvent is a mount-relative filesystem event captured by a daemon watcher.
// Directory is retained separately from Kind because remove/rename events may
// name an entry that no longer exists when the scan runs.
type ScanEvent struct {
	Path      string
	Kind      ScanEventKind
	Directory bool
}

// ScanHint bounds local discovery to the paths observed before a worker's
// batch fence. Full requests deliberately carry no events: they invalidate
// every cached path and use the ordinary walk.
type ScanHint struct {
	Full   bool
	Events []ScanEvent
}

// logInbound records one materialized peer path both ways: on this cycle's
// Result (for the post_sync hook, which fires from the cycle that did the
// work) and on the store's spool (for `bdrive sync --hook`, which runs in a
// later process). Both consumers want the same event; neither may consume the
// other's copy.
func (s *Session) logInbound(rel string, deleted bool) {
	s.inbound = append(s.inbound, store.InboundEvent{Path: rel, Deleted: deleted, Time: time.Now().UTC()})
	_ = s.Store.LogInboundForMount(s.MountID, rel, deleted) // best-effort: never fails a cycle
}

// readOnlyPath reports whether rel sits under a folder this account may read
// but not write. The prefixes are slash-terminated, so "locked/" governs
// "locked/x.md" and never a sibling named "lockedout.md".
func (s *Session) readOnlyPath(rel string) bool {
	return underAny(rel, s.readOnly) || underAny(rel, s.denied)
}

// deniedPath reports whether rel sits under a folder this account cannot read.
// Such a path is never materialized (no ops arrive for it) and never
// journaled; a local file there is this machine's own and stays that way.
func (s *Session) deniedPath(rel string) bool { return underAny(rel, s.denied) }

func underAny(rel string, prefixes []string) bool {
	for _, pre := range prefixes {
		if strings.HasPrefix(rel, pre) {
			return true
		}
	}
	return false
}

// loadScope refreshes this account's write scope from the hub, falling back to
// the last answer on disk. A hub that cannot be reached, or that is too old to
// answer, must not widen what this device will journal: the persisted list
// stands until the hub itself says otherwise.
//
// It runs before the scan, which is what keeps the scan from ever minting an
// op the hub would refuse — the refusal is a 403 on the whole journal PUT, and
// a journal is append-only, so one such op wedges this device's sync until
// somebody edits its journal by hand.
func (s *Session) loadScope(ctx context.Context, st *store.SyncState) (scopeMoved bool) {
	s.readOnly, s.denied, s.reverted = st.ReadOnly, st.Denied, nil
	sc, ok := s.Backend.(remote.Scoper)
	if !ok {
		return false
	}
	got, err := sc.Scope(ctx)
	switch {
	case errors.Is(err, remote.ErrNoScope):
		// The hub has no opinion: it predates folder permissions, so nothing
		// is restricted and any stale list on disk is from another hub.
		s.readOnly, s.denied = nil, nil
		moved := st.ScopeTag != ""
		st.ReadOnly, st.Denied, st.ScopeTag = nil, nil, ""
		return moved
	case err != nil:
		return false // unreachable: keep the last answer
	}
	ro, dn := sanePrefixes(got.ReadOnly), sanePrefixes(got.Deny)
	moved := st.ScopeTag != got.Tag
	s.readOnly, s.denied = ro, dn
	st.ReadOnly, st.Denied, st.ScopeTag = ro, dn, got.Tag
	return moved
}

// forgetPeerJournals drops this device's cached copies of every peer's journal
// so the next pull re-fetches them from zero.
//
// It is what a scope change requires and why the hub sends a tag at all. The
// hub serves each peer journal FILTERED to what this account may read, and the
// client resumes from a byte offset into the copy it already holds. Both
// directions of a rule change break that: a revocation makes the filtered
// journal shorter, and pull skips any journal whose listed size did not grow —
// so that peer's ops would never be read again, silently and forever. A new
// grant interleaves previously-hidden ops into the middle of a prefix the
// client already trusts, so the offset lands mid-stream.
//
// Rule changes are rare and a re-pull is cheap next to reasoning about a moved
// byte offset. Own journal is never touched: this device is its only writer.
func (s *Session) forgetPeerJournals() {
	dir := filepath.Join(s.Store.Dir(), "journal")
	ents, err := os.ReadDir(dir)
	if err != nil {
		s.Store.EvictPeerJournals(s.Device.ID)
		return // nothing cached yet, or unreadable: the next pull rebuilds it
	}
	own := s.Store.JournalPath(s.Device.ID)
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		// Both guards, in the same order pull applies them: the NAME first,
		// because sameJournalFile answers false when either stat fails — and
		// the file this must never delete is the one object this device is the
		// sole author of. A peer journal deleted by mistake is re-pulled; this
		// device's own ops are not anywhere else.
		dev := strings.TrimSuffix(e.Name(), ".jsonl")
		if strings.EqualFold(dev, s.Device.ID) || sameJournalFile(p, own) {
			continue
		}
		if err := os.Remove(p); err != nil {
			log.Printf("beardrive: could not drop a stale peer journal: %v", err)
		}
	}
	s.Store.EvictPeerJournals(s.Device.ID)
}

// sanePrefixes drops read-only prefixes this device will not act on. The
// remote may be a hostile hub — the same premise pull applies to its journal
// listing — and this list decides what this device will never journal again:
// an empty prefix matches every path, so one would stop the device syncing its
// own work at all, silently and with no error anywhere. A prefix that is not
// slash-terminated is the subtler version, matching "logs.md" for a rule about
// "log/".
//
// Refusing the entry rather than the whole answer is deliberate: a hub that
// sends one bad prefix among good ones should still restrict the good ones.
func sanePrefixes(in []string) []string {
	var out []string
	for _, p := range in {
		if p == "" || p == "/" || !strings.HasSuffix(p, "/") || !journal.SafePath(strings.TrimSuffix(p, "/")) {
			log.Printf("beardrive: ignoring an unusable read-only folder %q from the hub", p)
			continue
		}
		out = append(out, p)
	}
	return out
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
func computeIgnoreFingerprint(raw string, include []string, accepted, pulled string) [32]byte {
	h := sha256.New()
	h.Write([]byte(raw))
	h.Write([]byte{0})
	for _, item := range include {
		h.Write([]byte(item))
		h.Write([]byte{0})
	}
	h.Write([]byte(accepted))
	h.Write([]byte{0})
	h.Write([]byte(pulled))
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
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
	LocalOps  int // local changes committed to the journal
	PulledOps int // ops received from other devices
	Conflicts int // conflict copies created
	// Merged counts concurrent edits this cycle resolved by a three-way merge
	// instead of forking a conflict copy. Kept apart from Conflicts because
	// they mean opposite things to the person reading the line: a merge is
	// work the machine finished, a conflict is work it handed back.
	Merged int
	// Adopted counts paths where this folder's own content gave way to the
	// project's on join (step 1b). Not a conflict and not an error — the
	// superseded content stays in history — but the user asked for none of it,
	// so it is worth a line.
	Adopted      int
	Pruned       int  // paths removed from the hub by --prune (kept on disk)
	Materialized int  // files written/removed in the working folder
	Pushed       bool // own journal/blobs uploaded
	ScanDuration time.Duration
	ScanFull     bool
	// Backfilling reports that this cycle's backfill landed content and left
	// more waiting for a later batch, so the next remote pass should come
	// soon. It stays false while the backfill lands nothing, which is what
	// keeps blobs the hub cannot serve from holding a daemon at full cadence.
	Backfilling bool
	// Inbound names the paths this cycle materialized on a peer's behalf —
	// the same events the inbound spool queues, carried out of the cycle that
	// did the work so the post_sync hook can hand them to a local command. It
	// does NOT replace the spool: see the comment in internal/store/inbound.go
	// for why the two coexist.
	Inbound []store.InboundEvent
	// Offline reports that the remote leg of this cycle had a problem worth
	// telling the user about — usually "unreachable", but also "the hub served
	// bytes that are not their content address", which is the only signal that
	// case ever produces. It is a REPORT, not a gate: a content-level problem
	// with one object still lets this device push its own work (Pushed may be
	// true alongside it), because otherwise one peer's journal line decides
	// whether anyone else's edits ever leave their machine.
	Offline    bool
	OfflineErr error
	// Reverted names paths under a read-only folder whose local edit this
	// cycle put back from the hub's version, keeping the user's own copy
	// beside it. Distinct from Conflicts: no op was written and no peer will
	// ever see the change, so it is not a conflict, it is a refusal.
	Reverted []string
	// RetryPaths names relative paths that a bounded ScanHint walk visited
	// but could not stat, readlink, or hash this cycle. The daemon re-queues
	// them into nextHints so a consumed watcher event is retried on the next
	// tick instead of waiting for the safety full scan.
	RetryPaths []string
	ReadOnly   bool // the hub refused our push: pull-only from here
	NoAccess   bool // the hub refused our pull: sync paused, nothing touched
	AccessErr  error
}

// accessReason renders a hub refusal as the sentence a person can act on. The
// hub answers a device-registration 403 with what to DO about it, and every
// caller used to collapse that into "read-only (pull only)" — the one refusal
// that is not about project permissions at all, reported as if it were, so the
// user re-checked their access, saw `write`, and had nowhere left to look.
//
// The wrapper chain the CLI itself added ("forbidden: server: 403 Forbidden: ")
// is dropped; it tells a reader nothing the line does not already say.
//
// The remainder is the HUB's text landing in a log file, a terminal and an
// agent's context, so it passes journal.SafeText — the rule this repo already
// applies to every other peer-written string it renders — or it does not travel
// at all. Bounded for the same reason: this is persisted in sync.json and
// printed on one status line.
func accessReason(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if _, rest, ok := strings.Cut(msg, "Forbidden: "); ok && rest != "" {
		msg = rest
	}
	if len([]rune(msg)) > 300 || !journal.SafeText(msg) {
		return ""
	}
	return msg
}

// Reason is the hub's own words for a ReadOnly or NoAccess answer, empty when
// the hub gave none. It is what the CLI prints under the summary line.
func (r *Result) Reason() string { return accessReason(r.AccessErr) }

func (r *Result) Activity() bool {
	return r.LocalOps > 0 || r.PulledOps > 0 || r.Conflicts > 0 || r.Merged > 0 ||
		r.Adopted > 0 || r.Pruned > 0 || r.Materialized > 0 || len(r.Reverted) > 0
}

// The builtin exclusions (.bdrive — the mount's local identity, syncing it
// would let one device silently repoint another — and .git) are defined once
// in config, because the hub enforces the same set on the paths clients
// upload. Local aliases so the walk and the path checks below read plainly.
func ignoredDir(name string) bool { return config.ReservedDir(name) }

// maxLamport caps what this device's clock will absorb from a peer. Cycle
// raises st.Lamport to any value it pulls and scan increments it per local op,
// so one op carrying math.MaxInt64 wraps the clock negative and every op this
// device ever writes again sorts before everything it has already seen — a
// silent, permanent write lock installed by one line of JSON. A value this
// large is not a clock reading, so it is ignored rather than absorbed. Pulled
// ops are never rewritten: replay must agree between a device and its remote
// copy.
const maxLamport = journal.MaxValidLamport

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

// Cycle runs one full scan/sync/materialize pass under the volume lock, then
// fires the folder's post_sync hook. A nil hint is the compatibility path:
// every direct, CLI, hook, and desktop caller still performs a full discovery.
func (s *Session) Cycle(ctx context.Context) (*Result, error) {
	return s.cycle(ctx, nil)
}

// CycleWithScanHint is the daemon-only accelerator. Its hint never changes
// pull, replay, materialization, or push ordering; it only narrows local
// discovery when the watcher has a bounded, healthy batch.
func (s *Session) CycleWithScanHint(ctx context.Context, hint *ScanHint) (*Result, error) {
	return s.cycle(ctx, hint)
}

func (s *Session) cycle(ctx context.Context, hint *ScanHint) (*Result, error) {
	res, err := s.cycleLocked(ctx, hint)
	if err == nil && res != nil {
		s.firePostSync(res)
	}
	return res, err
}

func (s *Session) cycleLocked(ctx context.Context, hint *ScanHint) (*Result, error) {
	unlock, err := s.Store.Lock()
	if err != nil {
		return nil, fmt.Errorf("lock volume: %w", err)
	}
	defer unlock()

	s.inbound = nil
	s.fetched = map[string]bool{}
	s.cacheDirty = false
	res := &Result{}
	cache, err := s.Store.LoadCache(s.mountID())
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	st, err := s.Store.LoadSync()
	if err != nil {
		return nil, fmt.Errorf("load sync state: %w", err)
	}
	loadedSync := store.CloneSyncState(st)
	// Advisory telemetry about files this cycle will journal and push anyway,
	// so an unreadable record starts the cycle empty rather than failing it.
	sec := &secretLog{found: map[string][]secrets.Finding{}}
	if found, err := s.Store.LoadSecrets(s.mountID()); err == nil {
		sec.found = found
	}
	myOpCount, err := s.Store.OwnOpCount(s.Device.ID)
	if err != nil {
		return nil, fmt.Errorf("read own journal: %w", err)
	}
	fold, err := s.Store.ReplayFold()
	opsErr := err
	var existingState map[string]journal.FileState
	existingIgnoreLive := false
	if opsErr != nil {
		log.Printf("beardrive: could not re-derive the sync clock: %v", opsErr)
	}
	proj, ok, err := config.LoadProject(s.Folder)
	if err != nil {
		return nil, err
	}
	if s.MountID != "" {
		registered := false
		if mounts, merr := config.LoadMounts(); merr == nil {
			_, registered = mounts[s.MountID]
		}
		if registered && (!ok || proj.ID == "" || proj.ID != s.MountID) {
			return nil, fmt.Errorf("project config gone at %s", s.Folder)
		}
		if ok && proj.ID != "" && proj.ID != s.MountID {
			return nil, fmt.Errorf("project config gone at %s", s.Folder)
		}
	}
	if s.ExpectedRemote != "" && !config.SameRemote(proj.Remote, s.ExpectedRemote) {
		return nil, fmt.Errorf("project remote changed (%q -> %q)", s.ExpectedRemote, proj.Remote)
	}
	if st.Remote != "" && !config.SameRemote(proj.Remote, st.Remote) {
		return nil, fmt.Errorf("volume remote mismatch (%q -> %q)", st.Remote, proj.Remote)
	}
	hadCache := s.Store.HasCache(s.mountID())
	if proj.Remote != "" {
		st.Remote = proj.Remote
	}
	if s.Device.ID != "" {
		st.Device = s.Device.ID
	}
	deviceJoining := st.Lamport == 0 && st.PushedOps == 0
	mountJoining := !hadCache && s.MountID != "" && (st.Lamport > 0 || st.PushedOps > 0)
	joining := deviceJoining || mountJoining
	// Re-derive the clock from the journals this device ALREADY holds.
	//
	// Nothing else does. st.Lamport is only ever raised from a pull's new tail
	// (absorbLamport, step 2), and pull returns nothing once a peer's journal
	// is fully downloaded — it skips an object no larger than the local copy,
	// and one whose bytes are identical. So a device whose sync.json falls
	// below the journals in its own volume dir stays there forever, however
	// many times it syncs, and because journal.Less orders on lamport BEFORE
	// wall-clock time, every op it writes then LOSES replay: the cycle really
	// pushes the edit, replay resolves the path to a peer's older op, and
	// materialize writes those bytes back over the file — in the same run that
	// reported "remote: pushed". That is BEA-196, and it is silent.
	//
	// Any desync does it: a crash between AppendOps and SaveSync, a restored
	// backup, a pull that died on a 403 before it absorbed. Re-deriving heals
	// all of them the same way, which is why this is a derivation and not a
	// fix at one of those sites.
	//
	// Folded through absorbLamport rather than taken as a raw max: a journal on
	// disk holds a peer's bytes, and the maxLamport ceiling is what stops a
	// hostile peer installing a permanent write lock.
	//
	// Determine joining before raising the clock. Existing ops also identify
	// a populated store whose sync.json was not saved before a crash; a mount
	// without a cache must adopt that state rather than start a fresh import.
	//
	// This is also the in-memory op set reused by conflict detection and
	// materialization below. A remote cycle refreshes it only when pull or
	// scope filtering could have changed peer journals on disk.
	_, hadCachedIgnore := cache[IgnoreFile]
	if opsErr == nil {
		if fold.TotalOps > 0 {
			deviceJoining = false
			if !hadCache && s.MountID != "" {
				mountJoining = true
				joining = true
			}
		}
		if mountJoining {
			existingState = fold.Target
			_, existingIgnoreLive = existingState[IgnoreFile]
			if want, ok := existingState[IgnoreFile]; ok {
				wrote, _ := s.materializeFile(IgnoreFile, want, cache)
				if wrote {
					res.Materialized++
				}
			}
		} else if hadCachedIgnore && fold.HasIgnoreWinner && fold.IgnoreWinner.Kind == journal.KindPut {
			existingIgnoreLive = true
			want := journal.FileState{Blob: fold.IgnoreWinner.Blob, Size: fold.IgnoreWinner.Size, Mode: fold.IgnoreWinner.Mode, Link: fold.IgnoreWinner.Link}
			wrote, _ := s.materializeFile(IgnoreFile, want, cache)
			if wrote {
				res.Materialized++
			}
		}
		st.Lamport = absorbLamport(st.Lamport, fold.MaxLamport)
	}
	filter, err := loadFilter(s.Folder, proj.Include)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", IgnoreFile, err)
	}
	oldAccepted, oldPulled := st.IgnoreAccepted, st.IgnorePulled
	// Accept the rules on disk as this device's own whenever they are not the
	// copy a peer's version last wrote here — i.e. whenever somebody at this
	// machine authored them (`bdrive init --only`, `bdrive scope`, an editor).
	// Runs before the scan and before the pull, so the floor the scan applies
	// is always the last rules this device agreed to. See Filter.SkipUp.
	if cur, err := os.ReadFile(filepath.Join(s.Folder, IgnoreFile)); err == nil || os.IsNotExist(err) {
		text := string(cur)
		// Both fields are omitempty, so a SyncState written before they existed
		// carries neither — and the accept test below then reads whatever is on
		// disk as locally authored, including a peer's widening that landed one
		// cycle earlier (scan runs before pull, so it always does). A device that
		// has already synced is an upgrade, not a new joiner: seed the pair from
		// what this mount is demonstrably syncing already (vouchedFloor) instead
		// of taking the file's word for it.
		if st.IgnoreAccepted == "" && st.IgnorePulled == "" && !deviceJoining {
			synced := make([]string, 0, len(cache)+len(existingState))
			for rel := range cache {
				synced = append(synced, rel)
			}
			if mountJoining {
				for rel := range existingState {
					synced = append(synced, rel)
				}
			}
			prior, found := fold.LatestIgnorePut, fold.HasIgnorePut
			if (!mountJoining && text != "") || (mountJoining && !found && text != "") {
				st.IgnoreAccepted, st.IgnorePulled = vouchedFloor(text, synced), text
			} else if found {
				st.IgnoreAccepted = vouchedFloor("/*\n", synced)
				if prior.Blob != "" && prior.Link == "" {
					if f, err := s.Store.OpenBlob(prior.Blob); err == nil {
						data, readErr := io.ReadAll(f)
						_ = f.Close()
						if readErr == nil && len(data) > 0 {
							st.IgnoreAccepted = vouchedFloor(string(data), synced)
							if existingIgnoreLive {
								st.IgnorePulled = string(data)
							}
						}
					}
				}
				if mountJoining && text != "" {
					if _, live := existingState[IgnoreFile]; !live {
						st.IgnorePulled = text
					}
				}
			}
		} else if text != st.IgnorePulled && !(mountJoining && (st.IgnorePulled != "" || st.IgnoreAccepted != "")) {
			st.IgnoreAccepted = text
		}
	}
	filter.AcceptRules(st.IgnoreAccepted)
	// Before the scan, so this device never mints an op the hub would refuse:
	// a refusal is a 403 on the whole journal PUT, and a journal is
	// append-only, so one such op wedges this device until somebody edits its
	// journal by hand.
	scopeMoved := s.loadScope(ctx, &st)
	if scopeMoved {
		// What this account may see has changed. Every peer journal on disk
		// was filtered under the OLD scope, so it is neither a prefix of nor
		// the same length as what the hub will serve now.
		s.forgetPeerJournals()
	}

	fullScan := hint == nil || hint.Full || joining || scopeMoved ||
		st.IgnoreAccepted != oldAccepted || st.IgnorePulled != oldPulled ||
		s.Prune || scanHintTouchesIgnore(hint)
	if fullScan {
		hint = &ScanHint{Full: true}
	}
	preScanIgnoreFP := computeIgnoreFingerprint(filter.RawIgnoreText(), proj.Include, st.IgnoreAccepted, st.IgnorePulled)
	canUseFastScan := !s.Prune && !mountJoining &&
		(filter.RawIgnoreText() == st.IgnoreAccepted || !filter.HasUpGuard()) &&
		s.Store.IgnoreFingerprintMatches(s.mountID(), preScanIgnoreFP)
	res.ScanFull = hint.Full
	// 1. Scan the working folder and journal any local changes.
	scanStart := time.Now()
	localOps, retryPaths, err := s.scan(cache, &st, int64(myOpCount), filter, sec, hint, canUseFastScan)
	res.ScanDuration = time.Since(scanStart)
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	res.RetryPaths = retryPaths
	commitLocal := func() error {
		if len(localOps) == 0 {
			return nil
		}
		if ok {
			cur, stillOK, cerr := config.LoadProject(s.Folder)
			if cerr != nil || !stillOK || cur.ID != proj.ID {
				return fmt.Errorf("project config gone at %s", s.Folder)
			}
		}
		if err := s.Store.AppendOps(s.Device.ID, localOps); err != nil {
			return fmt.Errorf("append journal: %w", err)
		}
		myOpCount += len(localOps)
		res.LocalOps = len(localOps)
		localOps = nil
		return nil
	}
	if !joining {
		// The normal path: journal local edits before the pull, so nothing
		// remote can overwrite an edit that was never captured (see the package
		// doc). A joining device holds its ops back for the length of the pull
		// only — long enough to learn which paths the project already has.
		if err := commitLocal(); err != nil {
			return nil, err
		}
	}

	// 2. Pull journals + blobs from other devices.
	//
	// blocked is "the remote leg cannot continue this cycle" and is separate
	// from res.Offline, which is what the user is TOLD. They used to be one
	// flag, so any pull error withheld this device's own push — and one of
	// those errors is "a blob's bytes are not its content address", which one
	// peer produces at will by understating Op.Size on one journal line.
	blocked := false
	var pulled, gone []journal.Op
	if s.Backend != nil {
		pulled, gone, err = s.pull(ctx, cache)
		// Before the switch, not after it: pull WRITES each peer journal to
		// disk as it goes, and the ErrForbidden arm returns without ever
		// reaching an absorb that sat below it — so a pull that died partway
		// left this device holding ops its clock was below, and the
		// commitLocal() in that same arm then journalled local edits under
		// them. The derivation above heals that on the next cycle; this keeps
		// the cycle that hit it from minting losing ops in the first place.
		res.PulledOps = len(pulled)
		for _, op := range pulled {
			st.Lamport = absorbLamport(st.Lamport, op.Lamport)
		}
		switch {
		case err == nil:
			if st.Access == store.AccessNone {
				// The pull that was refused now succeeds, so read access is back.
				// Whether writes are back is the push leg's to answer below.
				st.Access, st.AccessReason = store.AccessOK, ""
			}
		case errors.Is(err, remote.ErrForbidden):
			// Access to this project was revoked. Stop here: materializing a
			// replay we can no longer refresh would look like the hub
			// reverting the user's files. Nothing is pushed, nothing is
			// deleted, and the next cycle re-checks.
			res.NoAccess, res.AccessErr = true, err
			st.Access, st.AccessReason = store.AccessNone, accessReason(err)
			// The scan already claimed these files in the state cache, which
			// finish is about to persist — journal them or the next scan sees
			// nothing changed and this folder's content is never captured.
			if cerr := commitLocal(); cerr != nil {
				return nil, cerr
			}
			return res, s.finish(cache, st, loadedSync, sec, s.cacheDirty)
		case errors.Is(err, errBlobContent):
			// Reported — it is the only signal a device ever gets that its hub
			// is serving bytes that are not what they are addressed as — but
			// NOT blocking: one object's content says nothing about whether the
			// hub is reachable, and treating it as such withheld this device's
			// own journal and blobs for one integer in one peer's journal line.
			// The path itself stays unwritten until real content shows up.
			res.Offline = true
			res.OfflineErr = err
		default:
			res.Offline = true
			res.OfflineErr = err
			blocked = true
		}
	}

	// 1b. Adoption. A device joining a project it has never synced is not
	// editing that project's files: it is bringing a folder that happens to
	// hold some of the same paths — a git checkout of the same docs, an
	// agent-written AGENTS.md, the .bdriveignore `bdrive init` seeds. Treating
	// those as concurrent edits forked every one of them: whichever side's
	// clock sorted higher won, and the other landed beside it as a
	// `.bdrive-conflict-<device>-<time>` file. So connecting a folder littered
	// it with copies of files nobody had edited, and half the time the joiner's
	// stale copy is what won — replacing the team's version for everyone.
	//
	// The project's version wins instead, deterministically: the local op is
	// demoted under every op the project can hold (scan's clock starts at 1, so
	// lamport 0 loses to all of them on every device). It is still journaled and
	// pushed, so nothing is lost — the folder's content at join time is in
	// History and `bdrive restore` brings it back — it just never materializes.
	if joining && len(localOps) > 0 {
		candidateOps := append([]journal.Op(nil), pulled...)
		if mountJoining && opsErr == nil {
			existing, err := s.Store.AllOps()
			if err != nil {
				return nil, fmt.Errorf("read journals for adoption: %w", err)
			}
			candidateOps = append(candidateOps, existing...)
		}
		if len(candidateOps) > 0 {
			theirs := map[string]journal.Op{}
			for _, op := range candidateOps {
				if prev, ok := theirs[op.Path]; !ok || journal.Less(prev, op) {
					theirs[op.Path] = op
				}
			}
			kept := localOps[:0]
			for _, op := range localOps {
				// Only a path the project actually HOLDS is adopted. Where its
				// last op is a delete there is no version to adopt, so the local
				// file is this device's own and keeps its clock.
				if t, ok := theirs[op.Path]; ok && t.Kind == journal.KindPut {
					if mountJoining && op.Kind == journal.KindPut && op.Blob == t.Blob && op.Link == t.Link {
						continue
					}
					op.Lamport = 0
					op.Note = adoptNote
					res.Adopted++
				}
				kept = append(kept, op)
			}
			localOps = kept
			if mountJoining {
				for i := range localOps {
					localOps[i].Seq = int64(myOpCount + i + 1)
				}
			}
		}
	}
	if err := commitLocal(); err != nil {
		return nil, err
	}

	fold, err = s.Store.ReplayFold()
	if err != nil {
		return nil, fmt.Errorf("read journals: %w", err)
	}
	target := fold.Target

	// 3. Preserve losing local edits as conflict copies.
	if len(pulled) > 0 || st.PushedOps < int64(myOpCount) {
		unpushedOps, err := s.Store.OwnOpsFrom(s.Device.ID, st.PushedOps)
		if err != nil {
			return nil, fmt.Errorf("read own journal: %w", err)
		}
		conflictOps, err := s.conflictCopiesWithState(unpushedOps, int64(myOpCount), pulled, &st, nil, target)
		if err != nil {
			return nil, err
		}
		if len(conflictOps) > 0 {
			if err := s.Store.AppendOps(s.Device.ID, conflictOps); err != nil {
				return nil, fmt.Errorf("append conflict ops: %w", err)
			}
			myOpCount += len(conflictOps)
			fold, err = s.Store.ReplayFold()
			if err != nil {
				return nil, fmt.Errorf("read journals: %w", err)
			}
			target = fold.Target
			for _, op := range conflictOps {
				if op.Note == mergeNote || op.Note == keptOverEmptyNote {
					res.Merged++
				} else {
					res.Conflicts++
				}
			}
		}
	}

	// 3b. Re-assert what a peer withdrew. An op this device already applied is
	// a file on this disk, and a file only leaves a disk because somebody
	// deleted it — so when a peer republishes its journal without an op we
	// already put on disk, we restate that op in OUR journal, keeping its
	// original clock so a genuinely later change still wins replay. The
	// alternative (refusing the peer's update) leaves this device holding a
	// state a first-time device can never reach, which is the divergence the
	// byte-offset resume exists to prevent.
	//
	// It runs AFTER conflictCopies, not before it. A re-asserted op carries the
	// withdrawn op's original — and therefore usually losing — lamport, so as a
	// member of the unpushed set it was a losing local edit by construction, and
	// step 3 did exactly what it exists to do: preserved it as a conflict copy.
	// Net effect, with no local edit on this side at all: a peer made this
	// device create, sign and push a file at a path that never existed in the
	// project, holding content the peer chose.
	if len(gone) > 0 {
		readopt := make([]journal.Op, 0, len(gone))
		for _, op := range gone {
			if !s.stillHold(op, cache) {
				continue // we cannot stand behind what this folder does not hold
			}
			if filter.Skip(op.Path) || neverSync(op.Path) || unsafeRel(op.Path) || s.readOnlyPath(op.Path) || s.deniedPath(op.Path) {
				// The filter is applied symmetrically in scan and materialize
				// precisely so a path this device refuses to write is also one
				// it never publishes. This is a third publishing site and owes
				// the same rule.
				continue
			}
			op.Device, op.DeviceName, op.Author = s.Device.ID, s.Device.Name, s.Device.Author
			op.Seq = int64(myOpCount) + int64(len(readopt)) + 1
			op.Note = reassertNote
			readopt = append(readopt, op)
		}
		if len(readopt) > 0 {
			if err := s.Store.AppendOps(s.Device.ID, readopt); err != nil {
				return nil, fmt.Errorf("append re-asserted ops: %w", err)
			}
			myOpCount += len(readopt)
			fold, err = s.Store.ReplayFold()
			if err != nil {
				return nil, fmt.Errorf("read journals: %w", err)
			}
			target = fold.Target
			res.LocalOps += len(readopt)
		}
	}

	// 4. Materialize the merged state into the working folder.
	// 4a. Backfill content the merged state names but this store lacks. pull
	// fetches blobs only for the ops it just downloaded, and a peer journal
	// already on disk is never downloaded again — so a blob lost in that cycle
	// (a dropped connection, a hub that shed load, a killed `bdrive init`)
	// would leave its path unwritten on this device for good.
	if s.Backend != nil && !blocked {
		ops, more := s.backfillOps(target, cache, filter)
		landed, err := s.fetchBlobs(ctx, ops, cache)
		res.Backfilling = more && landed > 0
		switch {
		case err == nil:
		case errors.Is(err, errBlobContent):
			// Logged, not reported: the pull that first met these bytes
			// already said so, and repeating it as Offline on every later
			// pass would show a device stuck offline over one peer's line.
			log.Printf("beardrive: backfill: %v", err)
		default:
			res.Offline, res.OfflineErr = true, err
			blocked = true
		}
	}

	// The ignore rules sync like any other file, so a peer can receive the new
	// .bdriveignore and the delete ops it justifies in the same batch. The
	// filter was loaded at the top of the cycle, before the pull, so write the
	// rules first and reload from them — otherwise materialize's delete loop
	// runs against stale rules, its filter guard never fires, and it unlinks
	// files that merely left sync scope.
	ignoreFromPeer := false
	if _, ok := target[IgnoreFile]; ok {
		ignoreFromPeer = fold.HasIgnoreWinner &&
			fold.IgnoreWinner.Kind == journal.KindPut &&
			fold.IgnoreWinner.Device != s.Device.ID
	}
	if want, ok := target[IgnoreFile]; !ok {
		// A peer DELETED the shared rules — the maximal widening there is, and
		// the one shape the bookkeeping below cannot see, since it is written in
		// terms of a file that still exists. materialize's delete loop unlinks
		// the local copy either way; recording the deletion as PULLED is what
		// stops the next cycle reading the now-absent file as locally authored
		// and dropping the accepted floor with it.
		if len(pulled) > 0 || hadCachedIgnore {
			st.IgnorePulled = ""
		}
	} else {
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
			// Remember the pulled text as pulled, NOT as accepted: these rules
			// came from a peer, so they may narrow what this device uploads but
			// never widen it until somebody here authors a change. The accepted
			// floor carries over unchanged.
			if len(pulled) > 0 || ignoreFromPeer {
				if cur, err := os.ReadFile(filepath.Join(s.Folder, IgnoreFile)); err == nil {
					st.IgnorePulled = string(cur)
				}
			}
			filter.AcceptRules(st.IgnoreAccepted)
		}
		// If the blob isn't fetched yet materializeFile skips it and the old
		// rules stand: the usual retry-next-cycle posture, and the guard in
		// materialize still protects the files either way.
	}

	// 4b. Prune: remove from the hub what the shared rules now exclude.
	if s.Prune {
		pruneOps, err := s.pruneOps(target, &st, int64(myOpCount))
		if err != nil {
			return nil, err
		}
		if len(pruneOps) > 0 {
			if err := s.Store.AppendOps(s.Device.ID, pruneOps); err != nil {
				return nil, fmt.Errorf("append prune ops: %w", err)
			}
			myOpCount += len(pruneOps)
			res.Pruned = len(pruneOps)
		}
	}

	n, err := s.materialize(target, cache, filter)
	if err != nil {
		return nil, fmt.Errorf("materialize: %w", err)
	}
	res.Materialized += n
	res.Inbound = s.inbound
	res.Reverted = s.reverted

	// 5. Push our blobs and journal.
	if s.Backend != nil && !blocked && int64(myOpCount) > st.PushedOps {
		myOps, err := s.Store.DeviceOps(s.Device.ID)
		if err != nil {
			return nil, fmt.Errorf("read own journal: %w", err)
		}
		switch err := s.push(ctx, myOps, &st); {
		case err == nil:
			res.Pushed = true
			st.Access, st.AccessReason = store.AccessOK, ""
		case errors.Is(err, remote.ErrForbidden):
			// Read-only on this project: pull and materialize already ran, so
			// pull-only is the steady state. Our own ops stay in the local
			// journal — never pushed, never dropped. The push is still
			// attempted once per remote interval (no hot loop, and a re-grant
			// self-heals).
			res.ReadOnly, res.AccessErr = true, err
			st.Access, st.AccessReason = store.AccessReadOnly, accessReason(err)
		default:
			res.Offline = true
			res.OfflineErr = err
			blocked = true
		}
	}

	// 6. Drain the agent read spool to the hub (read heatmap telemetry).
	// Strictly best-effort: a failed report keeps the batch queued for the
	// next cycle and never fails — or even marks offline — this one.
	if rr, ok := s.Backend.(remote.ReadReporter); ok && !blocked {
		if evs, err := s.Store.PendingReads(); err == nil && len(evs) > 0 {
			reads := make([]remote.ReadEvent, len(evs))
			for i, e := range evs {
				reads[i] = remote.ReadEvent{Path: e.Path, Session: e.Session, Time: e.Time}
			}
			// Agent traffic: these are the paths bdrive read-log spooled for a
			// coding agent on this device, so the device is the actor.
			if rr.ReportReads(ctx, remote.ReadKindAgent, reads) == nil {
				s.Store.ClearPendingReads()
			}
		}
	}

	// st.Access is NOT recomputed here. Only the leg that actually asked the hub
	// knows how it answered, so each one records its own verdict above and a
	// cycle that never asked leaves the last one standing. Resetting it to OK on
	// every cycle meant the daemon's cheap local-only ticks — three of them
	// between remote passes — each declared "access restored; syncing normally",
	// so a device the hub was refusing alternated between the two log lines
	// forever and `bdrive status` reported healthy sync moments after the push
	// it had just refused.
	if err := s.finish(cache, st, loadedSync, sec, s.cacheDirty); err != nil {
		return nil, err
	}
	postIgnoreFP := computeIgnoreFingerprint(filter.RawIgnoreText(), proj.Include, st.IgnoreAccepted, st.IgnorePulled)
	if hint.Full && postIgnoreFP == preScanIgnoreFP {
		s.Store.SetIgnoreFingerprint(s.mountID(), postIgnoreFP)
	} else if postIgnoreFP != preScanIgnoreFP {
		s.Store.ClearIgnoreFingerprint(s.mountID())
	}
	return res, nil
}

// finish persists the two pieces of state a cycle mutates. Saving the cache
// matters even on a cut-short cycle: the scan already journaled local edits,
// and dropping the cache would make the next scan journal them all again.
func (s *Session) finish(cache map[string]store.CachedFile, st store.SyncState, loadedSync store.SyncState, sec *secretLog, cacheDirty bool) error {
	if cacheDirty || !s.Store.HasCache(s.mountID()) {
		if err := s.Store.SaveCache(s.mountID(), cache); err != nil {
			return err
		}
	}
	// Fold caches are in-memory write-back state. Persist them while the
	// volume lock is still held, alongside the cache and sync state.
	s.Store.SaveChangedFoldsLocked()
	// Written only when the cycle changed it, so a quiet daemon tick every 3s
	// still writes nothing — and an error is logged, never returned: this is
	// advisory telemetry about ops that are already journaled and pushed, and
	// giving it a veto over convergence is the one invariant this feature is
	// close enough to break.
	if sec != nil && sec.dirty {
		if err := s.Store.SaveSecrets(s.mountID(), sec.found); err != nil {
			log.Printf("beardrive: could not record credential findings: %v", err)
		}
	}
	if !store.SyncStateEqual(st, loadedSync) {
		return s.Store.SaveSync(st)
	}
	if _, err := os.Stat(filepath.Join(s.Store.Dir(), "sync.json")); err != nil {
		return s.Store.SaveSync(st)
	}
	return nil
}

// scan diffs the working folder against the state cache and returns ops for
// every local change, storing new content in the blob store. Filtered paths
// are neither journaled nor deleted: a path that becomes ignored is dropped
// from the cache without a delete op, so opting out locally never removes
// the file from other devices.
func scanHintTouchesIgnore(hint *ScanHint) bool {
	if hint == nil || hint.Full {
		return false
	}
	for _, ev := range hint.Events {
		if ev.Path == IgnoreFile {
			return true
		}
	}
	return false
}

// scan diffs the working folder against the state cache and returns ops for
// every local change, storing new content in the blob store. Filtered paths
// are neither journaled nor deleted: a path that becomes ignored is dropped
// from the cache without a delete op, so opting out locally never removes
// the file from other devices.
func (s *Session) scan(cache map[string]store.CachedFile, st *store.SyncState, seqBase int64, filter *Filter, sec *secretLog, hint *ScanHint, canUseFastScan bool) ([]journal.Op, []string, error) {
	fullDiscovery := hint == nil || hint.Full
	var roots []string
	deleteScopes := map[string]bool{}
	if !fullDiscovery && hint != nil {
		for _, ev := range hint.Events {
			rel := filepath.ToSlash(filepath.Clean(ev.Path))
			if rel == "." || rel == "" || !journal.SafePath(rel) {
				fullDiscovery = true
				break
			}
			roots = append(roots, rel)
			if ev.Directory || ev.Kind == ScanRemove || ev.Kind == ScanRename {
				deleteScopes[rel] = true
			}
		}
	}
	seen := make(map[string]bool, len(cache))
	retrySeen := map[string]bool{}
	var retryPaths []string
	unreadable := map[string]bool{}
	unreadableRoot := false
	recordRetry := func(rel string) {
		if !fullDiscovery && !retrySeen[rel] {
			retrySeen[rel] = true
			retryPaths = append(retryPaths, rel)
		}
	}
	onWalkErr := func(rel string, _ error) {
		if rel == "" {
			unreadableRoot = true
			for _, r := range roots {
				recordRetry(r)
			}
			return
		}
		unreadable[rel] = true
		recordRetry(rel)
	}
	var ops []journal.Op
	note := s.Note
	if note == "" {
		note = s.Store.LoadNoteForMount(s.MountID)
	}
	// One scan is one commit: every op it produces carries the same Time. Op
	// order inside the batch is already carried by Lamport and Seq, which
	// journal.Less consults first and second — so replay is unaffected — while
	// a shared Time is what lets `bdrive log` recognise a batch and order it by
	// the files' own write times (SortForDisplay). Stamping each op separately
	// made a rename's two halves two different instants, which is the bug.
	committed := time.Now().UTC()
	nextOp := func(kind, rel string) journal.Op {
		st.Lamport = tickLamport(st.Lamport)
		seqBase++
		return journal.Op{
			Seq: seqBase, Lamport: st.Lamport, Time: committed,
			Device: s.Device.ID, DeviceName: s.Device.Name, Author: s.Device.Author,
			User: s.Account.Email, UserName: s.Account.Name,
			Kind: kind, Path: rel, Note: note, Session: s.SessionID,
		}
	}

	var fastUnchanged func(rel string, d fs.DirEntry) bool
	if canUseFastScan {
		fastUnchanged = func(rel string, d fs.DirEntry) bool {
			c, ok := cache[rel]
			if !ok || c.Link != "" || s.readOnlyPath(rel) {
				return false
			}
			info, err := d.Info()
			if err != nil || !info.Mode().IsRegular() {
				return false
			}
			if cachedPathMatches("", info, c) {
				seen[rel] = true
				return true
			}
			return false
		}
	}
	var walk func(string, *Filter, func(string, string, fs.DirEntry, verdict) error) error
	if fullDiscovery {
		walk = func(folder string, filter *Filter, fn func(string, string, fs.DirEntry, verdict) error) error {
			return walkFolderPathsFast(folder, filter, nil, fastUnchanged, onWalkErr, fn)
		}
	} else {
		walk = func(folder string, filter *Filter, fn func(string, string, fs.DirEntry, verdict) error) error {
			if len(roots) == 0 {
				return nil
			}
			return walkFolderPathsWithErr(folder, filter, roots, onWalkErr, fn)
		}
	}
	err := walk(s.Folder, filter, func(p, rel string, d fs.DirEntry, v verdict) error {
		if v != vSync && v != vSymlink {
			return nil
		}
		seen[rel] = true
		info, err := d.Info()
		if err != nil {
			recordRetry(rel)
			return nil
		}
		// A read-only folder is materialized like any other and journaled
		// never. Marking it seen first is what stops the delete pass below
		// minting a delete for it. The local edit is not lost and not
		// silently kept either: materialize puts the project's version back
		// and leaves the user's own copy beside it.
		//
		// A DENIED folder takes the same branch for a different reason: the
		// hub sends no ops for it, so nothing here is the project's — it is
		// this machine's own file at a path the project happens to use, and
		// journaling it would have the hub refuse this device's whole journal.
		if s.readOnlyPath(rel) {
			return nil
		}
		size, mt := info.Size(), info.ModTime().UnixNano()
		mode := uint32(info.Mode().Perm())
		c, ok := cache[rel]
		if ok && cachedPathMatches(p, info, c) {
			return nil // unchanged (cheap path)
		}
		if v == vSymlink {
			// A symlink is journaled as its target string and nothing else:
			// Readlink, not Open — a link to ~/.ssh/id_rsa ships the eleven
			// characters of the path and not one byte of the key. No blob is
			// put, so no content is ever read, hashed, or secret-scanned.
			target, err := os.Readlink(p)
			if err != nil {
				recordRetry(rel)
				return nil // vanished or unreadable; next cycle
			}
			if ok && c.Link == target {
				c.Size, c.MTimeNS = size, mt // touched, same target
				s.cacheDirty = true
				cache[rel] = c
				return nil
			}
			op := nextOp(journal.KindPut, rel)
			op.Link = target
			op.Mtime = info.ModTime().UTC()
			ops = append(ops, op)
			s.cacheDirty = true
			cache[rel] = store.CachedFile{Link: target, Size: size, MTimeNS: mt}
			return nil
		}
		sum, n, mt, dev, ino, err := s.Store.PutBlobFileVerified(p)
		if err != nil {
			if errors.Is(err, store.ErrFileChangedDuringRead) || os.IsNotExist(err) {
				recordRetry(rel)
			} else {
				recordRetry(rel)
			}
			return nil // file vanished, changed, or unreadable; next cycle
		}
		// Below the cheap size+mtime return above, so only a file that actually
		// changed is ever read for credentials — and covering both branches
		// below means a file touched back to previously-flagged content still
		// reports. Warn only: nothing here stops the op.
		sec.scanBlob(s.Store, rel, sum)
		if ok && c.Blob == sum {
			// content unchanged, just touched
			c.Size, c.MTimeNS, c.Mode, c.Dev, c.Ino = n, mt, mode, dev, ino
			s.cacheDirty = true
			cache[rel] = c
			return nil
		}
		op := nextOp(journal.KindPut, rel)
		op.Blob, op.Size, op.Mode = sum, n, mode
		if ok && c.Blob != "" {
			op.Base = c.Blob
		}
		op.Mtime = time.Unix(0, mt).UTC()
		ops = append(ops, op)
		s.cacheDirty = true
		cache[rel] = store.CachedFile{Blob: sum, Size: n, Mode: mode, MTimeNS: mt, Dev: dev, Ino: ino}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if !unreadableRoot {

		for rel := range cache {
			if len(unreadable) > 0 {
				blocked := false
				for u := range unreadable {
					if rel == u || strings.HasPrefix(rel, u+"/") {
						blocked = true
						break
					}
				}
				if blocked {
					continue
				}
			}
			if seen[rel] {
				continue
			}
			if !fullDiscovery {
				inScope := false
				for scope := range deleteScopes {
					if rel == scope || strings.HasPrefix(rel, scope+"/") {
						inScope = true
						break
					}
				}
				if !inScope {
					continue
				}
			}
			// The walk only ever produces clean in-mount paths, but this pass
			// mints an op from every cache key the walk did NOT see — and the
			// cache is a JSON file in $BDRIVE_HOME, not something the walk wrote.
			// Same rule the peer-op side applies, so this device never signs and
			// pushes a path it would refuse from anyone else.
			// Every arm below stops tracking the path, so every arm forgets its
			// findings too: a warning about a file this mount no longer syncs is
			// one nothing can clear.
			sec.drop(rel)
			// Deleting a read-only file locally is a local edit like any other:
			// not journaled, and put back by materialize on this same cycle.
			if s.readOnlyPath(rel) {
				continue
			}
			if unsafeRel(rel) || neverSync(rel) {
				delete(cache, rel)
				s.cacheDirty = true
				continue
			}
			if filter.Skip(rel) {
				delete(cache, rel) // newly filtered, not deleted: stop tracking silently
				s.cacheDirty = true
				continue
			}
			ops = append(ops, nextOp(journal.KindDelete, rel))
			delete(cache, rel)
			s.cacheDirty = true
		}
	}
	return ops, retryPaths, nil
}

// redefinesApplied reports whether have re-uses the (device, seq) of an op in
// applied for a DIFFERENT op. That slot is an op's identity within a journal —
// the one thing a peer cannot restate without contradicting itself — and
// contradicting it is how an already-applied op gets withdrawn while the op
// count, which is all the guard in pull can otherwise check, stays put.
//
// Sameness is exactly what journal.Less compares (the fields Replay reads), so
// two ops this calls equal fold to the same state; everything else on an Op is
// display only.
//
// A slot that is simply GONE is not a redefinition: a torn or corrupt line is
// the honest version of that, and pull's op-count guard is what covers it.
func redefinesApplied(have, applied []journal.Op) bool {
	if len(applied) == 0 {
		return false
	}
	appliedBySlot := make(map[string]string, len(applied))
	for _, op := range applied {
		slot := opSlot(op)
		id := opID(op)
		if prev, ok := appliedBySlot[slot]; ok && prev != id {
			return true
		}
		appliedBySlot[slot] = id
	}
	seenInHave := make(map[string]string, len(have))
	for _, op := range have {
		slot := opSlot(op)
		id := opID(op)
		if want, ok := appliedBySlot[slot]; ok && want != id {
			return true
		}
		if prev, ok := seenInHave[slot]; ok && prev != id {
			return true
		}
		seenInHave[slot] = id
	}
	return false
}

func opSlot(op journal.Op) string { return fmt.Sprintf("%s\x00%d", op.Device, op.Seq) }

func opID(op journal.Op) string {
	return fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%d\x00%s",
		opSlot(op), op.Lamport, op.Time.UTC().Format(time.RFC3339Nano),
		op.Kind, op.Path, op.Blob, op.Base, op.Size, op.Mode, op.Link)
}

// sizeGrowth is the headroom a bound gets over the size an object was
// declared at. Both bodies a device reads off the wire are bounded by what it
// was TOLD they were: the party serving the bytes must not also choose how
// many of them the daemon buffers (a journal body went straight into
// io.ReadAll, and a blob into an unbounded io.Copy in the volume's temp dir,
// with the hash check that would notice running after the copy — on a 3-second
// retry loop, forever). The slack is what makes the bound safe for the honest
// case: a journal legitimately grows between the LIST and the GET, and the
// next cycle picks up whatever did not fit.
const sizeGrowth = 1 << 20

func sizeBound(size int64) int64 {
	if size <= 0 || size > math.MaxInt64-sizeGrowth {
		return sizeGrowth
	}
	return size + sizeGrowth
}

// maxPullBytes is the absolute ceiling under sizeBound. sizeBound alone met its
// stated property only while the party serving the bytes was a PEER, whose
// numbers a separate hub had at least stored; when the peer IS the hub there is
// no second party at all, and one listing entry or one journal line sized the
// device's allocation and its disk. A journal is JSONL text (100 MiB is well
// over a million ops) and a blob this size is already far past what a synced
// project of notes and documents holds.
//
// It is a read CEILING, never an up-front refusal on the declared size: op.Size
// is a peer's integer with no relation to the object it names, so refusing on it
// let one peer line stop honest content from ever landing — the round-4 wedge
// class. An over-declared honest blob still reads to its real length and
// verifies; only bytes past the ceiling are refused.
//
// ponytail: an absolute constant, mirroring `bdrive import`'s maxImportBlob
// (256 << 20, raisable with --max-blob). A file larger than this does not
// materialize on receiving devices — if that becomes a real workload, this
// wants the same kind of knob, not a bigger constant. Raised from 32 MiB with
// delta sync: large files were the reason chunking shipped, and a ceiling
// below the files it was built for made it dead weight.
const maxPullBytes = 100 << 20

func pullBound(size int64) int64 { return min(sizeBound(size), maxPullBytes) }

// maxPeerJournals caps how many peer journal files one project's listing may
// mint on this disk. See pull.
const maxPeerJournals = 512

// reassertNote marks an op this device restated on a peer's behalf (step 3b).
// It is the only thing that distinguishes such an op from a local edit once it
// is in this device's journal, and conflictCopies depends on that distinction.
const reassertNote = "re-asserted: the device that published it withdrew it"

// adoptNote marks a local op demoted by the adoption step (1b): content this
// folder already held at a path the project it just joined also holds. Like
// reassertNote it is what tells conflictCopies the op is not a local edit — an
// adopted op is a loser by construction, so preserving it as a conflict copy is
// exactly the litter adoption exists to remove.
const adoptNote = "kept as history: the project's version of this path was adopted on join"

// errBlobContent marks "a blob's bytes are not its content address" — a
// statement about ONE object, never about whether the hub is reachable.
// Conflating the two let one peer integer (an understated Op.Size truncates an
// honest read and fails the sha) put the cycle in Offline, which is the flag
// step 5 checks before pushing anything: the victim's own journal and blobs
// stayed on the victim's disk, for one journal PUT per cycle.
var errBlobContent = errors.New("blob content mismatch")

// safeDevice reports whether a device id off a remote listing may become a
// path in this volume's journal dir. journal.SafePath is the repo's rule for a
// path a stranger named; a device id is one segment of one, so it also may not
// contain a separator and is bounded by NAME_MAX.
func safeDevice(dev string) bool {
	return dev != "" && len(dev) <= 200 && !strings.ContainsAny(dev, `/\`) && journal.SafePath(dev)
}

// sameJournalFile reports whether two journal paths name one file. os.SameFile
// is the only answer that survives a filesystem's own normalization (case on
// APFS/NTFS, unicode NFD on APFS) — string comparison is the check that let a
// hub address this device's own journal under another spelling.
func sameJournalFile(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

// stillHold reports whether this folder's own materialization currently stands
// behind op — the premise step 2b rests on ("an op this device already applied
// is a file on this disk"). The mount's state cache is the record of what THIS
// device wrote into THIS folder, so it is the only evidence that the op was
// applied here; the replayed state is not, since the withdrawal is exactly what
// changed it.
//
// A withdrawn DELETE is never re-asserted. The cache records what is here, never
// what left, so "this device applied that delete" and "this path never existed
// in the project" are the same observation — and re-asserting on it let a peer
// mint deletes of paths that never existed (round 7's inert-op primitive),
// withdraw them, and grow every teammate's append-only journal by that many ops
// for one journal PUT each. Withdrawing a delete instead resurrects the file,
// which only the op's own author can trigger and which they can reach anyway by
// re-putting the content.
func (s *Session) stillHold(op journal.Op, cache map[string]store.CachedFile) bool {
	if op.Kind != journal.KindPut || op.Blob == "" {
		return false
	}
	c, ok := cache[op.Path]
	return ok && c.Blob == op.Blob && s.Store.HasBlob(op.Blob)
}

// withdrawn reports the ops in applied whose (device, seq) slot is no longer
// present in have at all — an op this device already put on disk that the
// peer's republished journal simply does not carry any more.
//
// It is not a redefinition (redefinesApplied) and not a shrink (the op count
// can grow at the same time), so neither guard sees it; a peer replaces one
// applied line with bytes Parse drops, appends two, and a file leaves every
// teammate's folder with no delete op and nothing in History. Refusing the
// update instead is not available: a device that synced before the rewrite
// would then hold a state a device syncing for the first time can never reach
// (TestSec_Pull_APeerCannotChooseWhichOpsEachDeviceSees), and the peer would
// be choosing the split again. So the receiver RE-ASSERTS what it applied, as
// its own op — the one journal it is allowed to write — which both keeps the
// file and republishes the fact to every other device.
func withdrawn(have, applied []journal.Op) []journal.Op {
	if len(applied) == 0 {
		return nil
	}
	slots := make(map[string]bool, len(have))
	for _, op := range have {
		slots[opSlot(op)] = true
	}
	var out []journal.Op
	for _, op := range applied {
		if !slots[opSlot(op)] {
			out = append(out, op)
		}
	}
	return out
}
func isTransientOrServerTailErr(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "server: ")
}

// pull fetches journals that grew on the remote and any blobs we are missing
// for the new ops. It returns the ops we had not seen before, and any op a
// peer withdrew from a journal we had already applied (see withdrawn).
// cache is the mount's materialization state, read only for the delta basis:
// cache[path].Blob is the version of a file this device currently holds, and
// its chunks source a chunked pull locally (fetchChunked).
func (s *Session) pull(ctx context.Context, cache map[string]store.CachedFile) ([]journal.Op, []journal.Op, error) {
	objs, err := s.Backend.List(ctx, "journal/")
	if err != nil {
		return nil, nil, err
	}
	// The listing is the remote's answer, and the remote may be a hostile hub:
	// every key in it becomes a path in this volume's journal dir, and every
	// journal file there is re-read by AllOps on every later cycle. Round 7
	// capped the listing BODY; nothing capped the object COUNT, so one in-bounds
	// listing minted ~200k local files. New journals are admitted up to a
	// ceiling; existing ones always keep updating, so a real project's peers are
	// never starved by a flood.
	//
	// ponytail: 512 peer journals per project. A project with more devices than
	// that needs a paginated/authenticated peer list, not a bigger number.
	newJournals := maxPeerJournals
	if ents, err := os.ReadDir(filepath.Join(s.Store.Dir(), "journal")); err == nil {
		newJournals -= len(ents)
	}
	own := s.Store.JournalPath(s.Device.ID)
	var newOps, gone []journal.Op
	for _, o := range objs {
		name := strings.TrimPrefix(o.Key, "journal/")
		if !strings.HasSuffix(name, ".jsonl") || strings.Contains(name, "/") {
			continue
		}
		dev := strings.TrimSuffix(name, ".jsonl")
		if !safeDevice(dev) {
			// The hub picks these names and store.JournalPath validates
			// nothing. A name the OS cannot open (a NUL, a control byte, 300
			// bytes) is not a peer; it used to be a path this loop then failed
			// on, hiding every peer listed behind it.
			continue
		}
		lp := s.Store.JournalPath(dev)
		// Never this device's own journal — the one object it is the sole
		// writer of. An exact string compare is not the test: on a
		// case-insensitive filesystem (APFS and NTFS by default) "DEVA.jsonl"
		// is a different key and the SAME FILE, so the hub got to replace this
		// device's log and push it back as its own. The skip is on the FILE,
		// which is what the invariant is about.
		if strings.EqualFold(dev, s.Device.ID) || sameJournalFile(lp, own) {
			continue
		}
		var localSize int64
		isNew := true
		if fi, err := os.Stat(lp); err == nil {
			localSize, isNew = fi.Size(), false
		}
		if s.Store.PeerRemoteUnchanged(dev, localSize, o.Size, o.Modified) {
			continue
		}
		if isNew {
			if newJournals <= 0 {
				continue
			}
			newJournals--
		}
		var data []byte
		haveData := false
		prevFi, _ := os.Stat(lp)
		if localSize > 0 {
			if tg, ok := s.Backend.(remote.JournalTailGetter); ok {
				if statSize, prefixSHA, endsNL, statOK := s.Store.JournalPrefixStat(dev); statOK && statSize == localSize {
					rc, appended, tailErr := tg.GetJournalTail(ctx, o.Key, localSize, prefixSHA)
					if tailErr == nil {
						limit := pullBound(o.Size)
						if appended {
							limit = pullBound(max(int64(0), o.Size-localSize))
						}
						fetched, rerr := io.ReadAll(io.LimitReader(rc, limit))
						rc.Close()
						if rerr != nil {
							return newOps, gone, rerr
						}
						if appended {
							if len(fetched) == 0 {
								s.Store.RecordPeerRemoteMod(dev, localSize, o.Modified)
								continue
							}
							if endsNL {
								fresh, perr := journal.Parse(fetched)
								if perr == nil && s.Store.AppendValidatedPeerTail(dev, prevFi, localSize, fetched, fresh) {
									s.Store.RecordPeerRemoteMod(dev, localSize+int64(len(fetched)), o.Modified)
									newOps = append(newOps, fresh...)
									continue
								}
							}
						} else {
							data = fetched
							haveData = true
						}
					} else if errors.Is(tailErr, remote.ErrForbidden) || ctx.Err() != nil || isTransientOrServerTailErr(tailErr) {
						return newOps, gone, tailErr
					}
				}
			}
		}
		local, readErr := os.ReadFile(lp)
		if readErr != nil && !os.IsNotExist(readErr) {
			log.Printf("beardrive: skipping journal %s this cycle: %v", dev, readErr)
			continue
		}
		if !haveData {
			rc, getErr := s.Backend.Get(ctx, o.Key)
			if getErr != nil {
				return newOps, gone, getErr
			}
			data, err = io.ReadAll(io.LimitReader(rc, pullBound(o.Size)))
			rc.Close()
			if err != nil {
				return newOps, gone, err
			}
			haveData = true
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
		// The exact local bytes are retained above for byte-prefix validation.
		if bytes.Equal(local, data) {
			s.Store.RecordPeerRemoteMod(dev, int64(len(data)), o.Modified)
			continue
		}
		if len(local) > 0 && local[len(local)-1] == '\n' && bytes.HasPrefix(data, local) {
			tail := data[len(local):]
			fresh, err := journal.Parse(tail)
			if err == nil && s.Store.CanFastAppendPeer(dev, prevFi, local, fresh) {
				if err := store.WriteFileAtomic(lp, data, 0o600); err != nil {
					return newOps, gone, err
				}
				if newFi, stErr := os.Stat(lp); stErr == nil {
					s.Store.FoldPeerAppend(dev, prevFi, local, data, fresh, newFi)
				} else {
					s.Store.InvalidateJournal(dev)
				}
				s.Store.RecordPeerRemoteMod(dev, int64(len(data)), o.Modified)
				newOps = append(newOps, fresh...)
				continue
			}
		}
		// Resume only at a COMPLETE LINE. "Append-only" is the peer's claim
		// about its own object: publish a stage cut mid-line and both the size
		// gate and HasPrefix still pass, but the offset lands inside an op's
		// JSON, Parse drops the fragment, and the local copy is then
		// overwritten with the full object — so the next resume starts past an
		// op this device never applied, while every other device applies it.
		// The peer picks the split by choosing where to cut.
		accepted := local
		if i := bytes.LastIndexByte(accepted, '\n'); i >= 0 {
			accepted = accepted[:i+1]
		} else {
			accepted = nil
		}
		// An op we already applied must not be REDEFINED: if the object still
		// carries the (device, seq) slot we applied, it has to still be the
		// same op. This is an IDENTITY check, and it is what the op-count
		// guard below cannot do — a peer replaces an applied op's line with a
		// decodable but inert one (a delete of a path that never existed), the
		// count is preserved, and a file leaves every teammate's folder with no
		// delete op, nothing in the journal and nothing in History.
		//
		// It runs BEFORE the resume switch because it is needed on both arms:
		// `accepted` is trimmed to the last newline, but Parse does not require
		// a trailing newline, so an op published unterminated is APPLIED while
		// sitting outside the prefix HasPrefix protects — and a rewrite of that
		// op then looks like an ordinary append.
		applied, _ := journal.Parse(local)
		all, aerr := journal.Parse(data)
		if aerr != nil || redefinesApplied(all, applied) {
			continue
		}
		tail := data
		switch {
		case len(accepted) > 0 && bytes.HasPrefix(data, accepted):
			tail = data[len(accepted):]
		case len(accepted) > 0:
			// The peer rewrote its log instead of appending to it. Re-reading
			// it whole is fine for ordering (Replay is a fold) but it must not
			// UNDO: replacing an already-applied op's line with a longer
			// undecodable one grows the object and drops the op.
			prev, perr := journal.Parse(accepted)
			if perr != nil || len(all) < len(prev) {
				continue
			}
		}
		fresh, err := journal.Parse(tail)
		if err != nil {
			continue // corrupt remote journal; ignore rather than break sync
		}
		if err := store.WriteFileAtomic(lp, data, 0o600); err != nil {
			return newOps, gone, err
		}
		s.Store.InvalidateJournal(dev)
		s.Store.RecordPeerRemoteMod(dev, int64(len(data)), o.Modified)
		gone = append(gone, withdrawn(all, applied)...)
		newOps = append(newOps, fresh...)
	}

	fold, foldErr := s.Store.ReplayFold()
	if foldErr != nil {
		return newOps, gone, foldErr
	}
	fetchList := make([]journal.Op, 0, len(newOps))
	touched := map[string]bool{}
	for _, op := range newOps {
		touched[op.Path] = true
		if op.Note == adoptNote && op.Kind == journal.KindPut && op.Blob != "" && !s.Store.HasBlob(op.Blob) {
			fetchList = append(fetchList, op)
		}
	}
	for p := range touched {
		if win, ok := fold.Target[p]; ok && win.Blob != "" && !s.Store.HasBlob(win.Blob) {
			fetchList = append(fetchList, journal.Op{Kind: journal.KindPut, Path: p, Blob: win.Blob, Size: win.Size, Mode: win.Mode})
		}
	}
	var pushedOps int64
	if st, stErr := s.Store.LoadSync(); stErr == nil {
		pushedOps = st.PushedOps
	}
	myOps, myErr := s.Store.OwnOpsFrom(s.Device.ID, pushedOps)
	if myErr != nil {
		return newOps, gone, myErr
	}
	pending := map[string]journal.Op{}
	for _, op := range myOps {
		pending[op.Path] = op
	}
	if len(pending) > 0 {
		allOps, allErr := s.Store.AllOps()
		if allErr != nil {
			return newOps, gone, allErr
		}
		for p, mine := range pending {
			for _, their := range newOps {
				if their.Path != p || their.Kind != journal.KindPut {
					continue
				}
				fetchList = append(fetchList, their)
				if mine.Base != "" && !s.Store.HasBlob(mine.Base) {
					fetchList = append(fetchList, journal.Op{Kind: journal.KindPut, Path: p, Blob: mine.Base})
				}
				if anc, ok := commonAncestor(p, mine, their, allOps); ok && anc.Blob != "" && !s.Store.HasBlob(anc.Blob) {
					fetchList = append(fetchList, anc)
				}
			}
		}
	}
	_, err = s.fetchBlobs(ctx, fetchList, cache)
	return newOps, gone, err
}

// fetchBlobs downloads the content the given put ops name into the blob store
// and reports how many blobs landed. It returns an error only for a local
// write failure or, once every other op has been tried, for a blob whose bytes
// do not hash to its address (errBlobContent).
func (s *Session) fetchBlobs(ctx context.Context, ops []journal.Op, cache map[string]store.CachedFile) (int, error) {
	// Blobs are uploaded before journals on push, so anything referenced
	// should exist — but Op.Blob is a string a peer chose, so "missing" is a
	// case this loop has to survive rather than a contradiction. A blob that
	// cannot be fetched is left unfetched: materializeFile skips a path whose
	// content is not in the store yet and backfillOps hands it back here on a
	// later cycle, which is this package's posture for everything transient.
	// Abandoning the loop instead meant one op naming a blob that
	// was never pushed stopped every complete op behind it from ever landing.
	var (
		mu     sync.Mutex
		bad    error
		landed atomic.Int64
	)
	// Downloads run in parallel like push's uploads: a peer's large batch of
	// real changes is bound on serial round-trips, not bandwidth. The dispatch
	// loop below — not the workers — owns s.fetched, so it stays a plain map.
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(pullConcurrency)
	for _, op := range ops {
		if op.Kind != journal.KindPut || op.Blob == "" || s.fetched[op.Blob] || s.Store.HasBlob(op.Blob) {
			continue
		}
		if s.fetched != nil {
			s.fetched[op.Blob] = true
		}
		g.Go(func() error {
			ok, cerr, abort := s.fetchOneBlob(gctx, op, cache)
			if ok {
				landed.Add(1)
			}
			if cerr != nil {
				mu.Lock()
				if bad == nil {
					bad = cerr
				}
				mu.Unlock()
			}
			return abort
		})
	}
	if err := g.Wait(); err != nil {
		return int(landed.Load()), err
	}
	return int(landed.Load()), bad
}

// fetchOneBlob fetches the content a single put op names, trying the
// manifest/chunk path first for large files and falling back to the whole
// blob. ok reports whether the blob landed. cerr is a non-fatal problem worth
// surfacing once the rest of the batch has been tried (errBlobContent); it
// never aborts the batch. abort is a genuine local write failure, serious
// enough to cancel the rest of the batch — fetchBlobs returns it directly.
func (s *Session) fetchOneBlob(ctx context.Context, op journal.Op, cache map[string]store.CachedFile) (ok bool, cerr error, abort error) {
	// Large files: try the manifest first, sourcing unchanged chunks from
	// the version of this path we already hold. EVERY failure falls
	// through to the whole-blob path, not just errNoManifest: the whole
	// blob is independently hash-verified, so there is nothing to lose by
	// trying it — and not falling through let one member-written manifest
	// (the only object in the key space that is neither content-addressed
	// nor hash-checked at ingest) permanently deny a file whose correct
	// whole blob was sitting right there. A hash contradiction is still
	// remembered as the one signal worth surfacing (errBlobContent).
	if op.Size > chunkThreshold {
		var basis string
		if c, has := cache[op.Path]; has {
			basis = c.Blob
		}
		cerr2 := s.fetchChunked(ctx, op, basis)
		if cerr2 == nil {
			return true, nil, nil
		}
		// Fall through to the whole blob only when one actually EXISTS
		// (Exists never triggers hub reassembly). When it does, the
		// fallthrough is unconditional — the whole blob is independently
		// hash-verified, so a poisoned manifest cannot deny it. When it
		// does not — the normal chunked-only case — a transient chunk
		// failure retries chunks next cycle instead of asking the hub to
		// reassemble and re-download the entire file every tick.
		//
		// A chunked-path hash contradiction is recorded ONLY when no
		// fallback can land the file: when the whole blob arrives fine,
		// "blob corrupt on remote" would page an operator about a file
		// that just converged.
		if existsOK, eerr := s.Backend.Exists(ctx, "blobs/"+op.Blob); eerr != nil || !existsOK {
			if errors.Is(cerr2, errBlobContent) {
				return false, cerr2, nil
			}
			return false, nil, nil
		}
	}
	rc, err := s.Backend.Get(ctx, "blobs/"+op.Blob)
	if err != nil {
		return false, nil, nil
	}
	sum, _, err := s.Store.PutBlobReader(io.LimitReader(rc, pullBound(op.Size)))
	rc.Close()
	if err != nil {
		return false, nil, err
	}
	if sum != op.Blob {
		// Report it, but do NOT abandon the batch. Content addressing
		// already protects the disk (PutBlobReader files bytes under their
		// COMPUTED hash, so HasBlob(op.Blob) stays false and materializeFile
		// keeps skipping the path); the check earns its place as the only
		// SIGNAL that a hub is serving bytes that are not what they are
		// addressed as, since otherwise the case is indistinguishable from
		// "not uploaded yet".
		//
		// Returning here made that signal a weapon. op.Size bounds the read,
		// and op.Size is a field the PEER wrote with no relation to the
		// object it names — so declaring 1 byte for a real 3 MiB blob
		// truncated an honest read, failed the sha, and dropped every blob
		// queued BEHIND it in the same batch. The journal is on local disk
		// by then, so the next cycle yields no new ops and the loop is never
		// re-entered: one integer in one journal line decided which of a
		// peer's files each teammate never received. The mismatch is now
		// remembered and returned once the rest of the batch has been
		// fetched, so it still lands in res.Offline/OfflineErr.
		return false, fmt.Errorf("%w: blob %s corrupt on remote (got %s)", errBlobContent, shortSha(op.Blob), shortSha(sum)), nil
	}
	return true, nil, nil
}

// maxBackfill and maxBackfillBytes cap one cycle's retries by count and by the
// bytes their declared sizes allow it to read. The paths and sizes come from
// peers' journals, so a peer naming thousands of blobs it never uploaded, or a
// hub serving wrong bytes under a large declared size, would otherwise cost
// that much on every remote pass. The shuffle keeps such entries from starving
// real content that happens to sort first.
var (
	maxBackfill      = 512
	maxBackfillBytes = int64(2 * maxPullBytes)
)

// backfillOps lists put ops for the paths of the merged state this device
// would write but holds no content for, one cycle's worth, and whether any were
// left for a later cycle. A path the state cache already records at the wanted
// blob is on disk, so a converged volume yields nothing.
func (s *Session) backfillOps(target map[string]journal.FileState, cache map[string]store.CachedFile, filter *Filter) (picked []journal.Op, more bool) {
	var ops []journal.Op
	for rel, want := range target {
		if want.Blob == "" || neverSync(rel) || unsafeRel(rel) {
			continue
		}
		if c, ok := cache[rel]; ok && c.Blob == want.Blob {
			continue
		}
		if filter.Skip(rel) {
			continue
		}
		if s.fetched[want.Blob] || s.Store.HasBlob(want.Blob) {
			continue
		}
		ops = append(ops, journal.Op{Kind: journal.KindPut, Path: rel, Blob: want.Blob, Size: want.Size, Mode: want.Mode})
	}
	rand.Shuffle(len(ops), func(i, j int) { ops[i], ops[j] = ops[j], ops[i] })
	var budget int64
	for _, op := range ops {
		if len(picked) == maxBackfill {
			return picked, true
		}
		b := pullBound(op.Size)
		if len(picked) > 0 && budget+b > maxBackfillBytes {
			more = true
			continue
		}
		picked = append(picked, op)
		budget += b
	}
	return picked, more
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
// It runs BEFORE the re-assertion step on purpose: a re-asserted op is not this
// device's edit — it carries the withdrawn op's original (and therefore usually
// losing) lamport — so having it in myOps here made it a losing local unpushed
// op by construction, and a peer with no collision at all could make a victim
// author, sign and push a conflict copy at a path that never existed, holding
// content the peer chose.
func (s *Session) conflictCopies(myOps []journal.Op, pushed int64, pulled []journal.Op, st *store.SyncState) ([]journal.Op, error) {
	all, err := s.Store.AllOps()
	if err != nil {
		return nil, err
	}
	if pushed < 0 {
		pushed = 0
	}
	if pushed > int64(len(myOps)) {
		pushed = int64(len(myOps))
	}
	unpushed := myOps[pushed:]
	return s.conflictCopiesWithState(unpushed, int64(len(myOps)), pulled, st, all, journal.Replay(all))
}

func (s *Session) conflictCopiesWithState(unpushedOps []journal.Op, seqBase int64, pulled []journal.Op, st *store.SyncState, all []journal.Op, state map[string]journal.FileState) ([]journal.Op, error) {
	unpushed := map[string]journal.Op{}
	for _, op := range unpushedOps {
		// adoptNote: the same reasoning one step removed — an adopted op was
		// demoted precisely because the project's version wins, so it is a
		// losing unpushed local op by construction and would conflict-copy
		// every path a joining folder shares with the project. That is the
		// litter step 1b exists to remove.
		if op.Note == reassertNote || op.Note == adoptNote {
			// A re-asserted op is not an edit this device made — it restates a
			// peer's op that the peer withdrew, carrying that op's original
			// (and therefore usually losing) clock. Round 9 kept it out of the
			// unpushed set by ORDERING (re-assert after this step), which holds
			// for exactly one cycle: `pushed` only advances on a successful
			// push, and a read-only member is the documented steady state where
			// local ops stay journaled and unpushed forever. So it came back
			// into this set on every later cycle, and two withdrawals made the
			// victim author and journal peer-chosen content at a new path.
			// Marked, not ordered.
			delete(unpushed, op.Path)
			continue
		}
		unpushed[op.Path] = op // latest local op per path
	}
	pulledByPath := map[string][]journal.Op{}
	for _, op := range pulled {
		if op.Kind != journal.KindPut {
			continue
		}
		if _, ok := unpushed[op.Path]; !ok {
			continue
		}
		pulledByPath[op.Path] = append(pulledByPath[op.Path], op)
	}
	for p := range pulledByPath {
		sort.Slice(pulledByPath[p], func(i, j int) bool {
			return journal.Less(pulledByPath[p][i], pulledByPath[p][j])
		})
	}
	if len(unpushed) == 0 {
		return nil, nil
	}
	var out []journal.Op
	// Every unpushed local op, not just the paths a pulled op touched: what
	// makes an edit need preserving is that replay did not resolve to it, and
	// whether the op that beat it arrived THIS cycle or was already on disk
	// makes no difference to the user whose bytes are about to be overwritten.
	for p, mine := range unpushed {
		theirsList := pulledByPath[p]
		collided := len(theirsList) > 0
		var theirs journal.Op
		if collided {
			theirs = theirsList[len(theirsList)-1]
		}
		cur, exists := state[p]
		mineWon := (mine.Kind == journal.KindPut && exists && cur.Blob == mine.Blob) ||
			(mine.Kind == journal.KindDelete && !exists)
		loser := mine
		if mineWon {
			// Our version is what replay resolved to, so the only thing that
			// could need preserving is the op that lost to it — and with no
			// pull this cycle there is no such op. This is the common case now
			// that the candidate set is every unpushed op: a healthy device's
			// own edits win, and nothing is written.
			if !collided {
				continue
			}
			for _, candidate := range theirsList {
				if candidate.Blob != "" && !s.Store.HasBlob(candidate.Blob) {
					_, _ = s.fetchBlobs(context.Background(), []journal.Op{candidate}, nil)
				}
			}
			loser = theirs
		}
		if loser.Kind != journal.KindPut || loser.Blob == "" {
			continue // a lost delete needs no preservation
		}
		// By blob, never by Size: a blob is the hash of its bytes, and Size
		// is only what a peer's journal claims.
		if loser.Blob == emptyBlob {
			continue // an empty version holds nothing to preserve
		}
		if exists && cur.Blob == loser.Blob {
			continue // identical content; nothing actually lost
		}
		if !s.Store.HasBlob(loser.Blob) {
			continue // content unavailable (partial pull); skip rather than fail
		}
		// Already preserved. `pushed` only advances on a successful push, so a
		// losing op stays in the unpushed set and is re-examined on every later
		// cycle — and the copy's name is derived from the loser, so without
		// this the same bytes are re-journaled under the same path forever.
		// Only reachable now that a collision is no longer required.
		if kept, ok := state[conflictName(p, loser.DeviceName, loser.Time)]; ok && kept.Blob == loser.Blob {
			continue
		}
		// Two people editing DIFFERENT parts of one text file is the common
		// case, and forking it into a conflict copy makes both of them do a
		// merge by hand that the machine could have done. Try the merge; if
		// the edits overlap, fall through to exactly the behaviour below.
		//
		// The merged op is an ordinary put of ordinary content — the same
		// shape the conflict copy is, at the real path instead of a new one —
		// so nothing about replay, ordering or the one-writer rule changes.
		// Only against an op that actually arrived: tryMerge takes THEIR op,
		// and without a collision there is no such op to merge with.
		// An empty file that beat content is almost never an edit anyone
		// meant: it is a truncated save, a failed read written back, a tool
		// that wrote before it had the bytes. The content goes back to the
		// real path; a copy beside an empty file leaves the reader with an
		// empty file to look at and the work under a name nobody opens.
		if exists && cur.Link == "" && cur.Blob == emptyBlob {
			// The size this op states is the blob's own, read here: the loser's
			// Size is a peer's claim, and a puller bounds its read by it.
			fi, err := os.Stat(s.Store.BlobPath(loser.Blob))
			if err != nil {
				continue
			}
			st.Lamport = tickLamport(st.Lamport)
			seqBase++
			out = append(out, journal.Op{
				Seq: seqBase, Lamport: st.Lamport, Time: time.Now().UTC(),
				Device: s.Device.ID, DeviceName: s.Device.Name, Author: s.Device.Author,
				User: s.Account.Email, UserName: s.Account.Name,
				Kind: journal.KindPut, Path: p,
				Blob: loser.Blob, Size: fi.Size(), Mode: loser.Mode,
				Note: keptOverEmptyNote,
			})
			continue
		}
		// What the loser is merged against is the op that WON — the version
		// replay resolved to — so the merge proves by content alone whether
		// that version already holds the loser's edit. Lineage is never
		// guessed from Base links: in X → A → X → B, B built on the second X
		// does not hold A even though a chain of blobs leads back through it.
		merged, canMerge := mergedBlob{}, false
		other, haveOther := theirs, collided
		if !mineWon && exists && cur.Link == "" && cur.Blob != "" {
			if all == nil {
				var err error
				all, err = s.Store.AllOps()
				if err != nil {
					return nil, err
				}
			}
			if winner, ok := latestPut(p, cur.Blob, theirsList, unpushedOps, all); ok {
				other, haveOther = winner, true
			}
		}
		if haveOther {
			if all == nil {
				var err error
				all, err = s.Store.AllOps()
				if err != nil {
					return nil, err
				}
			}
			if other.Blob != "" && !s.Store.HasBlob(other.Blob) {
				_, _ = s.fetchBlobs(context.Background(), []journal.Op{other}, nil)
			}
			merged, canMerge = s.tryMerge(p, mine, other, all)
		}
		if canMerge && exists && merged.blob == cur.Blob {
			// Content proof: merging the loser into the winner yields the
			// winner's exact bytes, so it already holds both sides — another
			// device (or the hub) computed the merge first. Fast-forward: no
			// second op for it.
			continue
		}
		if canMerge {
			st.Lamport = tickLamport(st.Lamport)
			seqBase++
			op := journal.Op{
				Seq: seqBase, Lamport: st.Lamport, Time: time.Now().UTC(),
				Device: s.Device.ID, DeviceName: s.Device.Name, Author: s.Device.Author,
				User: s.Account.Email, UserName: s.Account.Name,
				Kind: journal.KindPut, Path: p,
				Blob: merged.blob, Size: merged.size, Mode: mine.Mode,
				Note: mergeNote,
			}
			if exists && cur.Link == "" {
				op.Base = cur.Blob // built on the version that won
			}
			out = append(out, op)
			continue
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

// readOnlyDrifted reports whether a read-only path's file on disk no longer
// matches what this device last materialized there — edited, or removed. Only
// consulted for read-only paths, so it costs one Stat on exactly the files
// whose local changes can never be journaled.
func (s *Session) readOnlyDrifted(rel, abs string, c store.CachedFile) bool {
	// A denied path has no project version to put back — the hub sends no ops
	// for it — so there is nothing to revert to and the local file is simply
	// this machine's own.
	if !s.readOnlyPath(rel) || s.deniedPath(rel) {
		return false
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return true // removed locally: the project still holds it, so put it back
	}
	return fi.Size() != c.Size || fi.ModTime().UnixNano() != c.MTimeNS
}

// keepLocalCopy preserves a read-only file's local edit beside it before the
// project's version is written back over it. Named like a conflict copy so it
// reads the same way in a file listing, but it is not one: no op is written,
// no peer will ever see it, and it stays on this machine.
//
// A copy that already holds these exact bytes is not made again — otherwise
// every cycle after an edit mints another one.
func (s *Session) keepLocalCopy(abs string) error {
	dir, base := filepath.Split(abs)
	ext := filepath.Ext(base)
	copyAbs := filepath.Join(dir, strings.TrimSuffix(base, ext)+" (local, not synced)"+ext)
	mine, err := hashFile(abs)
	if err != nil {
		return err
	}
	if theirs, err := hashFile(copyAbs); err == nil && theirs == mine {
		return nil
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	return os.WriteFile(copyAbs, data, 0o644)
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
		if neverSync(rel) {
			continue
		}
		if want.Link == "" {
			if c, ok := cache[rel]; ok && c.Link == "" && c.Blob == want.Blob &&
				c.Mode == safeMode(want.Mode) && !s.readOnlyPath(rel) {
				continue
			}
		}
		if filter.Skip(rel) {
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
		// The write loop above got unsafeRel, neverSync and UnderRoot in round
		// 4; this loop got none of them, and it ends in os.Remove.
		if unsafeRel(rel) || neverSync(rel) {
			delete(cache, rel)
			s.cacheDirty = true
			continue
		}
		if filter.Skip(rel) {
			// The path left sync scope rather than being deleted — someone
			// ignored it, or `--prune` removed it from the hub. Stop tracking
			// it; the file itself is ours to keep. Without this guard a prune
			// (or any delete op for a now-filtered path) unlinks every peer's
			// local copy, which is the data loss the feature exists to avoid.
			delete(cache, rel)
			s.cacheDirty = true
			continue
		}
		abs := filepath.Join(s.Folder, filepath.FromSlash(rel))
		if !store.UnderRoot(s.Folder, filepath.Dir(abs)) {
			delete(cache, rel)
			s.cacheDirty = true
			continue
		}
		if fi, err := os.Lstat(abs); err == nil {
			if !cachedPathMatches(abs, fi, c) {
				continue // dirty; do not delete fresh local edits
			}
			if err := os.Remove(abs); err != nil {
				skip(err)
				continue
			}
			pruneEmptyDirs(s.Folder, filepath.Dir(abs))
			// Tell the next agent turn what vanished under it. Best-effort:
			// a spool failure must never fail a cycle. Only the branch that
			// actually unlinked a file logs — the paths below it were already
			// gone locally, so nothing changed under the agent.
			s.logInbound(rel, true)
		}
		delete(cache, rel)
		s.cacheDirty = true
		changed++
	}
	return changed, nil
}

// materializeFile writes one path of the merged state into the working
// folder, reporting whether it wrote. It never clobbers a file that changed
// since the scan earlier in this cycle. Split out of materialize so the cycle
// can land .bdriveignore on its own, before the rules are needed.
func (s *Session) materializeFile(rel string, want journal.FileState, cache map[string]store.CachedFile) (bool, error) {
	if want.Link != "" {
		return s.materializeSymlink(rel, want, cache)
	}
	want.Mode = safeMode(want.Mode) // before the cache compare, or every cycle rewrites
	abs := filepath.Join(s.Folder, filepath.FromSlash(rel))
	c, ok := cache[rel]
	// The cache agreeing with the target normally means there is nothing to
	// do — a local edit would have been journaled by the scan, which updates
	// the cache. Under a read-only folder the scan journals nothing and
	// updates nothing, so the cache still agrees while the file on disk has
	// moved: the file itself is the only evidence, and without this the revert
	// below is unreachable and a read-only file silently diverges forever.
	if ok && c.Blob == want.Blob && c.Mode == want.Mode && !s.readOnlyDrifted(rel, abs, c) {
		return false, nil
	}
	if ok && !s.readOnlyPath(rel) {
		fi, err := os.Lstat(abs)
		if err != nil || !cachedPathMatches(abs, fi, c) {
			// A tracked writable path is safe to replace only when the
			// current entry still matches the cached type and fingerprint.
			// Missing entries and local type/content changes belong to the
			// next targeted scan.
			return false, nil
		}
	}
	if !ok {
		// An untracked symlink is local user state, even when its target is
		// readable. Lstat is required here: Stat would follow a non-dangling
		// link into content that can be adopted or a dangling link into an
		// error, and both cases would fall through to writeFile and replace
		// the link with peer bytes.
		if fi, err := os.Lstat(abs); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return false, nil
		}
	}
	if fi, err := os.Stat(abs); err == nil {
		if ok && c.Link == "" && (fi.Size() != c.Size || fi.ModTime().UnixNano() != c.MTimeNS) {
			// The one place a dirty file is not left alone. Everywhere else
			// "dirty" means the next scan will commit it — under a read-only
			// folder the next scan will NOT, so leaving it would mean a file
			// that silently stops receiving the project's updates forever and
			// a local edit nobody else ever sees.
			//
			// The user's bytes are never dropped: they are copied aside first,
			// under the same read-only prefix, so that copy is not journaled
			// either and stays this machine's business.
			if !s.readOnlyPath(rel) {
				return false, nil // dirty: changed mid-cycle, next scan commits it
			}
			if err := s.keepLocalCopy(abs); err != nil {
				return false, err
			}
			s.reverted = append(s.reverted, rel)
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
	// Cache the snapshot we wrote, not a later local writer's fingerprint.
	// Otherwise that writer's bytes could be mistaken for want.Blob forever.
	tmpFi, err := s.writeFileStat(abs, want)
	if err != nil {
		return false, fmt.Errorf("write %s: %w", rel, err)
	}
	postFi, err := os.Lstat(abs)
	if err != nil {
		return true, nil // a local removal belongs to the next scan
	}
	tmpDev, tmpIno, tmpOK := store.FileIdentity(tmpFi)
	postDev, postIno, postOK := store.FileIdentity(postFi)
	if !postFi.Mode().IsRegular() || postFi.Size() != tmpFi.Size() ||
		postFi.ModTime().UnixNano() != tmpFi.ModTime().UnixNano() ||
		(tmpOK && postOK && (tmpDev != postDev || tmpIno != postIno)) {
		return true, nil
	}
	s.cacheDirty = true
	cache[rel] = store.CachedFile{
		Blob: want.Blob, Size: tmpFi.Size(), Mode: want.Mode,
		MTimeNS: tmpFi.ModTime().UnixNano(), Dev: tmpDev, Ino: tmpIno,
	}
	// Spool it for the next agent turn ("changed since your last turn"). Only
	// peer content reaches here — a local edit is journaled by the scan and
	// lands in the cache, so the compare above short-circuits before any
	// write. Best-effort: a spool failure must never fail a cycle.
	//
	// Known trade: the first cycle on a fresh mount materializes the whole
	// project, so that one turn's list is "everything" — bounded by the
	// hook's render cap, and cheaper than special-casing an empty cache.
	s.logInbound(rel, false)
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
//
// One spelling, config.ReservedPath, rather than a per-segment loop of its
// parts: the builtin exclusions grew a whole-PATH rule (an agent's
// project-level hook config, config.AgentHookConfig) that "a reserved dir plus
// a reserved base name" cannot express, and a second copy of the rule is a copy
// that will miss the next one.
func neverSync(rel string) bool {
	return unsafeRel(rel) || config.ReservedPath(rel)
}

// unsafeRel reports whether an op's Path escapes the mount root. scan only
// ever produces clean relative paths, but Op.Path is arbitrary JSON off a
// peer's journal and materialize resolves it with filepath.Join(s.Folder, …),
// which walks above the root without complaint — one pushed line would reach
// ~/.ssh/authorized_keys on every teammate's machine.
//
// The rule itself is journal.SafePath, shared with the hub's two ingest doors:
// this spelling used to be its own copy and was the one MISSING the
// control-character clause, so the /store/* journal door accepted paths
// /upload/commit answers 400 to.
func unsafeRel(rel string) bool { return !journal.SafePath(rel) }

// safeMode is the only mode materialize will apply. scan records
// info.Mode().Perm(), but Op.Mode is a raw uint32 off the wire and
// fs.ModeSetuid/ModeSetgid live in that same word — os.Chmod would turn a
// peer's op into a setuid binary in every teammate's folder. Group/other write
// goes too: a synced file is never a drop box for other users on the machine.
func safeMode(m uint32) uint32 { return m & 0o777 &^ 0o022 }

// cachedPathMatches reports whether the current filesystem entry still has
// the type and fingerprint captured in the materialization cache. Lstat is
// required: a cached dangling link must be compared as a link rather than
// treated as a missing file through Stat.
func cachedPathMatches(abs string, fi os.FileInfo, c store.CachedFile) bool {
	if c.Link != "" {
		if fi.Mode()&os.ModeSymlink == 0 {
			return false
		}
		cur, err := os.Readlink(abs)
		return err == nil && cur == c.Link &&
			fi.Size() == c.Size &&
			fi.ModTime().UnixNano() == c.MTimeNS
	}
	if c.Dev != 0 && c.Ino != 0 {
		if dev, ino, ok := store.FileIdentity(fi); ok && (dev != c.Dev || ino != c.Ino) {
			return false
		}
	}
	return fi.Mode().IsRegular() &&
		fi.Size() == c.Size &&
		fi.ModTime().UnixNano() == c.MTimeNS
}

// materializeSymlink recreates a peer's symbolic link. It writes only the link
// entry: the target string is reproduced verbatim and never resolved, so a link
// pointing outside the mount lands as a dangling or escaping link that this
// device's own filesystem may resolve, but that the sync engine never opens.
func (s *Session) materializeSymlink(rel string, want journal.FileState, cache map[string]store.CachedFile) (bool, error) {
	abs := filepath.Join(s.Folder, filepath.FromSlash(rel))
	c, ok := cache[rel]
	if ok && !s.readOnlyPath(rel) {
		fi, err := os.Lstat(abs)
		if err != nil || !cachedPathMatches(abs, fi, c) {
			// A tracked writable path is safe to replace only when the
			// current entry still matches the cached type and fingerprint.
			// Missing entries and local type/content changes belong to the
			// next targeted scan.
			return false, nil
		}
		if c.Link != "" {
			cur, err := os.Readlink(abs)
			if err == nil && cur == want.Link {
				return false, nil
			}
		}
	}
	if fi, err := os.Lstat(abs); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			// A link already here: adopt it if it points where we want, else
			// (an untracked, differing link) leave it for the next scan.
			cur, rerr := os.Readlink(abs)
			if rerr == nil && cur == want.Link {
				s.cacheDirty = true
				cache[rel] = store.CachedFile{Link: want.Link, Size: fi.Size(), MTimeNS: fi.ModTime().UnixNano()}
				return false, nil
			}
			if !ok {
				return false, nil
			}
		} else if !ok {
			// An untracked real file sits at this path: never clobber it.
			return false, nil
		}
	}
	if err := s.writeSymlink(abs, want.Link); err != nil {
		return false, fmt.Errorf("symlink %s: %w", rel, err)
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return false, err
	}
	s.cacheDirty = true
	cache[rel] = store.CachedFile{Link: want.Link, Size: fi.Size(), MTimeNS: fi.ModTime().UnixNano()}
	s.logInbound(rel, false)
	return true, nil
}

// writeSymlink atomically places a link at abs pointing at target. The root
// check is on the PARENT directory, not abs: os.Symlink writes the link entry
// into the parent, and a symlink is deliberately allowed to POINT outside the
// mount — what must stay inside is where the link itself is created. (UnderRoot
// on abs would also reject replacing an existing dangling link, which is us.)
func (s *Session) writeSymlink(abs, target string) error {
	dir := filepath.Dir(abs)
	if !store.UnderRoot(s.Folder, dir) {
		return fmt.Errorf("resolves outside the mount root")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".bdrive-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	tmp.Close()
	if err := os.Remove(tmpName); err != nil {
		return err
	}
	if err := os.Symlink(target, tmpName); err != nil {
		return err
	}
	if err := os.Rename(tmpName, abs); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

func (s *Session) writeFile(abs string, want journal.FileState) error {
	_, err := s.writeFileStat(abs, want)
	return err
}

func (s *Session) writeFileStat(abs string, want journal.FileState) (os.FileInfo, error) {
	// unsafeRel judged the path's SPELLING; this is the same boundary on disk.
	// MkdirAll, CreateTemp and Rename all follow symlinks, so a directory
	// inside the mount that is a symlink makes "docs/x.md" a perfectly clean
	// relative path landing outside the mount — and walkFolder refuses to
	// descend into one, so such a directory is a one-way door: it takes peer
	// writes and never reports them. Checked before MkdirAll, so a refused op
	// does not even build the parent chain on the far side.
	//
	// The final entry itself may be a dangling symlink from a tracked prior
	// state. Checking its parent permits replacing that entry atomically while
	// still rejecting symlinked ancestor directories.
	if !store.UnderRoot(s.Folder, filepath.Dir(abs)) {
		return nil, fmt.Errorf("resolves outside the mount root")
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, err
	}
	mode := os.FileMode(want.Mode)
	if mode == 0 {
		mode = 0o644
	}
	tmpPath, fi, err := s.Store.MaterializeBlobToTemp(want.Blob, filepath.Dir(abs), mode)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmpPath)
	if err := os.Rename(tmpPath, abs); err != nil {
		return nil, err
	}
	return fi, nil
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
			// Large files move as chunks + a manifest (chunks.go); the whole
			// blob is never uploaded for them. Everything else is unchanged.
			if j.size > chunkThreshold {
				n, err := s.pushChunked(gctx, j.blob)
				if err != nil {
					return err
				}
				atomic.AddInt64(&done, 1)
				atomic.AddInt64(&bytesDone, n)
				report()
				return nil
			}
			key := "blobs/" + j.blob
			if v, ok := s.Backend.(interface{ IsVerified(string) bool }); ok && v.IsVerified(key) {
				atomic.AddInt64(&done, 1)
				atomic.AddInt64(&bytesDone, j.size)
				report()
				return nil
			}
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

	if st.PushedOps > 0 && st.PushedOps < int64(len(myOps)) {
		if ap, ok := s.Backend.(remote.JournalAppender); ok {
			if off, prevSHA, ok := s.Store.JournalPrefixAt(s.Device.ID, st.PushedOps); ok && off >= 0 {
				jf, err := os.Open(s.Store.JournalPath(s.Device.ID))
				if err != nil {
					return err
				}
				_, serr := jf.Seek(off, io.SeekStart)
				tailData, rerr := io.ReadAll(jf)
				jf.Close()
				if serr != nil {
					return serr
				}
				if rerr != nil {
					return rerr
				}
				if len(tailData) > 0 {
					err := ap.AppendJournal(ctx, remote.JournalAppendRequest{
						Device: s.Device.ID, PrevSize: off, PrevSHA256: prevSHA, Data: tailData,
					})
					if err == nil {
						st.PushedOps = int64(len(myOps))
						return nil
					}
					if !errors.Is(err, remote.ErrJournalAppendUnsupported) &&
						!errors.Is(err, remote.ErrJournalPrefixMismatch) {
						return err
					}
				}
			}
		}
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
	//
	// Op.Time is no more verified than Op.Mtime — same JSON line, same peer —
	// so bounding one by the other only moves the value a field to the left.
	// The one clock a peer does not own is this machine's: a stamp later than
	// now is not a write time at all, and an op we cannot date does not get to
	// outrank the changes we can. It sorts last rather than first, which is
	// the direction that cannot be aimed.
	now := time.Now()
	if !op.Mtime.IsZero() && !op.Mtime.After(op.Time) && !op.Mtime.After(now) {
		return op.Mtime
	}
	if op.Time.After(now) {
		return time.Time{}
	}
	return op.Time
}

// CommitTime is when a change entered the project, which is the question
// `bdrive log` answers. It is not DisplayTime: `mv` preserves mtime, so the put
// half of a rename carries the original file's write time and sorts away from
// the delete half of the same rename — a file that appeared seconds ago lands
// below the fold.
//
// Same clamp as DisplayTime, for the same reason: Op.Time is a peer's JSON, and
// the one clock a peer does not own is this machine's. An op we cannot date
// sorts last rather than first, which is the direction that cannot be aimed.
func CommitTime(op journal.Op) time.Time {
	if op.Time.After(time.Now()) {
		return time.Time{}
	}
	return op.Time
}

// SortForDisplay orders ops newest-first by CommitTime — when the change
// entered the project — so the two halves of a rename sit together and an old
// file added today sorts by when it arrived. DisplayTime breaks ties and is
// load-bearing: everything from one scan shares a commit second, and the files'
// own write times are the only thing that orders it inside that second. Ties
// beyond that fall back to reversed journal.Less to stay deterministic. This is
// deliberately NOT the replay order: LogEntries keeps returning causal order
// because bdrive restore walks it to find a file's previous version.
func SortForDisplay(ops []journal.Op) {
	sort.SliceStable(ops, func(i, j int) bool {
		ti, tj := CommitTime(ops[i]), CommitTime(ops[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		di, dj := DisplayTime(ops[i]), DisplayTime(ops[j])
		if !di.Equal(dj) {
			return di.After(dj)
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
