// Package store manages a volume's local on-disk state: a content-addressed
// blob store, the per-device journals, and small JSON state files. Everything
// a volume needs works offline; the remote is only used to exchange blobs and
// journals.
//
// Layout under <beardrive home>/volumes/<volume>/:
//
//	blobs/<aa>/<sha256>   content-addressed file contents (immutable)
//	journal/<device>.jsonl per-device op logs (own + cached copies of peers)
//	state.json            what is currently materialized in the folder
//	sync.json             lamport clock + push cursor
//	lock                  flock guarding cycles
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/runbear-io/beardrive/internal/journal"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

var ErrFileChangedDuringRead = errors.New("file changed during read")

type Store struct {
	dir                string
	mu                 sync.Mutex
	stateCaches        map[string]cacheEntry
	folds              map[string]*deviceFold
	ignoreFingerprints map[string][32]byte
	nextFoldGen        uint64
	cachedFold         FoldResult
	cachedFoldDevs     []string
	cachedFoldGens     []uint64
	cachedFoldOK       bool
}

func Open(dir string) (*Store, error) {
	for _, d := range []string{dir, filepath.Join(dir, "blobs"), filepath.Join(dir, "journal"), filepath.Join(dir, "tmp")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{dir: dir, stateCaches: make(map[string]cacheEntry), folds: make(map[string]*deviceFold), ignoreFingerprints: make(map[string][32]byte)}, nil
}

func (s *Store) Dir() string    { return s.dir }
func (s *Store) tmpDir() string { return filepath.Join(s.dir, "tmp") }

// ---- blobs ----

// blobSumRe is the only thing BlobPath will resolve. Op.Blob is copied
// verbatim out of a peer's journal, so it is a string someone else chose, not
// a hash: "../secret.txt" made HasBlob answer true for a file outside the
// store and OpenBlob hand its contents to writeFile as that path's content —
// a peer reading any file on every teammate's machine — and a Blob shorter
// than two characters panicked the daemon on sum[:2].
var blobSumRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// BlobPath is the on-disk location of a blob. A key that is not a sha256
// resolves to a name inside the blob dir that PutBlobReader never writes, so
// HasBlob and OpenBlob fail naturally instead of reaching somewhere else.
func (s *Store) BlobPath(sum string) string {
	if !blobSumRe.MatchString(sum) {
		return filepath.Join(s.dir, "blobs", "invalid")
	}
	return filepath.Join(s.dir, "blobs", sum[:2], sum)
}

func (s *Store) HasBlob(sum string) bool {
	if !blobSumRe.MatchString(sum) {
		return false
	}
	_, err := os.Stat(s.BlobPath(sum))
	return err == nil
}

// PutBlobReader streams r into the blob store, returning its sha256 and size.
func (s *Store) PutBlobReader(r io.Reader) (string, int64, error) {
	tmp, err := os.CreateTemp(s.tmpDir(), "blob-*")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if s.HasBlob(sum) {
		return sum, n, nil
	}
	dst := s.BlobPath(sum)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", 0, err
	}
	return sum, n, nil
}

func (s *Store) PutBlobFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	return s.PutBlobReader(f)
}

// PutBlobFileVerified captures a regular file without caching a mixed or stale read.
func (s *Store) PutBlobFileVerified(srcPath string) (sha string, size int64, mtimeNS int64, dev uint64, ino uint64, err error) {
	preFi, err := os.Lstat(srcPath)
	if err != nil {
		return "", 0, 0, 0, 0, err
	}
	if !preFi.Mode().IsRegular() {
		return "", 0, 0, 0, 0, fmt.Errorf("%s is not a regular file", srcPath)
	}
	preDev, preIno, _ := FileIdentity(preFi)
	preSize, preMtime := preFi.Size(), preFi.ModTime().UnixNano()
	blobDir := filepath.Join(s.dir, "blobs")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		return "", 0, 0, 0, 0, err
	}
	tmp, err := os.CreateTemp(blobDir, ".bdrive-tmp-*")
	if err != nil {
		return "", 0, 0, 0, 0, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Close(); err != nil {
		return "", 0, 0, 0, 0, err
	}
	var n int64
	h := sha256.New()
	if cloneErr := tryCloneFile(srcPath, tmpPath); cloneErr == nil {
		f, openErr := os.Open(tmpPath)
		if openErr != nil {
			return "", 0, 0, 0, 0, openErr
		}
		n, err = io.Copy(h, f)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	} else {
		src, openErr := os.Open(srcPath)
		if openErr != nil {
			return "", 0, 0, 0, 0, openErr
		}
		dst, createErr := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if createErr != nil {
			_ = src.Close()
			return "", 0, 0, 0, 0, createErr
		}
		n, err = io.Copy(io.MultiWriter(dst, h), src)
		if closeErr := dst.Close(); err == nil {
			err = closeErr
		}
		if closeErr := src.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		return "", 0, 0, 0, 0, err
	}
	postFi, postErr := os.Lstat(srcPath)
	if postErr != nil || !postFi.Mode().IsRegular() || n != preSize || postFi.Size() != preSize || postFi.ModTime().UnixNano() != preMtime {
		return "", 0, 0, 0, 0, ErrFileChangedDuringRead
	}
	postDev, postIno, postOK := FileIdentity(postFi)
	if (preDev != 0 || preIno != 0) && (!postOK || postDev != preDev || postIno != preIno) {
		return "", 0, 0, 0, 0, ErrFileChangedDuringRead
	}
	sum := hex.EncodeToString(h.Sum(nil))
	_ = os.Chmod(tmpPath, 0o444)
	dst := s.BlobPath(sum)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", 0, 0, 0, 0, err
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		return sum, preSize, preMtime, preDev, preIno, nil
	} else if !os.IsNotExist(statErr) {
		return "", 0, 0, 0, 0, statErr
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return "", 0, 0, 0, 0, err
	}
	return sum, preSize, preMtime, preDev, preIno, nil
}

// MaterializeBlobToTemp creates a writable working-tree temporary from a blob.
func (s *Store) MaterializeBlobToTemp(sha, dstDir string, mode os.FileMode) (tmpPath string, fi os.FileInfo, err error) {
	if !s.HasBlob(sha) {
		return "", nil, fmt.Errorf("blob %q not found", sha)
	}
	tmp, err := os.CreateTemp(dstDir, ".bdrive-tmp-*")
	if err != nil {
		return "", nil, err
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Close(); err != nil {
		return "", nil, err
	}
	if cloneErr := tryCloneFile(s.BlobPath(sha), name); cloneErr != nil {
		src, openErr := s.OpenBlob(sha)
		if openErr != nil {
			return "", nil, openErr
		}
		dst, createErr := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if createErr != nil {
			_ = src.Close()
			return "", nil, createErr
		}
		_, copyErr := io.Copy(dst, src)
		if closeErr := dst.Close(); copyErr == nil {
			copyErr = closeErr
		}
		if closeErr := src.Close(); copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil {
			return "", nil, copyErr
		}
	}
	if mode == 0 {
		mode = 0o644
	}
	if err := os.Chmod(name, mode); err != nil {
		return "", nil, err
	}
	fi, err = os.Lstat(name)
	return name, fi, err
}

func (s *Store) collectCachedBlobs(keep map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range s.stateCaches {
		for _, cached := range entry.data {
			if cached.Blob != "" {
				keep[cached.Blob] = true
			}
		}
	}
}

// PruneLocalBlobs removes blobs not reachable from current state or journal history.
func (s *Store) PruneLocalBlobs(extraKeep map[string]bool, minAge time.Duration) (removed int, freedBytes int64, err error) {
	keep := make(map[string]bool, len(extraKeep))
	for blob := range extraKeep {
		keep[blob] = true
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, 0, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name != "state.json" && !(strings.HasPrefix(name, "state-") && strings.HasSuffix(name, ".json")) {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(s.dir, name))
		if readErr != nil {
			return 0, 0, readErr
		}
		var cache map[string]CachedFile
		if json.Unmarshal(raw, &cache) != nil {
			continue
		}
		for _, cached := range cache {
			if cached.Blob != "" {
				keep[cached.Blob] = true
			}
		}
	}
	s.collectCachedBlobs(keep)
	fold, err := s.ReplayFold()
	if err != nil {
		return 0, 0, err
	}
	for _, state := range fold.Target {
		if state.Blob != "" {
			keep[state.Blob] = true
		}
	}
	ops, err := s.AllOps()
	if err != nil {
		return 0, 0, err
	}
	for _, op := range ops {
		if op.Blob != "" {
			keep[op.Blob] = true
		}
		if op.Base != "" {
			keep[op.Base] = true
		}
	}
	err = filepath.WalkDir(filepath.Join(s.dir, "blobs"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".bdrive-tmp-") {
			return nil
		}
		info, statErr := entry.Info()
		if statErr != nil {
			return statErr
		}
		if minAge > 0 && time.Since(info.ModTime()) < minAge {
			return nil
		}
		if keep[entry.Name()] {
			return nil
		}
		if removeErr := os.Remove(path); removeErr != nil {
			return removeErr
		}
		removed++
		freedBytes += info.Size()
		return nil
	})
	return removed, freedBytes, err
}

func (s *Store) PutBlobBytes(b []byte) (string, int64, error) {
	return s.PutBlobReader(strings.NewReader(string(b)))
}

func (s *Store) OpenBlob(sum string) (*os.File, error) {
	return os.Open(s.BlobPath(sum))
}

// ---- journals ----
func (s *Store) JournalPath(device string) string {
	return filepath.Join(s.dir, "journal", device+".jsonl")
}

func (s *Store) DeviceOps(device string) ([]journal.Op, error) {
	return journal.ReadFile(s.JournalPath(device))
}

// Devices lists device IDs that have a local journal copy.
func (s *Store) Devices() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "journal"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			out = append(out, strings.TrimSuffix(e.Name(), ".jsonl"))
		}
	}
	sort.Strings(out)
	return out, nil
}

// AllOps returns the union of every journal known locally.
func (s *Store) AllOps() ([]journal.Op, error) {
	devs, err := s.Devices()
	if err != nil {
		return nil, err
	}
	var all []journal.Op
	for _, d := range devs {
		ops, err := s.DeviceOps(d)
		if err != nil {
			return nil, fmt.Errorf("journal %s: %w", d, err)
		}
		all = append(all, ops...)
	}
	return all, nil
}

// ---- materialized-state cache (state.json) ----

// CachedFile records what beardrive last wrote to / observed in the working folder
// for a path. Size+MTimeNS make change detection cheap; Blob ties it back to
// content. The cache is per mount (one volume can be materialized into
// several folders, each with its own stat fingerprints).
type CachedFile struct {
	Blob    string `json:"blob"`
	Size    int64  `json:"size"`
	Mode    uint32 `json:"mode"`
	MTimeNS int64  `json:"mtime_ns"`
	Dev     uint64 `json:"dev,omitempty"`
	Ino     uint64 `json:"ino,omitempty"`
	// Link is the target of a symbolic link. When set, Blob is empty: the
	// cached entry describes a link, not content, and change detection also
	// compares this so retargeting a link to a same-length path is still seen.
	Link string `json:"link,omitempty"`
}

// mountStatePath names a per-mount state file. The mount id comes from a
// folder's .bdrive/config.json — a file that travels with the folder — so it is
// checked here as well as where it is read: prefix+mountID+".json" is joined
// onto the volume dir, and a separator in it puts the cache (and everything
// keyed the same way) wherever its author chose.
func (s *Store) mountStatePath(prefix, mountID string) (string, error) {
	if mountID == "" || mountID == "." || mountID == ".." || strings.ContainsAny(mountID, `/\`) {
		return "", fmt.Errorf("invalid mount id %q", mountID)
	}
	return filepath.Join(s.dir, prefix+mountID+".json"), nil
}

// cachePath names the per-mount materialization cache (state-<mount>.json).
func (s *Store) cachePath(mountID string) (string, error) {
	return s.mountStatePath("state-", mountID)
}

// HasCache reports whether a materialization cache file already exists on
// disk for mountID. A mount with no cache file has never completed a sync
// cycle in its working folder, even when it attaches to a volume store
func (s *Store) HasCache(mountID string) bool {
	p, err := s.cachePath(mountID)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// cleanRel reports whether rel is a clean, relative, in-volume path.
func cleanRel(rel string) bool {
	if rel == "" || rel == "." || rel == ".." || path.IsAbs(rel) || filepath.IsAbs(rel) {
		return false
	}
	return path.Clean(rel) == rel && !strings.HasPrefix(rel, "../")
}

// ---- sync state (sync.json) ----

// Access records how the hub answered this device on the last cycle that
// reached it. It is persisted so `bdrive status` — which never runs a cycle —
// can report a degraded state, and so the daemon can log a transition once
// instead of on every tick.
const (
	AccessOK       = ""          // normal read+write sync
	AccessReadOnly = "read-only" // pushes refused: pull-only
	AccessNone     = "no-access" // pulls refused too: sync paused
)

type SyncState struct {
	Remote    string `json:"remote,omitempty"`
	Lamport   int64  `json:"lamport"`
	PushedOps int64  `json:"pushed_ops"`       // how many of our own ops the remote has
	Access    string `json:"access,omitempty"` // "", "read-only", or "no-access"
	// AccessReason is the hub's own words for the last refusal. Without it every
	// 403 renders as the same "read-only (pull only)" line, and the hub's most
	// actionable answer — "this device is not registered to your account on this
	// hub; run `bdrive login`" — reached nobody: the one state a user cannot
	// diagnose from the outside was the one the CLI summarized away.
	AccessReason string `json:"access_reason,omitempty"`

	// IgnoreAccepted is the .bdriveignore text whose scan scope THIS device has
	// accepted, and IgnorePulled is the text a peer's version last wrote here.
	// Together they tell a locally-authored rule change from one that arrived
	// over the wire, which is what keeps a teammate's `!` from widening what
	// leaves this disk. See syncer.Filter.SkipUp.
	IgnoreAccepted string `json:"ignore_accepted,omitempty"`
	IgnorePulled   string `json:"ignore_pulled,omitempty"`

	// ReadOnly are the folder prefixes this account may sync down but must
	// never journal a change to, as the hub last reported them
	// (remote.Scoper). Persisted so an offline cycle keeps honouring the last
	// answer instead of falling back to "everything is writable" and building
	// up a journal the hub will refuse the moment the network returns.
	ReadOnly []string `json:"read_only,omitempty"`
	// Denied are prefixes this account may not read at all. Persisted for the
	// same reason ReadOnly is: an unreachable hub must not widen what this
	// device is willing to journal.
	Denied []string `json:"denied,omitempty"`
	// ScopeTag identifies the answer ReadOnly came from. It changes only when
	// what THIS account can see changes.
	ScopeTag string `json:"scope_tag,omitempty"`
}

func (s *Store) LoadSync() (SyncState, error) {
	var st SyncState
	if err := readJSON(filepath.Join(s.dir, "sync.json"), &st); err != nil {
		return st, err
	}
	return st, nil
}

func (s *Store) SaveSync(st SyncState) error {
	return WriteJSONAtomic(filepath.Join(s.dir, "sync.json"), st)
}

// ---- locking ----

// Lock takes an exclusive flock for the volume, serializing sync cycles
// between the daemon and one-shot commands. Blocks until acquired.
func (s *Store) Lock() (func() error, error) {
	f, err := os.OpenFile(filepath.Join(s.dir, "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() error {
		defer f.Close()
		return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}, nil
}

// TryLock attempts a non-blocking exclusive flock for the volume.
// If another process holds the lock, it returns (nil, nil).
func (s *Store) TryLock() (func() error, error) {
	f, err := os.OpenFile(filepath.Join(s.dir, "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, nil
		}
		return nil, err
	}
	return func() error {
		defer f.Close()
		return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}, nil
}

func lockSpool(path string) (func() error, error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() error {
		unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		closeErr := f.Close()
		if unlockErr != nil {
			return unlockErr
		}
		return closeErr
	}, nil
}

// ---- small JSON helpers ----

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return json.Unmarshal(data, v)
}

// WriteJSONAtomic writes v as JSON via temp-file + rename. 0600 for the same
// reason as the journals: these files list a private project's paths and the
// accounts that touched them, inside a 0755 $BDRIVE_HOME.
func WriteJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, data, 0o600)
}

// WriteFileAtomic writes data via a temp file in the same directory + rename.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bdrive-tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
