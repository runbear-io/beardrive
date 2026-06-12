// Package store manages a volume's local on-disk state: a content-addressed
// blob store, the per-device journals, and small JSON state files. Everything
// a volume needs works offline; the remote is only used to exchange blobs and
// journals.
//
// Layout under <sfs home>/volumes/<volume>/:
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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/runbear-io/sfs/internal/journal"
)

type Store struct {
	dir string
}

func Open(dir string) (*Store, error) {
	for _, d := range []string{dir, filepath.Join(dir, "blobs"), filepath.Join(dir, "journal"), filepath.Join(dir, "tmp")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{dir: dir}, nil
}

func (s *Store) Dir() string    { return s.dir }
func (s *Store) tmpDir() string { return filepath.Join(s.dir, "tmp") }

// ---- blobs ----

func (s *Store) BlobPath(sum string) string {
	return filepath.Join(s.dir, "blobs", sum[:2], sum)
}

func (s *Store) HasBlob(sum string) bool {
	if len(sum) < 3 {
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

func (s *Store) AppendOps(device string, ops []journal.Op) error {
	return journal.Append(s.JournalPath(device), ops)
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

// CachedFile records what sfs last wrote to / observed in the working folder
// for a path. Size+MTimeNS make change detection cheap; Blob ties it back to
// content.
type CachedFile struct {
	Blob    string `json:"blob"`
	Size    int64  `json:"size"`
	Mode    uint32 `json:"mode"`
	MTimeNS int64  `json:"mtime_ns"`
}

func (s *Store) LoadCache() (map[string]CachedFile, error) {
	out := map[string]CachedFile{}
	if err := readJSON(filepath.Join(s.dir, "state.json"), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) SaveCache(c map[string]CachedFile) error {
	return WriteJSONAtomic(filepath.Join(s.dir, "state.json"), c)
}

// ---- sync state (sync.json) ----

type SyncState struct {
	Lamport   int64 `json:"lamport"`
	PushedOps int64 `json:"pushed_ops"` // how many of our own ops the remote has
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

// WriteJSONAtomic writes v as JSON via temp-file + rename.
func WriteJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, data, 0o644)
}

// WriteFileAtomic writes data via a temp file in the same directory + rename.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".sfs-tmp-*")
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
